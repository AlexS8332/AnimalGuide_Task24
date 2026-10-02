package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testToken = "секретный-токен"

// testDaemon — сервер-демон в миниатюре: инструмент на чтение и «платный»
// run_now без ReadOnlyHint, Streamable HTTP и проверка Bearer-токена.
type testDaemon struct {
	srv    *httptest.Server
	runNow atomic.Int64
	search atomic.Int64
}

func newTestDaemon(t *testing.T) *testDaemon {
	t.Helper()
	d := &testDaemon{}
	s := mcp.NewServer(&mcp.Implementation{Name: "test-daemon", Version: "0.1"}, nil)
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)
	text := func(s string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
	}
	s.AddTool(&mcp.Tool{Name: "search_wikipedia", Description: "Поиск.", InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			d.search.Add(1)
			return text(`{"found":["Рысь"]}`), nil
		})
	s.AddTool(&mcp.Tool{Name: "run_now", Description: "Запустить задачу сейчас.", InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			d.runNow.Add(1)
			return text(`{"started":true}`), nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "нужен токен", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		d.srv.CloseClientConnections()
		d.srv.Close()
	})
	return d
}

// runHTTP — run с -url, вывод перехвачен.
func runHTTP(t *testing.T, url, token string, o options) (string, error) {
	t.Helper()
	tg, err := httpTarget(url, token)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var runErr error
	out := captureStdout(t, func() { runErr = run(ctx, tg, o) })
	return out, runErr
}

// captureStdout — run печатает прямо в os.Stdout: подменяем его трубой.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stdout = saved }()
	fn()
	w.Close()
	return <-done
}

func TestHTTPList(t *testing.T) {
	d := newTestDaemon(t)
	out, err := runHTTP(t, d.srv.URL, testToken, options{noDemo: true, schemas: true})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	for _, want := range []string{"Streamable HTTP", d.srv.URL + "/mcp", "test-daemon", "Инструменты (2)", "search_wikipedia", "схема:"} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, testToken) {
		t.Fatalf("токен напечатан:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		marked := strings.Contains(line, writeMark)
		switch {
		case strings.Contains(line, ". run_now") && !marked:
			t.Errorf("run_now без пометки: %q", line)
		case strings.Contains(line, ". search_wikipedia") && marked:
			t.Errorf("search_wikipedia с пометкой: %q", line)
		}
	}
	if d.runNow.Load() != 0 || d.search.Load() != 0 {
		t.Fatal("-no-demo что-то вызвал")
	}
}

func TestHTTPCall(t *testing.T) {
	d := newTestDaemon(t)
	// Явный вызов «платного» инструмента разрешён: человек его попросил.
	out, err := runHTTP(t, d.srv.URL+"/mcp", testToken, options{call: "run_now"})
	if err != nil || !strings.Contains(out, `"started": true`) || d.runNow.Load() != 1 {
		t.Fatalf("-call run_now: %v, вызовов %d\n%s", err, d.runNow.Load(), out)
	}
	out, err = runHTTP(t, d.srv.URL, testToken, options{call: "search_wikipedia", args: "query=рысь"})
	if err != nil || !strings.Contains(out, "Рысь") || d.search.Load() != 1 {
		t.Fatalf("-call search_wikipedia: %v\n%s", err, out)
	}
}

// Проверочные вызовы по HTTP не трогают инструменты без ReadOnlyHint, даже
// если такой попал в список: демон не должен запускать задачу от проверки.
func TestHTTPDemoSkipsWrite(t *testing.T) {
	d := newTestDaemon(t)
	saved := demoCalls
	t.Cleanup(func() { demoCalls = saved })
	demoCalls = append(append(demoCalls[:0:0], saved...), struct {
		Tool string
		Args map[string]any
	}{Tool: "run_now"})

	out, err := runHTTP(t, d.srv.URL, testToken, options{})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if d.runNow.Load() != 0 {
		t.Fatalf("demo вызвал run_now:\n%s", out)
	}
	if d.search.Load() != 1 {
		t.Fatalf("demo не вызвал search_wikipedia: %d\n%s", d.search.Load(), out)
	}
	if !strings.Contains(out, "run_now пропущен: меняет состояние") || !strings.Contains(out, "match_taxon пропущен: на сервере нет") {
		t.Fatalf("пропуски не объяснены:\n%s", out)
	}
}

func TestHTTPBadToken(t *testing.T) {
	d := newTestDaemon(t)
	for _, token := range []string{"", "чужой"} {
		_, err := runHTTP(t, d.srv.URL, token, options{noDemo: true})
		if err == nil || err.Error() != "MCP-сервер отверг токен (401): задай MCP_TOKEN" {
			t.Fatalf("токен %q: %v", token, err)
		}
	}
}

func TestRemoteEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:8766":  "http://127.0.0.1:8766/mcp",
		"127.0.0.1:8766/":        "http://127.0.0.1:8766/mcp",
		"https://host:1/other":   "https://host:1/other",
		"http://localhost:8766/": "http://localhost:8766/mcp",
	} {
		if got, err := remoteEndpoint(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"ftp://host", "http://", "http://u:p@host"} {
		if _, err := remoteEndpoint(bad); err == nil {
			t.Errorf("%q принят", bad)
		}
	}
}
