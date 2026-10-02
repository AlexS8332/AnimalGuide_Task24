package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/daemon"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

func TestCheckListen(t *testing.T) {
	for _, c := range []struct {
		addr, token string
		ok          bool
	}{
		{"127.0.0.1:8766", "", true},
		{"localhost:8766", "", true},
		{"[::1]:8766", "", true},
		{"127.0.0.2:8766", "", true},
		{"0.0.0.0:8766", "", false},
		{":8766", "", false},
		{"192.168.1.5:8766", "", false},
		{"myhost:8766", "", false},
		{"0.0.0.0:8766", "t", true},
		{":8766", "t", true},
		{"127.0.0.1", "", false}, // без порта
		{"127.0.0.1:", "", false},
	} {
		err := checkListen(c.addr, c.token)
		if (err == nil) != c.ok {
			t.Errorf("checkListen(%q, token=%v) = %v", c.addr, c.token != "", err)
		}
	}
	if err := checkListen("0.0.0.0:8766", ""); err == nil || !strings.Contains(err.Error(), "MCP_TOKEN") {
		t.Errorf("отказ без подсказки про токен: %v", err)
	}
}

func TestDaemonFlagsCheck(t *testing.T) {
	f := daemonFlags{http: "127.0.0.1:8766"}
	if err := f.check(); err != nil || !f.on {
		t.Errorf("-http без -daemon: err=%v on=%v", err, f.on)
	}
	f = daemonFlags{http: "0.0.0.0:8766"}
	if err := f.check(); err == nil {
		t.Error("0.0.0.0 без токена принят")
	}
	f = daemonFlags{http: "127.0.0.1:8766", run: "issue"}
	if err := f.check(); err == nil {
		t.Error("-http вместе с -run принят")
	}
	f = daemonFlags{on: true}
	if err := f.check(); err != nil {
		t.Errorf("демон без HTTP: %v", err)
	}
}

// Порядок остановки: сначала HTTP (начатый запрос успевает ответить), и
// только потом гаснет контекст планировщика.
func TestServeUntilOrder(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var schedStopped atomic.Bool
	inHandler := make(chan struct{})
	release := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inHandler)
		<-release
		if schedStopped.Load() {
			http.Error(w, "планировщик остановлен раньше HTTP", 500)
			return
		}
		w.Write([]byte("ok"))
	})}
	run := func(ctx context.Context) error {
		<-ctx.Done()
		schedStopped.Store(true)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveUntil(ctx, srv, ln, run) }()

	resp := make(chan int, 1)
	go func() {
		r, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			resp <- 0
			return
		}
		r.Body.Close()
		resp <- r.StatusCode
	}()
	<-inHandler
	cancel()
	time.Sleep(50 * time.Millisecond) // Shutdown уже ждёт начатый запрос
	close(release)
	if c := <-resp; c != http.StatusOK {
		t.Errorf("начатый запрос: %d", c)
	}
	if err := <-done; err != nil {
		t.Errorf("serveUntil: %v", err)
	}
	if !schedStopped.Load() {
		t.Error("планировщик не остановлен")
	}
}

func TestServeUntilSchedulerFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler()}
	boom := errors.New("база недоступна")
	err = serveUntil(context.Background(), srv, ln, func(context.Context) error { return boom })
	if !errors.Is(err, boom) {
		t.Errorf("serveUntil = %v", err)
	}
}

type noLLM struct{}

func (noLLM) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("модель в тесте не вызывается")
}

// Точка подключения инструментов демона: что вернёт daemonTools, то и
// окажется на HTTP-сервере рядом с источниками и MDD; server_info несёт
// состояние демона.
func TestDaemonServerTools(t *testing.T) {
	d, err := daemon.Open(t.Context(), daemon.Config{DataDir: t.TempDir(), LLM: noLLM{}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var got daemon.Service
	old := daemonTools
	defer func() { daemonTools = old }()
	daemonTools = func(s daemon.Service) []tools.Tool {
		got = s
		return []tools.Tool{
			tools.Func{S: tools.Spec{Name: "facts_fake", Description: "Чтение."},
				Fn: func(context.Context, json.RawMessage) (string, error) { return `[]`, nil }},
			tools.Func{S: tools.Spec{Name: "run_fake", Description: "Платно.", Write: true},
				Fn: func(context.Context, json.RawMessage) (string, error) { return `{}`, nil }},
		}
	}
	srv, n := daemonServer(d, sources{}, slog.New(slog.DiscardHandler))
	if got != daemon.Service(d) {
		t.Error("daemonTools получил не демон")
	}
	ts := httptest.NewServer(srv.HTTPHandler(mcp.HTTPOptions{}))
	defer ts.Close()

	c := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(t.Context(), &sdk.StreamableClientTransport{Endpoint: ts.URL + mcp.MCPPath, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]*sdk.Tool{}
	for _, tl := range list.Tools {
		names[tl.Name] = tl
	}
	for _, want := range []string{"search_wikipedia", "mdd_get", "facts_fake", "run_fake", mcp.InfoTool} {
		if names[want] == nil {
			t.Errorf("нет инструмента %s", want)
		}
	}
	if len(list.Tools) != n+1 {
		t.Errorf("инструментов %d, ждали %d + server_info", len(list.Tools), n)
	}
	if a := names["run_fake"].Annotations; a == nil || a.ReadOnlyHint {
		t.Errorf("run_fake помечен только для чтения: %+v", a)
	}

	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: mcp.InfoTool})
	if err != nil || res.IsError {
		t.Fatalf("server_info: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), `"mode":"daemon"`) || !strings.Contains(string(raw), `"budget_usd":0.5`) {
		t.Errorf("server_info без состояния демона: %s", raw)
	}
}
