package trivia

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// collectFake — подставные ru/en Википедия и GBIF на одном сервере:
// /ru/w/api.php, /en/w/api.php, /gbif/…
type collectFake struct {
	ru, en map[string]string // заголовок → текст статьи (extract)
	// Фасеты стран occurrence/search за всё время и за окно (запрос с
	// eventDate); *Fail — ответить 500.
	total, recent         map[string]int
	totalN, recentN       int
	totalFail, recentFail bool
	vernacular            []map[string]string

	mu        sync.Mutex
	eventDate string
	calls     []string
}

func (f *collectFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.URL.Path)
		f.mu.Unlock()
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/ru/w/api.php" || r.URL.Path == "/en/w/api.php":
			arts := f.ru
			if strings.HasPrefix(r.URL.Path, "/en") {
				arts = f.en
			}
			title := q.Get("titles")
			text, ok := arts[title]
			page := map[string]any{"title": title, "extract": text}
			if !ok {
				page = map[string]any{"title": title, "missing": true}
			}
			collectJSON(w, map[string]any{"query": map[string]any{"pages": []any{page}}})
		case r.URL.Path == "/gbif/occurrence/search":
			if q.Get("facet") != "country" || q.Get("limit") != "0" || q.Get("facetLimit") != "20" {
				http.Error(w, "плохой запрос", http.StatusBadRequest)
				return
			}
			counts, n, fail := f.total, f.totalN, f.totalFail
			if d := q.Get("eventDate"); d != "" {
				f.mu.Lock()
				f.eventDate = d
				f.mu.Unlock()
				counts, n, fail = f.recent, f.recentN, f.recentFail
			}
			if fail {
				http.Error(w, "сбой", http.StatusInternalServerError)
				return
			}
			var cs []map[string]any
			for code, c := range counts {
				cs = append(cs, map[string]any{"name": code, "count": c})
			}
			collectJSON(w, map[string]any{"count": n, "results": []any{},
				"facets": []any{map[string]any{"field": "COUNTRY", "counts": cs}}})
		case strings.HasPrefix(r.URL.Path, "/gbif/species/") && strings.HasSuffix(r.URL.Path, "/vernacularNames"):
			collectJSON(w, map[string]any{"results": f.vernacular})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func collectJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

var collectNow = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

func (f *collectFake) collector(t *testing.T) *WebCollector {
	srv := f.server(t)
	c := NewWebCollector(tools.NewFetcher())
	c.RuWikiBase = srv.URL + "/ru"
	c.EnWikiBase = srv.URL + "/en"
	c.GBIFBase = srv.URL + "/gbif"
	c.Now = func() time.Time { return collectNow }
	return c
}

// collectPara — абзац заданной длины: разделы короче порога не берутся.
func collectPara(word string, runes int) string {
	var b strings.Builder
	for utf8.RuneCountInString(b.String()) < runes {
		b.WriteString(word)
		b.WriteString(" ")
	}
	return strings.TrimSpace(b.String())
}

var collectManulRu = strings.Join([]string{
	"Манул — дикая кошка Центральной Азии.",
	"== Описание и внешний вид ==", collectPara("мех", 900),
	"=== Шерсть и окрас ===", collectPara("окрас", 400),
	"== Распространение и численность ==", collectPara("степь", 600),
	"== Биология и экология ==", collectPara("экология", 300),
	"=== Поведение ===", collectPara("сумерки", 700),
	"=== Охота и питание ===", collectPara("пищухи", 700),
	"=== Размножение и жизненный цикл ===", collectPara("котята", 700),
	"== Манул в культуре ==", collectPara("мем", 500),
	"== Примечания ==", collectPara("сноска", 2000),
	"== Литература ==", collectPara("книга", 2000),
}, "\n")

func collectManul() (Pick, mdd.Species) {
	sp := mdd.Species{
		ID: 1006010, SciName: "Otocolobus manul", CommonName: "Pallas's Cat",
		OtherCommonNames: []string{"Manul"}, Order: "Carnivora", Family: "Felidae", Genus: "Otocolobus",
		Authority: "(Pallas, 1776)", Year: 1776, IUCN: "LC",
		Countries: []string{"Russia", "Mongolia", "China"}, CountriesUncertain: []string{"Tajikistan"},
		Continents: []string{"Asia"}, Realms: []string{"Palearctic"},
		TypeLocality: "S of Lake Baikal", DistributionNotes: collectPara("note", 2000),
	}
	p := Pick{SpeciesID: sp.ID, SciName: sp.SciName, IUCN: "LC", Eligibility: Eligibility{
		SpeciesID: sp.ID, SciName: sp.SciName, OK: true,
		WikiLang: "ru", WikiTitle: "Манул (животное)", EnTitle: "Pallas's cat", GBIFKey: 2435023, Occurrences: 839,
	}}
	return p, sp
}

func TestCollectRuArticle(t *testing.T) {
	f := &collectFake{
		ru:     map[string]string{"Манул (животное)": collectManulRu},
		total:  map[string]int{"RU": 400, "MN": 100, "TJ": 7, "DE": 11, "AD": 2, "ZZ": 3},
		totalN: 523,
		recent: map[string]int{"CN": 1, "DE": 1}, recentN: 2,
	}
	c := f.collector(t)
	p, sp := collectManul()
	d, err := c.Collect(context.Background(), p, sp)
	if err != nil {
		t.Fatal(err)
	}

	if d.NameRu != "Манул" {
		t.Errorf("NameRu = %q, ждали «Манул» (уточнение в скобках снимается)", d.NameRu)
	}
	// Took может быть 0: на Windows быстрая сборка на подставных серверах
	// укладывается в один тик часов.
	if !d.CollectedAt.Equal(collectNow) || d.Took < 0 || d.Pick.SpeciesID != sp.ID || d.Species.SciName != sp.SciName {
		t.Errorf("служебные поля досье: %+v", d)
	}

	var titles []string
	for i, m := range d.Materials {
		if m.ID != "S"+strconv.Itoa(i+1) {
			t.Errorf("материал %d: ID %q", i, m.ID)
		}
		titles = append(titles, m.Kind+"|"+m.Title)
	}
	want := []string{
		"mdd|MDD: Otocolobus manul",
		"wikipedia|Википедия (ru): Манул (животное)",
		"wikipedia|Википедия (ru): Манул (животное) — Описание и внешний вид",
		"wikipedia|Википедия (ru): Манул (животное) — Поведение",
		"wikipedia|Википедия (ru): Манул (животное) — Охота и питание",
		"wikipedia|Википедия (ru): Манул (животное) — Размножение и жизненный цикл",
		"gbif|GBIF: наблюдения Otocolobus manul",
	}
	if strings.Join(titles, "\n") != strings.Join(want, "\n") {
		t.Fatalf("материалы:\n%s\nждали:\n%s", strings.Join(titles, "\n"), strings.Join(want, "\n"))
	}

	mddM := d.Materials[0]
	for _, s := range []string{"Латинское название: Otocolobus manul", "(Pallas, 1776)", "отряд Carnivora",
		"Статус МСОП: LC (", "Вымерший вид: нет", "Страны обитания: Russia, Mongolia, China",
		"под вопросом: Tajikistan", "Palearctic", "…[обрезано]"} {
		if !strings.Contains(mddM.Text, s) {
			t.Errorf("материал MDD без %q:\n%s", s, mddM.Text)
		}
	}
	if mddM.URL != sp.URL() || mddM.Tool != "mdd" {
		t.Errorf("материал MDD: URL %q, Tool %q", mddM.URL, mddM.Tool)
	}
	if u := d.Materials[3].URL; !strings.HasSuffix(u, "/ru/wiki/"+webPathTitle("Манул (животное)")+"#"+webPathTitle("Поведение")) {
		t.Errorf("URL раздела с якорем: %q", u)
	}

	o := d.Observations
	if o.GBIFKey != 2435023 || o.Total != 523 || o.Recent != 2 || o.WindowDays != 30 {
		t.Errorf("наблюдения: %+v", o)
	}
	if f.date() != "2026-08-25,2026-09-24" {
		t.Errorf("eventDate = %q", f.date())
	}
	got := map[string]string{}
	for _, cc := range o.ByCountry {
		got[cc.Code] = cc.Name + "/" + cc.Range
	}
	wantR := map[string]string{"RU": "Russia/in", "MN": "Mongolia/in", "TJ": "Tajikistan/uncertain",
		"DE": "Germany/out", "AD": "/unknown", "ZZ": "/unknown"}
	for code, w := range wantR {
		if got[code] != w {
			t.Errorf("страна %s: %q, ждали %q", code, got[code], w)
		}
	}
	if o.ByCountry[0].Code != "RU" || o.ByCountry[1].Code != "MN" || o.ByCountry[2].Code != "DE" {
		t.Errorf("страны не по убыванию: %+v", o.ByCountry)
	}
	if len(o.OutOfRange) != 1 || o.OutOfRange[0].Code != "DE" {
		t.Errorf("OutOfRange = %+v", o.OutOfRange)
	}
	if len(o.RecentByCountry) != 2 || o.RecentByCountry[0].Range == "" {
		t.Errorf("RecentByCountry = %+v", o.RecentByCountry)
	}

	g := d.Materials[len(d.Materials)-1]
	for _, s := range []string{"Записей за всё время: 523", "за последние 30 дней (2026-08-25 — 2026-09-24, по дате наблюдения): 2",
		"RU Russia — 400 (в ареале MDD)", "TJ Tajikistan — 7 (в ареале MDD под вопросом)",
		"DE Germany — 11 (вне ареала MDD)", "AD Andorra — 2 (не сопоставлена с MDD)", "вне ареала MDD: DE Germany — 11"} {
		if !strings.Contains(g.Text, s) {
			t.Errorf("материал GBIF без %q:\n%s", s, g.Text)
		}
	}
	if g.URL != "https://www.gbif.org/species/2435023" {
		t.Errorf("URL GBIF: %q", g.URL)
	}
}

func TestCollectEnOnly(t *testing.T) {
	f := &collectFake{
		en: map[string]string{"Silent grass mouse": "The silent grass mouse is a rodent.\n" +
			"== Distribution and habitat ==\n" + collectPara("Peru", 300) + "\n" +
			"== Threats ==\n" + collectPara("threat", 250) + "\n== References ==\n" + collectPara("ref", 900)},
		total: map[string]int{"PE": 53}, totalN: 53,
		recent: map[string]int{}, recentN: 0,
		vernacular: []map[string]string{
			{"vernacularName": "Silent Grass Mouse", "language": "eng"},
			{"vernacularName": "безмолвный хомячок", "language": "rus"},
			{"vernacularName": "другое имя", "language": "rus"},
		},
	}
	c := f.collector(t)
	sp := mdd.Species{ID: 1001, SciName: "Akodon surdus", Countries: []string{"Peru"}}
	p := Pick{SpeciesID: sp.ID, SciName: sp.SciName, Eligibility: Eligibility{
		WikiLang: "en", WikiTitle: "Silent grass mouse", EnTitle: "Silent grass mouse", GBIFKey: 42}}
	d, err := c.Collect(context.Background(), p, sp)
	if err != nil {
		t.Fatal(err)
	}
	if d.NameRu != "Безмолвный хомячок" {
		t.Errorf("NameRu = %q", d.NameRu)
	}
	var titles []string
	for _, m := range d.Materials {
		titles = append(titles, m.Title)
	}
	want := "MDD: Akodon surdus|Википедия (en): Silent grass mouse|Википедия (en): Silent grass mouse — Distribution and habitat|" +
		"Википедия (en): Silent grass mouse — Threats|GBIF: наблюдения Akodon surdus"
	if strings.Join(titles, "|") != want {
		t.Errorf("материалы: %s", strings.Join(titles, "|"))
	}
	if d.Observations.Total != 53 || d.Observations.ByCountry[0].Range != RangeIn || len(d.Observations.OutOfRange) != 0 {
		t.Errorf("наблюдения: %+v", d.Observations)
	}
}

// Русская статья не прочиталась — досье собирается по английской.
func TestCollectRuFallsBackToEn(t *testing.T) {
	f := &collectFake{
		en:         map[string]string{"Pallas's cat": "The Pallas's cat is a small wild cat."},
		totalFail:  true,
		vernacular: []map[string]string{{"vernacularName": "манул", "language": "rus"}},
	}
	c := f.collector(t)
	p, sp := collectManul()
	d, err := c.Collect(context.Background(), p, sp)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Materials) != 2 || d.Materials[1].Title != "Википедия (en): Pallas's cat" {
		t.Errorf("материалы: %+v", d.Materials)
	}
	if d.NameRu != "Манул" {
		t.Errorf("NameRu = %q", d.NameRu)
	}
	// Общий запрос GBIF упал: не ошибка, но и материала наблюдений нет.
	if d.Observations.Total != 0 || d.Observations.GBIFKey != 2435023 || d.Observations.WindowDays != 30 {
		t.Errorf("наблюдения: %+v", d.Observations)
	}
}

func TestCollectNoArticle(t *testing.T) {
	f := &collectFake{total: map[string]int{"RU": 1}, totalN: 1}
	c := f.collector(t)
	p, sp := collectManul()
	if _, err := c.Collect(context.Background(), p, sp); err == nil || !strings.Contains(err.Error(), "Википедии") {
		t.Errorf("статьи нет ни в ru, ни в en — ждали ошибку, получили %v", err)
	}

	p.Eligibility.WikiLang, p.Eligibility.WikiTitle, p.Eligibility.EnTitle = "", "", ""
	if _, err := c.Collect(context.Background(), p, sp); err == nil {
		t.Error("проверка без статьи — ждали ошибку")
	}

	p2, sp2 := collectManul()
	sp2.ID = 7
	if _, err := c.Collect(context.Background(), p2, sp2); err == nil {
		t.Error("выбор про другой вид — ждали ошибку")
	}
}

func TestCollectWindowFailure(t *testing.T) {
	f := &collectFake{
		ru:    map[string]string{"Манул (животное)": "Манул — кошка."},
		total: map[string]int{"RU": 5}, totalN: 5,
		recentFail: true,
	}
	c := f.collector(t)
	c.WindowDays = 7
	p, sp := collectManul()
	d, err := c.Collect(context.Background(), p, sp)
	if err != nil {
		t.Fatal(err)
	}
	o := d.Observations
	if o.Total != 5 || o.Recent != 0 || o.WindowDays != 7 || o.RecentByCountry != nil {
		t.Errorf("наблюдения: %+v", o)
	}
	if f.date() != "2026-09-17,2026-09-24" {
		t.Errorf("eventDate = %q", f.date())
	}
	g := d.Materials[len(d.Materials)-1]
	if g.Kind != KindGBIF || !strings.Contains(g.Text, "за последние 7 дней: неизвестно") {
		t.Errorf("материал GBIF должен пометить сбой окна:\n%s", g.Text)
	}
}

func TestCollectNoGBIFKey(t *testing.T) {
	f := &collectFake{ru: map[string]string{"Манул (животное)": "Манул — кошка."}}
	c := f.collector(t)
	p, sp := collectManul()
	p.Eligibility.GBIFKey = 0
	d, err := c.Collect(context.Background(), p, sp)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range f.seen() {
		if strings.HasPrefix(call, "/gbif") {
			t.Errorf("без ключа GBIF запросов быть не должно: %s", call)
		}
	}
	if len(d.Materials) != 2 {
		t.Errorf("материалы: %+v", d.Materials)
	}
}

func TestCollectPickSections(t *testing.T) {
	sec := func(title string, level, runes int) tools.Section {
		return tools.Section{Title: title, Level: level, Text: collectPara("x", runes)}
	}
	cases := []struct {
		name string
		secs []tools.Section
		want string
	}{
		{"подразделы глубже контейнера, служебные мимо", []tools.Section{
			sec("Этимология", 2, 500),
			sec("Описание", 2, 3000),
			sec("Биология и поведение", 2, 9000),
			sec("Социальная организация", 3, 2000),
			sec("Охота и питание", 3, 2000),
			sec("Размножение и жизненный цикл", 3, 2000),
			sec("Подвиды", 2, 5000),
			sec("Примечания", 2, 9000),
			sec("Комментарии", 3, 900),
		}, "Описание|Социальная организация|Охота и питание|Размножение и жизненный цикл"},
		{"английские темы, See also и References мимо", []tools.Section{
			sec("Taxonomy", 2, 3000),
			sec("Subspecies", 3, 3000),
			sec("Description", 2, 2000),
			sec("Distribution and habitat", 2, 1000),
			sec("Conservation", 2, 800),
			sec("See also", 2, 300),
			sec("References", 2, 5000),
		}, "Taxonomy|Description|Distribution and habitat|Conservation"},
		{"короткие и безымянные разделы добиваются по длине", []tools.Section{
			sec("Питание", 2, 100), // заглушка — короче порога
			sec("Находки", 2, 400),
			sec("Изучение", 2, 900),
			sec("Food sources", 2, 300), // «sources» в середине — не служебный
		}, "Находки|Изучение|Food sources"},
	}
	for _, tc := range cases {
		idx := collectPickSections(tc.secs)
		var got []string
		for _, i := range idx {
			got = append(got, tc.secs[i].Title)
		}
		if strings.Join(got, "|") != tc.want {
			t.Errorf("%s: %s, ждали %s", tc.name, strings.Join(got, "|"), tc.want)
		}
	}
}

// Бюджет делится «наливом»: короткий раздел берёт целиком, длинные
// обрезаются, сумма не выходит за MaxWikiRunes.
func TestCollectWikiBudget(t *testing.T) {
	art := &tools.Article{Title: "Т", URL: "https://x/wiki/T", Intro: collectPara("вступ", 3000), Sections: []tools.Section{
		{Title: "Описание", Level: 2, Text: collectPara("a", 5000)},
		{Title: "Питание", Level: 2, Text: collectPara("b", 300)},
		{Title: "Размножение", Level: 2, Text: collectPara("c", 5000)},
	}}
	mats := collectArticleMaterials("ru", art, 4000)
	if len(mats) != 4 {
		t.Fatalf("материалов %d", len(mats))
	}
	sum := 0
	for _, m := range mats {
		sum += utf8.RuneCountInString(strings.TrimSuffix(m.Text, " …[обрезано]"))
	}
	if sum > 4000 {
		t.Errorf("сумма %d > бюджета 4000", sum)
	}
	if n := utf8.RuneCountInString(mats[0].Text); n > 1000+20 || !strings.HasSuffix(mats[0].Text, "[обрезано]") {
		t.Errorf("вступление: %d знаков — больше четверти бюджета или без пометки обрезки", n)
	}
	if strings.Contains(mats[2].Text, "обрезано") {
		t.Error("короткий раздел «Питание» не должен обрезаться")
	}
	if !strings.HasSuffix(mats[1].Text, "[обрезано]") || !strings.HasSuffix(mats[3].Text, "[обрезано]") {
		t.Error("длинные разделы должны быть обрезаны с пометкой")
	}
	if got := collectBudget([]int{100, 5000, 5000}, 3000); got[0] != 100 || got[1] != 1450 || got[2] != 1450 {
		t.Errorf("collectBudget = %v", got)
	}
}

func collectMDDNames(t *testing.T) []string {
	t.Helper()
	fh, err := os.Open(filepath.Join("testdata", "mdd_countries.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	var names []string
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			names = append(names, s)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// Таблица стран покрывает все имена стран настоящей базы MDD, и в ней нет
// имён, которых MDD не пишет (опечатка сделала бы страну «вне ареала»).
func TestCountryTableCoversMDD(t *testing.T) {
	names := collectMDDNames(t)
	if len(names) < 200 {
		t.Fatalf("в testdata/mdd_countries.txt всего %d имён", len(names))
	}
	inMDD := map[string]bool{}
	for _, n := range names {
		inMDD[n] = true
	}
	covered := map[string]bool{}
	code := regexp.MustCompile(`^[A-Z]{2}$`)
	for c, list := range countryMDD {
		if !code.MatchString(c) {
			t.Errorf("код %q — не ISO-2", c)
		}
		if _, dup := countryNoMDD[c]; dup {
			t.Errorf("код %s и с именем MDD, и без", c)
		}
		for _, n := range list {
			if !inMDD[n] {
				t.Errorf("%s → %q: такого имени в MDD нет", c, n)
			}
			covered[n] = true
		}
	}
	var missing []string
	for _, n := range names {
		if !covered[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		t.Errorf("имена MDD без кода ISO: %v", missing)
	}
}

func TestCountryClassify(t *testing.T) {
	canary := mdd.Species{Countries: []string{"Canary Islands"}, CountriesUncertain: []string{"madeira"}}
	cases := []struct {
		sp         mdd.Species
		code, want string
	}{
		{canary, "ES", "Canary Islands/in"},
		{canary, "pt", "Madeira/uncertain"},
		{canary, "FR", "France/out"},
		{canary, "AD", "/unknown"},
		{canary, "ZZ", "/unknown"},
		{mdd.Species{Countries: []string{"China"}}, "HK", "China/in"},
		{mdd.Species{Countries: []string{"Russia"}}, "RU", "Russia/in"},
		{mdd.Species{Countries: []string{"Cote d'Ivoire"}}, "CI", "Cote d'Ivoire/in"},
	}
	for _, tc := range cases {
		name, rng := countryClassify(tc.code, tc.sp)
		if got := name + "/" + rng; got != tc.want {
			t.Errorf("%s: %s, ждали %s", tc.code, got, tc.want)
		}
	}
	if got := countryLabel(CountryCount{Code: "ZZ"}); got != "ZZ страна не указана" {
		t.Errorf("countryLabel(ZZ) = %q", got)
	}
}

// Досье из живого прогона (TestLiveCollect с TRIVIA_WRITE_DOSSIER=1) —
// основа офлайн-тестов редактора: проверяется, что они читаются и
// соблюдают контракт.
func TestCollectDossierTestdata(t *testing.T) {
	for _, tc := range []struct{ file, lang string }{
		{"dossier_manul.json", "ru"}, {"dossier_akodon.json", "en"},
	} {
		data, err := os.ReadFile(filepath.Join("testdata", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		var d Dossier
		if err := json.Unmarshal(data, &d); err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if d.Pick.Eligibility.WikiLang != tc.lang || d.NameRu == "" || d.Species.ID == 0 || d.Observations.Total == 0 {
			t.Errorf("%s: досье неполное: lang=%q name=%q", tc.file, d.Pick.Eligibility.WikiLang, d.NameRu)
		}
		kinds := map[string]int{}
		for i, m := range d.Materials {
			if m.ID != fmt.Sprintf("S%d", i+1) || m.Text == "" || m.URL == "" {
				t.Errorf("%s: материал %d: %+v", tc.file, i, m)
			}
			kinds[m.Kind]++
		}
		if d.Materials[0].Kind != KindMDD || kinds[KindWikipedia] == 0 || kinds[KindGBIF] != 1 {
			t.Errorf("%s: виды материалов %v", tc.file, kinds)
		}
	}
}

// date и seen читают под замком: запросы пишет горутина сервера.
func (f *collectFake) date() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.eventDate
}

func (f *collectFake) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Латинский заголовок русской статьи — не русское название.
func TestCollectHasCyrillic(t *testing.T) {
	for s, want := range map[string]bool{"Манул": true, "Lepilemur tymerlachsonorum": false, "": false, "Ёж": true} {
		if got := collectHasCyrillic(s); got != want {
			t.Errorf("collectHasCyrillic(%q) = %v", s, got)
		}
	}
}
