package corpus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func init() {
	fetchBackoff = 0
	now = func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) }
}

// wikiServer — подставной API Википедии: отдаёт body на любой запрос и
// запоминает последний.
func wikiServer(t *testing.T, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	var last http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = *r
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

// apiBody — ответ API с одним extract.
func apiBody(t *testing.T, title string, revid int64, extract string) string {
	t.Helper()
	page := map[string]any{
		"pageid": 1, "ns": 0, "title": title, "extract": extract,
		"revisions": []map[string]any{{"revid": revid, "parentid": revid - 1, "timestamp": "2026-01-01T00:00:00Z"}},
		"lastrevid": revid,
	}
	raw, err := json.Marshal(map[string]any{"batchcomplete": true, "query": map[string]any{"pages": []any{page}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestFetcherRealResponse(t *testing.T) {
	// testdata/manul_api.json — настоящий ответ ru.wikipedia.org на тот же
	// запрос (статья «Манул»), сохранённый как есть.
	raw, err := os.ReadFile("testdata/manul_api.json")
	if err != nil {
		t.Fatal(err)
	}
	srv, req := wikiServer(t, string(raw))
	f := Fetcher{Base: srv.URL + "/", HTTP: srv.Client(), UserAgent: "test-agent/1"}
	sp := &Species{Latin: "Otocolobus manul", Ru: "манул"}
	d, err := f.Wikipedia(context.Background(), "manul", "Манул", sp)
	if err != nil {
		t.Fatal(err)
	}

	// Запрос: один, со всем нужным.
	q := req.URL.Query()
	for k, v := range map[string]string{
		"action": "query", "prop": "extracts|revisions|info", "explaintext": "1",
		"exsectionformat": "wiki", "redirects": "1", "rvprop": "ids|timestamp",
		"format": "json", "formatversion": "2", "titles": "Манул",
	} {
		if q.Get(k) != v {
			t.Errorf("параметр %s = %q, ждали %q", k, q.Get(k), v)
		}
	}
	if req.URL.Path != "/w/api.php" || req.Header.Get("User-Agent") != "test-agent/1" {
		t.Errorf("путь %s, агент %q", req.URL.Path, req.Header.Get("User-Agent"))
	}

	if d.ID != "manul" || d.Title != "Манул" || d.Source != SourceWikipedia || d.License != LicenseWikipedia || d.Schema != Schema {
		t.Fatalf("поля: %+v", d)
	}
	if d.RevID != 154481742 || d.OldURL != srv.URL+"/w/index.php?oldid=154481742" {
		t.Errorf("ревизия: %d %s", d.RevID, d.OldURL)
	}
	if d.URL != srv.URL+"/wiki/%D0%9C%D0%B0%D0%BD%D1%83%D0%BB" {
		t.Errorf("URL %s", d.URL)
	}
	if d.Fetched != "2026-10-02T12:00:00Z" || d.Species != sp {
		t.Errorf("fetched %s, species %v", d.Fetched, d.Species)
	}
	if !strings.HasPrefix(d.Intro, "Манул, или палласов кот (лат. Otocolobus manul)") {
		t.Errorf("вступление (ударения должны быть сняты): %.80q", d.Intro)
	}

	var paths []string
	byPath := map[string]Section{}
	for _, s := range d.Sections {
		p := strings.Join(s.Path, " › ")
		paths = append(paths, p)
		byPath[p] = s
		if s.Title != s.Path[len(s.Path)-1] || s.Level != len(s.Path)+1 {
			t.Errorf("раздел %q: title %q, level %d", p, s.Title, s.Level)
		}
	}
	want := []string{
		"Описание и внешний вид",
		"Описание и внешний вид › Шерсть и окрас",
		"Описание и внешний вид › Скелет",
		"Распространение и численность",
		"Биология и экология",
		"Биология и экология › Среда обитания",
		"Биология и экология › Поведение",
		"Биология и экология › Охота и питание",
		"Биология и экология › Размножение и жизненный цикл",
		"Биология и экология › Угрозы и болезни",
		"Взаимодействие с человеком и охранный статус",
		"Систематика и изучение",
		"Систематика и изучение › Филогенетика",
		"Манул в культуре",
	}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("разделы:\n%s", strings.Join(paths, "\n"))
	}

	// Собственные тела: у «Биологии и экологии» текста нет, только
	// подразделы; текст родителя не содержит текстов подразделов.
	if byPath["Биология и экология"].Text != "" {
		t.Errorf("у раздела-контейнера текст: %.80q", byPath["Биология и экология"].Text)
	}
	parent := byPath["Описание и внешний вид"].Text
	child := byPath["Описание и внешний вид › Шерсть и окрас"].Text
	if parent == "" || child == "" || strings.Contains(parent, child[:60]) {
		t.Error("текст подраздела попал в родителя")
	}
	// Формула превращена в строку, MathML-мусора нет.
	if skel := byPath["Описание и внешний вид › Скелет"].Text; !strings.Contains(skel, "Зубная формула для манула — I 3/3 C 1/1 P 2/2 M 1/1 =28. Вторые") {
		t.Errorf("формула: %q", skel[strings.Index(skel, "Зубная"):][:120])
	}
	text := d.Text()
	for _, junk := range []string{"Литература", "Примечания", "Комментарии", "Источники", "Огнев С. И.", "displaystyle", "textstyle", "\n\n\n", "  ", "==", string(rune(0x301))} {
		if strings.Contains(text, junk) {
			t.Errorf("в тексте осталось %q", junk)
		}
	}
	for _, s := range d.Sections {
		if strings.Contains(s.Text, "\n\n") || s.Text != strings.TrimSpace(s.Text) {
			t.Errorf("раздел %v: пустые строки или пробелы по краям", s.Path)
		}
	}
}

func TestParseExtract(t *testing.T) {
	extract := "Вступление.\n\n\n" +
		"== Описание ==\nТело описания.\n\n\n" +
		"==== Глубоко ====\nПрыжок через уровень.\n\n\n" +
		"=== Окрас ===\n  Окрас  рыжий.  \n\n\n" +
		"== Пусто ==\n\n\n" +
		"=== Тоже пусто ===\n\n\n" +
		"== Примечания ==\n\n\n=== Комментарии ===\nКомментарий.\n\n\n=== Источники ===\n\n\n" +
		"==== Ещё глубже в служебном ====\nМусор.\n\n\n" +
		"== Распространение ==\nАзия.\n\n\n" +
		"== См. также ==\nСсылки.\n\n\n" +
		"== ЛИТЕРАТУРА ==\nКнига.\n\n\n" +
		"== Галерея ==\n\n\n"
	intro, secs := parseExtract(extract)
	if intro != "Вступление." {
		t.Errorf("intro %q", intro)
	}
	var got []string
	for _, s := range secs {
		got = append(got, strings.Join(s.Path, "/")+"="+s.Text)
	}
	want := []string{
		"Описание=Тело описания.",
		"Описание/Глубоко=Прыжок через уровень.",
		"Описание/Окрас=Окрас рыжий.",
		"Распространение=Азия.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("разделы:\n%s", strings.Join(got, "\n"))
	}
	if secs[1].Level != 4 || secs[2].Level != 3 {
		t.Errorf("уровни %d %d", secs[1].Level, secs[2].Level)
	}
}

func TestFetcherRedirectMissingAndErrors(t *testing.T) {
	ctx := context.Background()

	// Перенаправление: заголовок — итоговый.
	srv, _ := wikiServer(t, apiBody(t, "Ирбис", 7, "Ирбис — кошка.\n\n\n== Описание ==\nПятнистый."))
	d, err := Fetcher{Base: srv.URL, HTTP: srv.Client()}.Wikipedia(ctx, "snow-leopard", "Снежный барс", nil)
	if err != nil || d.Title != "Ирбис" || d.RevID != 7 || d.Species != nil {
		t.Fatalf("перенаправление: %+v, %v", d, err)
	}

	// Статьи нет.
	srv, _ = wikiServer(t, `{"batchcomplete":true,"query":{"pages":[{"ns":0,"title":"Нет такой","missing":true}]}}`)
	if _, err := (Fetcher{Base: srv.URL, HTTP: srv.Client()}).Wikipedia(ctx, "x", "Нет такой", nil); err == nil || !strings.Contains(err.Error(), "нет") {
		t.Fatalf("missing: %v", err)
	}

	// Ошибка API.
	srv, _ = wikiServer(t, `{"error":{"code":"badvalue","info":"плохо"}}`)
	if _, err := (Fetcher{Base: srv.URL, HTTP: srv.Client()}).Wikipedia(ctx, "x", "X", nil); err == nil || !strings.Contains(err.Error(), "badvalue") {
		t.Fatalf("error: %v", err)
	}

	// Без revid — ошибка.
	srv, _ = wikiServer(t, `{"query":{"pages":[{"title":"X","extract":"Текст."}]}}`)
	if _, err := (Fetcher{Base: srv.URL, HTTP: srv.Client()}).Wikipedia(ctx, "x", "X", nil); err == nil || !strings.Contains(err.Error(), "revid") {
		t.Fatalf("без revid: %v", err)
	}

	// Пустой текст — ошибка.
	srv, _ = wikiServer(t, apiBody(t, "X", 1, "\n\n== Литература ==\nКнига."))
	if _, err := (Fetcher{Base: srv.URL, HTTP: srv.Client()}).Wikipedia(ctx, "x", "X", nil); err == nil {
		t.Fatal("пустая статья принята")
	}

	// 404 — без повторов.
	var hits atomic.Int32
	nf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer nf.Close()
	if _, err := (Fetcher{Base: nf.URL, HTTP: nf.Client()}).Wikipedia(ctx, "x", "X", nil); err == nil || hits.Load() != 1 {
		t.Fatalf("404: %v, запросов %d", err, hits.Load())
	}
}

func TestFetcherRetriesTemporaryErrors(t *testing.T) {
	var hits atomic.Int32
	body := apiBody(t, "Манул", 5, "Текст.")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != DefaultUserAgent {
			t.Errorf("агент по умолчанию: %q", r.Header.Get("User-Agent"))
		}
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	d, err := Fetcher{Base: srv.URL, HTTP: srv.Client()}.Wikipedia(context.Background(), "manul", "Манул", nil)
	if err != nil || d.RevID != 5 || hits.Load() != 3 {
		t.Fatalf("повтор: %v, запросов %d", err, hits.Load())
	}
}

func TestLatexToText(t *testing.T) {
	cases := map[string]string{
		`I{\frac {3}{3}}C{\frac {1}{1}}=28`: "I 3/3 C 1/1 =28",
		`2\times 10^{3}`:                    "2 × 10^3",
		`\mathrm {kg}`:                      "kg",
	}
	for in, want := range cases {
		if got := latexToText(in); got != want {
			t.Errorf("latexToText(%q) = %q, ждали %q", in, got, want)
		}
	}
}
