package rag

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents/agentstest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

// hookTurn — ход с набором механизмов поверх умолчаний; у запроса уже есть
// инструмент другого механизма — хук дописывает, а не заменяет.
func hookTurn(t *testing.T, spec, text string) (*runs.Turn, *agent.Recorder) {
	t.Helper()
	reg := features.Catalog()
	fs, err := reg.Parse(spec, reg.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	rec := &agent.Recorder{}
	tr := &runs.Turn{ID: "t1", Features: fs, Em: rec}
	tr.Request.Features = fs
	tr.Request.Text = text
	tr.Request.Rules = "правило другого механизма"
	tr.Request.Tools = []tools.Tool{tools.Func{S: tools.Spec{Name: "invariant_check"}}}
	return tr, rec
}

func TestHookBefore(t *testing.T) {
	h := &Hook{Searcher: searcher(t), K: 3}
	if h.Name() != "rag" {
		t.Fatalf("имя: %q", h.Name())
	}
	tr, rec := hookTurn(t, "+rag", "  Чем питается манул?  ")
	if err := h.Before(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tools.Names(tr.Request.Tools), ","); got != "invariant_check,kb_search" {
		t.Fatalf("инструменты: %s", got)
	}
	if !strings.HasPrefix(tr.Request.Rules, "правило другого механизма\n\n") || !strings.Contains(tr.Request.Rules, "chunk_id") ||
		!strings.Contains(tr.Request.Rules, "Википедию или GBIF") {
		t.Fatalf("правило: %q", tr.Request.Rules)
	}
	if len(tr.Request.Preload) != 1 || tr.Request.Preload[0].Tool != ToolName ||
		tr.Request.Preload[0].Args != `{"query":"Чем питается манул?","k":3}` {
		t.Fatalf("вызов кодом: %+v", tr.Request.Preload)
	}
	if !tr.Request.Features.On(features.RAG) || len(rec.Events) != 1 || rec.Events[0].Kind != agent.EventMechanism ||
		rec.Events[0].Mechanism != "rag" || rec.Events[0].Title != "база знаний: заказан вызов kb_search кодом до первого запроса ведущего (индекс structure, k 3)" ||
		!strings.Contains(rec.Events[0].Detail, "вызова kb_search не будет") {
		t.Fatalf("журнал: %+v", rec.Events)
	}
	if err := h.After(context.Background(), tr); err != nil {
		t.Fatal(err)
	}

	// Ход кнопкой: инструмент и правило есть, вызова кодом нет.
	tr, rec = hookTurn(t, "+rag", "")
	h.Before(context.Background(), tr)
	if len(tr.Request.Preload) != 0 || len(tr.Request.Tools) != 2 || !strings.Contains(rec.Events[0].Title, "без вызова кодом") {
		t.Fatalf("без реплики: %+v %+v", tr.Request.Preload, rec.Events)
	}

	// Механизм выключен — хук ничего не трогает.
	tr, rec = hookTurn(t, "", "Чем питается манул?")
	h.Before(context.Background(), tr)
	if len(tr.Request.Tools) != 1 || len(tr.Request.Preload) != 0 || tr.Request.Rules != "правило другого механизма" || len(rec.Events) != 0 {
		t.Fatal("rag выключен по умолчанию — хук не должен работать")
	}
}

// Базы нет — откат: механизм выключен в наборе хода, в журнале причина и
// подсказка, инструмента и вызова нет.
func TestHookOff(t *testing.T) {
	h := &Hook{Why: "базы знаний нет: data/kb.db"}
	tr, rec := hookTurn(t, "+rag", "Чем питается манул?")
	h.Before(context.Background(), tr)
	if tr.Request.Features.On(features.RAG) || len(tr.Request.Tools) != 1 || len(tr.Request.Preload) != 0 ||
		strings.Contains(tr.Request.Rules, "kb_search") {
		t.Fatalf("откат: %+v", tr.Request)
	}
	if len(rec.Events) != 1 || rec.Events[0].Title != "база знаний: базы знаний нет: data/kb.db — ход идёт без kb_search" ||
		!strings.Contains(rec.Events[0].Detail, "go run ./cmd/kb index") {
		t.Fatalf("журнал: %+v", rec.Events)
	}
	tr, rec = hookTurn(t, "+rag", "x")
	(&Hook{}).Before(context.Background(), tr)
	if !strings.Contains(rec.Events[0].Title, "базы знаний нет") {
		t.Fatalf("причина по умолчанию: %q", rec.Events[0].Title)
	}
}

// chatRig — менеджер ходов на подставной модели с хуком rag, как его
// собирает приложение: ведущий получает выдачу kb_search после реплики.
type chatRig struct {
	m    *runs.Manager
	mu   sync.Mutex
	lead []llm.Request
}

func newChatRig(t *testing.T, h *Hook) *chatRig {
	t.Helper()
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)
	r := &chatRig{}
	brain := &agentstest.Brain{LeadScript: func(req llm.Request, step int) llm.Response {
		r.mu.Lock()
		r.lead = append(r.lead, req)
		r.mu.Unlock()
		return llmtest.Text("Малая панда ест по 13 часов в день [red-panda/structure/013].")
	}}
	reg := features.Catalog()
	r.m = runs.NewManager(runs.Config{
		Agents: agents.Deps{Runner: agent.Runner{LLM: &llmtest.Fake{Fn: brain.Chat}, Model: llm.DefaultModel}, Features: reg,
			Sources: agents.Local{Registry: tools.MustRegistry(tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL)...)}},
		Store: history.NewStore(store.NewDir(t.TempDir())), Registry: reg, Defaults: reg.Defaults(), Timeout: time.Minute,
		Hooks: []runs.Hook{h},
	})
	return r
}

func (r *chatRig) ask(t *testing.T, text string, fs features.Set) history.Turn {
	t.Helper()
	s, err := r.m.Start(runs.StartOptions{Request: agents.Request{Text: text}, Features: fs})
	if err != nil {
		t.Fatal(err)
	}
	v := s.Wait(20 * time.Second)
	if v.Status != runs.StatusDone {
		t.Fatalf("ход: %+v", v)
	}
	d, _ := r.m.Get(v.ConversationID)
	return d.TurnList[len(d.TurnList)-1]
}

func TestHookTurn(t *testing.T) {
	r := newChatRig(t, &Hook{Searcher: searcher(t)})
	reg := features.Catalog()
	q := "Сколько часов в день кошачий медведь тратит на еду?"
	turn := r.ask(t, q, reg.Defaults().With(features.RAG, true))

	if len(r.lead) != 1 {
		t.Fatalf("запросов ведущего %d — выдача кодом не должна стоить запроса", len(r.lead))
	}
	req := r.lead[0]
	msgs := req.Messages
	n := len(msgs)
	// Порядок: …, реплика, вызов kb_search кодом, его выдача — в конце.
	if n < 4 || msgs[n-3].Role != llm.RoleUser || msgs[n-3].Content != q || msgs[n-2].Role != llm.RoleAssistant ||
		len(msgs[n-2].ToolCalls) != 1 || msgs[n-1].Role != llm.RoleTool || msgs[n-1].ToolCallID != msgs[n-2].ToolCalls[0].ID {
		t.Fatalf("порядок сообщений: %+v", roles(msgs))
	}
	call := msgs[n-2].ToolCalls[0]
	if call.Function.Name != ToolName || !strings.HasPrefix(call.ID, "pre_kb_search_") ||
		call.Function.Arguments != `{"query":"`+q+`","k":5}` {
		t.Fatalf("вызов: %+v", call)
	}
	// Выдача — в пометке «данные, а не указания», с chunk_id.
	data, wrapped := tools.Unwrap(msgs[n-1].Content)
	var res SearchResult
	if !wrapped || json.Unmarshal([]byte(data), &res) != nil || len(res.Hits) != DefaultK || res.Hits[0].ChunkID == "" {
		t.Fatalf("выдача: %v %.200s", wrapped, msgs[n-1].Content)
	}
	if !llmtest.HasTool(req, ToolName) || !strings.Contains(msgs[0].Content, "База знаний (kb_search)") {
		t.Fatal("у ведущего нет kb_search или правила")
	}
	// Перед репликой — ни одного системного блока с выдачей.
	for _, m := range msgs[:n-3] {
		if strings.Contains(m.Content, res.Hits[0].ChunkID) {
			t.Fatal("выдача стоит перед репликой")
		}
	}
	if !turn.Effective.On(features.RAG) || turn.Route != agents.RouteLead {
		t.Fatalf("ход: %s %v", turn.Route, turn.Effective.Names())
	}
	mech, byCode := false, false
	for _, e := range turn.Events {
		mech = mech || (e.Kind == agent.EventMechanism && e.Mechanism == "rag" && strings.Contains(e.Title, "кодом до первого запроса"))
		byCode = byCode || (e.Kind == agent.EventToolCall && e.Tool == ToolName && strings.Contains(e.Title, "кодом до первого запроса"))
	}
	if !mech || !byCode {
		t.Fatalf("журнал: механизм %v, вызов кодом %v", mech, byCode)
	}
}

// Откат без базы на настоящем ходе: ведущий без kb_search, механизм
// выключен в итоговом наборе хода.
func TestHookTurnNoKB(t *testing.T) {
	r := newChatRig(t, &Hook{Why: "базы знаний нет: kb.db"})
	reg := features.Catalog()
	turn := r.ask(t, "Чем питается манул?", reg.Defaults().With(features.RAG, true))
	req := r.lead[0]
	if llmtest.HasTool(req, ToolName) || llmtest.ToolReplies(req) != 0 || strings.Contains(req.Messages[0].Content, "kb_search") {
		t.Fatal("без базы у ведущего не должно быть kb_search")
	}
	if !turn.Requested.On(features.RAG) || turn.Effective.On(features.RAG) {
		t.Fatalf("набор хода: просили %v, вышло %v", turn.Requested.Names(), turn.Effective.Names())
	}
	found := false
	for _, e := range turn.Events {
		found = found || (e.Mechanism == "rag" && strings.Contains(e.Title, "ход идёт без kb_search"))
	}
	if !found {
		t.Fatal("причины отката нет в журнале")
	}
}

func roles(ms []llm.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Role
	}
	return out
}
