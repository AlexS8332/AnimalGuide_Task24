package retrieve

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
)

// topicEmbedder — эмбеддер по темам: признак темы есть, если слово текста
// начинается с одной из её основ (наличие, а не счёт), плюс малая общая
// составляющая. Косинусы осмысленны, как у настоящей модели: «вес харзы» к
// разделу о массе харзы ближе, чем «вес жирафа».
type topicEmbedder struct{}

var topics = [][]string{
	{"харз"}, {"манул"}, {"жираф"}, {"вес", "масс", "кг"}, {"водит", "обита", "росси", "распростран"},
}

func (topicEmbedder) Model() string { return "topic-test" }
func (topicEmbedder) Dims() int     { return len(topics) + 1 }
func (topicEmbedder) Embed(ctx context.Context, kind embed.Kind, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, len(topics)+1)
		v[len(topics)] = 0.1
		for _, w := range strings.Fields(strings.ToLower(text)) {
			for j, stems := range topics {
				for _, s := range stems {
					if strings.HasPrefix(strings.Trim(w, ".,?!—"), s) {
						v[j] = 1
					}
				}
			}
		}
		var n float64
		for _, x := range v {
			n += float64(x * x)
		}
		for j := range v {
			v[j] = float32(float64(v[j]) / math.Sqrt(n))
		}
		out[i] = v
	}
	return out, nil
}

// topicPipeline — харза и манул, разделы «Распространение» и «Размеры»
// отдельными фрагментами.
func topicPipeline(t *testing.T) *Pipeline {
	t.Helper()
	ctx := context.Background()
	docs := []corpus.Doc{
		{ID: "harza", Source: corpus.SourceWikipedia, Title: "Харза", License: "CC BY-SA 4.0",
			Species: &corpus.Species{Latin: "Martes flavigula", Ru: "харза"},
			Intro:   "Харза — крупная куница с яркой окраской.",
			Sections: []corpus.Section{
				{Path: []string{"Распространение"}, Title: "Распространение", Level: 2, Text: "Харза водится в России в Приморье."},
				{Path: []string{"Размеры"}, Title: "Размеры", Level: 2, Text: "Харза весит до шести кг."},
			}},
		{ID: "manul", Source: corpus.SourceWikipedia, Title: "Манул", License: "CC BY-SA 4.0",
			Species: &corpus.Species{Latin: "Otocolobus manul", Ru: "манул"},
			Intro:   "Манул — дикая кошка Центральной Азии.",
			Sections: []corpus.Section{
				{Path: []string{"Размеры"}, Title: "Размеры", Level: 2, Text: "Манул весит до пяти кг."},
			}},
	}
	st, err := kb.Open(ctx, filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.PutCorpus(ctx, docs, miniManifest(docs)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Build(ctx, kb.NewStructure(400, 10), topicEmbedder{}, nil); err != nil {
		t.Fatal(err)
	}
	return &Pipeline{Searcher: &kb.Searcher{Store: st, Embedder: topicEmbedder{}}}
}

// TestContextAndAnchor — контекст только для переписывания и только у
// продолжения; якорь — вид, названный в самой реплике.
func TestContextAndAnchor(t *testing.T) {
	ctx := context.Background()
	p := topicPipeline(t)
	filter := Config{Filter: true}
	both := Config{Rewrite: RewriteCode, Filter: true}

	// Новый вопрос о животном вне базы после вопроса о харзе: контекст не
	// склеивается и не подставляется, якоря нет — пол отсекает всё.
	giraffe := Query{Text: "Сколько весит взрослый жираф?", Context: []string{"Где водится харза?"}}
	for name, c := range map[string]Config{"filter": filter, "both": both} {
		tr, err := p.Search(ctx, giraffe, c)
		if err != nil {
			t.Fatal(err)
		}
		if !tr.Empty || len(tr.Anchored) != 0 || strings.Contains(tr.Queries[0], "харз") {
			t.Fatalf("%s, жираф: пусто %v, якорь %v, запрос %q, лучший %.3f", name, tr.Empty, tr.Anchored, tr.Queries[0], tr.TopDense)
		}
	}

	// Продолжение: прошлая реплика с видом и текущая — находит вес харзы;
	// вид унаследован, якоря нет, пол действует (и пройден).
	tr, err := p.Search(ctx, Query{Text: "А сколько она весит?", Context: []string{"Где водится харза?"}}, both)
	if err != nil {
		t.Fatal(err)
	}
	weight := false
	for _, h := range tr.Hits {
		weight = weight || (h.DocID == "harza" && h.Section == "Размеры")
	}
	if tr.Empty || !weight || len(tr.Anchored) != 0 || tr.Rewritten != "Где водится харза? А сколько она весит?" {
		t.Fatalf("продолжение: пусто %v, вес %v, якорь %v, запрос %q, хиты %+v", tr.Empty, weight, tr.Anchored, tr.Rewritten, tr.Hits)
	}
	if tr.Gap < 0 || tr.TopDense < tr.MinScore {
		t.Fatalf("продолжение: лучший %.3f, отрыв %.3f, порог %.3f", tr.TopDense, tr.Gap, tr.MinScore)
	}

	// Вид назван в реплике — контекст о мануле в запрос не идёт.
	tr, err = p.Search(ctx, Query{Text: "Где в России водится харза?", Context: []string{"Сколько весит манул?"}}, both)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tr.Rewritten, "манул") || strings.Contains(strings.Join(tr.QueriesBM25, " "), "манул") ||
		strings.Join(tr.Anchored, ",") != "харза" || tr.Empty {
		t.Fatalf("самостоятельная: %q %q %v", tr.Rewritten, tr.QueriesBM25, tr.Anchored)
	}
	// Латынь — только в BM25-запросе.
	if tr.Rewritten != tr.Original || len(tr.QueriesBM25) != 1 || !strings.HasSuffix(tr.QueriesBM25[0], "Martes flavigula") {
		t.Fatalf("латынь: %q %q", tr.Queries, tr.QueriesBM25)
	}
}

// TestRelativePerQuery — относительный порог — от лучшего косинуса своего
// подзапроса: находка второго подзапроса не отсекается лучшей находкой
// первого.
func TestRelativePerQuery(t *testing.T) {
	x := Trace{Config: resolve(Config{Filter: true}), Info: kb.SearchInfo{Mode: kb.Dense}, MinScore: 0.5, Anchored: []string{"а"},
		Queries: []string{"q1", "q2"}, Candidates: []Candidate{
			{Hit: kb.Hit{Chunk: kb.Chunk{ID: "a", DocID: "a"}}, Dense: 0.90, RankDense: 1},
			{Hit: kb.Hit{Chunk: kb.Chunk{ID: "b", DocID: "b"}}, Dense: 0.80, RankDense: 1},
			{Hit: kb.Hit{Chunk: kb.Chunk{ID: "c", DocID: "c"}}, Dense: 0.70, RankDense: 2},
		}}
	leads(&x, []map[string]float64{{"a": 0.90, "c": 0.70}, {"b": 0.80, "c": 0.70}})
	order(&x)
	finish(&x)
	kept := map[string]bool{}
	for _, h := range x.Hits {
		kept[h.ID] = true
	}
	if !kept["a"] || !kept["b"] || kept["c"] {
		t.Fatalf("относительный по подзапросам: %+v", x.Candidates)
	}
	if math.Abs(x.Gap-0.1) > 1e-9 || x.TopDense != 0.9 {
		t.Fatalf("отрыв: %v %v", x.Gap, x.TopDense)
	}
	// Без lead — как раньше: от общего лучшего.
	y := Trace{Config: x.Config, Info: x.Info, MinScore: 0.5, Anchored: []string{"а"}, Candidates: []Candidate{
		{Hit: kb.Hit{Chunk: kb.Chunk{ID: "a", DocID: "a"}}, Dense: 0.90},
		{Hit: kb.Hit{Chunk: kb.Chunk{ID: "b", DocID: "b"}}, Dense: 0.80},
	}}
	order(&y)
	finish(&y)
	if len(y.Hits) != 1 {
		t.Fatalf("общий лучший: %+v", y.Candidates)
	}
}

// TestCalibrateGap — порог — середина зазора между лучшим косинусом вопроса
// вне базы и косинусом доказательства неякорного dev; якорные вопросы в
// зазор не входят; без зазора — прежнее правило с предупреждением.
func TestCalibrateGap(t *testing.T) {
	ctx := context.Background()
	p := topicPipeline(t)
	dev := kb.Question{ID: "D1", Split: kb.SplitDev, Type: "fact", Q: "Где в Приморье водится эта куница?", Answerable: true,
		Evidence: []kb.Evidence{{DocID: "harza", Quote: "Харза водится в России в Приморье."}}}
	anchored := kb.Question{ID: "D2", Split: kb.SplitDev, Type: "number", Q: "Сколько весит манул?", Answerable: true,
		Evidence: []kb.Evidence{{DocID: "manul", Quote: "Манул весит до пяти кг."}}}
	out := kb.Question{ID: "O1", Split: kb.SplitOut, Type: "out-of-base", Q: "Сколько весит жираф?"}
	test := kb.Question{ID: "T1", Split: kb.SplitTest, Type: "fact", Q: "Где обитает эта куница?", Answerable: true}
	cal, err := Calibrate(ctx, p, kb.QuestionSet{Questions: []kb.Question{dev, anchored, out, test}}, "", 0, 0.05, false)
	if err != nil {
		t.Fatal(err)
	}
	if cal.Rule != CalibGap || cal.OutMaxID != "O1" || cal.EvidenceMinID != "D1" || cal.Gap <= 0 ||
		math.Abs(cal.Chosen-(cal.OutMax+cal.EvidenceMin)/2) > 6e-4 || math.Abs(cal.MarginOut-cal.MarginDev) > 1.1e-3 || cal.MarginOut <= 0 {
		t.Fatalf("зазор: %+v", cal)
	}
	if strings.Join(cal.FloorDev, ",") != "D1" || strings.Join(cal.FloorOut, ",") != "O1" || strings.Join(cal.FloorTest, ",") != "T1" ||
		strings.Join(cal.Anchored, ",") != "D2" || cal.At.DevRecall != 1 || cal.At.OutEmpty != 1 || cal.Note != "" {
		t.Fatalf("чувствительные к полу: %+v", cal)
	}
	md := cal.Markdown()
	for _, want := range []string{"середина зазора", "## Зазор", "dev 1 из 2 (D1)", "out 1 из 1 (O1)", "test 1 (T1)", "запас до вопросов вне базы +"} {
		if !strings.Contains(md, want) {
			t.Errorf("в отчёте нет %q:\n%s", want, md)
		}
	}

	// Вопрос вне базы ближе доказательства — зазора нет: max-drop и
	// предупреждение.
	near := kb.Question{ID: "O2", Split: kb.SplitOut, Type: "out-of-base", Q: "Где это водится в России?"}
	cal, err = Calibrate(ctx, p, kb.QuestionSet{Questions: []kb.Question{dev, near}}, "", 0, 0.05, false)
	if err != nil {
		t.Fatal(err)
	}
	if cal.Rule != CalibMaxDrop || cal.Gap > 0 || !strings.Contains(cal.Note, "зазора нет") || !strings.Contains(cal.Markdown(), "**Внимание:**") {
		t.Fatalf("без зазора: %+v", cal)
	}
}
