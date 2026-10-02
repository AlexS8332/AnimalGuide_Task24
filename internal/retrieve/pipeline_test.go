package retrieve

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
)

// miniDocs — мини-корпус с синонимами: малая панда («кошачий медведь»),
// корсак («степная лисица»), бурый медведь и обзор без вида.
func miniDocs() []corpus.Doc {
	return []corpus.Doc{
		{ID: "red-panda", Source: corpus.SourceWikipedia, Title: "Малая панда", License: "CC BY-SA 4.0",
			Species: &corpus.Species{Latin: "Ailurus fulgens", Ru: "малая панда", Aliases: []string{"кошачий медведь", "красная панда"}},
			Intro:   "Малая панда — млекопитающее семейства пандовых. Обитает в Гималаях.",
			Sections: []corpus.Section{
				{Path: []string{"Питание"}, Title: "Питание", Level: 2,
					Text: "Малые панды питаются бамбуком. На приёмы пищи малые панды тратят по 13 часов в день."},
				{Path: []string{"Размеры"}, Title: "Размеры", Level: 2, Text: "Масса малой панды — от 3 до 6 кг."},
			}},
		{ID: "corsac", Source: corpus.SourceWikipedia, Title: "Корсак", License: "CC BY-SA 4.0",
			Species: &corpus.Species{Latin: "Vulpes corsac", Ru: "корсак", Aliases: []string{"степная лисица"}},
			Intro:   "Корсак — хищник рода лисиц. Обитает в степях Азии.",
			Sections: []corpus.Section{
				{Path: []string{"Размеры"}, Title: "Размеры", Level: 2, Text: "Корсак весит от 2,5 до 4 кг."},
				{Path: []string{"Бег"}, Title: "Бег", Level: 2, Text: "Корсак бегает со скоростью до 60 км/ч."},
			}},
		{ID: "brown-bear", Source: corpus.SourceWikipedia, Title: "Бурый медведь", License: "CC BY-SA 4.0",
			Species: &corpus.Species{Latin: "Ursus arctos", Ru: "бурый медведь", Aliases: []string{"медведь"}},
			Intro:   "Бурый медведь — хищное млекопитающее семейства медвежьих. Весит до 600 кг."},
		{ID: "carnivora", Source: corpus.SourceWikipedia, Title: "Хищные", License: "CC BY-SA 4.0",
			Intro: "Хищные — отряд млекопитающих. Пандовые, псовые и медвежьи — семейства хищных."},
	}
}

func miniManifest(docs []corpus.Doc) corpus.Manifest {
	m := corpus.Manifest{Schema: corpus.Schema, CorpusSHA: "sha-mini"}
	for _, d := range docs {
		m.Entries = append(m.Entries, corpus.Entry{ID: d.ID, File: d.ID + ".json", Title: d.Title, Source: d.Source,
			Chars: d.Chars(), SHA256: "sha-" + d.ID})
		m.Chars += d.Chars()
	}
	return m
}

// miniStore — kb.db мини-корпуса с индексом structure на hash-эмбеддере.
func miniStore(t *testing.T) *kb.Store {
	t.Helper()
	ctx := context.Background()
	st, err := kb.Open(ctx, filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	docs := miniDocs()
	if err := st.PutCorpus(ctx, docs, miniManifest(docs)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Build(ctx, kb.NewStructure(0, 0), embed.Hash{}, nil); err != nil {
		t.Fatal(err)
	}
	return st
}

func miniPipeline(t *testing.T) *Pipeline {
	t.Helper()
	return &Pipeline{Searcher: &kb.Searcher{Store: miniStore(t), Embedder: embed.Hash{}}}
}

func TestSearchRewriteCode(t *testing.T) {
	ctx := context.Background()
	p := miniPipeline(t)
	tr, err := p.Search(ctx, Query{Text: "Сколько часов в день кошачий медведь тратит на еду?"}, Config{Rewrite: RewriteCode})
	if err != nil {
		t.Fatal(err)
	}
	// Dense — реплика и канон, без латыни; латынь — только в BM25.
	if !strings.HasSuffix(tr.Rewritten, "? малая панда") || tr.RewriteBy != "code" ||
		len(tr.Expanded) != 1 || tr.Expanded[0] != "кошачий медведь → малая панда" || len(tr.Queries) != 1 || tr.Queries[0] != tr.Rewritten ||
		len(tr.QueriesBM25) != 1 || tr.QueriesBM25[0] != tr.Rewritten+" Ailurus fulgens" {
		t.Fatalf("переписывание: %q %q %v", tr.Rewritten, tr.QueriesBM25, tr.Expanded)
	}
	if strings.Join(tr.Anchored, ",") != "малая панда" {
		t.Fatalf("якорь: %v", tr.Anchored)
	}
	if tr.Info.Mode != kb.Dense || tr.Config.K0 != DefaultK0 || tr.Config.K1 != DefaultK1 || tr.Config.Index != DefaultIndex ||
		tr.MinScore != DefaultMinScore || len(tr.Hits) == 0 || len(tr.Hits) > DefaultK1 {
		t.Fatalf("поиск: %+v", tr)
	}
	if tr.Hits[0].DocID != "red-panda" {
		t.Fatalf("первый — не малая панда: %+v", tr.Hits[0])
	}
	// У каждого кандидата есть косинус: и у найденных только BM25 (Score).
	bm25Only := 0
	for _, c := range tr.Candidates {
		if c.Dense == 0 {
			t.Fatalf("кандидат без косинуса: %+v", c)
		}
		if c.RankDense == 0 {
			bm25Only++
		}
		if c.Final == 0 {
			t.Fatalf("без ранга: %+v", c)
		}
	}
	if tr.TopDense <= 0 {
		t.Fatal("нет лучшего косинуса")
	}

	// Канон в реплике — dense-запрос не меняется, латынь — в BM25.
	tr, err = p.Search(ctx, Query{Text: "Сколько весит корсак?"}, Config{Rewrite: RewriteCode})
	if err != nil || tr.Rewritten != tr.Original || tr.Queries[0] != tr.Original || tr.QueriesBM25[0] != "Сколько весит корсак? Vulpes corsac" {
		t.Fatalf("канон: %q %q %v", tr.Queries, tr.QueriesBM25, err)
	}

	// Продолжение: прошлая реплика, где назван вид (последняя), и текущая;
	// вид из контекста — не якорь.
	tr, err = p.Search(ctx, Query{Text: "А сколько он весит?", Context: []string{"Чем питается малая панда?", "Где живёт корсак?"}},
		Config{Rewrite: RewriteCode})
	if err != nil || tr.Rewritten != "Где живёт корсак? А сколько он весит?" || tr.QueriesBM25[0] != "Где живёт корсак? Vulpes corsac А сколько он весит?" ||
		!strings.Contains(strings.Join(tr.Expanded, ";"), "вид из контекста → корсак") || len(tr.Anchored) != 0 {
		t.Fatalf("продолжение: %q %q %v %v %v", tr.Rewritten, tr.QueriesBM25, tr.Expanded, tr.Anchored, err)
	}
	// Синоним в прошлой реплике раскрывается и там.
	tr, _ = p.Search(ctx, Query{Text: "А она где живёт?", Context: []string{"Что ест кошачий медведь?"}}, Config{Rewrite: RewriteCode})
	if tr.Rewritten != "Что ест кошачий медведь? малая панда А она где живёт?" {
		t.Fatalf("продолжение с синонимом: %q", tr.Rewritten)
	}
	// Самостоятельная реплика (вид назван или названо другое животное) —
	// контекст не учитывается.
	for _, q := range []Query{
		{Text: "Сколько весит взрослый жираф?", Context: []string{"Где живёт корсак?"}},
		{Text: "Где живёт малая панда?", Context: []string{"Сколько весит корсак?"}},
		{Text: "Чем питаются хищники в степях Азии и Гималаях?", Context: []string{"Где живёт корсак?"}},
	} {
		tr, err = p.Search(ctx, q, Config{Rewrite: RewriteCode})
		if err != nil || strings.Contains(tr.Rewritten, "корсак") || tr.Note != "" {
			t.Fatalf("самостоятельная %q: %q %q %v", q.Text, tr.Rewritten, tr.Note, err)
		}
	}
	// Вида в контексте нет — запрос как без переписывания, с заметкой.
	tr, err = p.Search(ctx, Query{Text: "А сколько их?", Context: []string{"Расскажи про хищных"}}, Config{Rewrite: RewriteCode})
	if err != nil || tr.Rewritten != "Расскажи про хищных А сколько их?" || !strings.Contains(tr.Note, "вид в контексте не назван") {
		t.Fatalf("без вида: %q %q %v", tr.Rewritten, tr.Note, err)
	}
	// Без переписывания контекст в запрос не идёт: только реплика.
	tr, err = p.Search(ctx, Query{Text: "А сколько он весит?", Context: []string{"Где живёт корсак?"}}, Config{Filter: true})
	if err != nil || tr.Rewritten != tr.Original || len(tr.Queries) != 1 || tr.Queries[0] != "А сколько он весит?" || tr.QueriesBM25 != nil ||
		tr.RewriteBy != "" || len(tr.Anchored) != 0 {
		t.Fatalf("base: %q %q %v %v", tr.Queries, tr.QueriesBM25, tr.Anchored, err)
	}
}

// TestSearchBaseIsDense — base (без фильтра и переписывания) совпадает с
// прямым dense-поиском: режим rag+… отличается только вторым этапом.
func TestSearchBaseIsDense(t *testing.T) {
	ctx := context.Background()
	p := miniPipeline(t)
	q := "Сколько весит малая панда?"
	tr, err := p.Search(ctx, Query{Text: q}, Config{K1: 3})
	if err != nil {
		t.Fatal(err)
	}
	hits, _, err := p.Searcher.Search(ctx, q, kb.SearchOptions{Index: "structure", K: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Hits) != len(hits) {
		t.Fatalf("выдача %d, dense %d", len(tr.Hits), len(hits))
	}
	for i := range hits {
		if tr.Hits[i].ID != hits[i].ID || tr.Hits[i].Rank != i+1 || tr.Hits[i].Score != hits[i].Score {
			t.Fatalf("%d: %s vs %s", i, tr.Hits[i].ID, hits[i].ID)
		}
	}
	for _, c := range tr.Candidates {
		if !c.Kept && c.Reason != ReasonBeyond {
			t.Fatalf("без фильтра отсечено не по K1: %+v", c)
		}
		if c.Rerank != 0 {
			t.Fatalf("без реранкинга есть балл: %+v", c)
		}
	}
}

func TestSearchFallbackBM25(t *testing.T) {
	ctx := context.Background()
	st := miniStore(t)
	p := &Pipeline{Searcher: &kb.Searcher{Store: st}}
	tr, err := p.Search(ctx, Query{Text: "корсак весит"}, Config{Filter: true})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Info.Mode != kb.BM25 || tr.Info.Fallback == "" || !strings.Contains(tr.Note, noteBM25) || tr.MinScore != 0 {
		t.Fatalf("откат: %+v %q", tr.Info, tr.Note)
	}
	if len(tr.Hits) == 0 || tr.Hits[0].DocID != "corsac" {
		t.Fatalf("выдача BM25: %+v", tr.Hits)
	}
	for _, c := range tr.Candidates {
		if c.Dense != 0 || c.RankDense != 0 {
			t.Fatalf("косинус при откате: %+v", c)
		}
		if !c.Kept && strings.HasPrefix(c.Reason, "порог") {
			t.Fatalf("абсолютный порог при откате: %+v", c)
		}
	}
}

func TestSearchErrors(t *testing.T) {
	ctx := context.Background()
	p := miniPipeline(t)
	for _, c := range []struct {
		q   Query
		cfg Config
	}{
		{Query{Text: "  "}, Config{}},
		{Query{Text: "манул"}, Config{Rewrite: RewriteLLM}},
		{Query{Text: "манул"}, Config{Rerank: RerankLLM}},
		{Query{Text: "манул"}, Config{Rewrite: "magic"}},
		{Query{Text: "манул"}, Config{Rerank: "magic"}},
		{Query{Text: "манул"}, Config{Index: "nope"}},
	} {
		if _, err := p.Search(ctx, c.q, c.cfg); err == nil {
			t.Errorf("%+v %+v: нет ошибки", c.q, c.cfg)
		}
	}
	var nilP *Pipeline
	if _, err := nilP.Search(ctx, Query{Text: "манул"}, Config{}); err == nil {
		t.Error("конвейер без базы")
	}
	// Индекс — с порогом: он берётся, если в настройках 0.
	if err := p.Searcher.Store.SetMinScore(ctx, "structure", 0.42); err != nil {
		t.Fatal(err)
	}
	tr, _ := p.Search(ctx, Query{Text: "манул"}, Config{Filter: true})
	if tr.MinScore != 0.42 {
		t.Fatalf("порог индекса: %v", tr.MinScore)
	}
	tr, _ = p.Search(ctx, Query{Text: "манул"}, Config{Filter: true, MinScore: 0.5})
	if tr.MinScore != 0.5 {
		t.Fatalf("порог настроек: %v", tr.MinScore)
	}
}

// cand — кандидат для тестов фильтра.
func cand(id, doc string, start, end int, dense float64, rankDense int) Candidate {
	return Candidate{Hit: kb.Hit{Chunk: kb.Chunk{ID: id, DocID: doc, Start: start, End: end, SHA: "sha-" + id, Tokens: 10}},
		Dense: dense, RankDense: rankDense}
}

func trace(cfg Config, cs ...Candidate) Trace {
	t := Trace{Config: resolve(cfg), Info: kb.SearchInfo{Mode: kb.Dense}, MinScore: 0.8, Candidates: cs}
	for i := range t.Candidates {
		t.Candidates[i].Final = i + 1
		t.TopDense = max(t.TopDense, t.Candidates[i].Dense)
	}
	return t
}

func reasons(t Trace) string {
	var out []string
	for _, c := range t.Candidates {
		r := c.Reason
		if c.Kept {
			r = "ok"
		}
		out = append(out, c.ID+":"+r)
	}
	return strings.Join(out, " ")
}

func TestFinishFilter(t *testing.T) {
	tr := trace(Config{Filter: true, K1: 3},
		cand("a", "x", 0, 100, 0.90, 1),
		cand("b", "x", 100, 200, 0.88, 2),
		cand("c", "x", 120, 190, 0.87, 3), // перекрыт b целиком — повтор
		cand("d", "y", 0, 50, 0.84, 4),    // хуже лучшего больше чем на 0.05
		cand("e", "y", 50, 90, 0.79, 5),   // ниже пола
		cand("f", "z", 0, 90, 0.86, 6),
		cand("g", "z", 90, 190, 0.86, 7), // прошёл фильтр, но K1 = 3
	)
	finish(&tr)
	want := "a:ok b:ok c:повтор текста d:хуже лучшего на 0.05 e:порог 0.800 f:ok g:за пределами K1"
	if got := reasons(tr); got != want {
		t.Fatalf("причины:\n%s\nждали\n%s", got, want)
	}
	if len(tr.Hits) != 3 || tr.Hits[2].ID != "f" || tr.Hits[2].Rank != 3 || tr.Hits[2].Score != 0.86 || tr.Empty {
		t.Fatalf("итог: %+v", tr.Hits)
	}
	for _, c := range tr.Candidates {
		if c.FilterCut() != (c.ID == "c" || c.ID == "d" || c.ID == "e") {
			t.Fatalf("FilterCut %s", c.ID)
		}
	}
	// Повтор — и по тексту (тот же text_sha в другом документе).
	dup := cand("h", "w", 0, 10, 0.89, 8)
	dup.SHA = "sha-a"
	tr = trace(Config{Filter: true}, cand("a", "x", 0, 100, 0.90, 1), dup)
	finish(&tr)
	if got := reasons(tr); got != "a:ok h:повтор текста" {
		t.Fatalf("тот же текст: %s", got)
	}

	// Идемпотентность: второй прогон с другим K1 пересчитывает всё.
	tr = trace(Config{Filter: true, K1: 1}, cand("a", "x", 0, 100, 0.90, 1), cand("f", "z", 0, 90, 0.89, 2))
	finish(&tr)
	tr.Config.K1 = 2
	finish(&tr)
	if got := reasons(tr); got != "a:ok f:ok" || len(tr.Hits) != 2 {
		t.Fatalf("повторный фильтр: %s", got)
	}

	// Без фильтра — только K1.
	tr = trace(Config{K1: 2}, cand("a", "x", 0, 100, 0.5, 1), cand("b", "x", 0, 100, 0.4, 2), cand("c", "y", 0, 9, 0.3, 3))
	finish(&tr)
	if got := reasons(tr); got != "a:ok b:ok c:за пределами K1" {
		t.Fatalf("без фильтра: %s", got)
	}
}

// TestFinishAnchorAndEmpty — пол режет всё у запроса без названного вида
// (пустой итог), а у запроса с якорем не применяется.
func TestFinishAnchorAndEmpty(t *testing.T) {
	low := func() Trace {
		return trace(Config{Filter: true}, cand("a", "x", 0, 9, 0.78, 1), cand("b", "y", 0, 9, 0.77, 2), cand("c", "z", 0, 9, 0.70, 3))
	}
	tr := low()
	finish(&tr)
	if !tr.Empty || len(tr.Hits) != 0 || reasons(tr) != "a:порог 0.800 b:порог 0.800 c:порог 0.800" {
		t.Fatalf("без якоря: %s", reasons(tr))
	}
	tr = low()
	tr.Anchored = []string{"манул"}
	finish(&tr)
	if tr.Empty || reasons(tr) != "a:ok b:ok c:хуже лучшего на 0.05" {
		t.Fatalf("с якорем: %s", reasons(tr))
	}
	// Пустой список кандидатов — пустой итог.
	tr = trace(Config{Filter: true})
	finish(&tr)
	if !tr.Empty {
		t.Fatal("нет кандидатов — не пусто")
	}
}

func TestFinishBM25(t *testing.T) {
	tr := trace(Config{Filter: true},
		Candidate{Hit: kb.Hit{Chunk: kb.Chunk{ID: "a", DocID: "x", End: 9, SHA: "1"}}, BM25: 1},
		Candidate{Hit: kb.Hit{Chunk: kb.Chunk{ID: "b", DocID: "y", End: 9, SHA: "2"}}, BM25: 0.6},
		Candidate{Hit: kb.Hit{Chunk: kb.Chunk{ID: "c", DocID: "z", End: 9, SHA: "3"}}, BM25: 0.4})
	tr.Info.Mode = kb.BM25
	finish(&tr)
	if got := reasons(tr); got != "a:ok b:ok c:BM25 ниже 50 % лучшего" || tr.Hits[1].Score != 0.6 {
		t.Fatalf("BM25: %s", got)
	}
}

// TestHybridExemption — BM25-находка в первой половине K1 при гибридном
// реранкинге освобождена от относительного порога, ниже — нет; пол —
// для всех.
func TestHybridExemption(t *testing.T) {
	mk := func(final int) Trace {
		cs := []Candidate{cand("a", "x", 0, 9, 0.90, 1)}
		for i := 2; i < final; i++ {
			cs = append(cs, cand(fmt.Sprintf("f%d", i), fmt.Sprintf("d%d", i), 0, 9, 0.89, i))
		}
		cs = append(cs, cand("bm", "y", 0, 9, 0.82, 0))
		return trace(Config{Filter: true, Rerank: RerankHybrid, K1: 5}, cs...)
	}
	tr := mk(2)
	finish(&tr)
	if !strings.Contains(reasons(tr), "bm:ok") {
		t.Fatalf("ранг 2: %s", reasons(tr))
	}
	tr = mk(4)
	finish(&tr)
	if !strings.Contains(reasons(tr), "bm:хуже лучшего") {
		t.Fatalf("ранг 4: %s", reasons(tr))
	}
	tr = mk(2)
	tr.Candidates[1].Dense = 0.79
	finish(&tr)
	if !strings.Contains(reasons(tr), "bm:порог") {
		t.Fatalf("пол для BM25-находки: %s", reasons(tr))
	}
	// Без гибрида освобождения нет.
	tr = mk(2)
	tr.Config.Rerank = RerankNone
	finish(&tr)
	if !strings.Contains(reasons(tr), "bm:хуже лучшего") {
		t.Fatalf("без гибрида: %s", reasons(tr))
	}
}

// TestOrderRRF — гибридный порядок по сумме 1/(60 + ранг) всех списков:
// кандидат, найденный обоими поисками, выше найденного одним.
func TestOrderRRF(t *testing.T) {
	ctx := context.Background()
	p := miniPipeline(t)
	tr, err := p.Search(ctx, Query{Text: "корсак степная лисица весит"}, Config{Rerank: RerankHybrid})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range tr.Candidates {
		want := 0.0
		if c.RankDense > 0 {
			want += 1 / float64(RRFK+c.RankDense)
		}
		if c.RankBM25 > 0 {
			want += 1 / float64(RRFK+c.RankBM25)
		}
		if diff := c.Rerank - want; diff > 1e-12 || diff < -1e-12 {
			t.Fatalf("%s: RRF %v, ждали %v", c.ID, c.Rerank, want)
		}
		if i > 0 && c.Rerank > tr.Candidates[i-1].Rerank {
			t.Fatalf("порядок RRF нарушен на %d", i)
		}
		if c.Final != i+1 {
			t.Fatalf("Final %d на месте %d", c.Final, i+1)
		}
	}
	// Синтетика: оба списка против одного.
	x := Trace{Config: resolve(Config{Rerank: RerankHybrid}), Info: kb.SearchInfo{Mode: kb.Dense}, Candidates: []Candidate{
		{Hit: kb.Hit{Chunk: kb.Chunk{ID: "one"}}, Dense: 0.9, RankDense: 1, Rerank: 1.0 / 61},
		{Hit: kb.Hit{Chunk: kb.Chunk{ID: "both"}}, Dense: 0.8, RankDense: 3, RankBM25: 2, Rerank: 1.0/63 + 1.0/62},
	}}
	order(&x)
	if x.Candidates[0].ID != "both" || x.Candidates[0].Final != 1 {
		t.Fatalf("RRF: %+v", x.Candidates)
	}
	x.Config.Rerank = RerankNone
	order(&x)
	if x.Candidates[0].ID != "one" || x.Candidates[0].Rerank != 0 {
		t.Fatalf("без реранкинга — по косинусу: %+v", x.Candidates)
	}
}

func TestRewriteLLM(t *testing.T) {
	ctx := context.Background()
	p := miniPipeline(t)
	var reply string
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if reply == "сбой" {
			return llm.Response{}, errors.New("сеть")
		}
		return llmtest.Text(reply), nil
	}}
	p.LLM, p.Model = fake, llm.DefaultModel

	reply = "```json\n{\"query\": \"сколько часов в день ест кошачий медведь\", \"queries\": [\"малая панда питание\", \"малая панда питание\", \"a\", \"b\", \"c\"]}\n```"
	tr, err := p.Search(ctx, Query{Text: "А сколько она ест?", Context: []string{"Расскажи про кошачьего медведя"}}, Config{Rewrite: RewriteLLM})
	if err != nil {
		t.Fatal(err)
	}
	if tr.RewriteBy != "llm" || len(tr.Queries) != 4 || !strings.HasSuffix(tr.Queries[0], "кошачий медведь малая панда") ||
		len(tr.QueriesBM25) != 4 || tr.QueriesBM25[0] != tr.Queries[0]+" Ailurus fulgens" || tr.QueriesBM25[3] != "b" ||
		tr.Rewritten != tr.Queries[0] || len(tr.Expanded) != 1 || tr.Usage.Total == 0 || !tr.Cost.Known || len(tr.Anchored) != 0 {
		t.Fatalf("llm: %q %q %v", tr.Queries, tr.QueriesBM25, tr.Anchored)
	}
	user := fake.Requests[0].Messages[1].Content
	if !strings.Contains(user, "Предыдущие реплики пользователя:\n- Расскажи про кошачьего медведя") || !strings.HasSuffix(user, "Вопрос: А сколько она ест?") ||
		fake.Requests[0].Messages[0].Content != rewriteSystem || fake.Requests[0].Temperature != 0 {
		t.Fatalf("запрос модели: %q", user)
	}

	reply = "не знаю, что сказать"
	tr, err = p.Search(ctx, Query{Text: "Что ест кошачий медведь?"}, Config{Rewrite: RewriteLLM})
	if err != nil || tr.RewriteBy != "code" || !strings.Contains(tr.Note, "неразборчиво") || !strings.Contains(tr.Rewritten, "малая панда") {
		t.Fatalf("откат на код: %+v %v", tr, err)
	}
	reply = `{"query": "  "}`
	if tr, _ = p.Search(ctx, Query{Text: "Что ест кошачий медведь?"}, Config{Rewrite: RewriteLLM}); tr.RewriteBy != "code" {
		t.Fatalf("пустой query: %+v", tr)
	}
	reply = "сбой"
	if _, err := p.Search(ctx, Query{Text: "манул"}, Config{Rewrite: RewriteLLM}); err == nil {
		t.Fatal("ошибка модели проглочена")
	}
}

var candID = regexp.MustCompile(`(?m)^\[([^\]]+)\] `)

func TestRerankLLM(t *testing.T) {
	ctx := context.Background()
	p := miniPipeline(t)
	var mode string
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		ids := candID.FindAllStringSubmatch(req.Messages[1].Content, -1)
		switch mode {
		case "bad":
			return llmtest.Text("всё хорошо"), nil
		case "list":
			return llmtest.Text(fmt.Sprintf(`{"scores": [{"id": %q, "score": 3}]}`, ids[len(ids)-1][1])), nil
		}
		var parts []string
		for i, m := range ids {
			v := 0
			if i == len(ids)-1 {
				v = 3 // последнему кандидату — высшая оценка
			} else if i == 0 {
				v = 1
			}
			parts = append(parts, fmt.Sprintf("%q: %d", m[1], v))
		}
		return llmtest.Text("{\"scores\": {" + strings.Join(parts, ", ") + "}}"), nil
	}}
	p.LLM = fake
	q := Query{Text: "Сколько весит корсак?"}
	tr, err := p.Search(ctx, q, Config{Rerank: RerankLLM, Filter: true, Delta: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	last := candID.FindAllStringSubmatch(fake.Requests[0].Messages[1].Content, -1)
	lastID := last[len(last)-1][1]
	if tr.Candidates[0].ID != lastID || tr.Candidates[0].Rerank != 3 || !tr.Candidates[0].Kept {
		t.Fatalf("оценка 3 — первым и в выдаче: %+v", tr.Candidates[0])
	}
	byLLM := 0
	for _, c := range tr.Candidates {
		if c.Rerank == 0 && c.Kept {
			t.Fatalf("оценка 0 в выдаче: %+v", c)
		}
		if c.Reason == ReasonLLM {
			byLLM++
		}
	}
	if byLLM == 0 {
		t.Fatalf("нет отсечённых по оценке модели: %s", reasons(tr))
	}
	if tr.Usage.Total == 0 || !strings.Contains(fake.Requests[0].Messages[1].Content, "Вопрос: Сколько весит корсак?") {
		t.Fatal("учёт запроса")
	}

	// Список вместо словаря; пропущенные — 0 с заметкой.
	mode = "list"
	tr, err = p.Search(ctx, q, Config{Rerank: RerankLLM})
	if err != nil || tr.Candidates[0].Rerank != 3 || !strings.Contains(tr.Note, "не оценил") {
		t.Fatalf("список: %+v %v", tr.Note, err)
	}
	// Неразборчиво — порядок RRF, без отсева по оценке.
	mode = "bad"
	tr, err = p.Search(ctx, q, Config{Rerank: RerankLLM, Filter: true})
	if err != nil || !strings.Contains(tr.Note, noteRerankFailed) {
		t.Fatalf("неразборчиво: %q %v", tr.Note, err)
	}
	for i, c := range tr.Candidates {
		if c.Reason == ReasonLLM || (i > 0 && c.Rerank > tr.Candidates[i-1].Rerank) {
			t.Fatalf("откат на RRF: %+v", c)
		}
	}
}

func TestPresetsAndDescribe(t *testing.T) {
	var names []string
	for _, n := range Presets(true) {
		names = append(names, n.Name+"="+n.Config.Describe())
	}
	want := "base=dense top-K1, без фильтра и переписывания; filter=filter; rewrite=rewrite code; both=rewrite code, filter, scope; " +
		"hybrid=rewrite code, rerank hybrid, filter; llm-rewrite=rewrite llm, filter; llm-rerank=rewrite code, rerank llm, filter"
	if got := strings.Join(names, "; "); got != want {
		t.Fatalf("пресеты:\n%s", got)
	}
	if len(Presets(false)) != 5 {
		t.Fatal("бесплатных — пять")
	}
}

// TestFinishScope — рамка якоря (v24): при названном виде фрагменты статей
// других видов отсекаются с причиной «другой вид», обзорные статьи и MDD
// (документы без вида) остаются; относительный порог меряется от лучшего
// фрагмента внутри рамки — лучший фрагмент чужого вида его не задаёт.
func TestFinishScope(t *testing.T) {
	mk := func(scope bool) Trace {
		tr := trace(Config{Filter: true, Scope: scope},
			cand("bear", "brown-bear", 0, 9, 0.90, 1), // чужой вид, лучший косинус
			cand("rp1", "red-panda", 0, 9, 0.84, 2),   // хуже лучшего на 0.06, но лучший в рамке
			cand("rp2", "red-panda", 10, 19, 0.83, 3),
			cand("ov", "carnivora", 0, 9, 0.82, 4), // обзор без вида
		)
		tr.Anchored = []string{"малая панда"}
		tr.cos = []map[string]float64{{"bear": 0.90, "rp1": 0.84, "rp2": 0.83, "ov": 0.82}}
		leads(&tr, tr.cos)
		return tr
	}
	al := AliasesOf(miniDocs())
	tr := mk(true)
	scopeOf(al, &tr)
	finish(&tr)
	if got := reasons(tr); got != "bear:другой вид rp1:ok rp2:ok ov:ok" || strings.Join(tr.Scope, ",") != "red-panda" {
		t.Fatalf("рамка: %s %v", got, tr.Scope)
	}
	if !tr.Candidates[0].FilterCut() {
		t.Fatal("«другой вид» — отсев фильтром")
	}
	// Без Scope — как в v23: чужой вид остаётся, свой отсечён относительным
	// порогом от чужого лучшего.
	tr = mk(false)
	scopeOf(al, &tr)
	finish(&tr)
	if got := reasons(tr); got != "bear:ok rp1:хуже лучшего на 0.05 rp2:хуже лучшего на 0.05 ov:хуже лучшего на 0.05" {
		t.Fatalf("без рамки: %s", got)
	}
	// Без якоря рамки нет.
	tr = mk(true)
	tr.Anchored = nil
	scopeOf(al, &tr)
	if len(tr.Scope) != 0 {
		t.Fatalf("без якоря: %v", tr.Scope)
	}
	// Словарь без документов — рамки нет.
	tr = mk(true)
	scopeOf(&Aliases{Canon: al.Canon}, &tr)
	if len(tr.Scope) != 0 {
		t.Fatal("словарь без Docs")
	}
}

// TestSearchScope — рамка на настоящем поиске мини-корпуса: «кошачий
// медведь» — только малая панда и обзор, медведь отсечён как другой вид.
func TestSearchScope(t *testing.T) {
	p := miniPipeline(t)
	tr, err := p.Search(context.Background(), Query{Text: "Сколько весит кошачий медведь?"},
		Config{Rewrite: RewriteCode, Filter: true, Scope: true, Delta: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tr.Scope, ",") != "red-panda" {
		t.Fatalf("рамка: %v", tr.Scope)
	}
	for _, h := range tr.Hits {
		if h.DocID == "brown-bear" || h.DocID == "corsac" {
			t.Fatalf("чужой вид в итоге: %s", h.ID)
		}
	}
	cut := 0
	for _, c := range tr.Candidates {
		if c.Reason == ReasonScope {
			cut++
			if c.DocID == "red-panda" || c.DocID == "carnivora" {
				t.Fatalf("отсечён свой или обзор: %s", c.ID)
			}
		}
	}
	if cut == 0 || len(tr.Hits) == 0 {
		t.Fatalf("рамка не сработала: %s", reasons(tr))
	}
}

// TestFinishScopePerSpecies — у каждого названного вида свой ориентир
// относительного порога: «харза или соболь» — статья о соболе дальше от
// запроса, и порог от лучшего фрагмента харзы отсекал бы её целиком.
func TestFinishScopePerSpecies(t *testing.T) {
	docs := []corpus.Doc{
		{ID: "kharza", Title: "Харза", Species: &corpus.Species{Ru: "харза"}},
		{ID: "sable", Title: "Соболь", Species: &corpus.Species{Ru: "соболь"}},
		{ID: "marten", Title: "Лесная куница", Species: &corpus.Species{Ru: "лесная куница"}},
	}
	tr := trace(Config{Filter: true, Scope: true},
		cand("kh1", "kharza", 0, 9, 0.84, 1),
		cand("other", "marten", 0, 9, 0.85, 2), // чужой вид
		cand("kh2", "kharza", 10, 19, 0.83, 3),
		cand("ov", "mustelidae", 0, 9, 0.80, 4), // обзор: от лучшего в рамке (0.84)
		cand("sb1", "sable", 0, 9, 0.78, 5),     // хуже харзы на 0.06, но лучший у соболя
		cand("sb2", "sable", 10, 19, 0.77, 6),
		cand("sb3", "sable", 20, 29, 0.70, 7), // хуже лучшего соболя на 0.08
	)
	tr.Anchored = []string{"харза", "соболь"}
	tr.cos = []map[string]float64{{"kh1": 0.84, "other": 0.85, "kh2": 0.83, "ov": 0.80, "sb1": 0.78, "sb2": 0.77, "sb3": 0.70}}
	leads(&tr, tr.cos)
	scopeOf(AliasesOf(docs), &tr)
	tr.Config.K1 = 10
	finish(&tr)
	want := "kh1:ok other:другой вид kh2:ok ov:ok sb1:ok sb2:ok sb3:хуже лучшего на 0.05"
	if got := reasons(tr); got != want {
		t.Fatalf("причины:\n%s\nждали\n%s", got, want)
	}
	// Мало места (K1 = 3): лучший фрагмент соболя всё равно в выдаче — за
	// счёт последнего места харзы и обзора.
	tr.Config.K1 = 3
	finish(&tr)
	want = "kh1:ok other:другой вид kh2:ok ov:за пределами K1 sb1:ok sb2:за пределами K1 sb3:хуже лучшего на 0.05"
	if got := reasons(tr); got != want || len(tr.Hits) != 3 || tr.Hits[2].ID != "sb1" || tr.Hits[2].Rank != 3 {
		t.Fatalf("K1 = 3:\n%s\nждали\n%s", got, want)
	}
	// Один вид — без оговорённых мест: как раньше.
	tr.Scope, tr.Anchored = []string{"kharza"}, []string{"харза"}
	finish(&tr)
	if got := reasons(tr); !strings.HasPrefix(got, "kh1:ok other:другой вид kh2:ok ov:ok sb1:другой вид") {
		t.Fatalf("один вид: %s", got)
	}
}

// TestSearchScopeAmbiguous — неоднозначное название в реплике («медведь»:
// словарь его не раскрывает) снимает рамку: иначе рамка по корсаку отсекла
// бы статью о медведе, о котором тоже спросили.
func TestSearchScopeAmbiguous(t *testing.T) {
	p := miniPipeline(t)
	tr, err := p.Search(context.Background(), Query{Text: "Сколько весит корсак и чем питается медведь?"},
		Config{Filter: true, Scope: true, Delta: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tr.Anchored, ",") != "корсак" || len(tr.Scope) != 0 || !strings.Contains(tr.Note, "рамка якоря не применена: в реплике неоднозначное название «медведь»") {
		t.Fatalf("рамка: якорь %v, рамка %v, заметка %q", tr.Anchored, tr.Scope, tr.Note)
	}
	for _, c := range tr.Candidates {
		if c.Reason == ReasonScope {
			t.Fatalf("отсечено рамкой: %s", c.ID)
		}
	}
	// Без неоднозначного названия рамка на месте.
	tr, err = p.Search(context.Background(), Query{Text: "Сколько весит корсак?"}, Config{Filter: true, Scope: true, Delta: 1})
	if err != nil || strings.Join(tr.Scope, ",") != "corsac" {
		t.Fatalf("рамка корсака: %v %v", tr.Scope, err)
	}
	if al, err := p.Names(context.Background()); err != nil || al == nil || len(al.Docs) == 0 {
		t.Fatalf("словарь конвейера: %v", err)
	}
}
