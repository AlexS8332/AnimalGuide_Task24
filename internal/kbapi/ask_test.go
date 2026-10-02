package kbapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
)

// miniQuestions — набор к miniDocs: два test (второй — продолжение) и dev.
func miniQuestions(t *testing.T) string {
	t.Helper()
	qs := kb.QuestionSet{Schema: kb.QuestionsSchema, Version: 1, Questions: []kb.Question{
		{ID: "T01", Split: kb.SplitTest, Type: "fact", Q: "Чем кормится манул?", Answerable: true,
			Expect:   &kb.Expect{Must: [][]string{{"грызун"}}, Note: "мелкими грызунами и пищухами"},
			Sources:  []kb.SourceRef{{DocID: "manul", Section: "Питание"}},
			Evidence: []kb.Evidence{{DocID: "manul", Quote: "Кормится манул почти исключительно мелкими грызунами и пищухами."}}},
		{ID: "T02", Split: kb.SplitTest, Type: "followup", Q: "А сколько весит?", Context: []string{"Расскажи про корсака"}, Answerable: true,
			Evidence: []kb.Evidence{{DocID: "corsac", Quote: "Весит от 2,5 до 4 кг."}}},
		{ID: "D01", Split: kb.SplitDev, Type: "fact", Q: "Где охраняется манул?", Answerable: true},
	}}
	b, err := json.Marshal(qs)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "questions.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func serve(t *testing.T, a *API) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for _, x := range a.Extension() {
		mux.Handle(x.Prefix, x.Handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// post — POST с телом JSON и разбор ответа.
func post(t *testing.T, srv *httptest.Server, path string, body, v any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	res, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if v != nil {
		if err := json.NewDecoder(res.Body).Decode(v); err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
	}
	return res.StatusCode
}

// fakeAsk — ответ режима: rag ищет в базе настоящим поиском (BM25) и
// ссылается на первый фрагмент, norag — «не знаю».
type fakeAsk struct {
	s    *kb.Searcher
	mu   sync.Mutex
	seen []rag.Question
	fail map[rag.Mode]error
}

func (f *fakeAsk) ask(ctx context.Context, q rag.Question, m rag.Mode) (rag.Answer, error) {
	f.mu.Lock()
	f.seen = append(f.seen, q)
	err := f.fail[m]
	f.mu.Unlock()
	if err != nil {
		return rag.Answer{}, err
	}
	a := rag.Answer{Mode: m, System: "Ты — справочник.", User: q.Text, Usage: llm.Usage{Prompt: 100, Completion: 20, Total: 120},
		Cost: llm.Cost{USD: 0.0001, Known: true}, Millis: 900}
	if m == rag.NoRAG {
		a.Text = "Не знаю."
		return a, nil
	}
	hits, info, err := f.s.Search(ctx, strings.Join(append(q.Context, q.Text), " "), kb.SearchOptions{Index: "structure", K: 3, Mode: kb.BM25})
	if err != nil {
		return rag.Answer{}, err
	}
	a.Hits, a.Search = hits, info
	a.Text = "Мелкими грызунами [" + hits[0].ID + "]."
	a.User += "\n\n" + hits[0].Text
	return a, nil
}

func ruleStub(q kb.Question, answer string) rag.RuleResult {
	if strings.Contains(answer, "грызун") {
		return rag.RuleResult{Verdict: rag.Correct, Hit: []string{"грызун"}}
	}
	if strings.Contains(answer, "Не знаю") {
		return rag.RuleResult{Verdict: rag.Abstain}
	}
	return rag.RuleResult{Verdict: rag.Wrong}
}

func askAPI(t *testing.T) (*API, *fakeAsk) {
	t.Helper()
	s := &kb.Searcher{Store: newStore(t, true), Embedder: embed.Hash{}}
	f := &fakeAsk{s: s}
	return &API{Searcher: s, Path: "kb.db", Questions: miniQuestions(t), Ask: f.ask, Rule: ruleStub}, f
}

func TestAsk(t *testing.T) {
	a, f := askAPI(t)
	srv := serve(t, a)

	var v AskView
	if code := post(t, srv, "/api/kb/ask", AskRequest{Q: "Чем кормится манул?", QuestionID: "T01"}, &v); code != http.StatusOK {
		t.Fatalf("код %d: %+v", code, v)
	}
	if len(v.Answers) != 2 || v.Answers[0].Mode != rag.NoRAG || v.Answers[1].Mode != rag.RAG || v.Error != "" {
		t.Fatalf("ответы: %+v", v)
	}
	if len(v.Answers[1].Hits) == 0 || !strings.Contains(v.Answers[1].Text, "[manul/structure/") {
		t.Fatalf("rag: %+v", v.Answers[1])
	}
	if len(v.Rows) != 2 || v.Rows[0].Final != rag.Abstain || v.Rows[1].Final != rag.Correct || v.Rows[1].Rule.Verdict != rag.Correct {
		t.Fatalf("оценки: %+v", v.Rows)
	}
	if v.Rows[0].Recall || !v.Rows[1].Recall {
		t.Fatalf("recall: norag %v, rag %v", v.Rows[0].Recall, v.Rows[1].Recall)
	}

	// Один режим, вопрос не из набора — без оценок.
	v = AskView{}
	if code := post(t, srv, "/api/kb/ask", AskRequest{Q: "манул", Modes: []rag.Mode{rag.RAG}}, &v); code != http.StatusOK || len(v.Answers) != 1 || v.Answers[0].Mode != rag.RAG || v.Rows != nil {
		t.Fatalf("один режим: %d %+v", code, v)
	}

	// Продолжение: контекст берётся из набора, если клиент его не прислал.
	f.seen = nil
	if code := post(t, srv, "/api/kb/ask", AskRequest{Q: "А сколько весит?", QuestionID: "T02", Modes: []rag.Mode{rag.NoRAG}}, nil); code != http.StatusOK {
		t.Fatalf("продолжение: %d", code)
	}
	if len(f.seen) != 1 || len(f.seen[0].Context) != 1 || f.seen[0].Context[0] != "Расскажи про корсака" {
		t.Fatalf("контекст: %+v", f.seen)
	}

	// Ошибка одного режима — 200 и Error; обоих — 502.
	f.fail = map[rag.Mode]error{rag.RAG: errors.New("модель молчит")}
	v = AskView{}
	if code := post(t, srv, "/api/kb/ask", AskRequest{Q: "манул"}, &v); code != http.StatusOK || !strings.Contains(v.Error, "rag: модель молчит") || v.Answers[0].Text == "" {
		t.Fatalf("ошибка rag: %d %+v", code, v)
	}
	f.fail[rag.NoRAG] = errors.New("и эта")
	if code := post(t, srv, "/api/kb/ask", AskRequest{Q: "манул"}, &v); code != http.StatusBadGateway {
		t.Fatalf("ошибки обоих: %d", code)
	}
}

func TestAskValidation(t *testing.T) {
	a, _ := askAPI(t)
	srv := serve(t, a)
	for name, body := range map[string]any{
		"пусто":            AskRequest{Q: "  "},
		"длинно":           AskRequest{Q: strings.Repeat("я", maxQ+1)},
		"режим":            AskRequest{Q: "манул", Modes: []rag.Mode{"wiki"}},
		"режим дважды":     AskRequest{Q: "манул", Modes: []rag.Mode{rag.RAG, rag.RAG}},
		"нет вопроса":      AskRequest{Q: "манул", QuestionID: "T99"},
		"длинный контекст": AskRequest{Q: "манул", Context: []string{strings.Repeat("я", maxQ+1)}},
		"не JSON":          "манул",
	} {
		var e map[string]string
		if code := post(t, srv, "/api/kb/ask", body, &e); code != http.StatusBadRequest || e["error"] == "" {
			t.Fatalf("%s: %d %v", name, code, e)
		}
	}
	// Ровно maxQ символов — можно.
	if code := post(t, srv, "/api/kb/ask", AskRequest{Q: strings.Repeat("я", maxQ), Modes: []rag.Mode{rag.NoRAG}}, nil); code != http.StatusOK {
		t.Fatalf("%d символов: %d", maxQ, code)
	}
	if code := do(t, srv, http.MethodGet, "/api/kb/ask", nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET ask: %d", code)
	}
}

func TestAskNoModel(t *testing.T) {
	// База есть, модели нет — подсказка про ключ.
	srv := serve(t, &API{Searcher: &kb.Searcher{Store: newStore(t, true)}, Questions: miniQuestions(t)})
	for _, p := range []string{"/api/kb/ask", "/api/kb/evals"} {
		var e map[string]string
		if code := post(t, srv, p, map[string]string{"q": "манул"}, &e); code != http.StatusServiceUnavailable || e["hint"] != HintNoModel || e["why"] == "" {
			t.Fatalf("%s: %d %v", p, code, e)
		}
	}
	// Базы нет — причина в базе.
	srv = serve(t, &API{Path: "kb.db", Questions: miniQuestions(t)})
	var e map[string]string
	if code := post(t, srv, "/api/kb/ask", map[string]string{"q": "манул"}, &e); code != http.StatusServiceUnavailable || e["hint"] != HintNoBase || e["why"] != "базы знаний нет: kb.db" {
		t.Fatalf("без базы: %d %v", code, e)
	}
	// Вопросы набора видны и без модели.
	var qs []kb.Question
	if code := get(t, srv, "/api/kb/questions", &qs); code != http.StatusOK || len(qs) != 3 {
		t.Fatalf("вопросы без модели: %d %d", code, len(qs))
	}
}

func TestQuestions(t *testing.T) {
	a, _ := askAPI(t)
	srv := serve(t, a)
	var qs []kb.Question
	if code := get(t, srv, "/api/kb/questions", &qs); code != http.StatusOK || len(qs) != 3 || qs[1].Context[0] != "Расскажи про корсака" {
		t.Fatalf("%d %+v", code, qs)
	}
	a.Questions = filepath.Join(t.TempDir(), "нет.json")
	a.Eval = func(ctx context.Context, qs kb.QuestionSet, o rag.EvalOptions) (rag.Report, error) {
		return rag.Report{}, nil
	}
	var e map[string]string
	if code := get(t, srv, "/api/kb/questions", &e); code != http.StatusNotFound || !strings.Contains(e["error"], "нет.json") || e["hint"] == "" {
		t.Fatalf("нет файла: %d %v", code, e)
	}
	if code := post(t, srv, "/api/kb/evals", EvalRequest{}, &e); code != http.StatusNotFound {
		t.Fatalf("evals без файла: %d", code)
	}
	if code := do(t, srv, http.MethodPost, "/api/kb/questions", nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST questions: %d", code)
	}
}

// fakeEval — прогон, который отдаёт строки по сигналу step и завершается
// по закрытию done.
type fakeEval struct {
	step chan struct{}
	opts chan rag.EvalOptions
}

func (f *fakeEval) eval(ctx context.Context, qs kb.QuestionSet, o rag.EvalOptions) (rag.Report, error) {
	f.opts <- o
	var rows []rag.Row
	for _, s := range o.Splits {
		for _, q := range qs.Split(s) {
			if _, ok := <-f.step; !ok {
				return rag.Report{}, errors.New("прогон прерван")
			}
			row := rag.Row{Question: q, Runs: map[rag.Mode][]rag.Run{}, Majority: map[rag.Mode]rag.Verdict{}, Flips: map[rag.Mode]int{}}
			for _, m := range o.Modes {
				for i := 1; i <= o.Repeats; i++ {
					row.Runs[m] = append(row.Runs[m], rag.Run{Repeat: i, Final: rag.Correct})
					if o.Progress != nil {
						o.Progress(row)
					}
				}
				row.Majority[m] = rag.Correct
			}
			rows = append(rows, row)
		}
	}
	return rag.Report{Rows: rows, Repeats: o.Repeats, Conclusion: []string{"rag верен чаще"},
		Stats: []rag.ModeStats{{Mode: rag.NoRAG, Questions: len(rows)}, {Mode: rag.RAG, Questions: len(rows), Correct: len(rows)}}}, nil
}

// waitEval — опрос прогона, пока cond не выполнится.
func waitEval(t *testing.T, srv *httptest.Server, id string, cond func(EvalView) bool) EvalView {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for {
		var v EvalView
		if code := get(t, srv, "/api/kb/evals/"+id, &v); code != http.StatusOK {
			t.Fatalf("GET evals/%s: %d", id, code)
		}
		if cond(v) {
			return v
		}
		if time.Now().After(end) {
			t.Fatalf("не дождался: %+v", v)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEvals(t *testing.T) {
	a, _ := askAPI(t)
	f := &fakeEval{step: make(chan struct{}), opts: make(chan rag.EvalOptions, 1)}
	a.Eval = f.eval
	srv := serve(t, a)

	for name, body := range map[string]EvalRequest{
		"повторы":    {Repeats: maxRepeats + 1},
		"минус":      {Repeats: -1},
		"набор":      {Splits: []string{"prod"}},
		"режим":      {Modes: []rag.Mode{"x"}},
		"пустой out": {Splits: []string{kb.SplitOut}},
	} {
		var e map[string]string
		if code := post(t, srv, "/api/kb/evals", body, &e); code != http.StatusBadRequest || e["error"] == "" {
			t.Fatalf("%s: %d %v", name, code, e)
		}
	}

	var v EvalView
	if code := post(t, srv, "/api/kb/evals", EvalRequest{Repeats: 2, Judge: true}, &v); code != http.StatusAccepted {
		t.Fatalf("POST: %d %+v", code, v)
	}
	if v.ID != "e1" || v.State != StateRunning || v.Total != 2*2*2 || v.Done != 0 || v.Started == "" {
		t.Fatalf("старт: %+v", v)
	}
	// Судьи у API нет — прогон идёт правилом, и запрос это показывает.
	if v.Request.Judge || v.Request.Repeats != 2 || strings.Join(v.Request.Splits, ",") != "test" || len(v.Request.Modes) != 2 {
		t.Fatalf("запрос: %+v", v.Request)
	}
	o := <-f.opts
	if o.Judge != nil || o.Repeats != 2 || len(o.Modes) != 2 || o.Progress == nil {
		t.Fatalf("опции: %+v", o)
	}

	// Второй — 409 с id идущего.
	var busy map[string]string
	if code := post(t, srv, "/api/kb/evals", EvalRequest{}, &busy); code != http.StatusConflict || busy["id"] != "e1" || busy["error"] == "" {
		t.Fatalf("409: %d %v", code, busy)
	}

	// Строки — по мере готовности.
	f.step <- struct{}{}
	got := waitEval(t, srv, "e1", func(v EvalView) bool { return v.Done == 4 })
	if len(got.Rows) != 1 || got.Rows[0].Question.ID != "T01" || len(got.Rows[0].Runs[rag.RAG]) != 2 || got.State != StateRunning || got.Report != nil {
		t.Fatalf("первая строка: %+v", got)
	}
	f.step <- struct{}{}
	got = waitEval(t, srv, "e1", func(v EvalView) bool { return v.State == StateDone })
	if got.Done != got.Total || len(got.Rows) != 2 || got.Report == nil || got.Report.Rows != nil || len(got.Report.Stats) != 2 || got.Report.Conclusion[0] != "rag верен чаще" {
		t.Fatalf("итог: %+v", got)
	}

	// Новый прогон после конца — можно; сбой — failed с причиной.
	if code := post(t, srv, "/api/kb/evals", EvalRequest{Modes: []rag.Mode{rag.RAG}}, &v); code != http.StatusAccepted || v.ID != "e2" || v.Total != 2 {
		t.Fatalf("второй: %d %+v", code, v)
	}
	<-f.opts
	close(f.step)
	got = waitEval(t, srv, "e2", func(v EvalView) bool { return v.State != StateRunning })
	if got.State != StateFailed || got.Error != "прогон прерван" {
		t.Fatalf("сбой: %+v", got)
	}

	// Список — новые первыми, без строк.
	var list []EvalView
	if code := get(t, srv, "/api/kb/evals", &list); code != http.StatusOK || len(list) != 2 || list[0].ID != "e2" || list[1].ID != "e1" {
		t.Fatalf("список: %d %+v", code, list)
	}
	if list[1].Rows != nil || list[1].Report == nil || len(list[1].Report.Stats) != 2 {
		t.Fatalf("список со строками: %+v", list[1])
	}
	var e map[string]string
	if code := get(t, srv, "/api/kb/evals/e9", &e); code != http.StatusNotFound {
		t.Fatalf("нет прогона: %d", code)
	}
	if code := do(t, srv, http.MethodDelete, "/api/kb/evals", nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE evals: %d", code)
	}
}

func TestEvalsKeep(t *testing.T) {
	a, _ := askAPI(t)
	a.Eval = func(ctx context.Context, qs kb.QuestionSet, o rag.EvalOptions) (rag.Report, error) {
		return rag.Report{}, nil
	}
	srv := serve(t, a)
	for i := 0; i < keepEvals+3; i++ {
		var v EvalView
		if code := post(t, srv, "/api/kb/evals", EvalRequest{}, &v); code != http.StatusAccepted {
			t.Fatalf("%d: %d", i, code)
		}
		waitEval(t, srv, v.ID, func(v EvalView) bool { return v.State == StateDone })
	}
	var list []EvalView
	get(t, srv, "/api/kb/evals", &list)
	if len(list) != keepEvals || list[0].ID != "e13" {
		t.Fatalf("помнит %d, первый %s", len(list), list[0].ID)
	}
}

// DELETE evals/{id} — отмена идущего прогона: контекст горутины отменён,
// состояние сразу cancelled, строки — какие успели; новый прогон можно
// запускать сразу.
func TestEvalsCancel(t *testing.T) {
	a, _ := askAPI(t)
	started, stopped := make(chan struct{}), make(chan error, 1)
	a.Eval = func(ctx context.Context, qs kb.QuestionSet, o rag.EvalOptions) (rag.Report, error) {
		q := qs.Questions[0]
		o.Progress(rag.Row{Question: q, Runs: map[rag.Mode][]rag.Run{rag.RAG: {{Repeat: 1}}}})
		close(started)
		<-ctx.Done()
		stopped <- ctx.Err()
		return rag.Report{}, ctx.Err()
	}
	srv := serve(t, a)
	var v EvalView
	if code := post(t, srv, "/api/kb/evals", EvalRequest{}, &v); code != http.StatusAccepted {
		t.Fatalf("POST: %d", code)
	}
	<-started
	var got EvalView
	if code := do(t, srv, http.MethodDelete, "/api/kb/evals/"+v.ID, &got); code != http.StatusOK {
		t.Fatalf("DELETE: %d", code)
	}
	if got.State != StateCancelled || got.Done != 1 || len(got.Rows) != 1 || !strings.Contains(got.Error, "остановлен") {
		t.Fatalf("отменён: %+v", got)
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("контекст: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("контекст прогона не отменён")
	}
	// Итог горутины не перетирает cancelled.
	time.Sleep(50 * time.Millisecond)
	if after := waitEval(t, srv, v.ID, func(EvalView) bool { return true }); after.State != StateCancelled || len(after.Rows) != 1 {
		t.Fatalf("после конца горутины: %+v", after)
	}
	if code := do(t, srv, http.MethodDelete, "/api/kb/evals/"+v.ID, nil); code != http.StatusConflict {
		t.Fatalf("повторная отмена: %d", code)
	}
	if code := do(t, srv, http.MethodDelete, "/api/kb/evals/e9", nil); code != http.StatusNotFound {
		t.Fatalf("нет прогона: %d", code)
	}
	if code := do(t, srv, http.MethodPut, "/api/kb/evals/"+v.ID, nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: %d", code)
	}
	a.Eval = func(ctx context.Context, qs kb.QuestionSet, o rag.EvalOptions) (rag.Report, error) {
		return rag.Report{}, nil
	}
	if code := post(t, srv, "/api/kb/evals", EvalRequest{}, &v); code != http.StatusAccepted || v.ID != "e2" {
		t.Fatalf("новый прогон после отмены: %d %+v", code, v)
	}
}

// question_id с изменённым текстом — другой вопрос: ответы есть, оценки
// правилом нет, причина — в note.
func TestAskChangedQuestion(t *testing.T) {
	a, _ := askAPI(t)
	srv := serve(t, a)
	var v AskView
	if code := post(t, srv, "/api/kb/ask", AskRequest{Q: "Чем кормится корсак?", QuestionID: "T01"}, &v); code != http.StatusOK {
		t.Fatalf("код %d", code)
	}
	if len(v.Answers) != 2 || v.Rows != nil || !strings.Contains(v.Note, "не совпадает с T01") {
		t.Fatalf("изменённый вопрос: %+v", v)
	}
	// Пробелы не в счёт.
	v = AskView{}
	post(t, srv, "/api/kb/ask", AskRequest{Q: "  Чем кормится   манул? ", QuestionID: "T01"}, &v)
	if len(v.Rows) != 2 || v.Note != "" {
		t.Fatalf("тот же вопрос: %+v", v)
	}
}
