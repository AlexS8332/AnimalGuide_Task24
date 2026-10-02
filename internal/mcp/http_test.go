package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// httpTools — подставные инструменты: чтение, запись (как run_now) и
// инструмент, который всегда падает.
func httpTools() []tools.Tool {
	schema := json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)
	return []tools.Tool{
		tools.Func{S: tools.Spec{Name: "echo", Description: "Повторяет text.", Parameters: schema},
			Fn: func(_ context.Context, args json.RawMessage) (string, error) {
				var a struct{ Text string }
				if err := json.Unmarshal(args, &a); err != nil {
					return "", err
				}
				return `{"text":` + strconvQuote(a.Text) + `}`, nil
			}},
		tools.Func{S: tools.Spec{Name: "spend", Description: "Платное действие.", Write: true},
			Fn: func(context.Context, json.RawMessage) (string, error) { return `{"ok":true}`, nil }},
		tools.Func{S: tools.Spec{Name: "fail", Description: "Всегда ошибка."},
			Fn: func(context.Context, json.RawMessage) (string, error) {
				return "", errors.New("источник недоступен")
			}},
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// bearer — транспорт, добавляющий токен к каждому запросу.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connectHTTP(t *testing.T, url, token string) (*sdk.ClientSession, error) {
	t.Helper()
	tr := &sdk.StreamableClientTransport{Endpoint: url + MCPPath, MaxRetries: -1}
	if token != "" {
		tr.HTTPClient = &http.Client{Transport: bearer{token}}
	}
	c := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(t.Context(), tr, nil)
	if err == nil {
		t.Cleanup(func() { cs.Close() })
	}
	return cs, err
}

func TestHTTPTransport(t *testing.T) {
	srv := NewServer(httpTools(), ServerOptions{
		State: func(context.Context) any { return map[string]any{"budget_usd": 0.5} },
	})
	ts := httptest.NewServer(srv.HTTPHandler(HTTPOptions{Token: "s3cret"}))
	defer ts.Close()

	cs, err := connectHTTP(t, ts.URL, "s3cret")
	if err != nil {
		t.Fatal("initialize:", err)
	}
	if got := cs.InitializeResult().ServerInfo; got.Name != ServerName || got.Version != Version {
		t.Errorf("serverInfo = %+v", got)
	}

	list, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal("tools/list:", err)
	}
	byName := map[string]*sdk.Tool{}
	for _, tl := range list.Tools {
		byName[tl.Name] = tl
	}
	if len(byName) != 4 {
		t.Fatalf("инструментов %d, ждали 4 (три + server_info)", len(byName))
	}
	if a := byName["echo"].Annotations; !a.ReadOnlyHint || !a.IdempotentHint || a.OpenWorldHint == nil || !*a.OpenWorldHint {
		t.Errorf("echo: аннотации чтения %+v", a)
	}
	if a := byName["spend"].Annotations; a.ReadOnlyHint || a.IdempotentHint ||
		a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || !*a.OpenWorldHint {
		t.Errorf("spend: аннотации записи %+v", a)
	}

	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "ёж"}})
	if err != nil || res.IsError {
		t.Fatalf("echo: %v %+v", err, res)
	}
	if got := res.Content[0].(*sdk.TextContent).Text; got != `{"text":"ёж"}` {
		t.Errorf("echo = %s", got)
	}

	res, err = cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "fail"})
	if err != nil {
		t.Fatal("ошибка инструмента пришла сбоем протокола:", err)
	}
	if !res.IsError || res.Content[0].(*sdk.TextContent).Text != "источник недоступен" {
		t.Errorf("fail: %+v", res)
	}

	res, err = cs.CallTool(t.Context(), &sdk.CallToolParams{Name: InfoTool})
	if err != nil || res.IsError {
		t.Fatalf("server_info: %v %+v", err, res)
	}
	var info Info
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	if info.Calls["echo"] != 1 || info.Errors["fail"] != 1 || info.State == nil {
		t.Errorf("server_info: %s", raw)
	}
}

func TestHTTPToken(t *testing.T) {
	srv := NewServer(httpTools(), ServerOptions{})
	ts := httptest.NewServer(srv.HTTPHandler(HTTPOptions{Token: "s3cret"}))
	defer ts.Close()

	post := func(auth string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+MCPPath,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post(""); c != http.StatusUnauthorized {
		t.Errorf("без токена: %d", c)
	}
	if c := post("Bearer wrong"); c != http.StatusUnauthorized {
		t.Errorf("неверный токен: %d", c)
	}
	if c := post("Basic s3cret"); c != http.StatusUnauthorized {
		t.Errorf("не Bearer: %d", c)
	}
	if c := post("Bearer s3cret"); c != http.StatusOK {
		t.Errorf("верный токен: %d", c)
	}

	if _, err := connectHTTP(t, ts.URL, ""); err == nil {
		t.Error("клиент без токена подключился")
	}
	if _, err := connectHTTP(t, ts.URL, "wrong"); err == nil {
		t.Error("клиент с неверным токеном подключился")
	}

	// /healthz — без токена.
	resp, err := http.Get(ts.URL + HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d %v", resp.StatusCode, err)
	}
	if h.Status != "ok" || h.Version != Version || h.Tools != 3 {
		t.Errorf("healthz = %+v", h)
	}
}

// Без токена — открытый сервер (так его слушают только на loopback).
func TestHTTPNoToken(t *testing.T) {
	ts := httptest.NewServer(NewServer(httpTools(), ServerOptions{}).HTTPHandler(HTTPOptions{}))
	defer ts.Close()
	cs, err := connectHTTP(t, ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ListTools(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	// Запрос со страницы чужого сайта отклоняется.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+MCPPath, strings.NewReader(`{}`))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("кросс-сайтовый запрос: %d", resp.StatusCode)
	}
}

// TestHTTPLive — живая проверка запущенного сервера:
//
//	MCP_LIVE_URL=http://127.0.0.1:8799 [MCP_TOKEN=...] go test ./internal/mcp -run Live -v
func TestHTTPLive(t *testing.T) {
	url := os.Getenv("MCP_LIVE_URL")
	if url == "" {
		t.Skip("MCP_LIVE_URL не задан")
	}
	cs, err := connectHTTP(t, strings.TrimRight(url, "/"), os.Getenv("MCP_TOKEN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("сервер %s %s", cs.InitializeResult().ServerInfo.Name, cs.InitializeResult().ServerInfo.Version)
	list, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range list.Tools {
		a := tl.Annotations
		t.Logf("%-18s readOnly=%v idempotent=%v", tl.Name, a != nil && a.ReadOnlyHint, a != nil && a.IdempotentHint)
	}
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: InfoTool})
	if err != nil || res.IsError {
		t.Fatalf("server_info: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	t.Logf("server_info: %s", raw)
}
