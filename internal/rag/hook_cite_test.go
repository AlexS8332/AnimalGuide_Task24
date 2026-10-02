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
	"github.com/AlexS8332/AnimalGuide_Task24/internal/collection"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/compiler"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

// citeChat — менеджер ходов с хуками rag и составителя подборки на
// подставной модели; ведущий отвечает сценарием lead (шаг — сколько
// ответов инструментов уже в ходе).
type citeChat struct {
	m    *runs.Manager
	mu   sync.Mutex
	lead []llm.Request
	comp []llm.Request
}

func newCiteChat(t *testing.T, h *Hook, lead func(req llm.Request, step int) llm.Response) *citeChat {
	t.Helper()
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)
	r := &citeChat{}
	brain := &agentstest.Brain{
		LeadScript: func(req llm.Request, step int) llm.Response {
			r.mu.Lock()
			r.lead = append(r.lead, req)
			r.mu.Unlock()
			return lead(req, step)
		},
		Compiler: func(req llm.Request, step int) llm.Response {
			r.mu.Lock()
			r.comp = append(r.comp, req)
			r.mu.Unlock()
			return llmtest.Text("План подборки: рысь и манул. Согласны?")
		},
	}
	reg := features.Catalog()
	dir := store.NewDir(t.TempDir())
	deps := agents.Deps{Runner: agent.Runner{LLM: &llmtest.Fake{Fn: brain.Chat}, Model: llm.DefaultModel}, Features: reg,
		Sources: agents.Local{Registry: tools.MustRegistry(tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL)...)}}
	r.m = runs.NewManager(runs.Config{Agents: deps, Store: history.NewStore(dir), Registry: reg, Defaults: reg.Defaults(),
		Timeout: time.Minute, Hooks: []runs.Hook{&compiler.Hook{Agents: deps, Store: collection.NewStore(dir)}, h}})
	return r
}

func (r *citeChat) ask(t *testing.T, text string, fs features.Set) history.Turn {
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

// issuedHits — выдача kb_search в запросе ведущего.
func issuedHits(t *testing.T, req llm.Request) []SearchHit {
	t.Helper()
	var res SearchResult
	out := agentstest.LastReply(req, ToolName)
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("выдача kb_search: %v %.200s", err, out)
	}
	return res.Hits
}

// verbatim — kb_answer с дословной цитатой из первого фрагмента выдачи.
func verbatim(t *testing.T, req llm.Request) Cited {
	hs := issuedHits(t, req)
	if len(hs) == 0 {
		t.Fatal("выдача пуста")
	}
	q := string([]rune(hs[0].Text)[:min(60, len([]rune(hs[0].Text)))])
	return cited("Ответ по базе знаний.", []string{hs[0].ChunkID}, [2]string{hs[0].ChunkID, q})
}

func citeSet(extra ...features.Name) features.Set {
	fs := features.Catalog().Defaults().With(features.RAG, true).With(features.RAGCite, true)
	for _, n := range extra {
		fs = fs.With(n, true)
	}
	return fs
}

// Ответ ведущего — текст CitedResult; первый kb_answer с пересказом
// получает отказ, второй — принят; итог — в Extras хода.
func TestHookCiteTurn(t *testing.T) {
	var first Cited
	r := newCiteChat(t, &Hook{Searcher: searcher(t)}, func(req llm.Request, step int) llm.Response {
		switch step {
		case 1: // после выдачи кодом
			first = verbatim(t, req)
			bad := first
			bad.Quotes = []CitedQuote{{ChunkID: first.Quotes[0].ChunkID, Text: "пересказ, которого во фрагменте нет"}}
			return llmtest.ToolCall(FinishName, string(args(bad)))
		default:
			return llmtest.ToolCall(FinishName, string(args(first)))
		}
	})
	turn := r.ask(t, "Сколько часов в день кошачий медведь тратит на еду?", citeSet())
	if len(r.lead) != 2 {
		t.Fatalf("запросов ведущего %d, ждали 2 (отказ и исправление)", len(r.lead))
	}
	req := r.lead[0]
	if !llmtest.HasTool(req, FinishName) || !strings.Contains(req.Messages[0].Content, "Если среди твоих инструментов есть kb_answer") ||
		strings.Contains(req.Messages[0].Content, "иди в Википедию") {
		t.Fatal("у ведущего нет kb_answer или правила rag.cite")
	}
	if reply := llmtest.LastToolReply(r.lead[1]); !strings.Contains(reply, "ответ не принят: цитата 1") {
		t.Fatalf("отказ не дошёл до модели: %s", reply)
	}
	var v CiteView
	if !turn.Extra(string(features.RAGCite), &v) || !v.Check.OK || v.Check.Rejects != 1 || len(v.Sources) != 1 || !v.Sources[0].Issued ||
		v.Sources[0].Title == "" {
		t.Fatalf("итог хода: %+v", v)
	}
	if turn.Reply != v.Text || !strings.HasPrefix(turn.Reply, "Ответ по базе знаний.\n\n**Источники:**\n[1] ") || !strings.Contains(turn.Reply, "**Цитаты:**") {
		t.Fatalf("ответ хода: %q", turn.Reply)
	}
	mech := false
	for _, e := range turn.Events {
		mech = mech || (e.Mechanism == string(features.RAGCite) && strings.Contains(e.Title, "только kb_answer"))
	}
	if !mech {
		t.Fatal("нет события механизма rag.cite")
	}
}

// Две ошибки подряд и третья — ответ принят «не проверено»; chunk_id из
// вызова kb_search моделью тоже считается выданным.
func TestHookCiteUnverifiedAndModelSearch(t *testing.T) {
	r := newCiteChat(t, &Hook{Searcher: searcher(t)}, func(req llm.Request, step int) llm.Response {
		if step == 1 {
			return llmtest.ToolCall(ToolName, `{"query":"харза масса самцов"}`)
		}
		c := verbatim(t, req) // фрагмент из вызова моделью
		c.Quotes[0].Text = "чужие слова, которых в выдаче нет"
		return llmtest.ToolCall(FinishName, string(args(c)))
	})
	turn := r.ask(t, "Чем питается манул?", citeSet())
	var v CiteView
	if !turn.Extra(string(features.RAGCite), &v) || !v.Check.Unverified || v.Check.Rejects != MaxRejects || len(v.Check.UnknownIDs) != 0 {
		t.Fatalf("не проверено: %+v", v.Check)
	}
	if len(r.lead) != 2+MaxRejects || !strings.Contains(turn.Reply, "_Не проверено:") {
		t.Fatalf("запросов %d, ответ %q", len(r.lead), turn.Reply)
	}
}

// Gate: фильтр отсёк всё (вид не назван, порог высок) — схема kb_answer
// только с unknown, пометка в выдаче kb_search; answered — отказ.
func TestHookCiteGate(t *testing.T) {
	h := &Hook{Searcher: searcher(t)}
	ctx := context.Background()
	if err := h.Searcher.Store.SetMinScore(ctx, DefaultIndex, 0.999); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Searcher.Store.SetMinScore(ctx, DefaultIndex, 0) })
	r := newCiteChat(t, h, func(req llm.Request, step int) llm.Response {
		if step == 1 {
			return llmtest.ToolCall(FinishName, string(args(cited("Жираф ростом 5 м.", []string{"x/structure/001"}, [2]string{"x/structure/001", "жираф ростом пять метров"}))))
		}
		return llmtest.ToolCall(FinishName, string(args(Cited{Status: StatusUnknown, Answer: "в базе знаний нет данных о жирафах", Clarify: "Рассказать о хищных?"})))
	})
	turn := r.ask(t, "Какого роста бывает взрослый жираф?", citeSet(features.RAGFilter, features.RAGRewrite))
	req := r.lead[0]
	var schema string
	for _, d := range req.Tools {
		if d.Function.Name == FinishName {
			schema = string(d.Function.Parameters)
		}
	}
	out := agentstest.LastReply(req, ToolName)
	if !strings.Contains(schema, `"enum":["unknown"]`) || !strings.Contains(out, "kb_answer на этот ход примет только status unknown") {
		t.Fatalf("gate: схема %s\nвыдача %s", schema, out)
	}
	if !strings.Contains(llmtest.LastToolReply(r.lead[1]), onlyUnknownProblem) {
		t.Fatal("answered при gate не отклонён")
	}
	var v CiteView
	if !turn.Extra(string(features.RAGCite), &v) || !v.Gated || !v.Check.Forced || !strings.HasPrefix(turn.Reply, "Не знаю: в базе знаний нет данных о жирафах\n\nУточните: ") {
		t.Fatalf("итог: %+v %q", v, turn.Reply)
	}
	// Вызов кодом не искал второй раз: в журнале один вызов kb_search.
	calls := 0
	for _, e := range turn.Events {
		if e.Kind == agent.EventToolCall && e.Tool == ToolName {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("вызовов kb_search %d", calls)
	}
}

// Составитель подборки получает kb_search и правило, но не kb_answer:
// Finish — только у ведущего, ход подборки заканчивается текстом.
func TestHookCiteCompiler(t *testing.T) {
	r := newCiteChat(t, &Hook{Searcher: searcher(t)}, func(req llm.Request, step int) llm.Response {
		return llmtest.Text("ведущий не должен отвечать")
	})
	turn := r.ask(t, "Собери подборку: кошки нашей фауны, два вида", citeSet(features.CollectionState))
	if turn.Route != compiler.RouteCollection || len(r.comp) == 0 || len(r.lead) != 0 {
		t.Fatalf("ход подборки: %s, составитель %d, ведущий %d", turn.Route, len(r.comp), len(r.lead))
	}
	if llmtest.HasTool(r.comp[0], FinishName) || !llmtest.HasTool(r.comp[0], ToolName) || turn.Reply != "План подборки: рысь и манул. Согласны?" {
		t.Fatalf("составитель: kb_answer %v, ответ %q", llmtest.HasTool(r.comp[0], FinishName), turn.Reply)
	}
}
