package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// factsDaemon — подставной демон фактов: три инструмента, которые трогает
// приложение без участия человека.
func factsDaemon(t *testing.T, token string) *httptest.Server {
	t.Helper()
	fn := func(v string) tools.CallFunc {
		return func(context.Context, json.RawMessage) (string, error) { return v, nil }
	}
	schema := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`)
	ts := []tools.Tool{
		tools.Func{S: tools.Spec{Name: "facts_get", Description: "Выпуск о виде.", Parameters: schema}, Fn: fn(`{"id":1}`)},
		tools.Func{S: tools.Spec{Name: "facts_latest", Description: "Последние выпуски.", Parameters: schema}, Fn: fn(`{"issues":[]}`)},
		tools.Func{S: tools.Spec{Name: "schedule_status", Description: "Расписание.", Parameters: schema}, Fn: fn(`{"jobs":[]}`)},
	}
	srv := httptest.NewServer(mcp.NewServer(ts, mcp.ServerOptions{}).HTTPHandler(mcp.HTTPOptions{Token: token}))
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	return srv
}

// Адрес демона: флаг (и пустой — «выключено»), иначе TRIVIA_SERVER, иначе
// адрес по умолчанию.
func TestFactsServerOption(t *testing.T) {
	t.Setenv("TRIVIA_SERVER", "")
	withArgs(t)
	if o := parseFlags(); o.factsSet || o.facts() != feed.DefaultServer {
		t.Fatalf("умолчание: %+v %q", o, o.facts())
	}
	t.Setenv("TRIVIA_SERVER", "http://127.0.0.1:9001")
	withArgs(t)
	if o := parseFlags(); o.facts() != "http://127.0.0.1:9001" {
		t.Fatalf("из окружения: %q", o.facts())
	}
	withArgs(t, "-facts-server", "127.0.0.1:9002")
	if o := parseFlags(); o.facts() != "127.0.0.1:9002" {
		t.Fatalf("флаг главнее окружения: %q", o.facts())
	}
	withArgs(t, "-facts-server", "")
	if o := parseFlags(); !o.factsSet || o.facts() != "" {
		t.Fatalf("пустой флаг: %+v", o)
	}
}

// wire собирает приложение с механизмом: ведущий хода с trivia получает
// инструменты демона и правило о выпусках; ход без trivia — нет. Стенд
// (механизм выключен по умолчанию) демон не трогает.
func TestWireTrivia(t *testing.T) {
	srv := factsDaemon(t, "токен-стенда")
	t.Setenv("MCP_TOKEN", "токен-стенда")
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "Здравствуйте!"}, FinishReason: "stop"}, nil
	}}
	registry := features.Catalog()
	defaults := registry.Defaults()
	if defaults.On(features.Trivia) {
		t.Fatal("trivia включён по умолчанию — стенд пошёл бы к демону")
	}
	o := options{window: history.DefaultWindow, keep: history.DefaultKeepToolRunes, factsServer: srv.URL, factsSet: true}
	a, err := wire(o, registry, defaults, agent.Runner{LLM: fake, Model: llm.DefaultModel}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Feed == nil || a.Feed.Remote.Server() != srv.URL || len(a.Feed.Extension()) != 1 {
		t.Fatalf("Feed: %+v", a.Feed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if st := a.Feed.Remote.Status(ctx); st.Conn != feed.ConnOK {
		t.Fatalf("состояние демона: %+v", st)
	}

	lead := func(spec string) (names []string, system string) {
		fs, err := registry.Parse(spec, defaults)
		if err != nil {
			t.Fatal(err)
		}
		n := len(fake.Requests)
		s, err := a.Manager.Start(runs.StartOptions{Features: fs,
			Request: agents.Request{Kind: agents.KindMessage, Text: "Привет! Что нового интересного?"}})
		if err != nil {
			t.Fatal(err)
		}
		if v := s.Wait(20 * time.Second); v.Status != runs.StatusDone {
			t.Fatalf("ход %s: %+v", spec, v)
		}
		for _, req := range fake.Requests[n:] {
			for _, td := range req.Tools {
				if td.Function.Name == "open_card" { // запрос ведущего
					for _, td := range req.Tools {
						names = append(names, td.Function.Name)
					}
					return names, req.Messages[0].Content
				}
			}
		}
		t.Fatalf("ход %s: запроса ведущего нет", spec)
		return nil, ""
	}
	names, system := lead("+trivia")
	joined := "," + strings.Join(names, ",") + ","
	if !strings.Contains(joined, ",facts_get,facts_latest,") || strings.Contains(joined, "schedule_status") {
		t.Fatalf("инструменты ведущего с trivia: %s", joined)
	}
	if !strings.Contains(system, "created_at") {
		t.Fatalf("правило о выпусках не в системном промпте:\n%s", system)
	}
	names, system = lead("")
	if strings.Contains(strings.Join(names, ","), "facts_") || strings.Contains(system, "created_at") {
		t.Fatalf("без trivia: %v", names)
	}
}
