//go:build edge

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed/embedtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kbapi"
)

// edgeKB — REST окна «База знаний» за /api/kb/: настоящий kbapi над
// временной kb.db из настоящего корпуса (обе стратегии, векторы
// embed.Hash, сохранённый отчёт сравнения) плюс документ с разметкой в
// заголовке, тексте и разделе — она не должна стать элементами. Эмбеддер
// для статуса — подставной сайдкар (embedtest), отвечающий той же моделью
// hash-256. Сценарии kb-none видят приложение без базы (503) и с
// неотвечающим эмбеддером — по адресу страницы (Referer), как facts-down.
type edgeKB struct {
	ok, none http.Handler
	// nofiles — база есть, а матрицы режимов и калибровки ещё нет (v23):
	// сценарии с «kb-nofiles» в адресе.
	nofiles http.Handler
	// rag — подставной отвечающий агент (v22): ask и evals без модели.
	rag *edgeRAG
}

// edgeKBXSS — документ с разметкой; его URL — javascript:, ссылки быть не
// должно.
func edgeKBXSS() corpus.Doc {
	return corpus.Doc{Schema: corpus.Schema, ID: "xss-test", Source: corpus.SourceWikipedia,
		Title:   `<b>Ксенофоб</b> <img src=x onerror="window.__xss=41">`,
		URL:     "javascript:window.__xss=40",
		License: "CC BY-SA 4.0",
		Intro:   `Ксенофоб — проверочный зверёк. <script>window.__xss=42</script> Питается разметкой <b>жирно</b>.`,
		Sections: []corpus.Section{{Path: []string{`<i>Питание</i>`}, Title: `<i>Питание</i>`, Level: 2,
			Text: `Ксенофоб ест теги <img src=x onerror="window.__xss=43"> и запивает их амперсандами &amp;.`}},
	}
}

func newEdgeKB(t *testing.T) *edgeKB {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "kb.db")
	st, err := kb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	docs, m, err := corpus.Load("corpus")
	if err != nil {
		t.Fatal(err)
	}
	x := edgeKBXSS()
	docs = append(docs, x)
	m.Entries = append(m.Entries, corpus.Entry{ID: x.ID, File: x.ID + ".json", Title: x.Title, Source: x.Source, Chars: x.Chars(), SHA256: "sha-xss"})
	m.Chars += x.Chars()
	m.Pages = float64(m.Chars) / corpus.PageChars
	if err := st.PutCorpus(ctx, docs, m); err != nil {
		t.Fatal(err)
	}
	// Как kb index -strategy all: fixed — размером в медиану структурных
	// чанков, перекрытие 15 %.
	if _, err := st.Build(ctx, kb.NewStructure(0, 0), embed.Hash{}, nil); err != nil {
		t.Fatal(err)
	}
	chunks, err := st.Chunks(ctx, string(kb.Structure), "")
	if err != nil {
		t.Fatal(err)
	}
	n := kb.MedianChars(chunks)
	if _, err := st.Build(ctx, kb.NewFixed(n, n*kb.DefaultOverlapPct/100), embed.Hash{}, nil); err != nil {
		t.Fatal(err)
	}
	s := &kb.Searcher{Store: st, Embedder: embed.Hash{}}
	qs, err := kb.LoadQuestions(filepath.Join("eval", "questions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kb.Compare(ctx, s, qs, kb.CompareOptions{Mode: kb.BM25}); err != nil {
		t.Fatal(err)
	}

	fake := embedtest.Server(t)
	fake.Model = embed.Hash{}.Model()
	fake.SetHash(embed.Hash{})
	questions := filepath.Join("eval", "questions.json")
	ok := &kbapi.API{Searcher: s, Embedder: &embed.HTTP{BaseURL: fake.URL, Name: fake.Model}, Path: path, Questions: questions}
	r := newEdgeRAG(s, qs)
	r.wire(ok)
	// v23: подставной конвейер, матрица режимов и калибровка — файлы, как их
	// пишут kb matrix и kb calibrate.
	ok.MatrixPath = edgeWriteJSON(t, dir, "filter.json", edgeMatrix())
	ok.CalibrationPath = edgeWriteJSON(t, dir, "calibrate.json", edgeCalibration())
	nofiles := &kbapi.API{Searcher: s, Embedder: ok.Embedder, Path: path, Questions: questions,
		MatrixPath: filepath.Join(dir, "нет", "filter.json"), CalibrationPath: filepath.Join(dir, "нет", "calibrate.json")}
	r.wire(nofiles)

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	nonePath := filepath.Join(dir, "нет", "kb.db")
	none := &kbapi.API{Embedder: &embed.HTTP{BaseURL: deadURL, Name: embed.DefaultModel}, Path: nonePath, Why: "базы знаний нет: " + nonePath,
		Questions: questions}

	return &edgeKB{ok: edgeKBMux(ok), none: edgeKBMux(none), nofiles: edgeKBMux(nofiles), rag: r}
}

func edgeKBMux(a *kbapi.API) http.Handler {
	mux := http.NewServeMux()
	for _, x := range a.Extension() {
		mux.Handle(x.Prefix, x.Handler)
	}
	return mux
}

func (e *edgeKB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Referer(), "kb-none") {
		e.none.ServeHTTP(w, r)
		return
	}
	if strings.Contains(r.Referer(), "kb-nofiles") {
		e.nofiles.ServeHTTP(w, r)
		return
	}
	// Опрос прогона пропускает следующий вопрос — после ответа: окно
	// увидит новую строку следующим опросом.
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, kbapi.Prefix+"evals/") {
		defer e.rag.poll()
	}
	e.ok.ServeHTTP(w, r)
}
