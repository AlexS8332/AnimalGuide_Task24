package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
)

func miniQuestions() QuestionSet {
	return QuestionSet{Schema: QuestionsSchema, Questions: []Question{
		{ID: "T01", Split: SplitTest, Type: "fact", Q: "Чем кормится манул?", Answerable: true,
			Sources:  []SourceRef{{DocID: "manul"}},
			Evidence: []Evidence{{DocID: "manul", Quote: "Кормится манул почти исключительно мелкими грызунами и пищухами."}}},
		{ID: "T02", Split: SplitTest, Type: "number", Q: "Сколько весит корсак?", Answerable: true,
			Sources:  []SourceRef{{DocID: "corsac"}},
			Evidence: []Evidence{{DocID: "corsac", Quote: "Весит от 2,5 до 4 кг."}}},
		{ID: "T03", Split: SplitTest, Type: "aspect-missing", Q: "Какой у манула пульс?"},
		{ID: "D01", Split: SplitDev, Type: "followup", Q: "А когда рождаются котята?", Context: []string{"Расскажи о мануле"},
			Answerable: true, Sources: []SourceRef{{DocID: "manul"}},
			Evidence: []Evidence{{DocID: "manul", Quote: "Котята рождаются в апреле–мае."}}},
		{ID: "O01", Split: SplitOut, Type: "out-of-base", Q: "Сколько весит жираф?"},
	}}
}

// TestCompare — числа отчёта сходятся с поиском, out и вопросы без
// доказательств не считаются, разорванные доказательства видны.
func TestCompare(t *testing.T) {
	ctx := context.Background()
	st := miniStore(t)
	if _, err := st.Build(ctx, NewStructure(300, 80), embed.Hash{}, nil); err != nil {
		t.Fatal(err)
	}
	// Окно в 40 символов с перекрытием 5: 63-символьное доказательство
	// T01 не помещается ни в один чанк.
	if _, err := st.Build(ctx, NewFixed(40, 5), embed.Hash{}, nil); err != nil {
		t.Fatal(err)
	}
	s := &Searcher{Store: st, Embedder: embed.Hash{}}
	qs := miniQuestions()
	r, err := Compare(ctx, s, qs, CompareOptions{Mode: BM25})
	if err != nil {
		t.Fatal(err)
	}
	if r.CorpusSHA != "sha-1" || r.Docs != 2 || r.Embedder != "hash-256" || r.Budget != DefaultBudget {
		t.Fatalf("шапка: %+v", r)
	}
	if len(r.Stats) != 2 || r.Stats[0].Index != "structure" || r.Stats[1].Index != "fixed" {
		t.Fatalf("stats: %+v", r.Stats)
	}
	ss, fs := r.Stats[0], r.Stats[1]
	if ss.OverlapShare != 0 || fs.OverlapShare <= 0 || fs.MidSentence <= ss.MidSentence || fs.SplitSections <= ss.SplitSections ||
		ss.P50 == 0 || ss.P95 < ss.P50 || ss.Bytes == 0 || fs.MixedShare == 0 {
		t.Fatalf("структурные метрики: %+v / %+v", ss, fs)
	}
	// 2 индекса × 2 режима × 2 набора.
	if len(r.Retrieval) != 8 {
		t.Fatalf("строк поиска %d", len(r.Retrieval))
	}
	for _, x := range r.Retrieval {
		wantN := map[string]int{SplitTest: 2, SplitDev: 1}[x.Split]
		if x.N != wantN || len(x.Rows) != wantN {
			t.Fatalf("%s/%s/%s: N=%d, ждали %d (out и без доказательств не считаются)", x.Index, x.Mode, x.Split, x.N, wantN)
		}
		// Recall и MRR пересчитываются из рангов.
		hit := map[int]int{}
		var rr float64
		for _, row := range x.Rows {
			if row.Rank > 0 {
				rr += 1 / float64(row.Rank)
				for _, k := range []int{1, 3, 5} {
					if row.Rank <= k {
						hit[k]++
					}
				}
			}
			if len(row.Top) == 0 || len(row.Top) > 5 {
				t.Fatalf("топ %v", row.Top)
			}
			// Ранг — по настоящему поиску.
			q := findQ(qs, row.ID)
			query := strings.Join(append(append([]string(nil), q.Context...), q.Q), " ")
			hits, _, _ := s.Search(ctx, query, SearchOptions{Index: x.Index, K: searchDepth, Mode: x.Mode})
			if hits[0].ID != row.Top[0] {
				t.Fatalf("%s: топ отчёта %s, поиск %s", row.ID, row.Top[0], hits[0].ID)
			}
		}
		for _, k := range []int{1, 3, 5} {
			if x.Recall[k] != float64(hit[k])/float64(x.N) {
				t.Fatalf("%s: recall@%d %v", x.Index, k, x.Recall[k])
			}
		}
		if x.MRR != rr/float64(x.N) {
			t.Fatalf("MRR %v", x.MRR)
		}
		switch {
		case x.Index == "structure" && x.BrokenEvidence != 0:
			t.Fatalf("structure разорвал доказательство: %v", x.BrokenEvidence)
		case x.Index == "fixed" && x.Split == SplitTest && x.BrokenEvidence == 0:
			t.Fatalf("fixed: окно в 40 символов не разорвало ни одного доказательства")
		}
		if x.Index == "fixed" && x.Split == SplitTest && x.Rows[0].Rank != 0 {
			t.Fatalf("разорванное доказательство «нашлось»: ранг %d", x.Rows[0].Rank)
		}
	}
	if len(r.Conclusion) < 5 || len(r.Conclusion) > 8 {
		t.Fatalf("вывод: %q", r.Conclusion)
	}
	if !strings.Contains(r.Conclusion[0], "structure: recall@5 3 из 3 против 0 из 3 у fixed (+3)") || !regexp.MustCompile(`\d\.\d\d`).MatchString(r.Conclusion[0]) {
		t.Fatalf("вывод без чисел: %q", r.Conclusion[0])
	}
	if !strings.Contains(strings.Join(r.Conclusion, "\n"), "BM25 (справочно)") {
		t.Fatalf("нет справочной строки BM25: %q", r.Conclusion)
	}

	md := r.Markdown()
	for _, want := range []string{"# Сравнение стратегий чанкинга", "sha-1", "## Структура индексов", "## Поиск",
		"## Вопросы test", "| T01 |", "| T02 |", "## Вывод", "structure bm25", "fixed dense"} {
		if !strings.Contains(md, want) {
			t.Errorf("в markdown нет %q", want)
		}
	}
	if strings.Contains(md, "| T03 |") || strings.Contains(md, "O01") {
		t.Error("вопрос без доказательств попал в таблицу")
	}

	last, ok, err := st.LastReport(ctx)
	if err != nil || !ok {
		t.Fatalf("LastReport: %v %v", ok, err)
	}
	a, _ := json.Marshal(last)
	b, _ := json.Marshal(r)
	if string(a) != string(b) {
		t.Fatal("сохранённый отчёт отличается")
	}
}

// TestCompareFallbackAndEmpty — без эмбеддера сравнение идёт по BM25 и
// честно говорит об этом; пустая база — ошибка, а не пустой отчёт.
func TestCompareFallbackAndEmpty(t *testing.T) {
	ctx := context.Background()
	st := miniStore(t)
	if _, ok, err := st.LastReport(ctx); ok || err != nil {
		t.Fatalf("отчёта ещё нет: %v %v", ok, err)
	}
	if _, err := Compare(ctx, &Searcher{Store: st}, miniQuestions(), CompareOptions{}); err == nil {
		t.Fatal("без индексов — без ошибки")
	}
	if _, err := st.Build(ctx, NewStructure(300, 80), nil, nil); err != nil {
		t.Fatal(err)
	}
	r, err := Compare(ctx, &Searcher{Store: st}, miniQuestions(), CompareOptions{Mode: BM25, Splits: []string{SplitTest}, K: []int{1, 5}, Budget: 50})
	if err != nil {
		t.Fatal(err)
	}
	// Dense откатился на BM25 — справочная строка BM25 не дублируется.
	if len(r.Retrieval) != 1 || r.Retrieval[0].Mode != BM25 || r.Retrieval[0].Fallback == "" || r.Budget != 50 {
		t.Fatalf("строки: %+v", r.Retrieval)
	}
	if !strings.Contains(r.Embedder, "BM25") {
		t.Fatalf("эмбеддер: %q", r.Embedder)
	}
	joined := strings.Join(r.Conclusion, "\n")
	if !strings.Contains(joined, "structure: recall@5 2 из 2, recall@1") || strings.Contains(joined, "recall@3") ||
		!strings.Contains(joined, "Векторный поиск не состоялся") {
		t.Fatalf("вывод: %q", r.Conclusion)
	}
	if !strings.Contains(r.Markdown(), "bm25 (откат)") {
		t.Fatal("в markdown не отмечен откат")
	}
}

func findQ(qs QuestionSet, id string) Question {
	for _, q := range qs.Questions {
		if q.ID == id {
			return q
		}
	}
	return Question{}
}

// TestRealCorpusEvidence — на настоящем корпусе и разметке structure с
// умолчаниями не рвёт ни одного доказательства (fixed — может: это и
// меряется).
func TestRealCorpusEvidence(t *testing.T) {
	docs := realDocs(t)
	qs, err := LoadQuestions("../../eval/questions.json")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]corpus.Doc{}
	for _, d := range docs {
		byID[d.ID] = d
	}
	ch := NewStructure(0, 0)
	cache := map[string][]Chunk{}
	for _, q := range qs.Questions {
		for _, e := range q.Evidence {
			d := byID[e.DocID]
			if cache[d.ID] == nil {
				cache[d.ID] = ch.Split(d)
			}
			start, n := corpus.Find(d.Text(), e.Quote)
			if !coveredBy(cache[d.ID], evidence{doc: d.ID, s: start, e: start + n}) {
				t.Errorf("%s: доказательство разорвано structure: %q", q.ID, e.Quote)
			}
		}
	}
}

// TestRecallAllAndWholeEvidence — многофактный вопрос: recall@5 засчитан по
// одному найденному факту, а recall_all@5 — только когда в топе все
// доказательства; «целиком в одном чанке» различает стратегии там, где
// «разорвано» молчит.
func TestRecallAllAndWholeEvidence(t *testing.T) {
	ctx := context.Background()
	st := miniStore(t)
	if _, err := st.Build(ctx, NewStructure(300, 80), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Build(ctx, NewFixed(45, 20), nil, nil); err != nil {
		t.Fatal(err)
	}
	qs := QuestionSet{Schema: QuestionsSchema, Questions: []Question{
		{ID: "T01", Split: SplitTest, Type: "compare", Q: "Чем кормится манул и сколько весит корсак?", Answerable: true,
			Evidence: []Evidence{
				{DocID: "manul", Quote: "Кормится манул почти исключительно мелкими грызунами и пищухами."},
				{DocID: "corsac", Quote: "Весит от 2,5 до 4 кг."}}},
	}}
	r, err := Compare(ctx, &Searcher{Store: st}, qs, CompareOptions{Splits: []string{SplitTest}})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range r.Retrieval {
		if x.Multi != 1 || len(x.Rows) != 1 {
			t.Fatalf("%s: multi %d, строк %d", x.Index, x.Multi, len(x.Rows))
		}
		row := x.Rows[0]
		switch x.Index {
		case "structure":
			if !row.All5 || x.RecallAll5 != 1 || x.RecallAllBudget != 1 {
				t.Fatalf("structure: %+v", x)
			}
		case "fixed":
			// Корсак найден (recall@5 = 1), а цитата о мануле (63 символа)
			// в окно 45 не влезает и на 80 % — «всех в топе» нет.
			if row.Rank == 0 || row.All5 || x.Recall[5] != 1 || x.RecallAll5 != 0 || x.RecallAllBudget != 0 {
				t.Fatalf("fixed: %+v", x)
			}
		}
	}
	s, _ := r.stats("structure")
	f, _ := r.stats("fixed")
	if s.Evidence != 2 || s.WholeEvidence != 1 || f.WholeEvidence != 0.5 {
		t.Fatalf("целиком в одном чанке: structure %v из %d, fixed %v", s.WholeEvidence, s.Evidence, f.WholeEvidence)
	}
	md := r.Markdown()
	for _, want := range []string{"recall_all@5", "все в бюджете", "Доказательство целиком в одном чанке", "100.0 % из 2", "50.0 % из 2", "| 1 (1) |"} {
		if !strings.Contains(md, want) {
			t.Errorf("в markdown нет %q", want)
		}
	}
	if j := strings.Join(r.Conclusion, "\n"); !strings.Contains(j, "recall_all@5): 1 из 1 у structure против 0 из 1 у fixed") {
		t.Fatalf("вывод: %s", j)
	}
}

// TestPairVerdict — «лучше» только при перевесе ≥ NoiseQuestions вопросов
// по recall@5; ранги сравниваются попарно, не найденное хуже любого ранга.
func TestPairVerdict(t *testing.T) {
	rows := func(ranks ...int) []RetrievalRow {
		var out []RetrievalRow
		for i, r := range ranks {
			out = append(out, RetrievalRow{ID: fmt.Sprintf("Q%02d", i), Rank: r})
		}
		return out
	}
	// A: 1 2 0 7 3 1;  B: 2 2 4 0 0 0.
	p := pair(rows(1, 2, 0, 7, 3, 1), rows(2, 2, 4, 0, 0, 0))
	if p.n != 6 || p.hitA != 4 || p.hitB != 3 || p.onlyA != 2 || p.onlyB != 1 ||
		p.better != 4 || p.worse != 1 || p.equal != 1 {
		t.Fatalf("pair: %+v", p)
	}
	if v := p.verdict("structure", "fixed"); !strings.HasPrefix(v, "разница в пределах шума") || !strings.Contains(v, "— 1,") {
		t.Fatalf("перевес 1: %q", v)
	}
	p = pair(rows(1, 1, 1, 1, 0), rows(0, 0, 0, 9, 1))
	if v := p.verdict("structure", "fixed"); !strings.HasPrefix(v, "structure лучше") {
		t.Fatalf("перевес 3: %q (%+v)", v, p)
	}
	if v := pair(rows(0, 0, 0, 1), rows(1, 2, 3, 1)).verdict("structure", "fixed"); !strings.HasPrefix(v, "fixed лучше") {
		t.Fatalf("перевес −3: %q", v)
	}
	// Строки без пары не считаются.
	if p := pair(rows(1, 1), rows(1)); p.n != 1 {
		t.Fatalf("без пары: %+v", p)
	}
}
