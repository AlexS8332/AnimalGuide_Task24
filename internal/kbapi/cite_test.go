package kbapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
)

// citeAnswer — ответ rag+cite заготовкой: источник и цитата из первого
// фрагмента выдачи, проверка и судья смысла — как их заполнит rag.
func citeAnswer(a rag.Answer) rag.Answer {
	h := a.Hits[0]
	quote := strings.SplitAfter(h.Text, ".")[0]
	a.Cited = &rag.CitedResult{
		Cited: rag.Cited{Status: rag.StatusAnswered, Answer: "Мелкими грызунами и пищухами.",
			Sources: []rag.CitedSource{{ChunkID: h.ID}}, Quotes: []rag.CitedQuote{{ChunkID: h.ID, Text: quote}}},
		Check: rag.CiteCheck{OK: true, HasSources: true, HasQuotes: true, Verbatim: 1, Rejects: 1,
			Problems: []string{"первая попытка: цитата не дословная"}},
		Hits: a.Hits,
	}
	a.Support = &rag.Support{Claims: []rag.SupportClaim{{Claim: "Манул ест грызунов", Supported: true, Quote: 0}},
		Supported: 1, OK: true}
	return a
}

func citeAPI(t *testing.T) *API {
	t.Helper()
	a, f := askAPI(t)
	a.Ask = func(ctx context.Context, q rag.Question, m rag.Mode) (rag.Answer, error) {
		mode := m
		if m == rag.RAGCite {
			mode = rag.RAG // поиск и текст — как у rag
		}
		ans, err := f.ask(ctx, q, mode)
		if err != nil || m != rag.RAGCite {
			return ans, err
		}
		return citeAnswer(ans), nil
	}
	return a
}

func TestAskCite(t *testing.T) {
	srv := serve(t, citeAPI(t))

	// rag+cite рядом с двумя другими режимами; cited и support — в ответе
	// как есть, под своими ключами JSON.
	b, _ := json.Marshal(AskRequest{Q: "Чем кормится манул?", QuestionID: "T01", Modes: []rag.Mode{rag.NoRAG, rag.RAG, rag.RAGCite}})
	res, err := http.Post(srv.URL+"/api/kb/ask", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("код %d", res.StatusCode)
	}
	var raw struct {
		Answers []map[string]json.RawMessage `json:"answers"`
		Runs    []json.RawMessage            `json:"runs"`
	}
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Answers) != 3 || len(raw.Runs) != 3 {
		t.Fatalf("ответов %d, оценок %d", len(raw.Answers), len(raw.Runs))
	}
	if _, ok := raw.Answers[1]["cited"]; ok {
		t.Fatal("cited у rag")
	}
	var cited map[string]map[string]any
	if err := json.Unmarshal(raw.Answers[2]["cited"], &cited); err != nil {
		t.Fatalf("cited: %v %s", err, raw.Answers[2]["cited"])
	}
	c, ch := cited["cited"], cited["check"]
	if c["status"] != "answered" || len(c["sources"].([]any)) != 1 || len(c["quotes"].([]any)) != 1 {
		t.Fatalf("cited.cited: %v", c)
	}
	q := c["quotes"].([]any)[0].(map[string]any)
	if !strings.HasPrefix(q["chunk_id"].(string), "manul/structure/") || q["text"] == "" {
		t.Fatalf("цитата: %v", q)
	}
	if ch["ok"] != true || ch["has_sources"] != true || ch["has_quotes"] != true || ch["verbatim"] != 1.0 || ch["rejects"] != 1.0 {
		t.Fatalf("cited.check: %v", ch)
	}
	if _, ok := cited["hits"]; ok {
		t.Fatal("выдача продублирована в cited")
	}
	var sup rag.Support
	if err := json.Unmarshal(raw.Answers[2]["support"], &sup); err != nil || !sup.OK || len(sup.Claims) != 1 || sup.Claims[0].Quote != 0 {
		t.Fatalf("support: %v %s", err, raw.Answers[2]["support"])
	}
	var run rag.Run
	if err := json.Unmarshal(raw.Runs[2], &run); err != nil || run.Answer.Cited == nil || run.Answer.Cited.Cited.Status != rag.StatusAnswered {
		t.Fatalf("оценка rag+cite: %v %s", err, raw.Runs[2])
	}
}

func TestEvalsCite(t *testing.T) {
	a := citeAPI(t)
	ask := a.Ask
	a.Eval = func(ctx context.Context, qs kb.QuestionSet, o rag.EvalOptions) (rag.Report, error) {
		var rows []rag.Row
		for _, q := range qs.Split(kb.SplitTest) {
			row := rag.Row{Question: q, Runs: map[rag.Mode][]rag.Run{}, Majority: map[rag.Mode]rag.Verdict{}, Flips: map[rag.Mode]int{}}
			for _, m := range o.Modes {
				ans, err := ask(ctx, rag.Question{Text: q.Q, Context: q.Context}, m)
				if err != nil {
					return rag.Report{}, err
				}
				row.Runs[m] = []rag.Run{{Repeat: 1, Answer: ans, Final: rag.Correct}}
				row.Majority[m] = rag.Correct
			}
			o.Progress(row)
			rows = append(rows, row)
		}
		return rag.Report{Rows: rows, Repeats: 1, Stats: []rag.ModeStats{{Mode: rag.RAGCite, Questions: len(rows)}}}, nil
	}
	srv := serve(t, a)
	var ev EvalView
	if code := post(t, srv, "/api/kb/evals", EvalRequest{Modes: []rag.Mode{rag.RAG, rag.RAGCite}}, &ev); code != http.StatusAccepted || len(ev.Request.Modes) != 2 {
		t.Fatalf("прогон rag+cite: %d %+v", code, ev)
	}
	v := waitEval(t, srv, ev.ID, func(v EvalView) bool { return v.State == StateDone })
	if len(v.Rows) != 2 {
		t.Fatalf("строк %d", len(v.Rows))
	}
	for _, row := range v.Rows {
		runs := row.Runs[rag.RAGCite]
		if len(runs) != 1 || runs[0].Answer.Cited == nil || runs[0].Answer.Support == nil || !runs[0].Answer.Cited.Check.HasQuotes {
			t.Fatalf("%s: %+v", row.Question.ID, runs)
		}
		if row.Runs[rag.RAG][0].Answer.Cited != nil {
			t.Fatalf("%s: cited у rag", row.Question.ID)
		}
	}
	if v.Report == nil || len(v.Report.Stats) != 1 || v.Report.Stats[0].Mode != rag.RAGCite {
		t.Fatalf("итог: %+v", v.Report)
	}
}

func TestChunk(t *testing.T) {
	st := newStore(t, true)
	srv := newAPI(t, st, nil)
	chunks, err := st.Chunks(context.Background(), "structure", "manul")
	if err != nil {
		t.Fatal(err)
	}
	var want kb.Chunk
	for _, c := range chunks {
		if c.Section == "Питание" {
			want = c
		}
	}
	if want.ID == "" {
		t.Fatalf("нет чанка «Питание»: %+v", chunks)
	}

	// Косые черты id — как есть и закодированными (%2F).
	for _, path := range []string{"/api/kb/chunk/" + want.ID, "/api/kb/chunk/" + strings.ReplaceAll(want.ID, "/", "%2F")} {
		var v ChunkView
		if code := get(t, srv, path, &v); code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, code)
		}
		if v.Chunk.ID != want.ID || !strings.Contains(v.Chunk.Text, "мелкими грызунами") || v.Chunk.Start != want.Start || v.Chunk.End != want.End {
			t.Fatalf("чанк: %+v", v.Chunk)
		}
		if strings.Join(v.Chunk.Path, " › ") != "Образ жизни › Питание" || v.Chunk.Title != "Манул" {
			t.Fatalf("путь: %v %q", v.Chunk.Path, v.Chunk.Title)
		}
		if v.Doc.ID != "manul" || v.Doc.Title != "Манул" || v.Doc.RevID != 7 || v.Doc.Chars == 0 {
			t.Fatalf("документ: %+v", v.Doc)
		}
	}

	var e map[string]string
	for _, path := range []string{"/api/kb/chunk/manul/structure/999", "/api/kb/chunk/", "/api/kb/chunk/нет"} {
		if code := get(t, srv, path, &e); code != http.StatusNotFound || e["error"] == "" {
			t.Fatalf("GET %s: %d %v", path, code, e)
		}
	}
	if code := do(t, srv, http.MethodPost, "/api/kb/chunk/"+want.ID, nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST chunk: %d", code)
	}
	// Базы нет — 503 с подсказкой, как у остальных разделов.
	none := newAPI(t, nil, nil)
	if code := get(t, none, "/api/kb/chunk/"+want.ID, &e); code != http.StatusServiceUnavailable || e["hint"] == "" {
		t.Fatalf("без базы: %d %v", code, e)
	}
}
