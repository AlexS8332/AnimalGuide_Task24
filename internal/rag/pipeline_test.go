package rag

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Режимы v23: тот же агент и промпт, что у rag; выдача — итог конвейера.

func TestModeConfig(t *testing.T) {
	cases := map[Mode]string{
		RAGFilter:  "filter",
		RAGRewrite: "rewrite code",
		RAGBoth:    "rewrite code, filter",
		RAG:        "dense top-K1, без фильтра и переписывания",
		NoRAG:      "dense top-K1, без фильтра и переписывания",
	}
	for m, want := range cases {
		if got := ModeConfig(m).Describe(); got != want {
			t.Errorf("%s: %q", m, got)
		}
	}
	for _, m := range []Mode{RAGFilter, RAGRewrite, RAGBoth} {
		if !m.Pipelined() || !m.UsesBase() || !m.Known() || System(m) != System(RAG) {
			t.Errorf("%s: признаки режима", m)
		}
	}
	if RAG.Pipelined() || NoRAG.UsesBase() || Mode("both").Known() {
		t.Error("признаки rag/norag")
	}
	a := &Answerer{K: 3, Configs: map[Mode]retrieve.Config{RAGFilter: {MinScore: 0.83, Delta: 0.1, Rewrite: retrieve.RewriteCode, K0: 30}}}
	c := a.Config(RAGFilter)
	if !c.Filter || c.MinScore != 0.83 || c.Delta != 0.1 || c.Rewrite != retrieve.RewriteCode || c.K0 != 30 || c.K1 != 3 || c.Index != DefaultIndex {
		t.Fatalf("настройки поверх умолчаний: %+v", c)
	}
	if c := a.Config(RAGBoth); c.Rerank != retrieve.RerankNone || !c.Filter || c.MinScore != 0 || c.K1 != 3 {
		t.Fatalf("без переопределения: %+v", c)
	}
}

func TestAnswerPipelined(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) { return llmtest.Text("13 часов."), nil }}
	s := searcher(t)
	a := &Answerer{LLM: fake, Searcher: s, K: 3, Pipeline: &retrieve.Pipeline{Searcher: s}}
	ctx := context.Background()
	q := Question{Text: "Сколько часов в день кошачий медведь тратит на еду?"}
	ans, err := a.Answer(ctx, q, RAGRewrite)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Trace == nil || ans.Mode != RAGRewrite || !strings.Contains(ans.Trace.Rewritten, "малая панда") || len(ans.Hits) != 3 ||
		ans.Hits[0].ID != ans.Trace.Hits[0].ID || ans.Search.Mode != kb.Dense {
		t.Fatalf("ответ: %+v", ans)
	}
	m := fake.Requests[0].Messages
	if m[0].Content != System(RAG) || askedText(m[1].Content) != q.Text || !strings.Contains(m[1].Content, "\n\nФрагменты базы знаний") {
		t.Fatalf("модель получила переписанный запрос вместо вопроса: %q", m[1].Content)
	}

	// Пустой итог фильтра — явная строка «ничего не найдено».
	a.Configs = map[Mode]retrieve.Config{RAGFilter: {MinScore: 0.999}}
	ans, err = a.Answer(ctx, Question{Text: "Как высиживают яйцо императорские пингвины?"}, RAGFilter)
	if err != nil {
		t.Fatal(err)
	}
	if !ans.Trace.Empty || len(ans.Hits) != 0 || !strings.HasSuffix(fake.Requests[1].Messages[1].Content, Compose(nil)) {
		t.Fatalf("пусто: %+v %q", ans.Trace.Empty, fake.Requests[1].Messages[1].Content)
	}

	// Без конвейера режим v23 — ошибка, а не тихий rag.
	if _, err := (&Answerer{LLM: fake, Searcher: s}).Answer(ctx, q, RAGBoth); err == nil || !strings.Contains(err.Error(), "конвейер") {
		t.Fatalf("без конвейера: %v", err)
	}
}

// TestEvalPipelined — Eval с режимами v23: recall по итоговой выдаче,
// вывод называет режимы.
func TestEvalPipelined(t *testing.T) {
	qs := questions(t)
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if isJudge(req) {
			return verdictJSON(Partial, "ok"), nil
		}
		return llmtest.Text("Не знаю."), nil
	}}
	s := searcher(t)
	a := &Answerer{LLM: fake, Searcher: s, Pipeline: &retrieve.Pipeline{Searcher: s}}
	rep, err := Eval(context.Background(), a, qs, EvalOptions{Modes: []Mode{RAG, RAGBoth}, Judge: &Judge{LLM: fake}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Stats) != 2 || rep.Stats[1].Mode != RAGBoth || rep.Stats[1].Questions != 10 {
		t.Fatalf("сводка: %+v", rep.Stats)
	}
	recalled := 0
	for _, row := range rep.Rows {
		run := row.Runs[RAGBoth][0]
		if run.Answer.Trace == nil || run.Error != "" {
			t.Fatalf("%s: %+v", row.Question.ID, run)
		}
		if run.Recall {
			recalled++
		}
	}
	if recalled == 0 || rep.Stats[1].Recall == 0 {
		t.Fatal("recall режима v23 не считается")
	}
	text := strings.Join(rep.Conclusion, "\n")
	if !strings.Contains(text, "Доказательство в выдаче rag+both") || !strings.Contains(rep.Markdown(), "`rag+both`") {
		t.Fatalf("вывод: %s", text)
	}
	if _, err := Eval(context.Background(), &Answerer{LLM: fake, Searcher: s}, qs, EvalOptions{Modes: []Mode{RAGFilter}}); err == nil {
		t.Fatal("режим v23 без конвейера")
	}
	if _, err := Eval(context.Background(), a, qs, EvalOptions{Modes: []Mode{"x"}}); err == nil {
		t.Fatal("неизвестный режим")
	}
}

func TestPipelineRanks(t *testing.T) {
	s := searcher(t)
	p := &retrieve.Pipeline{Searcher: s}
	qs := questions(t)
	rows, info, err := PipelineRanks(context.Background(), p, qs, []string{kb.SplitDev, kb.SplitTest}, ModeConfig(RAGRewrite))
	if err != nil || len(rows) != 27 || info.Mode != kb.Dense {
		t.Fatalf("ранги: %d %+v %v", len(rows), info, err)
	}
	base, _, err := Ranks(context.Background(), s, qs, []string{kb.SplitDev, kb.SplitTest}, "")
	if err != nil || len(base) != len(rows) {
		t.Fatal(err)
	}
	if rec, hit := RecallAt(rows, 20); hit == 0 || rec <= 0 {
		t.Fatalf("recall@20 переписанного: %v", rec)
	}
}

// TestHookPipeline — rag.filter и rag.rewrite: kb_search и вызов кодом
// через конвейер, контекст — прошлые реплики человека из окна, журнал
// называет механизмы.
func TestHookPipeline(t *testing.T) {
	h := &Hook{Searcher: searcher(t), K: 3}
	// Порог — из индекса (как после kb calibrate -write): хэш-эмбеддер даёт
	// косинусы ниже умолчания, а вид у продолжения унаследован — якоря нет,
	// пол действует.
	ctx := context.Background()
	if err := h.Searcher.Store.SetMinScore(ctx, DefaultIndex, 0.05); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Searcher.Store.SetMinScore(ctx, DefaultIndex, 0) })
	if v, from, err := h.pipeline().MinScore(ctx, ModeConfig(RAGBoth)); err != nil || v != 0.05 || from != retrieve.MinScoreIndex {
		t.Fatalf("порог хука: %v %q %v", v, from, err)
	}
	tr, rec := hookTurn(t, "+rag,+rag.filter,+rag.rewrite", "А сколько она весит?")
	tr.Request.Window = []llm.Message{
		{Role: llm.RoleUser, Content: "Расскажи про манула"},
		{Role: llm.RoleAssistant, Content: "Манул — дикий кот."},
		{Role: llm.RoleUser, Content: "Где в России водится харза?"},
		{Role: llm.RoleAssistant, Content: "На Дальнем Востоке."},
	}
	if got := humanTurns(tr); strings.Join(got, "|") != "Расскажи про манула|Где в России водится харза?" {
		t.Fatalf("реплики окна: %v", got)
	}
	if err := h.Before(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if len(rec.Events) != 1 || !strings.Contains(rec.Events[0].Title, "(индекс structure, k 3; rag.filter, rag.rewrite)") ||
		!strings.Contains(rec.Events[0].Detail, "Второй этап поиска (rag.filter, rag.rewrite): rewrite code, filter") {
		t.Fatalf("журнал: %+v", rec.Events)
	}
	var tool tools.Tool
	for _, x := range tr.Request.Tools {
		if x.Spec().Name == ToolName {
			tool = x
		}
	}
	if tool == nil || len(tr.Request.Preload) != 1 {
		t.Fatal("нет kb_search или вызова кодом")
	}
	out, err := tool.Call(context.Background(), json.RawMessage(tr.Request.Preload[0].Args))
	if err != nil {
		t.Fatal(err)
	}
	var res SearchResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Rewritten != "Где в России водится харза? А сколько она весит?" || res.Query != "А сколько она весит?" || len(res.Hits) == 0 ||
		len(res.Hits) > 3 || res.Hits[0].ChunkID == "" {
		t.Fatalf("выдача: %+v", res)
	}
	if res.Filtered == 0 {
		t.Fatalf("фильтр ничего не отсёк из 20+ кандидатов: %+v", res)
	}
	// k вызова моделью — K1, но не больше MaxK.
	out, _ = tool.Call(context.Background(), json.RawMessage(`{"query":"чем питается манул","k":50}`))
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Hits) > MaxK {
		t.Fatalf("k: %d %v", len(res.Hits), err)
	}
	if _, err := tool.Call(context.Background(), json.RawMessage(`{"query":" "}`)); err == nil {
		t.Fatal("пустой запрос")
	}

	// Только rag.rewrite: фильтра нет — ничего не отсечено.
	tr, rec = hookTurn(t, "+rag,+rag.rewrite", "Что ест кошачий медведь?")
	h.Before(context.Background(), tr)
	if !strings.HasSuffix(rec.Events[0].Title, "; rag.rewrite)") {
		t.Fatalf("журнал rewrite: %q", rec.Events[0].Title)
	}
	for _, x := range tr.Request.Tools {
		if x.Spec().Name == ToolName {
			out, _ := x.Call(context.Background(), json.RawMessage(tr.Request.Preload[0].Args))
			res = SearchResult{}
			if json.Unmarshal([]byte(out), &res) != nil || res.Filtered != 0 || !strings.Contains(res.Rewritten, "малая панда") {
				t.Fatalf("rewrite: %+v", res)
			}
		}
	}

	// Без механизмов v23 — прежний kb_search (без rewritten), тот же журнал.
	tr, rec = hookTurn(t, "+rag", "Что ест кошачий медведь?")
	h.Before(context.Background(), tr)
	if strings.Contains(rec.Events[0].Title, "rag.") || rec.Events[0].Kind != agent.EventMechanism {
		t.Fatalf("без механизмов: %q", rec.Events[0].Title)
	}
}
