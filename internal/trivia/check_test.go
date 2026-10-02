package trivia

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// checkFake — подставные англовики и GBIF на одном сервере: /en/w/api.php и
// /gbif/…. Ответы — по заголовку и названию; неизвестное — «нет такого».
type checkFake struct {
	t     *testing.T
	wiki  map[string]string // заголовок → страница (JSON-объект из pages)
	match map[string]string // латинское название → ответ species/match
	occ   map[string]int    // taxonKey → count
	fail  map[string]int    // путь → код ответа вместо обычного

	mu    sync.Mutex
	calls []string // «wiki:<title>», «match:<name>», «occ:<key>»
}

func (f *checkFake) log(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *checkFake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *checkFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if code := f.fail[r.URL.Path]; code != 0 {
		w.WriteHeader(code)
		return
	}
	switch r.URL.Path {
	case "/en/w/api.php":
		title := q.Get("titles")
		f.log("wiki:" + title)
		// Запрос обязан спрашивать то, что потом разбирается.
		for k, v := range map[string]string{"action": "query", "format": "json", "formatversion": "2",
			"redirects": "1", "lllang": "ru", "inprop": "url", "ppprop": "disambiguation"} {
			if q.Get(k) != v {
				f.t.Errorf("вики: %s=%q, ожидалось %q", k, q.Get(k), v)
			}
		}
		if p := q.Get("prop"); !strings.Contains(p, "langlinks") || !strings.Contains(p, "info") || !strings.Contains(p, "pageprops") {
			f.t.Errorf("вики: prop=%q", p)
		}
		page, ok := f.wiki[title]
		if !ok {
			page = fmt.Sprintf(`{"ns":0,"title":%q,"missing":true}`, title)
		}
		fmt.Fprintf(w, `{"batchcomplete":true,"query":{"pages":[%s]}}`, page)
	case "/gbif/species/match":
		name := q.Get("name")
		f.log("match:" + name)
		if q.Get("kingdom") != "Animalia" || q.Get("class") != "Mammalia" || q.Get("rank") != "SPECIES" {
			f.t.Errorf("GBIF match без уточнений: %s", r.URL.RawQuery)
		}
		resp, ok := f.match[name]
		if !ok {
			resp = `{"confidence":100,"matchType":"NONE","synonym":false}`
		}
		fmt.Fprint(w, resp)
	case "/gbif/occurrence/search":
		key := q.Get("taxonKey")
		f.log("occ:" + key)
		if q.Get("limit") != "0" {
			f.t.Errorf("GBIF occurrence: limit=%q — записи не нужны, только count", q.Get("limit"))
		}
		fmt.Fprintf(w, `{"offset":0,"limit":0,"endOfRecords":false,"count":%d,"results":[]}`, f.occ[key])
	default:
		http.NotFound(w, r)
	}
}

var checkFixedNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func checkNewFake(t *testing.T) (*checkFake, *WebChecker) {
	t.Helper()
	f := &checkFake{
		t: t,
		wiki: map[string]string{
			// Редирект с латинского названия на английскую статью со ссылкой на ru.
			"Otocolobus manul": `{"pageid":1,"ns":0,"title":"Pallas's cat","fullurl":"https://en.test/wiki/Pallas%27s_cat",
				"langlinks":[{"lang":"ru","title":"Манул"}]}`,
			// Только английская статья.
			"Rattus solus": `{"pageid":2,"ns":0,"title":"Lonely rat","fullurl":"https://en.test/wiki/Lonely_rat"}`,
			// По латинскому нет, по английскому есть.
			"Tree critter": `{"pageid":3,"ns":0,"title":"Tree critter","fullurl":"https://en.test/wiki/Tree_critter",
				"langlinks":[{"lang":"ru","title":"Древесная зверушка"}]}`,
			// Английское название — неоднозначность, как «Beira» на живом API.
			"Beira": `{"pageid":4,"ns":0,"title":"Beira","fullurl":"https://en.test/wiki/Beira",
				"langlinks":[{"lang":"ru","title":"Бейра"}],"pageprops":{"disambiguation":""}}`,
			// Латинское — неоднозначность, английское — статья.
			"Ambigua ambigua": `{"pageid":5,"ns":0,"title":"Ambigua ambigua","pageprops":{"disambiguation":""}}`,
			"Real ambigua":    `{"pageid":6,"ns":0,"title":"Real ambigua","fullurl":"https://en.test/wiki/Real_ambigua"}`,
			// Статья без fullurl: адрес собирается сам.
			"Nourl nourl": `{"pageid":7,"ns":0,"title":"No url deer"}`,
		},
		match: map[string]string{},
		occ:   map[string]int{},
		fail:  map[string]int{},
	}
	for i, name := range []string{"Otocolobus manul", "Rattus solus", "Arborea critter", "Ambigua beira",
		"Ambigua ambigua", "Nulla nulla", "Nourl nourl"} {
		key := 100 + i
		f.match[name] = fmt.Sprintf(`{"usageKey":%d,"scientificName":%q,"rank":"SPECIES","status":"ACCEPTED","confidence":99,"matchType":"EXACT"}`, key, name)
		f.occ[fmt.Sprint(key)] = 500
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := &WebChecker{
		Fetcher:    tools.NewFetcher(),
		EnWikiBase: srv.URL + "/en/",
		RuWikiBase: "https://ru.test",
		GBIFBase:   srv.URL + "/gbif",
		Now:        func() time.Time { return checkFixedNow },
	}
	return f, c
}

func checkSpecies(id int, sci, common string) mdd.Species {
	return mdd.Species{ID: id, SciName: sci, CommonName: common}
}

func TestCheckArticleRu(t *testing.T) {
	f, c := checkNewFake(t)
	e, err := c.Check(context.Background(), checkSpecies(1006010, "Otocolobus manul", "Pallas's cat"), 50)
	if err != nil {
		t.Fatal(err)
	}
	want := Eligibility{SpeciesID: 1006010, SciName: "Otocolobus manul", CheckedAt: checkFixedNow, OK: true,
		WikiLang: "ru", WikiTitle: "Манул", WikiURL: "https://ru.test/wiki/%D0%9C%D0%B0%D0%BD%D1%83%D0%BB",
		EnTitle: "Pallas's cat", GBIFKey: 100, Occurrences: 500}
	if e != want {
		t.Errorf("проверка:\n got %+v\nwant %+v", e, want)
	}
	// GBIF идёт первым (см. Check), статья — одним запросом по латинскому.
	if got := strings.Join(f.Calls(), ","); got != "match:Otocolobus manul,occ:100,wiki:Otocolobus manul" {
		t.Errorf("запросы: %s", got)
	}
}

func TestCheckArticleEnOnly(t *testing.T) {
	_, c := checkNewFake(t)
	e, err := c.Check(context.Background(), checkSpecies(2, "Rattus solus", "Lonely rat"), 50)
	if err != nil {
		t.Fatal(err)
	}
	if !e.OK || e.WikiLang != "en" || e.WikiTitle != "Lonely rat" || e.EnTitle != "Lonely rat" ||
		e.WikiURL != "https://en.test/wiki/Lonely_rat" {
		t.Errorf("только en: %+v", e)
	}
}

func TestCheckArticleURLWithoutFullURL(t *testing.T) {
	_, c := checkNewFake(t)
	e, err := c.Check(context.Background(), checkSpecies(7, "Nourl nourl", ""), 50)
	if err != nil {
		t.Fatal(err)
	}
	if !e.OK || !strings.HasSuffix(e.WikiURL, "/en/wiki/No_url_deer") || strings.Contains(e.WikiURL, "//wiki") {
		t.Errorf("адрес статьи: %q", e.WikiURL)
	}
}

func TestCheckArticleByCommonName(t *testing.T) {
	f, c := checkNewFake(t)
	e, err := c.Check(context.Background(), checkSpecies(3, "Arborea critter", "Tree critter"), 50)
	if err != nil {
		t.Fatal(err)
	}
	if !e.OK || e.WikiLang != "ru" || e.WikiTitle != "Древесная зверушка" || e.EnTitle != "Tree critter" {
		t.Errorf("по английскому названию: %+v", e)
	}
	calls := strings.Join(f.Calls(), ",")
	if !strings.HasSuffix(calls, "wiki:Arborea critter,wiki:Tree critter") {
		t.Errorf("сначала латинское, потом английское: %s", calls)
	}
}

func TestCheckNoArticle(t *testing.T) {
	_, c := checkNewFake(t)
	e, err := c.Check(context.Background(), checkSpecies(6, "Nulla nulla", "Null mouse"), 50)
	if err != nil {
		t.Fatal(err)
	}
	want := Eligibility{SpeciesID: 6, SciName: "Nulla nulla", CheckedAt: checkFixedNow, Reason: ReasonNoArticle,
		GBIFKey: 105, Occurrences: 500}
	if e != want {
		t.Errorf("без статьи:\n got %+v\nwant %+v", e, want)
	}
}

// Страница-неоднозначность — не статья о виде: по ней факты не собрать.
func TestCheckDisambiguation(t *testing.T) {
	_, c := checkNewFake(t)
	e, err := c.Check(context.Background(), checkSpecies(4, "Ambigua beira", "Beira"), 50)
	if err != nil {
		t.Fatal(err)
	}
	if e.OK || e.Reason != ReasonNoArticle || e.WikiTitle != "" || e.EnTitle != "" {
		t.Errorf("неоднозначность засчитана статьёй: %+v", e)
	}
	// Неоднозначность по латинскому не мешает найти статью по английскому.
	e, err = c.Check(context.Background(), checkSpecies(5, "Ambigua ambigua", "Real ambigua"), 50)
	if err != nil {
		t.Fatal(err)
	}
	if !e.OK || e.EnTitle != "Real ambigua" {
		t.Errorf("после неоднозначности: %+v", e)
	}
}

func TestCheckDomesticWithoutNetwork(t *testing.T) {
	f, c := checkNewFake(t)
	sp := checkSpecies(9, "Otocolobus manul", "")
	sp.Domestic = true
	e, err := c.Check(context.Background(), sp, 50)
	if err != nil {
		t.Fatal(err)
	}
	if e.OK || e.Reason != ReasonDomestic || e.SpeciesID != 9 || !e.CheckedAt.Equal(checkFixedNow) {
		t.Errorf("домашний: %+v", e)
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("домашний вид проверялся по сети: %v", calls)
	}
}

// Всё, что не точное совпадение на уровне вида, — «GBIF не знает», и до
// статьи дело не доходит.
func TestCheckNoGBIF(t *testing.T) {
	for name, resp := range map[string]string{
		"нет совпадения": ``,
		"только род":     `{"usageKey":2435528,"rank":"GENUS","matchType":"HIGHERRANK","confidence":99}`,
		"нечёткое":       `{"usageKey":77,"rank":"SPECIES","matchType":"FUZZY","confidence":95}`,
		"точно, но род":  `{"usageKey":78,"rank":"GENUS","matchType":"EXACT","confidence":99}`,
	} {
		t.Run(name, func(t *testing.T) {
			f, c := checkNewFake(t)
			if resp != "" {
				f.match["Otocolobus manul"] = resp
			} else {
				delete(f.match, "Otocolobus manul")
			}
			e, err := c.Check(context.Background(), checkSpecies(1, "Otocolobus manul", "Pallas's cat"), 50)
			if err != nil {
				t.Fatal(err)
			}
			if e.OK || e.Reason != ReasonNoGBIF || e.GBIFKey != 0 || e.WikiTitle != "" {
				t.Errorf("нет в GBIF: %+v", e)
			}
			if calls := f.Calls(); len(calls) != 1 {
				t.Errorf("после отказа GBIF лишние запросы: %v", calls)
			}
		})
	}
}

// Синоним: наблюдения — под принятым названием (acceptedUsageKey).
func TestCheckSynonymUsesAcceptedKey(t *testing.T) {
	f, c := checkNewFake(t)
	f.match["Otocolobus manul"] = `{"usageKey":5219399,"acceptedUsageKey":2435023,"rank":"SPECIES",
		"status":"SYNONYM","matchType":"EXACT","confidence":100}`
	f.occ["5219399"] = 3
	f.occ["2435023"] = 839
	e, err := c.Check(context.Background(), checkSpecies(1006010, "Otocolobus manul", ""), 50)
	if err != nil {
		t.Fatal(err)
	}
	if !e.OK || e.GBIFKey != 2435023 || e.Occurrences != 839 {
		t.Errorf("синоним: %+v", e)
	}
}

// Принятым названием синонима бывает подвид — и это годится.
func TestCheckSynonymOfSubspecies(t *testing.T) {
	f, c := checkNewFake(t)
	f.match["Otocolobus manul"] = `{"usageKey":5,"acceptedUsageKey":6,"rank":"SUBSPECIES","status":"SYNONYM","matchType":"EXACT"}`
	f.occ["6"] = 60
	e, err := c.Check(context.Background(), checkSpecies(1, "Otocolobus manul", ""), 50)
	if err != nil || !e.OK || e.GBIFKey != 6 {
		t.Errorf("синоним подвида: %+v, %v", e, err)
	}
}

func TestCheckFewRecords(t *testing.T) {
	f, c := checkNewFake(t)
	f.occ["100"] = 12
	e, err := c.Check(context.Background(), checkSpecies(1, "Otocolobus manul", ""), 50)
	if err != nil {
		t.Fatal(err)
	}
	want := Eligibility{SpeciesID: 1, SciName: "Otocolobus manul", CheckedAt: checkFixedNow,
		Reason: ReasonFewRecords, GBIFKey: 100, Occurrences: 12}
	if e != want {
		t.Errorf("мало наблюдений:\n got %+v\nwant %+v", e, want)
	}
	// Порог включительный: ровно minOccurrences — достаточно.
	e, err = c.Check(context.Background(), checkSpecies(1, "Otocolobus manul", ""), 12)
	if err != nil || !e.OK {
		t.Errorf("на пороге: %+v, %v", e, err)
	}
}

// Сбой источника — err и ReasonCheckFailed (не кэшируется), а не «нет
// статьи» или «нет в GBIF», которые запомнились бы на CheckTTL.
func TestCheckSourceFailures(t *testing.T) {
	for _, path := range []string{"/gbif/species/match", "/gbif/occurrence/search", "/en/w/api.php"} {
		for _, code := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusNotFound} {
			t.Run(fmt.Sprint(path, code), func(t *testing.T) {
				f, c := checkNewFake(t)
				f.fail[path] = code
				e, err := c.Check(context.Background(), checkSpecies(1, "Otocolobus manul", "Pallas's cat"), 50)
				if err == nil {
					t.Fatalf("сбой %s %d не стал ошибкой: %+v", path, code, e)
				}
				if !strings.Contains(err.Error(), "Otocolobus manul") || !strings.Contains(err.Error(), fmt.Sprint(code)) {
					t.Errorf("в ошибке нет вида или кода: %v", err)
				}
				if e.OK || e.Reason != ReasonCheckFailed || e.SpeciesID != 1 || e.GBIFKey != 0 || e.WikiTitle != "" {
					t.Errorf("итог при сбое: %+v", e)
				}
			})
		}
	}
}

func TestCheckNetworkError(t *testing.T) {
	_, c := checkNewFake(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	dead := srv.URL
	srv.Close() // порт закрыт: соединение не установится
	c.GBIFBase = dead
	e, err := c.Check(context.Background(), checkSpecies(1, "Otocolobus manul", ""), 50)
	if err == nil || e.Reason != ReasonCheckFailed || e.OK {
		t.Errorf("сетевая ошибка: %+v, %v", e, err)
	}

	// Отменённый контекст — тоже ошибка, а не отказ.
	_, c = checkNewFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e, err := c.Check(ctx, checkSpecies(1, "Otocolobus manul", ""), 50); err == nil || e.Reason != ReasonCheckFailed {
		t.Errorf("отменённый контекст: %+v, %v", e, err)
	}
}

// Пустые базы — боевые адреса, nil Now — текущее время, nil Fetcher — общий.
func TestCheckDefaults(t *testing.T) {
	c := NewWebChecker(nil)
	if c.webFetcher() == nil {
		t.Fatal("нет Fetcher по умолчанию")
	}
	if got := webBase(c.EnWikiBase, webEnWikiBase); got != "https://en.wikipedia.org" {
		t.Errorf("en: %s", got)
	}
	if got := webBase(c.GBIFBase, tools.DefaultGBIFBase); got != tools.DefaultGBIFBase {
		t.Errorf("gbif: %s", got)
	}
	sp := checkSpecies(1, "Bos taurus", "")
	sp.Domestic = true
	before := time.Now()
	e, err := c.Check(context.Background(), sp, 50)
	if err != nil || e.CheckedAt.Before(before) || e.Reason != ReasonDomestic {
		t.Errorf("время по умолчанию: %+v, %v", e, err)
	}
}
