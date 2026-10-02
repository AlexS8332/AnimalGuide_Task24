package kbapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed/embedtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
)

// miniDocs — два документа: манул с разделами (structure режет по ним,
// fixed — поперёк) и корсак из одного вступления.
func miniDocs() []corpus.Doc {
	long := strings.Repeat("Манул подстерегает добычу у норы и долго сидит неподвижно. ", 12)
	return []corpus.Doc{
		{ID: "manul", Source: corpus.SourceWikipedia, Title: "Манул", URL: "https://ru.wikipedia.org/wiki/Манул", RevID: 7,
			License: "CC BY-SA 4.0",
			Intro:   "Манул, или палласов кот, — хищное млекопитающее семейства кошачьих.",
			Sections: []corpus.Section{
				{Path: []string{"Описание"}, Title: "Описание", Level: 2, Text: long},
				{Path: []string{"Образ жизни"}, Title: "Образ жизни", Level: 2},
				{Path: []string{"Образ жизни", "Питание"}, Title: "Питание", Level: 3,
					Text: "Кормится манул почти исключительно мелкими грызунами и пищухами. Иногда ловит птиц."},
				{Path: []string{"Охрана"}, Title: "Охрана", Level: 2, Text: "Занесён в Красную книгу России."},
			}},
		{ID: "corsac", Source: corpus.SourceWikipedia, Title: "Корсак", URL: "https://ru.wikipedia.org/wiki/Корсак",
			License: "CC BY-SA 4.0", Intro: "Корсак — степная лисица. Весит от 2,5 до 4 кг."},
	}
}

// newStore — kb.db во временном каталоге с мини-корпусом; indexes —
// собрать ли оба индекса (embed.Hash).
func newStore(t *testing.T, indexes bool) *kb.Store {
	t.Helper()
	ctx := context.Background()
	st, err := kb.Open(ctx, filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	docs := miniDocs()
	m := corpus.Manifest{Schema: corpus.Schema, CorpusSHA: "sha-mini"}
	for _, d := range docs {
		m.Entries = append(m.Entries, corpus.Entry{ID: d.ID, File: d.ID + ".json", Title: d.Title, Source: d.Source,
			RevID: d.RevID, Chars: d.Chars(), SHA256: "sha-" + d.ID})
		m.Chars += d.Chars()
	}
	m.Pages = float64(m.Chars) / corpus.PageChars
	if err := st.PutCorpus(ctx, docs, m); err != nil {
		t.Fatal(err)
	}
	if indexes {
		if _, err := st.Build(ctx, kb.NewStructure(300, 80), embed.Hash{}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Build(ctx, kb.NewFixed(120, 18), embed.Hash{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func newAPI(t *testing.T, st *kb.Store, emb *embed.HTTP) *httptest.Server {
	t.Helper()
	a := &API{Embedder: emb, Path: "kb.db"}
	if st != nil {
		a.Searcher = &kb.Searcher{Store: st, Embedder: embed.Hash{}}
	}
	mux := http.NewServeMux()
	for _, x := range a.Extension() {
		mux.Handle(x.Prefix, x.Handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// get — GET и разбор JSON; v nil — тело не разбирается.
func get(t *testing.T, srv *httptest.Server, path string, v any) int {
	t.Helper()
	return do(t, srv, http.MethodGet, path, v)
}

func do(t *testing.T, srv *httptest.Server, method, path string, v any) int {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("%s %s: Content-Type %q", method, path, ct)
	}
	if v != nil {
		if err := json.NewDecoder(res.Body).Decode(v); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	return res.StatusCode
}

// deadEmbedder — адрес, где никто не слушает.
func deadEmbedder(t *testing.T) *embed.HTTP {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return &embed.HTTP{BaseURL: u, Name: embed.DefaultModel}
}

func TestInfo(t *testing.T) {
	fake := embedtest.Server(t)
	srv := newAPI(t, newStore(t, true), &embed.HTTP{BaseURL: fake.URL, Name: embed.DefaultModel})
	var v InfoView
	if code := get(t, srv, "/api/kb/info", &v); code != http.StatusOK {
		t.Fatalf("код %d", code)
	}
	if !v.OK || v.Docs != 2 || v.Manifest.CorpusSHA != "sha-mini" || len(v.Manifest.Entries) != 2 || v.Pages <= 0 || v.Path != "kb.db" {
		t.Fatalf("info: %+v", v)
	}
	if len(v.Indexes) != 2 || v.Indexes[0].Chunks == 0 || v.Indexes[0].Embedder != "hash-256" {
		t.Fatalf("индексы: %+v", v.Indexes)
	}
	if !v.Embedder.OK || v.Embedder.Device != "test" || v.Embedder.Model != embed.DefaultModel {
		t.Fatalf("эмбеддер: %+v", v.Embedder)
	}
	if v.Report || v.Why != "" {
		t.Fatalf("отчёта не было: %+v", v)
	}

	// Эмбеддер не отвечает — info всё равно 200, причина и подсказка в статусе.
	srv = newAPI(t, newStore(t, false), deadEmbedder(t))
	v = InfoView{}
	if code := get(t, srv, "/api/kb/info", &v); code != http.StatusOK {
		t.Fatalf("код %d", code)
	}
	if v.Embedder.OK || v.Embedder.Why == "" || v.Embedder.Hint == "" {
		t.Fatalf("эмбеддер: %+v", v.Embedder)
	}
	if len(v.Indexes) != 0 || v.Indexes == nil || v.Hint != HintNoBase {
		t.Fatalf("база без индексов: %+v", v)
	}
}

func TestNoBase(t *testing.T) {
	srv := newAPI(t, nil, nil)
	var info struct {
		InfoView
		Error string `json:"error"`
	}
	if code := get(t, srv, "/api/kb/info", &info); code != http.StatusServiceUnavailable {
		t.Fatalf("info: код %d", code)
	}
	if info.OK || info.Why != "базы знаний нет: kb.db" || info.Hint != HintNoBase || info.Error == "" || info.Embedder.Why == "" {
		t.Fatalf("info: %+v", info)
	}
	for _, p := range []string{"docs", "docs/manul", "search?q=манул", "report"} {
		var e map[string]string
		if code := get(t, srv, "/api/kb/"+p, &e); code != http.StatusServiceUnavailable {
			t.Fatalf("%s: код %d", p, code)
		}
		if e["error"] == "" || e["why"] == "" || e["hint"] != HintNoBase {
			t.Fatalf("%s: %v", p, e)
		}
	}
}

func TestDocs(t *testing.T) {
	srv := newAPI(t, newStore(t, true), nil)
	var docs []kb.DocInfo
	if code := get(t, srv, "/api/kb/docs", &docs); code != http.StatusOK {
		t.Fatalf("код %d", code)
	}
	if len(docs) != 2 || docs[0].ID != "manul" || docs[0].RevID != 7 || docs[0].Chars != miniDocs()[0].Chars() || docs[1].License == "" {
		t.Fatalf("docs: %+v", docs)
	}
}

func TestDoc(t *testing.T) {
	srv := newAPI(t, newStore(t, true), nil)
	text := miniDocs()[0].Text()
	n := map[string]int{}
	for _, index := range []string{"structure", "fixed"} {
		var v DocView
		if code := get(t, srv, "/api/kb/docs/manul?index="+index, &v); code != http.StatusOK {
			t.Fatalf("%s: код %d", index, code)
		}
		if v.Doc.ID != "manul" || v.Doc.Title != "Манул" || v.Index != index || v.Text != text {
			t.Fatalf("%s: %+v", index, v.Doc)
		}
		runes := len([]rune(v.Text))
		for i, c := range v.Chunks {
			if c.Text != "" {
				t.Fatalf("текст чанка ушёл в ответ: %s", c.ID)
			}
			if c.DocID != "manul" || c.Strategy != kb.Strategy(index) || c.Start < 0 || c.End > runes || c.Start >= c.End || c.Tokens == 0 || c.Section == "" {
				t.Fatalf("чанк %d: %+v", i, c)
			}
			if i > 0 && c.Start < v.Chunks[i-1].Start {
				t.Fatalf("чанки не по порядку: %+v", v.Chunks)
			}
		}
		n[index] = len(v.Chunks)
	}
	if n["structure"] == 0 || n["fixed"] == 0 || n["structure"] == n["fixed"] {
		t.Fatalf("чанков: %v", n)
	}

	// Без index — первый индекс базы.
	var v DocView
	if code := get(t, srv, "/api/kb/docs/manul", &v); code != http.StatusOK || v.Index == "" || len(v.Chunks) == 0 {
		t.Fatalf("без index: %d %+v", code, v.Index)
	}

	for _, p := range []string{"/api/kb/docs/giraffe", "/api/kb/docs/manul?index=nope", "/api/kb/docs/manul/x"} {
		var e map[string]string
		if code := get(t, srv, p, &e); code != http.StatusNotFound || e["error"] == "" {
			t.Fatalf("%s: %d %v", p, code, e)
		}
	}
}

func TestSearch(t *testing.T) {
	srv := newAPI(t, newStore(t, true), nil)
	q := url.QueryEscape("чем кормится манул")

	var v SearchView
	if code := get(t, srv, "/api/kb/search?q="+q+"&k=3", &v); code != http.StatusOK {
		t.Fatalf("код %d", code)
	}
	if v.Query != "чем кормится манул" || len(v.Results) != 2 || v.Results[0].Info.Index != "structure" || v.Results[1].Info.Index != "fixed" {
		t.Fatalf("оба индекса: %+v", v)
	}
	for _, r := range v.Results {
		if r.Error != "" || len(r.Hits) == 0 || len(r.Hits) > 3 || r.Info.Mode != kb.Dense || r.Info.Embedder != "hash-256" {
			t.Fatalf("%s: %+v", r.Info.Index, r)
		}
		if r.Hits[0].Rank != 1 || r.Hits[0].Text == "" || r.Hits[0].DocID == "" {
			t.Fatalf("попадание: %+v", r.Hits[0])
		}
	}

	v = SearchView{}
	if code := get(t, srv, "/api/kb/search?q="+q+"&index=fixed&mode=bm25", &v); code != http.StatusOK {
		t.Fatalf("код %d", code)
	}
	if len(v.Results) != 1 || v.Results[0].Info.Index != "fixed" || v.Results[0].Info.Mode != kb.BM25 || v.Results[0].Info.Fallback != "" {
		t.Fatalf("bm25: %+v", v.Results)
	}
	if h := v.Results[0].Hits; len(h) == 0 || h[0].DocID != "manul" {
		t.Fatalf("bm25 попадания: %+v", h)
	}

	v = SearchView{}
	if code := get(t, srv, "/api/kb/search?q="+q+"&index=all&k=20", &v); code != http.StatusOK || len(v.Results) != 2 {
		t.Fatalf("index=all: %d %+v", code, v)
	}

	bad := map[string]int{
		"/api/kb/search":                       http.StatusBadRequest,
		"/api/kb/search?q=++":                  http.StatusBadRequest,
		"/api/kb/search?q=" + q + "&k=0":       http.StatusBadRequest,
		"/api/kb/search?q=" + q + "&k=21":      http.StatusBadRequest,
		"/api/kb/search?q=" + q + "&k=пять":    http.StatusBadRequest,
		"/api/kb/search?q=" + q + "&mode=fts":  http.StatusBadRequest,
		"/api/kb/search?q=" + q + "&index=xyz": http.StatusNotFound,
	}
	for p, want := range bad {
		var e map[string]string
		if code := get(t, srv, p, &e); code != want || e["error"] == "" {
			t.Fatalf("%s: %d %v, ждали %d", p, code, e, want)
		}
	}
}

// TestSearchNoIndexes — база без индексов: поиску искать негде.
func TestSearchNoIndexes(t *testing.T) {
	srv := newAPI(t, newStore(t, false), nil)
	var e map[string]string
	if code := get(t, srv, "/api/kb/search?q=манул", &e); code != http.StatusNotFound || !strings.Contains(e["error"], "kb index") {
		t.Fatalf("%d %v", code, e)
	}
}

func TestReport(t *testing.T) {
	st := newStore(t, true)
	srv := newAPI(t, st, nil)
	var e map[string]string
	if code := get(t, srv, "/api/kb/report", &e); code != http.StatusNotFound || !strings.Contains(e["error"], "kb eval") {
		t.Fatalf("без отчёта: %d %v", code, e)
	}
	qs := kb.QuestionSet{Schema: kb.QuestionsSchema, Questions: []kb.Question{
		{ID: "T01", Split: kb.SplitTest, Type: "fact", Q: "Чем кормится манул?", Answerable: true,
			Sources:  []kb.SourceRef{{DocID: "manul"}},
			Evidence: []kb.Evidence{{DocID: "manul", Quote: "Кормится манул почти исключительно мелкими грызунами и пищухами."}}},
	}}
	if _, err := kb.Compare(context.Background(), &kb.Searcher{Store: st, Embedder: embed.Hash{}}, qs, kb.CompareOptions{}); err != nil {
		t.Fatal(err)
	}
	var r kb.Report
	if code := get(t, srv, "/api/kb/report", &r); code != http.StatusOK {
		t.Fatalf("код %d", code)
	}
	if r.CorpusSHA != "sha-mini" || len(r.Stats) != 2 || len(r.Retrieval) == 0 || len(r.Conclusion) == 0 || r.Retrieval[0].Recall[1] < 0 {
		t.Fatalf("отчёт: %+v", r)
	}
	var v InfoView
	if get(t, srv, "/api/kb/info", &v); !v.Report {
		t.Fatal("info не видит отчёт")
	}
}

func TestMethodsAndRoutes(t *testing.T) {
	srv := newAPI(t, newStore(t, true), nil)
	for _, p := range []string{"/api/kb/info", "/api/kb/docs", "/api/kb/docs/manul", "/api/kb/search?q=x", "/api/kb/report"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+p, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != http.MethodGet {
			t.Fatalf("POST %s: %d, Allow %q", p, res.StatusCode, res.Header.Get("Allow"))
		}
	}
	var e map[string]string
	if code := get(t, srv, "/api/kb/nope", &e); code != http.StatusNotFound || !strings.Contains(e["error"], "/api/kb/nope") {
		t.Fatalf("нет раздела: %d %v", code, e)
	}
	// Неизвестный раздел — 404 и без базы, и любым методом.
	srv = newAPI(t, nil, nil)
	if code := do(t, srv, http.MethodDelete, "/api/kb/nope", nil); code != http.StatusNotFound {
		t.Fatalf("без базы: %d", code)
	}
}
