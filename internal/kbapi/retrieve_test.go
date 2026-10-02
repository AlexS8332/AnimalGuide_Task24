package kbapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
)

// fakeRetrieve — конвейер: кандидаты — настоящий поиск BM25 по запросу,
// первый остаётся, остальные отсечены порогом; запросы и настройки
// запоминаются.
type fakeRetrieve struct {
	s    *kb.Searcher
	mu   sync.Mutex
	qs   []retrieve.Query
	cs   []retrieve.Config
	fail error
}

func (f *fakeRetrieve) search(ctx context.Context, q retrieve.Query, c retrieve.Config) (retrieve.Trace, error) {
	f.mu.Lock()
	f.qs = append(f.qs, q)
	f.cs = append(f.cs, c)
	err := f.fail
	f.mu.Unlock()
	if err != nil {
		return retrieve.Trace{}, err
	}
	hits, info, err := f.s.Search(ctx, q.Text, kb.SearchOptions{Index: c.Index, K: c.K0, Mode: kb.BM25})
	if err != nil {
		return retrieve.Trace{}, err
	}
	tr := retrieve.Trace{Original: q.Text, Rewritten: q.Text + " палласов кот", Queries: []string{q.Text + " палласов кот"},
		Expanded: []string{"манул → палласов кот"}, Config: c, MinScore: retrieve.DefaultMinScore, Info: info}
	for i, h := range hits {
		cand := retrieve.Candidate{Hit: h, Dense: 0.88 - 0.02*float64(i), RankDense: i + 1, RankBM25: i + 1, Final: i + 1, Kept: i == 0}
		if !cand.Kept {
			cand.Reason = "порог 0.80"
		} else {
			tr.Hits = append(tr.Hits, h)
		}
		tr.Candidates = append(tr.Candidates, cand)
	}
	tr.TopDense = 0.88
	return tr, nil
}

func pipeAPI(t *testing.T) (*API, *fakeRetrieve) {
	t.Helper()
	s := &kb.Searcher{Store: newStore(t, true), Embedder: embed.Hash{}}
	f := &fakeRetrieve{s: s}
	return &API{Searcher: s, Path: "kb.db", Retrieve: f.search}, f
}

func TestSearchPipeline(t *testing.T) {
	a, f := pipeAPI(t)
	srv := serve(t, a)
	q := url.QueryEscape("манул грызуны")

	// Без параметров конвейера — как раньше: оба индекса, без Trace.
	var v SearchView
	if code := get(t, srv, "/api/kb/search?q="+q, &v); code != http.StatusOK || len(v.Results) != 2 || v.Results[0].Trace != nil {
		t.Fatalf("прямой поиск: %d %+v", code, v)
	}
	if len(f.qs) != 0 {
		t.Fatalf("конвейер вызван без параметров: %+v", f.qs)
	}

	// С параметрами — один индекс (structure по умолчанию), Trace, настройки
	// и контекст дошли до конвейера.
	v = SearchView{}
	path := "/api/kb/search?q=" + q + "&k=3&rewrite=code&rerank=hybrid&filter=1&k0=10&context=" + url.QueryEscape("Расскажи про манула") + "&context=+&context=" + url.QueryEscape("А где живёт?")
	if code := get(t, srv, path, &v); code != http.StatusOK || len(v.Results) != 1 {
		t.Fatalf("конвейер: %d %+v", code, v)
	}
	r := v.Results[0]
	if r.Trace == nil || r.Error != "" || r.Info.Index != "structure" || len(r.Hits) != 1 || len(r.Trace.Candidates) < 2 {
		t.Fatalf("выдача: %+v", r)
	}
	if r.Trace.Candidates[0].ID == "" || !r.Trace.Candidates[0].Kept || r.Trace.Candidates[1].Reason != "порог 0.80" || r.Trace.Expanded[0] != "манул → палласов кот" {
		t.Fatalf("кандидаты: %+v", r.Trace.Candidates)
	}
	c := f.cs[0]
	want := retrieve.Config{Index: "structure", K0: 10, K1: 3, Rewrite: retrieve.RewriteCode, Rerank: retrieve.RerankHybrid, Filter: true}
	if c != want {
		t.Fatalf("настройки: %+v", c)
	}
	if f.qs[0].Text != "манул грызуны" || strings.Join(f.qs[0].Context, "|") != "Расскажи про манула|А где живёт?" {
		t.Fatalf("запрос: %+v", f.qs[0])
	}

	// Один параметр — уже конвейер; k0 по умолчанию — 20, индекс — выбранный.
	v = SearchView{}
	if code := get(t, srv, "/api/kb/search?q="+q+"&filter=0&index=fixed", &v); code != http.StatusOK || v.Results[0].Trace == nil || v.Results[0].Info.Index != "fixed" {
		t.Fatalf("filter=0: %d %+v", code, v)
	}
	if c := f.cs[1]; c.K0 != retrieve.DefaultK0 || c.K1 != kb.DefaultK || c.Filter || c.Index != "fixed" || c.Rewrite != "" {
		t.Fatalf("умолчания: %+v", c)
	}

	// Ошибка конвейера — 200, причина в Error, Trace нет.
	f.fail = errors.New("эмбеддер молчит")
	v = SearchView{}
	if code := get(t, srv, "/api/kb/search?q="+q+"&filter=1", &v); code != http.StatusOK || v.Results[0].Error != "эмбеддер молчит" || v.Results[0].Trace != nil || v.Results[0].Hits == nil {
		t.Fatalf("ошибка конвейера: %d %+v", code, v)
	}
}

func TestSearchPipelineValidation(t *testing.T) {
	a, _ := pipeAPI(t)
	srv := serve(t, a)
	q := "/api/kb/search?q=" + url.QueryEscape("манул")
	long := url.QueryEscape(strings.Repeat("я", maxQ+1))
	for p, code := range map[string]int{
		q + "&rewrite=magic":            http.StatusBadRequest,
		q + "&rerank=cross":             http.StatusBadRequest,
		q + "&filter=yes":               http.StatusBadRequest,
		q + "&k0=0":                     http.StatusBadRequest,
		q + "&k0=51":                    http.StatusBadRequest,
		q + "&k0=x":                     http.StatusBadRequest,
		q + "&k0=3&k=5":                 http.StatusBadRequest,
		q + "&filter=1&context=" + long: http.StatusBadRequest,
		q + "&filter=1&index=nope":      http.StatusNotFound,
		q + "&k0=50&k=20&filter=1":      http.StatusOK,
		q + "&rewrite=&rerank=":         http.StatusOK,
	} {
		var e map[string]any
		if got := get(t, srv, p, &e); got != code {
			t.Fatalf("%s: %d, а ждали %d (%v)", p, got, code, e)
		}
	}
	// Реплик контекста больше maxContext — 400.
	p := q + "&filter=1" + strings.Repeat("&context=x", maxContext+1)
	if code := get(t, srv, p, nil); code != http.StatusBadRequest {
		t.Fatalf("много контекста: %d", code)
	}
}

func TestSearchPipelinePaid(t *testing.T) {
	a, f := pipeAPI(t)
	srv := serve(t, a)
	q := "/api/kb/search?q=" + url.QueryEscape("манул")
	// Модели нет — платные режимы 503 с подсказкой про ключ, бесплатные идут.
	for _, p := range []string{"&rewrite=llm", "&rerank=llm", "&rewrite=code&rerank=llm"} {
		var e map[string]string
		if code := get(t, srv, q+p, &e); code != http.StatusServiceUnavailable || e["hint"] != HintNoModel || e["why"] == "" {
			t.Fatalf("%s: %d %v", p, code, e)
		}
	}
	if len(f.cs) != 0 {
		t.Fatalf("платный режим без модели дошёл до конвейера: %+v", f.cs)
	}
	// Модель есть — идут.
	a.Pipeline = &retrieve.Pipeline{Searcher: a.Searcher, LLM: &llmtest.Fake{}}
	var v SearchView
	if code := get(t, srv, q+"&rewrite=llm&rerank=llm", &v); code != http.StatusOK || v.Results[0].Trace == nil {
		t.Fatalf("с моделью: %d %+v", code, v)
	}
	if c := f.cs[0]; c.Rewrite != retrieve.RewriteLLM || c.Rerank != retrieve.RerankLLM {
		t.Fatalf("настройки: %+v", c)
	}

	// Конвейера нет вовсе — 503; прямой поиск при этом работает.
	srv = serve(t, &API{Searcher: a.Searcher, Path: "kb.db"})
	var e map[string]string
	if code := get(t, srv, q+"&filter=1", &e); code != http.StatusServiceUnavailable || e["hint"] != hintNoPipeline {
		t.Fatalf("без конвейера: %d %v", code, e)
	}
	if code := get(t, srv, q, nil); code != http.StatusOK {
		t.Fatalf("прямой поиск без конвейера: %d", code)
	}
	// Базы нет — 503 базы, как у прямого поиска.
	srv = serve(t, &API{Path: "kb.db", Retrieve: f.search})
	if code := get(t, srv, q+"&filter=1", &e); code != http.StatusServiceUnavailable || e["hint"] != HintNoBase {
		t.Fatalf("без базы: %d %v", code, e)
	}
}

func TestMatrixAndCalibration(t *testing.T) {
	dir := t.TempDir()
	mp, cp := filepath.Join(dir, "filter.json"), filepath.Join(dir, "calibrate.json")
	// Базы нет — файлы всё равно видны.
	srv := serve(t, &API{Path: "kb.db", MatrixPath: mp, CalibrationPath: cp})

	for _, x := range []struct{ path, hint string }{{"/api/kb/matrix", hintMatrix}, {"/api/kb/calibration", hintCalibration}} {
		var e map[string]string
		if code := get(t, srv, x.path, &e); code != http.StatusNotFound || e["hint"] != x.hint || !strings.Contains(e["error"], x.hint) {
			t.Fatalf("%s без файла: %d %v", x.path, code, e)
		}
		if code := do(t, srv, http.MethodPost, x.path, nil); code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s: %d", x.path, code)
		}
	}

	m := retrieve.Matrix{Created: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Embedder: "e5-base", Index: "structure", MinScore: 0.8, Delta: 0.05,
		Rows:       []retrieve.MatrixRow{{Name: "both", Split: kb.SplitTest, K1: 5, N: 10, RecallBefore: 1, RecallAfter: 0.9, OutEmpty: 0.83, OutN: 6}},
		Conclusion: []string{"both: recall 0.90"}}
	cal := retrieve.Calibration{Index: "structure", Chosen: 0.815, Table: []retrieve.CalibRow{{MinScore: 0.815, DevRecall: 0.9, OutEmpty: 0.83}},
		DevTop: []float64{0.86, 0.88}, OutTop: []float64{0.79}}
	for p, v := range map[string]any{mp: m, cp: cal} {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var gm retrieve.Matrix
	if code := get(t, srv, "/api/kb/matrix", &gm); code != http.StatusOK || len(gm.Rows) != 1 || gm.Rows[0].Name != "both" || gm.Rows[0].OutEmpty != 0.83 || gm.Conclusion[0] != "both: recall 0.90" {
		t.Fatalf("матрица: %d %+v", code, gm)
	}
	var gc retrieve.Calibration
	if code := get(t, srv, "/api/kb/calibration", &gc); code != http.StatusOK || gc.Chosen != 0.815 || len(gc.DevTop) != 2 || gc.Table[0].DevRecall != 0.9 {
		t.Fatalf("калибровка: %d %+v", code, gc)
	}

	// Битый файл — 500 с путём.
	if err := os.WriteFile(mp, []byte("{не json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var e map[string]string
	if code := get(t, srv, "/api/kb/matrix", &e); code != http.StatusInternalServerError || !strings.Contains(e["error"], "filter.json") {
		t.Fatalf("битый: %d %v", code, e)
	}

	// Пути по умолчанию — examples/rag рядом с приложением.
	a := &API{}
	if a.MatrixPath != "" || DefaultMatrixPath != "examples/rag/filter.json" || DefaultCalibrationPath != "examples/rag/calibrate.json" {
		t.Fatal("пути по умолчанию")
	}
}

func TestAskModesV23(t *testing.T) {
	a, _ := askAPI(t)
	srv := serve(t, a)

	// Три режима рядом, в том числе с конвейером; recall — у всех режимов с
	// базой.
	var v AskView
	ms := []rag.Mode{rag.NoRAG, rag.RAG, rag.RAGBoth}
	if code := post(t, srv, "/api/kb/ask", AskRequest{Q: "Чем кормится манул?", QuestionID: "T01", Modes: ms}, &v); code != http.StatusOK {
		t.Fatalf("три режима: %d %+v", code, v)
	}
	if len(v.Answers) != 3 || v.Answers[2].Mode != rag.RAGBoth || len(v.Rows) != 3 {
		t.Fatalf("ответы: %+v", v)
	}
	if v.Rows[0].Recall || !v.Rows[1].Recall || !v.Rows[2].Recall {
		t.Fatalf("recall: %v %v %v", v.Rows[0].Recall, v.Rows[1].Recall, v.Rows[2].Recall)
	}
	for _, m := range []rag.Mode{rag.RAGFilter, rag.RAGRewrite} {
		if code := post(t, srv, "/api/kb/ask", AskRequest{Q: "манул", Modes: []rag.Mode{m}}, nil); code != http.StatusOK {
			t.Fatalf("%s: %d", m, code)
		}
	}

	// Четыре — 400; неизвестный — 400 со списком режимов.
	var e map[string]string
	if code := post(t, srv, "/api/kb/ask", AskRequest{Q: "манул", Modes: []rag.Mode{rag.NoRAG, rag.RAG, rag.RAGFilter, rag.RAGBoth}}, &e); code != http.StatusBadRequest || !strings.Contains(e["error"], "трёх") {
		t.Fatalf("четыре режима: %d %v", code, e)
	}
	if code := post(t, srv, "/api/kb/ask", AskRequest{Q: "манул", Modes: []rag.Mode{"rag+all"}}, &e); code != http.StatusBadRequest || !strings.Contains(e["error"], "rag+both") {
		t.Fatalf("неизвестный: %d %v", code, e)
	}

	// Прогон — все режимы можно (v24: шесть, с rag+cite).
	f := &fakeEval{step: make(chan struct{}), opts: make(chan rag.EvalOptions, 1)}
	a.Eval = f.eval
	var ev EvalView
	if code := post(t, srv, "/api/kb/evals", EvalRequest{Modes: allModes}, &ev); code != http.StatusAccepted || len(ev.Request.Modes) != 6 || ev.Total != 2*6 {
		t.Fatalf("прогон всех режимов: %d %+v", code, ev)
	}
	if o := <-f.opts; len(o.Modes) != 6 || o.Modes[4] != rag.RAGBoth || o.Modes[5] != rag.RAGCite {
		t.Fatalf("режимы прогона: %+v", o.Modes)
	}
	close(f.step)
	waitEval(t, srv, ev.ID, func(v EvalView) bool { return v.State != StateRunning })
	if code := post(t, srv, "/api/kb/evals", EvalRequest{Modes: []rag.Mode{rag.RAG, "rag+x"}}, &e); code != http.StatusBadRequest {
		t.Fatalf("прогон с неизвестным режимом: %d %v", code, e)
	}
}
