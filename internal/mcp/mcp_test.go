package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

// rig — подставные Википедия и GBIF, локальный набор инструментов и
// сервер над своим набором (как в жизни: у сервера свой кэш), соединённые
// в памяти. Каждое подключение клиента — новая серверная сессия того же
// сервера: так сервер «перезапускается», не теряя счётчиков.
type rig struct {
	wiki  *toolstest.Wiki
	gbif  *toolstest.GBIF
	local []tools.Tool
	srv   *Server

	mu       sync.Mutex
	sessions []*sdk.ServerSession
	dials    atomic.Int64
	failDial error
}

func newRig(t *testing.T, serverTools func([]tools.Tool) []tools.Tool) *rig {
	t.Helper()
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)
	r := &rig{wiki: wiki, gbif: gbif, local: tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL)}
	fetcher := tools.NewFetcher()
	ts := tools.LocalTools(fetcher, wiki.URL, gbif.URL)
	if serverTools != nil {
		ts = serverTools(ts)
	}
	r.srv = NewServer(ts, ServerOptions{WikiBase: wiki.URL, GBIFBase: gbif.URL, Fetcher: fetcher})
	return r
}

func (r *rig) dial(ctx context.Context) (sdk.Transport, *exec.Cmd, error) {
	r.dials.Add(1)
	r.mu.Lock()
	fail := r.failDial
	r.mu.Unlock()
	if fail != nil {
		return nil, nil, fail
	}
	ct, st := sdk.NewInMemoryTransports()
	ss, err := r.srv.SDK().Connect(ctx, st, nil)
	if err != nil {
		return nil, nil, err
	}
	r.mu.Lock()
	r.sessions = append(r.sessions, ss)
	r.mu.Unlock()
	return ct, nil, nil
}

// crash — сервер «падает»: последняя серверная сессия закрывается.
func (r *rig) crash() {
	r.mu.Lock()
	ss := r.sessions[len(r.sessions)-1]
	r.mu.Unlock()
	ss.Close()
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func (r *rig) client(t *testing.T, mod func(*Options)) *Client {
	t.Helper()
	o := Options{Dial: r.dial, Want: tools.Fingerprint(r.local), Logger: quiet()}
	if mod != nil {
		mod(&o)
	}
	c := NewClient(o)
	t.Cleanup(func() { c.Close() })
	return c
}

func mustRegistry(t *testing.T, c *Client) *tools.Registry {
	t.Helper()
	reg, _, err := c.Registry(context.Background())
	if err != nil {
		t.Fatalf("подключение: %v", err)
	}
	return reg
}

// waitStatus ждёт, пока клиент заметит конец соединения.
func waitStatus(t *testing.T, c *Client, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.State().Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("статус %q, ждали %q", c.State().Status, want)
}

func TestDefsByteEqual(t *testing.T) {
	r := newRig(t, nil)
	c := r.client(t, nil)
	reg := mustRegistry(t, c)
	remote := reg.Pick(tools.SourceTools...)

	a, _ := json.Marshal(tools.Defs(r.local))
	b, _ := json.Marshal(tools.Defs(remote))
	if !bytes.Equal(a, b) {
		t.Fatalf("описания разошлись:\nлокально %s\nчерез MCP %s", a, b)
	}
	if tools.Fingerprint(remote) != tools.Fingerprint(r.local) {
		t.Fatal("отпечатки разошлись")
	}
	for _, tl := range remote {
		s := tl.Spec()
		if !s.Untrusted || s.Via != tools.ViaMCP {
			t.Fatalf("%s: Untrusted=%v Via=%q", s.Name, s.Untrusted, s.Via)
		}
	}
	if len(c.Tools()) != len(tools.SourceTools) || c.ID() == "" {
		t.Fatalf("источник: %d инструментов, id %q", len(c.Tools()), c.ID())
	}
	st := c.State()
	if st.Status != StatusReady || st.Conn == nil || st.Conn.Server != ServerName || st.Conn.Protocol == "" || st.Fingerprint != st.Want {
		t.Fatalf("состояние: %+v", st)
	}
}

// Одни и те же вызовы на двух путях: результат и текст ошибки совпадают
// побайтно, включая битый JSON и аргументы не того типа.
func TestSameResultsAndErrors(t *testing.T) {
	r := newRig(t, nil)
	reg := mustRegistry(t, r.client(t, nil))
	local := tools.MustRegistry(r.local...)
	cases := []struct{ tool, args string }{
		{"search_wikipedia", `{"query":"рысь"}`},
		{"search_wikipedia", `{"query":"несуществующее"}`},
		{"search_wikipedia", `{}`},
		{"search_wikipedia", ``},
		{"search_wikipedia", `{"query":`},
		{"search_wikipedia", `[1,2]`},
		{"read_wikipedia", `{"title":"Рысь"}`},
		{"read_wikipedia", `{"title":"Обыкновенная рысь","section":"Размножение"}`},
		{"read_wikipedia", `{"title":"Обыкновенная рысь","section":"Нет такого"}`},
		{"read_wikipedia", `{"title":"Летучий кот"}`},
		{"read_wikipedia", `{"title":"Лесной кот"}`},
		{"match_taxon", `{"scientific_name":"Lynx lynx"}`},
		{"match_taxon", `{"scientific_name":"Felis venenosa"}`},
		{"match_taxon", `{"scientific_name":42}`},
		{"taxon_tree", `{"usage_key":2435240}`},
		{"taxon_tree", `{"usage_key":0}`},
		{"taxon_children", `{"usage_key":9703,"limit":2}`},
		{"vernacular_names", `{"usage_key":2435240}`},
		{"vernacular_names", `{"usage_key":2435240,"language":"eng"}`},
	}
	errs := 0
	for _, tc := range cases {
		lt, _ := local.Get(tc.tool)
		mt, _ := reg.Get(tc.tool)
		lo, le := lt.Call(context.Background(), json.RawMessage(tc.args))
		mo, me := mt.Call(context.Background(), json.RawMessage(tc.args))
		if lo != mo {
			t.Errorf("%s %s: результат\nлокально %s\nчерез MCP %s", tc.tool, tc.args, lo, mo)
		}
		if (le == nil) != (me == nil) || (le != nil && le.Error() != me.Error()) {
			t.Errorf("%s %s: ошибка\nлокально %v\nчерез MCP %v", tc.tool, tc.args, le, me)
		}
		if le != nil {
			errs++
		}
	}
	if errs < 4 {
		t.Fatalf("ошибок в таблице %d — проверка текстов ошибок ничего не проверила", errs)
	}
}

func TestDrift(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	// Описание одного инструмента поменялось — бинарник «устарел».
	r := newRig(t, func(ts []tools.Tool) []tools.Tool {
		s := ts[0].Spec()
		s.Description += " (старая версия)"
		ts[0] = tools.Func{S: s, Fn: ts[0].Call}
		return ts
	})
	c := r.client(t, func(o *Options) { o.Now = clock })
	_, err := c.Connect(context.Background())
	if !errors.Is(err, ErrDrift) {
		t.Fatalf("ждали ErrDrift, получили %v", err)
	}
	if st := c.State(); st.Status != StatusDrift || st.Until == nil {
		t.Fatalf("состояние: %+v", st)
	}
	// Пока срок не вышел, процесс заново не поднимается.
	before := r.dials.Load()
	if _, err := c.Connect(context.Background()); !errors.Is(err, ErrDrift) || r.dials.Load() != before {
		t.Fatalf("повтор: %v, подключений %d → %d", err, before, r.dials.Load())
	}
	// Вызов инструмента — ошибка словами, а не паника.
	_, err = c.call(context.Background(), "search_wikipedia", json.RawMessage(`{"query":"рысь"}`))
	if err == nil || !strings.HasPrefix(err.Error(), "MCP-сервер недоступен: ") {
		t.Fatalf("вызов при расхождении: %v", err)
	}

	// Сервер без одного из шести — тоже расхождение.
	r2 := newRig(t, func(ts []tools.Tool) []tools.Tool { return ts[:5] })
	if _, err := r2.client(t, nil).Connect(context.Background()); !errors.Is(err, ErrDrift) || !strings.Contains(err.Error(), "vernacular_names") {
		t.Fatalf("неполный набор: %v", err)
	}
}

func TestExtraToolsFiltered(t *testing.T) {
	r := newRig(t, func(ts []tools.Tool) []tools.Tool {
		return append(ts, tools.Func{
			S:  tools.Spec{Name: "drop_database", Description: "Удалить всё", Parameters: json.RawMessage(`{"type":"object"}`)},
			Fn: func(context.Context, json.RawMessage) (string, error) { return "удалено", nil },
		})
	})
	var logs bytes.Buffer
	c := r.client(t, func(o *Options) { o.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
	reg := mustRegistry(t, c)
	if names := reg.Names(); len(names) != 6 || reg.Has("drop_database") || reg.Has(InfoTool) {
		t.Fatalf("реестр: %v", names)
	}
	st := c.State()
	if !reflect.DeepEqual(st.Skipped, []string{"drop_database"}) || st.Status != StatusReady {
		t.Fatalf("пропущенные: %+v", st)
	}
	if !strings.Contains(logs.String(), "drop_database") {
		t.Fatalf("лишний инструмент не записан в журнал: %s", logs.String())
	}
}

// Сервер SDK обрабатывает tools/call асинхронно: параллельные вызовы
// специалистов не выстраиваются в очередь.
func TestParallelCalls(t *testing.T) {
	const delay = 200 * time.Millisecond
	var inflight, peak atomic.Int64
	r := newRig(t, func(ts []tools.Tool) []tools.Tool {
		for i := range ts {
			ts[i] = tools.Wrap(ts[i], func(ctx context.Context, args json.RawMessage, next tools.CallFunc) (string, error) {
				n := inflight.Add(1)
				defer inflight.Add(-1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(delay)
				return next(ctx, args)
			})
		}
		return ts
	})
	c := r.client(t, nil)
	reg := mustRegistry(t, c)
	tl, _ := reg.Get("read_wikipedia")
	const n = 6
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := tl.Call(context.Background(), json.RawMessage(`{"title":"Манул"}`))
			if err != nil || !strings.Contains(out, "Манул") {
				errs <- fmt.Errorf("%v: %s", err, out)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if peak.Load() < 2 || elapsed > n*delay/2 {
		t.Fatalf("вызовы шли последовательно: одновременно %d, всего %s", peak.Load(), elapsed)
	}
	st := c.State()
	for _, ti := range st.Tools {
		if ti.Name == "read_wikipedia" && (ti.Calls != n || ti.Errors != 0 || ti.Millis < int64(delay/time.Millisecond)) {
			t.Fatalf("счётчики клиента: %+v", ti)
		}
	}
	if info := r.srv.Stats(); info.Calls["read_wikipedia"] != n || info.TotalCalls != n {
		t.Fatalf("счётчики сервера: %+v", info)
	}
}

// Close посреди вызова: вызов получает ошибку словами, новый процесс не
// поднимается.
func TestCloseMidCall(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newRig(t, func(ts []tools.Tool) []tools.Tool {
		ts[0] = tools.Wrap(ts[0], func(ctx context.Context, args json.RawMessage, next tools.CallFunc) (string, error) {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return next(ctx, args)
		})
		return ts
	})
	defer close(release)
	c := r.client(t, nil)
	reg := mustRegistry(t, c)
	tl, _ := reg.Get("search_wikipedia")
	done := make(chan error, 1)
	go func() {
		_, err := tl.Call(context.Background(), json.RawMessage(`{"query":"рысь"}`))
		done <- err
	}()
	<-entered
	if err := c.Close(); err != nil {
		t.Logf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.HasPrefix(err.Error(), "MCP-сервер недоступен: ") {
			t.Fatalf("вызов посреди Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("вызов повис после Close")
	}
	dials := r.dials.Load()
	if _, err := tl.Call(context.Background(), json.RawMessage(`{"query":"рысь"}`)); !errors.Is(err, ErrClosed) {
		t.Fatalf("вызов после Close: %v", err)
	}
	if r.dials.Load() != dials || c.State().Status != StatusOff {
		t.Fatalf("после Close поднялось новое подключение: %d → %d, %+v", dials, r.dials.Load(), c.State())
	}
}

// Трекер карточки видит одно и то же на обоих путях: латынь, дерево,
// статья и разделы запоминаются одинаково.
func TestTrackerSame(t *testing.T) {
	r := newRig(t, nil)
	reg := mustRegistry(t, r.client(t, nil))
	calls := []struct{ tool, args string }{
		{"search_wikipedia", `{"query":"рысь"}`},
		{"read_wikipedia", `{"title":"Рысь"}`},
		{"read_wikipedia", `{"title":"Обыкновенная рысь","section":"Размножение"}`},
		{"match_taxon", `{"scientific_name":"Lynx lynx"}`},
		{"taxon_tree", `{"usage_key":2435240}`},
		{"taxon_children", `{"usage_key":9703}`},
		{"vernacular_names", `{"usage_key":2435240}`},
	}
	run := func(ts []tools.Tool) *card.Tracker {
		tr := card.NewTracker()
		r := tools.MustRegistry(tr.ObserveAll(ts)...)
		for i, c := range calls {
			tl, _ := r.Get(c.tool)
			if _, err := tl.Call(tools.WithCallID(context.Background(), fmt.Sprint("call-", i)), json.RawMessage(c.args)); err != nil {
				t.Fatalf("%s: %v", c.tool, err)
			}
		}
		return tr
	}
	a, b := run(r.local), run(reg.Pick(tools.SourceTools...))
	if a.Calls() != b.Calls() || a.Calls() == 0 {
		t.Fatalf("вызовов: %d и %d", a.Calls(), b.Calls())
	}
	ma, oka := a.Match("Lynx lynx")
	mb, okb := b.Match("Lynx lynx")
	if !oka || !okb || !reflect.DeepEqual(ma, mb) {
		t.Fatalf("сверка латыни: %+v / %+v", ma, mb)
	}
	ta, ida, _ := a.Tree(toolstest.KeyLynx)
	tb, idb, _ := b.Tree(toolstest.KeyLynx)
	if len(ta) == 0 || !reflect.DeepEqual(ta, tb) || ida != idb {
		t.Fatalf("дерево: %+v / %+v", ta, tb)
	}
	aa, _ := a.Article("Обыкновенная рысь")
	ab, _ := b.Article("Обыкновенная рысь")
	if aa.Title == "" || !reflect.DeepEqual(aa, ab) {
		t.Fatalf("статья: %+v / %+v", aa, ab)
	}
	sa, _ := a.SectionRead("Обыкновенная рысь", "Размножение")
	sb, _ := b.SectionRead("Обыкновенная рысь", "Размножение")
	if sa.Text == "" || !reflect.DeepEqual(sa, sb) {
		t.Fatalf("раздел: %+v / %+v", sa, sb)
	}
	ca, na, _, _ := a.Children(toolstest.KeyFelidae)
	cb, nb, _, _ := b.Children(toolstest.KeyFelidae)
	if len(ca) == 0 || !reflect.DeepEqual(ca, cb) || na != nb {
		t.Fatalf("дети: %+v / %+v", ca, cb)
	}
}

// Упавший сервер поднимается следующим вызовом; больше трёх перезапусков
// в минуту — сервер отложен на пять минут.
func TestRestartAndBackoff(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	r := newRig(t, nil)
	c := r.client(t, func(o *Options) { o.Now = clock })
	reg := mustRegistry(t, c)
	tl, _ := reg.Get("match_taxon")
	args := json.RawMessage(`{"scientific_name":"Lynx lynx"}`)
	for i := 1; i <= 3; i++ {
		r.crash()
		waitStatus(t, c, StatusDead)
		out, err := tl.Call(context.Background(), args)
		if err != nil || !strings.Contains(out, "2435240") {
			t.Fatalf("перезапуск %d: %v %s", i, err, out)
		}
		st := c.State()
		if st.Conn.N != i+1 || st.Restarts != i || st.Status != StatusReady {
			t.Fatalf("после перезапуска %d: %+v", i, st)
		}
		// Тот же реестр: ход, который держит его, продолжает работать.
		if again := mustRegistry(t, c); again != reg {
			t.Fatal("перезапуск с тем же набором заменил реестр")
		}
	}
	r.crash()
	waitStatus(t, c, StatusDead)
	_, err := tl.Call(context.Background(), args)
	if err == nil || !errors.Is(err, ErrUnavailable) || !strings.HasPrefix(err.Error(), "MCP-сервер недоступен: ") {
		t.Fatalf("четвёртый перезапуск за минуту: %v", err)
	}
	if st := c.State(); st.Status != StatusUnavailable || st.Until == nil {
		t.Fatalf("состояние: %+v", st)
	}
	dials := r.dials.Load()
	advance(time.Minute)
	if _, err := c.Connect(context.Background()); !errors.Is(err, ErrUnavailable) || r.dials.Load() != dials {
		t.Fatalf("до конца срока: %v", err)
	}
	advance(coolDown)
	if st := c.State(); st.Status != StatusDead {
		t.Fatalf("срок вышел, а статус %q", st.Status)
	}
	if out, err := tl.Call(context.Background(), args); err != nil || out == "" {
		t.Fatalf("после срока: %v", err)
	}
	// Ошибки самого подключения — статус dead и причина.
	r.mu.Lock()
	r.failDial = errors.New("нет бинарника")
	r.mu.Unlock()
	r.crash()
	waitStatus(t, c, StatusDead)
	if _, err := tl.Call(context.Background(), args); err == nil || !strings.Contains(err.Error(), "нет бинарника") {
		t.Fatalf("ошибка запуска: %v", err)
	}
	if st := c.State(); st.Status != StatusDead || st.Reason != "нет бинарника" {
		t.Fatalf("состояние: %+v", st)
	}
}

func TestKill(t *testing.T) {
	r := newRig(t, nil)
	c := r.client(t, nil)
	if err := c.Kill(); err == nil {
		t.Fatal("убит несуществующий сервер")
	}
	mustRegistry(t, c)
	if err := c.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitStatus(t, c, StatusDead)
}

func TestNoDialer(t *testing.T) {
	c := NewClient(Options{})
	if _, err := c.Connect(context.Background()); err == nil || !c.Started() {
		t.Fatalf("без Dial: %v", err)
	}
	if _, err := c.ServerInfo(context.Background()); err == nil {
		t.Fatal("server_info у незапущенного сервера")
	}
}

func TestServerInfo(t *testing.T) {
	r := newRig(t, nil)
	c := r.client(t, nil)
	reg := mustRegistry(t, c)
	tl, _ := reg.Get("taxon_tree")
	tl.Call(context.Background(), json.RawMessage(`{"usage_key":2435240}`))
	tl.Call(context.Background(), json.RawMessage(`{}`))
	info, err := c.ServerInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Server != ServerName || info.Version != Version || info.PID == 0 || info.Calls["taxon_tree"] != 2 ||
		info.Errors["taxon_tree"] != 1 || info.TotalCalls != 2 || info.HTTPRequests != r.gbif.Calls.Load() || info.HTTPRequests == 0 || len(info.Tools) != 6 || info.Sources[0].BaseURL != r.wiki.URL {
		t.Fatalf("server_info: %+v", info)
	}
}

// Выключенный механизм не запускает процесс вовсе; включённый — один раз
// на приложение, а журнал хода получает mcp.connect и mcp.tools.
func TestSwitch(t *testing.T) {
	r := newRig(t, nil)
	c := r.client(t, nil)
	local := tools.MustRegistry(r.local...)
	sw := &Switch{Local: local, Client: c, How: func() string { return "в памяти" }}
	off := features.Catalog().Defaults()
	if off.On(features.MCP) {
		t.Fatal("mcp включён по умолчанию")
	}

	rec := &agent.Recorder{}
	ctx := agent.WithEmitter(context.Background(), rec)
	reg, eff, why, err := sw.For(ctx, off)
	if err != nil || reg != local || why != "" || eff.On(features.MCP) {
		t.Fatalf("выключен: %v %q", err, why)
	}
	if r.dials.Load() != 0 || c.Started() || len(rec.Events) != 0 || c.State().Status != StatusOff {
		t.Fatalf("выключенный MCP запустил сервер: подключений %d, событий %d", r.dials.Load(), len(rec.Events))
	}

	on := off.With(features.MCP, true)
	for i := 0; i < 2; i++ {
		reg, eff, why, err = sw.For(ctx, on)
		if err != nil || reg == local || why != "" || !eff.On(features.MCP) {
			t.Fatalf("включён: %v %q", err, why)
		}
	}
	if r.dials.Load() != 1 {
		t.Fatalf("подключений %d, ждали одно на приложение", r.dials.Load())
	}
	kinds := rec.Kinds()
	if !reflect.DeepEqual(kinds, []string{EventConnect, EventTools, EventConnect, EventTools}) {
		t.Fatalf("события: %v", kinds)
	}
	first, second := rec.Events[0], rec.Events[2]
	if !strings.Contains(first.Title, "подключились") || !strings.Contains(second.Title, "открыто раньше") || !strings.Contains(first.Detail, "в памяти") {
		t.Fatalf("mcp.connect: %q / %q", first.Title, second.Title)
	}
	toolsEv := rec.Events[1]
	data, _ := json.Marshal(toolsEv.Data)
	if !strings.Contains(string(data), `"inputSchema"`) || !strings.Contains(toolsEv.Detail, "usage_key") || toolsEv.Via != tools.ViaMCP {
		t.Fatalf("mcp.tools: %s", data)
	}

	// Перезапуск виден в журнале следующего хода.
	r.crash()
	waitStatus(t, c, StatusDead)
	rec.Events = nil
	sw.For(ctx, on)
	if !strings.Contains(rec.Events[0].Title, "перезапуск №1") {
		t.Fatalf("перезапуск: %q", rec.Events[0].Title)
	}

	// Не подключились на старте хода — ход в процессе, mcp=false, причина.
	r.mu.Lock()
	r.failDial = errors.New("бинарник не найден")
	r.mu.Unlock()
	r.crash()
	waitStatus(t, c, StatusDead)
	reg, eff, why, err = sw.For(ctx, on)
	if err != nil || reg != local || eff.On(features.MCP) || !strings.Contains(why, "бинарник не найден") || !strings.Contains(why, "в процессе") {
		t.Fatalf("откат: %v %q", err, why)
	}

	// Без клиента — тоже откат, а не паника.
	if reg, eff, why, _ := (&Switch{Local: local}).For(ctx, on); reg != local || eff.On(features.MCP) || why == "" {
		t.Fatal("без клиента")
	}
}

func TestExtension(t *testing.T) {
	r := newRig(t, nil)
	c := r.client(t, nil)
	sw := &Switch{Local: tools.MustRegistry(r.local...), Client: c, How: func() string { return "в памяти" }}
	ext := sw.Extension()
	if len(ext) != 1 || ext[0].Prefix != "/api/mcp" {
		t.Fatalf("расширение: %+v", ext)
	}
	get := func(url string) View {
		rw := httptest.NewRecorder()
		ext[0].Handler.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, url, nil))
		if rw.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", url, rw.Code, rw.Body)
		}
		var v View
		if err := json.Unmarshal(rw.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	// Окно не поднимает сервер.
	if v := get("/api/mcp?server=1"); v.Status != StatusOff || v.Server != nil || r.dials.Load() != 0 || v.Binary != "в памяти" {
		t.Fatalf("до запуска: %+v", v)
	}
	reg := mustRegistry(t, c)
	tl, _ := reg.Get("search_wikipedia")
	tl.Call(context.Background(), json.RawMessage(`{"query":"манул"}`))
	v := get("/api/mcp?server=1")
	if v.Status != StatusReady || len(v.Tools) != 6 || v.Server == nil || v.Server.Calls["search_wikipedia"] != 1 {
		t.Fatalf("после вызова: %+v", v)
	}
	if v.Tools[0].Name != "search_wikipedia" || v.Tools[0].Calls != 1 || !json.Valid(v.Tools[0].InputSchema) {
		t.Fatalf("инструмент: %+v", v.Tools[0])
	}
	rw := httptest.NewRecorder()
	ext[0].Handler.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, "/api/mcp", nil))
	if rw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rw.Code)
	}
	if v := get("/api/mcp"); v.Server != nil {
		t.Fatal("счётчики сервера без запроса")
	}
	// Без клиента окно показывает «не запускался».
	rw = httptest.NewRecorder()
	(&Switch{}).Extension()[0].Handler.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/api/mcp", nil))
	if !strings.Contains(rw.Body.String(), `"status":"off"`) {
		t.Fatalf("без клиента: %s", rw.Body)
	}
}

func TestLogWriter(t *testing.T) {
	var buf bytes.Buffer
	w := &logWriter{log: slog.New(slog.NewTextHandler(&buf, nil))}
	io.WriteString(w, "первая строка\r\nвтор")
	io.WriteString(w, "ая\n\n")
	out := buf.String()
	if !strings.Contains(out, `msg="mcp: первая строка"`) || !strings.Contains(out, `msg="mcp: вторая"`) || strings.Count(out, "mcp:") != 2 {
		t.Fatalf("журнал: %s", out)
	}
}
