package retrieve

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
)

// realPipeline — настоящий корпус, индекс structure на hash-эмбеддере, и
// контрольные вопросы репозитория. Числа hash-поиска смысла не имеют:
// проверяется счёт, а не качество.
func realPipeline(t *testing.T) (*Pipeline, kb.QuestionSet) {
	t.Helper()
	ctx := context.Background()
	docs, man, err := corpus.Load("../../corpus")
	if err != nil {
		t.Fatal(err)
	}
	st, err := kb.Open(ctx, filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.PutCorpus(ctx, docs, man); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Build(ctx, kb.NewStructure(0, 0), embed.Hash{}, nil); err != nil {
		t.Fatal(err)
	}
	qs, err := kb.LoadQuestions("../../eval/questions.json")
	if err != nil {
		t.Fatal(err)
	}
	return &Pipeline{Searcher: &kb.Searcher{Store: st, Embedder: embed.Hash{}}}, qs
}

func TestRunMatrixAndCalibrate(t *testing.T) {
	ctx := context.Background()
	p, qs := realPipeline(t)
	m, err := RunMatrix(ctx, p, qs, nil, []int{3, 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Rows) != 5*2*3 || m.Index != "structure" || m.Embedder != "hash-256" || m.MinScore != DefaultMinScore ||
		m.Delta != DefaultDelta || m.K0 != DefaultK0 || m.Fallback != "" || m.CorpusSHA == "" || len(m.Configs) != 5 {
		t.Fatalf("матрица: %d строк, %+v", len(m.Rows), m.Configs)
	}
	for _, r := range m.Rows {
		wantN, wantOut := map[string]int{"dev": 19, "test": 8, "out": 0}[r.Split], map[string]int{"dev": 0, "test": 2, "out": 8}[r.Split]
		if r.N != wantN || r.OutN != wantOut || len(r.Rows) != map[string]int{"dev": 19, "test": 10, "out": 8}[r.Split] {
			t.Fatalf("%s %s %d: N %d, OutN %d, вопросов %d", r.Name, r.Split, r.K1, r.N, r.OutN, len(r.Rows))
		}
		if r.RecallAfter > r.RecallUnion+1e-9 || r.RecallBefore > r.RecallUnion+1e-9 || r.Precision > 1 || r.CutShare > 1 || r.WrongCut > 1 || r.LostQ > 1 {
			t.Fatalf("%s %s: метрики %+v", r.Name, r.Split, r)
		}
		if (r.Name == "base" || r.Name == "rewrite") && (r.CutShare != 0 || r.OutEmpty != 0) {
			t.Fatalf("без фильтра отсечено: %+v", r)
		}
		for _, q := range r.Rows {
			if q.RankAfter > r.K1 || q.Kept > r.K1 || (q.Empty && q.Kept != 0) {
				t.Fatalf("%s %s: %+v", r.Name, q.ID, q)
			}
			if q.ID == "T07" && r.Name == "rewrite" && (!strings.HasSuffix(q.Rewritten, "малая панда") || strings.Contains(q.Rewritten, "Ailurus")) {
				t.Fatalf("T07 не переписан: %q", q.Rewritten)
			}
			if q.ID == "T08" && r.Name == "both" && !strings.Contains(q.Rewritten, "харза") {
				t.Fatalf("T08 без вида из контекста: %q", q.Rewritten)
			}
			if q.Unanswerable != (q.ID == "T09" || q.ID == "T10" || strings.HasPrefix(q.ID, "O")) {
				t.Fatalf("%s: неотвечаемый %v", q.ID, q.Unanswerable)
			}
		}
	}
	md := m.Markdown()
	for _, want := range []string{"# Фильтр релевантности", "## Вывод", "## Матрица", "## Вопросы test (K1 = 5)", "## Вопросы dev (K1 = 5)",
		"| T07 | synonym |", "`both` — rewrite code, filter", "27 вопросов с доказательством", "Выборка мала"} {
		if !strings.Contains(md, want) {
			t.Errorf("в отчёте нет %q", want)
		}
	}
	if _, err := json.Marshal(m); err != nil {
		t.Fatal(err)
	}

	cal, err := Calibrate(ctx, p, qs, "", 0, 0.05, false)
	if err != nil {
		t.Fatal(err)
	}
	if cal.DevN != 19 || cal.OutN != 8 || len(cal.DevTop) != 19 || len(cal.OutTop) != 8 || cal.Written || cal.Index != "structure" ||
		cal.Delta != DefaultDelta || len(cal.Table) < 3 {
		t.Fatalf("калибровка: %+v", cal)
	}
	found := false
	for i, r := range cal.Table {
		if i > 0 {
			if d := r.MinScore - cal.Table[i-1].MinScore; d < CalibStep-1e-9 || d > CalibStep+1e-9 {
				t.Fatalf("шаг %v", d)
			}
			if r.DevRecall > cal.Table[i-1].DevRecall+1e-9 || r.OutEmpty+1e-9 < cal.Table[i-1].OutEmpty {
				t.Fatalf("выше порог — recall не растёт, пустых не меньше: %+v", r)
			}
		}
		if r.MinScore == cal.Chosen {
			found = true
			if r.DevRecall < cal.Base-0.05-1e-9 {
				t.Fatalf("выбран порог, теряющий recall: %+v", r)
			}
		}
		if r.MinScore > cal.Chosen && r.DevRecall >= cal.Base-0.05-1e-9 {
			t.Fatalf("выбран не наибольший порог: %v, а %v тоже годится", cal.Chosen, r.MinScore)
		}
	}
	if !found || !strings.Contains(cal.Markdown(), "## Таблица") {
		t.Fatalf("выбранный порог %v не из таблицы", cal.Chosen)
	}
	if ix, _ := p.Searcher.Store.Index(ctx, "structure"); ix.MinScore != 0 {
		t.Fatal("калибровка без write записала порог")
	}
	cal, err = Calibrate(ctx, p, qs, "structure", 0.1, 0.05, true)
	if err != nil || !cal.Written || !strings.Contains(cal.Markdown(), "Записан в индекс") {
		t.Fatalf("write: %v", err)
	}
	if ix, _ := p.Searcher.Store.Index(ctx, "structure"); ix.MinScore != cal.Chosen {
		t.Fatalf("в индексе %v, выбран %v", ix.MinScore, cal.Chosen)
	}

	// Без векторов калибровать нечего.
	bm := &Pipeline{Searcher: &kb.Searcher{Store: p.Searcher.Store}}
	if _, err := Calibrate(ctx, bm, qs, "", 0, 0.05, false); err == nil || !strings.Contains(err.Error(), "BM25") {
		t.Fatalf("калибровка по BM25: %v", err)
	}
	// Матрица по BM25 — с причиной отката.
	m, err = RunMatrix(ctx, bm, qs, Presets(false)[:2], []int{5}, []string{kb.SplitOut})
	if err != nil || m.Fallback == "" || !strings.Contains(m.Markdown(), "Поиск шёл без векторов") {
		t.Fatalf("откат: %q %v", m.Fallback, err)
	}
}

// TestConclude — вывод матрицы на подставных строках: вопросы, а не доли.
func TestConclude(t *testing.T) {
	q := func(id string, after int, empty, un bool) MatrixQ {
		return MatrixQ{ID: id, Type: "fact", RankBefor: max(after, 1), RankAfter: after, Kept: 3, Empty: empty,
			Answerable: !un, Unanswerable: un, Cut: after == 0 && !un}
	}
	m := Matrix{Configs: []Named{{Name: "base"}, {Name: "filter", Config: Config{Filter: true}}, {Name: "rewrite", Config: Config{Rewrite: RewriteCode}}}}
	m.Rows = []MatrixRow{
		{Name: "base", Split: "dev", K1: 5, N: 2, Rows: []MatrixQ{q("D1", 1, false, false), q("D2", 0, false, false)}},
		{Name: "base", Split: "out", K1: 5, OutN: 1, Tokens: 100, Rows: []MatrixQ{q("O1", 0, false, true)}},
		{Name: "filter", Split: "dev", K1: 5, N: 2, CutShare: 0.5, Rows: []MatrixQ{q("D1", 0, false, false), q("D2", 0, false, false)}},
		{Name: "filter", Split: "out", K1: 5, OutN: 1, CutShare: 1, Rows: []MatrixQ{q("O1", 0, true, true)}},
		{Name: "rewrite", Split: "dev", K1: 5, N: 2, Rows: []MatrixQ{q("D1", 1, false, false), q("D2", 3, false, false)}},
		{Name: "rewrite", Split: "out", K1: 5, OutN: 1, Rows: []MatrixQ{q("O1", 0, false, true)}},
	}
	got := strings.Join(conclude(m, []int{5}), "\n")
	for _, want := range []string{
		"K1 = 5, dev+test, 2 вопросов с доказательством: доказательство в итоговой выдаче — base 1, filter 0 (−D1), rewrite 2 (+D2).",
		"Неотвечаемые (out и answerable=false, 1): пусто после фильтра — base 0, filter 1, rewrite 0 из 1.",
		"filter: фильтр отсёк в среднем 75 % кандидатов; фильтр отсёк доказательство, которое base показывал: D1 (ранг до 1).",
		"Выборка мала",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("нет строки:\n%s\nв выводе:\n%s", want, got)
		}
	}
}

// TestMeasure — метрики вопроса: доказательство только в BM25 (не в dense
// K0), снято фильтром — релевантный среди отсечённых и «доказательство
// снято».
func TestMeasure(t *testing.T) {
	ev := []evidence{{doc: "a", s: 0, e: 10}}
	q := kb.Question{ID: "D1", Type: "fact", Split: kb.SplitDev, Answerable: true}
	tr := Trace{Info: kb.SearchInfo{Mode: kb.Dense}, TopDense: 0.9, Anchored: []string{"x"}, Candidates: []Candidate{
		{Hit: kb.Hit{Chunk: kb.Chunk{ID: "b", DocID: "b", Start: 0, End: 50}}, Dense: 0.9, RankDense: 1, Final: 1, Kept: true},
		{Hit: kb.Hit{Chunk: kb.Chunk{ID: "a", DocID: "a", Start: 0, End: 20}}, Dense: 0.7, RankBM25: 1, Final: 2, Reason: "хуже лучшего на 0.05"},
		{Hit: kb.Hit{Chunk: kb.Chunk{ID: "c", DocID: "c", Start: 0, End: 20}}, Dense: 0.6, RankDense: 2, Final: 3, Reason: "порог 0.800"},
	}}
	tr.Hits = []kb.Hit{tr.Candidates[0].Hit}
	row, m := measure(q, ev, tr)
	if row.InDense || row.RankBefor != 2 || row.RankAfter != 0 || m.cut != 2 || m.relCut != 1 || m.relHits != 0 || !row.Anchored || row.TopDense != 0.9 {
		t.Fatalf("measure: %+v %+v", row, m)
	}
	if !lostByFilter(ev, tr, row) {
		t.Fatal("доказательство снято фильтром, а не отмечено")
	}
	tr.Candidates[1].Reason = ReasonBeyond
	if lostByFilter(ev, tr, row) {
		t.Fatal("ушло за K1 — не снято фильтром")
	}
}
