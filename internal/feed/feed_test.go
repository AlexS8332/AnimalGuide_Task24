package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

const testToken = "тестовый-токен-фактов"

// fakeDaemon — подставной демон: те же имена инструментов, что у
// animals-mcp, ответы — простые JSON, аргументы запоминаются. Сервер и
// HTTP-обработчик настоящие (mcp.NewServer, HTTPHandler с токеном): так
// проверяется тот же путь, что с живым демоном.
type fakeDaemon struct {
	mu    sync.Mutex
	calls []fakeCall
	// schedule — сколько раз спросили schedule_status.
	schedule atomic.Int64
	// slow — задержка каждого вызова.
	slow time.Duration
}

type fakeCall struct {
	Tool string
	Args map[string]any
}

func (d *fakeDaemon) tool(name, desc string, fn func(args map[string]any) (any, error)) tools.Tool {
	return tools.Func{
		S: tools.Spec{Name: name, Description: desc,
			Parameters: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer"}},"additionalProperties":true}`)},
		Fn: func(ctx context.Context, raw json.RawMessage) (string, error) {
			args := map[string]any{}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
			}
			d.mu.Lock()
			d.calls = append(d.calls, fakeCall{name, args})
			slow := d.slow
			d.mu.Unlock()
			if slow > 0 {
				select {
				case <-time.After(slow):
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			v, err := fn(args)
			if err != nil {
				return "", err
			}
			return tools.Result(v)
		},
	}
}

func (d *fakeDaemon) last() fakeCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.calls) == 0 {
		return fakeCall{}
	}
	return d.calls[len(d.calls)-1]
}

func (d *fakeDaemon) count(tool string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		if c.Tool == tool {
			n++
		}
	}
	return n
}

func (d *fakeDaemon) tools() []tools.Tool {
	echo := func(args map[string]any) (any, error) { return map[string]any{"args": args}, nil }
	return []tools.Tool{
		d.tool("facts_latest", "Последние выпуски «Интересных фактов».", echo),
		d.tool("facts_get", "Один выпуск целиком.", func(args map[string]any) (any, error) {
			if args["species"] == "Рысь" {
				return nil, errors.New("выпусков о виде «Рысь» ещё не было")
			}
			return map[string]any{"id": 3, "created_at": "24.09.2026 11:30", "args": args}, nil
		}),
		d.tool("facts_search", "Поиск по выпускам.", echo),
		d.tool("summary_get", "Сводка.", echo),
		d.tool("summary_build", "Собрать сводку.", echo),
		d.tool("schedule_status", "Расписание.", func(map[string]any) (any, error) {
			d.schedule.Add(1)
			return map[string]any{"jobs": []string{"issue", "summary", "mdd"}}, nil
		}),
		d.tool("run_now", "Запустить задание.", func(args map[string]any) (any, error) {
			if args["job"] == "summary" {
				return nil, errors.New("задание summary уже идёт")
			}
			return map[string]any{"run": args["job"]}, nil
		}),
	}
}

// handler — демон с токеном.
func (d *fakeDaemon) handler(token string) http.Handler {
	return mcp.NewServer(d.tools(), mcp.ServerOptions{Version: "18.0.0"}).HTTPHandler(mcp.HTTPOptions{Token: token})
}

// serve поднимает демон на адресе; пустой адрес — любой свободный. Тот же
// адрес после остановки — «демон перезапущен».
func serve(t *testing.T, addr string, h http.Handler) *httptest.Server {
	t.Helper()
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var (
		ln  net.Listener
		err error
	)
	// Порт только что освободился: на Windows он иногда отдаётся не сразу.
	for deadline := time.Now().Add(5 * time.Second); ; {
		if ln, err = net.Listen("tcp", addr); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("адрес %s: %v", addr, err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(func() { stop(srv) })
	return srv
}

func stop(srv *httptest.Server) {
	srv.CloseClientConnections()
	srv.Close()
}

// deadAddr — адрес, где никто не слушает.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

func newRemote(t *testing.T, server, token string) *Remote {
	t.Helper()
	r := NewRemote(server, token, nil)
	t.Cleanup(r.Close)
	return r
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ------------------------------------------------------------ Remote

func TestStatusOK(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(testToken))
	r := newRemote(t, srv.URL, testToken)
	st := r.Status(ctxT(t))
	if st.Conn != ConnOK || st.Version != "18.0.0" || st.Server != srv.URL || st.Reason != "" || st.Hint != "" {
		t.Fatalf("состояние: %+v", st)
	}
	if !strings.Contains(string(st.Schedule), `"issue"`) || st.Checked.IsZero() {
		t.Fatalf("расписание: %s", st.Schedule)
	}
	if raw, _ := json.Marshal(st); strings.Contains(string(raw), testToken) {
		t.Fatalf("токен в состоянии: %s", raw)
	}
}

func TestStatusDown(t *testing.T) {
	r := newRemote(t, deadAddr(t), testToken)
	st := r.Status(ctxT(t))
	if st.Conn != ConnDown || st.Schedule != nil || st.Version != "" {
		t.Fatalf("состояние: %+v", st)
	}
	if !strings.Contains(st.Reason, "не отвечает") || !strings.Contains(st.Hint, "animals-mcp -http 127.0.0.1:") {
		t.Fatalf("причина и подсказка: %+v", st)
	}
}

func TestStatusDenied(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(testToken))
	for _, token := range []string{"", "чужой"} {
		r := newRemote(t, srv.URL, token)
		st := r.Status(ctxT(t))
		if st.Conn != ConnDenied || !strings.Contains(st.Hint, "MCP_TOKEN") || !strings.Contains(st.Reason, "401") {
			t.Fatalf("токен %q: %+v", token, st)
		}
		if token != "" && (strings.Contains(st.Reason, token) || strings.Contains(st.Hint, token)) {
			t.Fatalf("токен в состоянии: %+v", st)
		}
		if _, err := r.Call(ctxT(t), "facts_latest", nil); !errors.Is(err, ErrDenied) {
			t.Fatalf("вызов с токеном %q: %v", token, err)
		}
	}
}

func TestStatusOff(t *testing.T) {
	r := NewRemote("  ", "x", nil)
	if r.Configured() {
		t.Fatal("пустой адрес настроен")
	}
	st := r.Status(context.Background())
	if st.Conn != ConnOff || st.Hint == "" {
		t.Fatalf("состояние: %+v", st)
	}
	if _, err := r.Call(context.Background(), "facts_latest", nil); !errors.Is(err, ErrOff) {
		t.Fatalf("вызов: %v", err)
	}
	if _, err := r.Tools(context.Background(), "facts_get"); !errors.Is(err, ErrOff) {
		t.Fatalf("инструменты: %v", err)
	}
	r.Close()
}

// Состояние держится statusTTL: частый опрос окна не доходит до демона.
func TestStatusCache(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(""))
	r := newRemote(t, srv.URL, "")
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	r.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); r.Status(ctxT(t)) }()
	}
	wg.Wait()
	if n := d.schedule.Load(); n != 1 {
		t.Fatalf("одновременные опросы: schedule_status %d раз", n)
	}
	advance(4 * time.Second)
	r.Status(ctxT(t))
	if n := d.schedule.Load(); n != 1 {
		t.Fatalf("через 4 с: schedule_status %d раз", n)
	}
	advance(2 * time.Second)
	r.Status(ctxT(t))
	if n := d.schedule.Load(); n != 2 {
		t.Fatalf("через 6 с: schedule_status %d раз", n)
	}
	r.Forget()
	r.Status(ctxT(t))
	if n := d.schedule.Load(); n != 3 {
		t.Fatalf("после Forget: schedule_status %d раз", n)
	}
	// Прерванная вызывающим проверка в кэш не попадает.
	advance(10 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if st := r.Status(ctx); st.Conn != ConnDown {
		t.Fatalf("отменённая проверка: %+v", st)
	}
	if st := r.Status(ctxT(t)); st.Conn != ConnOK {
		t.Fatalf("после отменённой: %+v", st)
	}
}

func TestCall(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(testToken))
	r := newRemote(t, srv.URL+"/mcp", testToken)
	ctx := ctxT(t)

	raw, err := r.Call(ctx, "facts_get", map[string]any{"id": 3})
	if err != nil || !json.Valid(raw) || !strings.Contains(string(raw), `"created_at"`) {
		t.Fatalf("вызов: %v %s", err, raw)
	}
	if c := d.last(); c.Tool != "facts_get" || c.Args["id"] != float64(3) {
		t.Fatalf("аргументы: %+v", c)
	}
	// Аргументы json.RawMessage и nil.
	if _, err := r.Call(ctx, "facts_latest", json.RawMessage(`{"limit":2}`)); err != nil || d.last().Args["limit"] != float64(2) {
		t.Fatalf("RawMessage: %v %+v", err, d.last())
	}
	if _, err := r.Call(ctx, "facts_latest", nil); err != nil || len(d.last().Args) != 0 {
		t.Fatalf("nil: %v %+v", err, d.last())
	}

	// Ошибка инструмента — ToolError с текстом демона, соединение живо.
	_, err = r.Call(ctx, "facts_get", map[string]any{"species": "Рысь"})
	var te *ToolError
	if !errors.As(err, &te) || te.Text != "выпусков о виде «Рысь» ещё не было" || errors.Is(err, ErrDown) {
		t.Fatalf("ошибка инструмента: %v", err)
	}
	// Нет такого инструмента — отказ демона, а не «не отвечает».
	if _, err := r.Call(ctx, "no_such_tool", nil); !errors.As(err, &te) {
		t.Fatalf("незнакомый инструмент: %v", err)
	}
	if _, err := r.Call(ctx, "facts_latest", nil); err != nil {
		t.Fatalf("после отказа: %v", err)
	}
}

func TestCallDown(t *testing.T) {
	r := newRemote(t, deadAddr(t), "")
	_, err := r.Call(ctxT(t), "facts_latest", nil)
	if !errors.Is(err, ErrDown) || !strings.HasPrefix(err.Error(), "демон «Интересных фактов» не отвечает") {
		t.Fatalf("без демона: %v", err)
	}
	if r.Hint(err) == "" || r.Hint(errors.New("x")) != "" {
		t.Fatal("подсказка")
	}
	// Демон не успел: ErrDown по сроку вызывающего.
	d := &fakeDaemon{slow: 2 * time.Second}
	srv := serve(t, "", d.handler(""))
	r = newRemote(t, srv.URL, "")
	if _, err := r.Tools(ctxT(t), "facts_get"); err != nil { // подключиться заранее
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := r.Call(ctx, "facts_latest", nil); !errors.Is(err, ErrDown) || !strings.Contains(err.Error(), "вовремя") {
		t.Fatalf("медленный демон: %v", err)
	}
}

// Демон перезапущен на том же адресе: вызов, пока его нет, — ErrDown;
// следующий после подъёма идёт по новому соединению без вмешательства.
func TestReconnectAfterRestart(t *testing.T) {
	d := &fakeDaemon{}
	first := serve(t, "", d.handler(testToken))
	addr := first.Listener.Addr().String()
	r := newRemote(t, addr, testToken) // без схемы
	ctx := ctxT(t)
	if _, err := r.Call(ctx, "facts_latest", nil); err != nil {
		t.Fatalf("первый вызов: %v", err)
	}
	stop(first)
	if _, err := r.Call(ctx, "facts_latest", nil); !errors.Is(err, ErrDown) {
		t.Fatalf("без демона: %v", err)
	}
	if st := r.Status(ctx); st.Conn != ConnDown {
		t.Fatalf("состояние без демона: %+v", st)
	}
	serve(t, addr, d.handler(testToken))
	if _, err := r.Call(ctx, "facts_latest", nil); err != nil {
		t.Fatalf("после перезапуска: %v", err)
	}
	r.Forget()
	if st := r.Status(ctx); st.Conn != ConnOK {
		t.Fatalf("состояние после перезапуска: %+v", st)
	}
}

// cutter — обрывает соединение на следующем tools/call, не пропуская его
// к демону: так выглядит сбой транспорта посреди жизни соединения.
type cutter struct {
	next http.Handler
	cut  atomic.Int64 // сколько вызовов оборвать
}

func (c *cutter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && c.cut.Load() > 0 {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "tools/call") && c.cut.Add(-1) >= 0 {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				conn.Close()
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	c.next.ServeHTTP(w, r)
}

// Соединение оборвалось, а демон на месте: чтение переподключается само,
// одной попыткой. Платный вызов не повторяется — сбой мог случиться уже
// после того, как демон его принял.
func TestCallRetriesOnce(t *testing.T) {
	d := &fakeDaemon{}
	c := &cutter{next: d.handler("")}
	srv := serve(t, "", c)
	r := newRemote(t, srv.URL, "")
	ctx := ctxT(t)
	if _, err := r.Call(ctx, "facts_latest", nil); err != nil {
		t.Fatal(err)
	}
	c.cut.Store(1)
	before := d.count("facts_latest")
	if _, err := r.Call(ctx, "facts_latest", nil); err != nil {
		t.Fatalf("чтение после обрыва: %v", err)
	}
	if d.count("facts_latest") != before+1 {
		t.Fatalf("дошло до демона: %d", d.count("facts_latest")-before)
	}
	// Два обрыва подряд — одна попытка, затем ErrDown.
	c.cut.Store(2)
	if _, err := r.Call(ctx, "facts_latest", nil); !errors.Is(err, ErrDown) {
		t.Fatalf("два обрыва: %v", err)
	}

	c.cut.Store(1)
	if _, err := r.Call(ctx, "run_now", map[string]any{"job": "issue"}); !errors.Is(err, ErrDown) {
		t.Fatalf("платный после обрыва: %v", err)
	}
	if c.cut.Load() != 0 || d.count("run_now") != 0 {
		t.Fatalf("платный вызов повторён: осталось обрывов %d, вызовов %d", c.cut.Load(), d.count("run_now"))
	}
	if _, err := r.Call(ctx, "run_now", map[string]any{"job": "issue"}); err != nil || d.count("run_now") != 1 {
		t.Fatalf("платный следующим вызовом: %v", err)
	}
}

func TestTools(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(testToken))
	r := newRemote(t, srv.URL, testToken)
	ctx := ctxT(t)
	list, err := r.Tools(ctx, LeadTools...)
	if err != nil || len(list) != 2 {
		t.Fatalf("инструменты: %v %d", err, len(list))
	}
	for i, name := range LeadTools {
		s := list[i].Spec()
		if s.Name != name || !s.Untrusted || s.Via != tools.ViaMCP || s.Description == "" || !json.Valid(s.Parameters) {
			t.Fatalf("описание %s: %+v", name, s)
		}
	}
	if list[0].Spec().Description != "Один выпуск целиком." {
		t.Fatalf("описание не от демона: %q", list[0].Spec().Description)
	}
	out, err := list[0].Call(ctx, json.RawMessage(`{"id":3}`))
	if err != nil || !strings.Contains(out, `"created_at"`) {
		t.Fatalf("вызов через инструмент: %v %s", err, out)
	}
	if _, err := list[0].Call(ctx, json.RawMessage(`{"species":"Рысь"}`)); err == nil || err.Error() != "выпусков о виде «Рысь» ещё не было" {
		t.Fatalf("ошибка через инструмент: %v", err)
	}
	if _, err := r.Tools(ctx, "facts_get", "facts_nope"); err == nil || !strings.Contains(err.Error(), "facts_nope") {
		t.Fatalf("незнакомое имя: %v", err)
	}
}

func TestClose(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(""))
	r := NewRemote(srv.URL, "", nil)
	if _, err := r.Call(ctxT(t), "facts_latest", nil); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if _, err := r.Call(ctxT(t), "facts_latest", nil); !errors.Is(err, ErrDown) {
		t.Fatalf("после Close: %v", err)
	}
}

// Одновременные вызовы делят одно соединение (гоняется с -race).
func TestConcurrentCalls(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(""))
	r := newRemote(t, srv.URL, "")
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 3 {
			case 0:
				_, err := r.Call(ctxT(t), "facts_latest", map[string]any{"limit": i + 1})
				errs <- err
			case 1:
				_, err := r.Tools(ctxT(t), LeadTools...)
				errs <- err
			default:
				if st := r.Status(ctxT(t)); st.Conn != ConnOK {
					errs <- fmt.Errorf("состояние %+v", st)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
