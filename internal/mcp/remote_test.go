package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

const remoteToken = "тестовый-токен-42"

// remoteHandler — сервер-демон в миниатюре: Streamable HTTP над сервером
// SDK и проверка Bearer-токена перед ним. Токен сверяется до SDK, как это
// сделал бы демон: чужой запрос не должен даже открыть сессию.
func remoteHandler(s *sdk.Server, token string, bad *atomic.Int64) http.Handler {
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, nil)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			bad.Add(1)
			http.Error(w, "нужен токен", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// remoteServe поднимает сервер на адресе; пустой адрес — любой свободный.
// Тот же адрес после остановки — «демон перезапущен».
func remoteServe(t *testing.T, addr string, h http.Handler) *httptest.Server {
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
	t.Cleanup(func() { remoteStop(srv) })
	return srv
}

// remoteStop — остановка как при падении: поток событий (GET) открыт всё
// соединение, и вежливый Close ждал бы его вечно.
func remoteStop(srv *httptest.Server) {
	srv.CloseClientConnections()
	srv.Close()
}

func remoteClient(t *testing.T, r *rig, url, token string) *Client {
	t.Helper()
	c := NewClient(Options{Dial: HTTPDialer(url, token, nil), Want: tools.Fingerprint(r.local), Logger: quiet()})
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRemoteConnectAndCall(t *testing.T) {
	r := newRig(t, nil)
	var bad atomic.Int64
	srv := remoteServe(t, "", remoteHandler(r.srv.SDK(), remoteToken, &bad))
	c := remoteClient(t, r, srv.URL, remoteToken) // без пути: /mcp добавится

	reg, fresh, err := c.Registry(context.Background())
	if err != nil || !fresh {
		t.Fatalf("подключение: %v", err)
	}
	if len(c.Tools()) != len(tools.SourceTools) {
		t.Fatalf("инструментов %d", len(c.Tools()))
	}
	args := json.RawMessage(`{"scientific_name":"Lynx lynx"}`)
	lt, _ := tools.MustRegistry(r.local...).Get("match_taxon")
	want, _ := lt.Call(context.Background(), args)
	mt, _ := reg.Get("match_taxon")
	if got, err := mt.Call(context.Background(), args); err != nil || got != want {
		t.Fatalf("вызов: %v\n%s\n%s", err, got, want)
	}
	// Ошибка инструмента по HTTP — тем же текстом, что в процессе.
	rt, _ := reg.Get("read_wikipedia")
	if _, err := rt.Call(context.Background(), json.RawMessage(`{}`)); err == nil || strings.HasPrefix(err.Error(), "MCP-сервер недоступен") {
		t.Fatalf("ошибка инструмента: %v", err)
	}

	st := c.State()
	if st.Status != StatusReady || st.Conn == nil || st.Conn.PID != 0 || st.Conn.Addr != srv.URL+"/mcp" {
		t.Fatalf("состояние: %+v %+v", st, st.Conn)
	}
	if raw, _ := json.Marshal(st); strings.Contains(string(raw), remoteToken) {
		t.Fatalf("токен в состоянии: %s", raw)
	}
	info, err := c.ServerInfo(context.Background())
	if err != nil || info.Calls["match_taxon"] != 1 {
		t.Fatalf("server_info: %v %+v", err, info)
	}
	if bad.Load() != 0 {
		t.Fatalf("запросов без токена: %d", bad.Load())
	}

	// Close не висит, хотя поток событий сервера ещё открыт.
	done := make(chan error, 1)
	go func() { done <- c.Close() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close повис")
	}
	if _, err := mt.Call(context.Background(), args); err == nil || !errors.Is(err, ErrClosed) {
		t.Fatalf("вызов после Close: %v", err)
	}
}

func TestRemoteBadToken(t *testing.T) {
	r := newRig(t, nil)
	var bad atomic.Int64
	srv := remoteServe(t, "", remoteHandler(r.srv.SDK(), remoteToken, &bad))

	for _, token := range []string{"", "чужой"} {
		c := remoteClient(t, r, srv.URL+"/mcp", token)
		_, err := c.Connect(context.Background())
		if !errors.Is(err, ErrUnauthorized) || err.Error() != "MCP-сервер отверг токен (401): задай MCP_TOKEN" {
			t.Fatalf("токен %q: %v", token, err)
		}
		if st := c.State(); st.Status != StatusDead || st.Reason != ErrUnauthorized.Error() {
			t.Fatalf("токен %q: состояние %+v", token, st)
		}
		if token != "" && strings.Contains(err.Error(), token) {
			t.Fatalf("токен в тексте ошибки: %v", err)
		}
	}
	if bad.Load() == 0 {
		t.Fatal("сервер не видел запросов с плохим токеном")
	}
}

// Демон остановлен и поднят заново на том же адресе: вызов, пока его нет, —
// ошибка словами; следующий после подъёма — новая сессия.
func TestRemoteRestart(t *testing.T) {
	r := newRig(t, nil)
	var bad atomic.Int64
	first := remoteServe(t, "", remoteHandler(r.srv.SDK(), remoteToken, &bad))
	addr := first.Listener.Addr().String()
	c := remoteClient(t, r, "http://"+addr, remoteToken)
	reg := mustRegistry(t, c)
	mt, _ := reg.Get("match_taxon")
	args := json.RawMessage(`{"scientific_name":"Lynx lynx"}`)
	if _, err := mt.Call(context.Background(), args); err != nil {
		t.Fatalf("первый вызов: %v", err)
	}

	remoteStop(first)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := mt.Call(ctx, args); err == nil || !strings.HasPrefix(err.Error(), "MCP-сервер недоступен: ") {
		t.Fatalf("вызов без сервера: %v", err)
	}
	if st := c.State(); st.Status != StatusDead {
		t.Fatalf("без сервера статус %q", st.Status)
	}

	// Новый обработчик — новый «процесс»: прежней сессии он не знает.
	remoteServe(t, addr, remoteHandler(r.srv.SDK(), remoteToken, &bad))
	if out, err := mt.Call(ctx, args); err != nil || !strings.Contains(out, "2435240") {
		t.Fatalf("после перезапуска: %v %s", err, out)
	}
	if st := c.State(); st.Status != StatusReady || st.Conn.N != 2 || st.Conn.Addr != "http://"+addr+"/mcp" {
		t.Fatalf("после перезапуска: %+v %+v", st, st.Conn)
	}
	// Тот же набор — тот же реестр: ход, державший его, не заметил подмены.
	if again := mustRegistry(t, c); again != reg {
		t.Fatal("перезапуск заменил реестр")
	}

	// Kill без процесса рвёт соединение, следующий вызов его поднимает.
	if err := c.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitStatus(t, c, StatusDead)
	if _, err := mt.Call(ctx, args); err != nil || c.State().Conn.N != 3 {
		t.Fatalf("после Kill: %v %+v", err, c.State().Conn)
	}
}

// Демон падает, пока клиент просто держит соединение: клиент замечает это
// сам (поток событий оборвался, прежней сессии новый сервер не знает), без
// вызова.
func TestRemoteNoticesRestart(t *testing.T) {
	r := newRig(t, nil)
	var bad atomic.Int64
	first := remoteServe(t, "", remoteHandler(r.srv.SDK(), remoteToken, &bad))
	addr := first.Listener.Addr().String()
	c := remoteClient(t, r, addr, remoteToken) // без схемы и пути
	mustRegistry(t, c)
	remoteStop(first)
	remoteServe(t, addr, remoteHandler(r.srv.SDK(), remoteToken, &bad))

	deadline := time.Now().Add(15 * time.Second)
	for c.State().Status != StatusDead {
		if time.Now().After(deadline) {
			t.Fatalf("клиент не заметил перезапуска: %+v", c.State())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := c.Connect(context.Background()); err != nil || c.State().Conn.N != 2 {
		t.Fatalf("переподключение: %v %+v", err, c.State().Conn)
	}
}

func TestRemoteEndpoint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://127.0.0.1:8766", "http://127.0.0.1:8766/mcp"},
		{"http://127.0.0.1:8766/", "http://127.0.0.1:8766/mcp"},
		{"127.0.0.1:8766", "http://127.0.0.1:8766/mcp"},
		{"http://host:1/other", "http://host:1/other"},
		{"https://host/mcp", "https://host/mcp"},
	}
	for _, tc := range cases {
		if got, err := remoteEndpoint(tc.in); err != nil || got != tc.want {
			t.Errorf("%s: %q %v", tc.in, got, err)
		}
	}
	for _, bad := range []string{"", "ftp://host", "http://", "http://user:pass@host"} {
		if _, err := remoteEndpoint(bad); err == nil {
			t.Errorf("%q принят", bad)
		}
	}
	// Плохой адрес — ошибка подключения, а не паника при создании.
	c := NewClient(Options{Dial: HTTPDialer("ftp://x", "", nil), Logger: quiet()})
	defer c.Close()
	if _, err := c.Connect(context.Background()); err == nil || !strings.Contains(err.Error(), "схема") {
		t.Fatalf("плохой адрес: %v", err)
	}
}
