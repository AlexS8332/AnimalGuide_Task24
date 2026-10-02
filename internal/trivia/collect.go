package trivia

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

const (
	collectDefaultWindowDays = 30
	// Бюджет текста статьи на весь выпуск: вступление и четыре раздела по
	// ~1300 знаков. Больше модели-редактору не нужно для 3–5 фактов, а
	// проверяющий читает те же материалы ещё раз — длинное досье платится
	// дважды.
	collectDefaultWikiRunes = 7000
	collectMaxSections      = 4
	// Раздел короче этого — заглушка («Основная статья: …») или подпись к
	// таблице: фактов из него не извлечь.
	collectMinSectionRunes = 200
	// Вступлению — не больше четверти бюджета: оно пересказывает статью, и
	// его избыток съел бы место разделов.
	collectIntroShare = 4
	collectNotesRunes = 800
	collectFacetLimit = 20
	// Сколько стран показывать в материале GBIF: модели хватает верхушки
	// распределения, хвост — это единичные записи.
	collectTopCountries = 10
)

// WebCollector — Collector на живых источниках: карточка MDD (без сети),
// статья Википедии (ru или en — как нашёл Checker) и наблюдения GBIF по
// странам со сверкой с ареалом MDD.
type WebCollector struct {
	Fetcher    *tools.Fetcher
	RuWikiBase string // пусто — https://ru.wikipedia.org
	EnWikiBase string // пусто — https://en.wikipedia.org
	GBIFBase   string // пусто — tools.DefaultGBIFBase
	// WindowDays — окно «свежих» наблюдений; 0 — 30 дней.
	WindowDays int
	// MaxWikiRunes — бюджет текста статьи на всё досье (вступление и
	// разделы вместе); 0 — 7000.
	MaxWikiRunes int
	Now          func() time.Time
}

var _ Collector = (*WebCollector)(nil)

// NewWebCollector — сборщик с боевыми адресами источников.
func NewWebCollector(f *tools.Fetcher) *WebCollector {
	return &WebCollector{Fetcher: f}
}

// Collect собирает досье. Порядок материалов: MDD, вступление статьи,
// разделы статьи, наблюдения GBIF. Статья обязательна — без неё выпуск не
// собирается; GBIF — нет: его сбой даёт досье без материала наблюдений.
func (c *WebCollector) Collect(ctx context.Context, p Pick, sp mdd.Species) (Dossier, error) {
	start := time.Now()
	if p.SpeciesID != 0 && sp.ID != 0 && p.SpeciesID != sp.ID {
		return Dossier{}, fmt.Errorf("сборка досье: выбор относится к виду %d, а передан вид %d (%s)", p.SpeciesID, sp.ID, sp.SciName)
	}
	e := p.Eligibility
	window := c.WindowDays
	if window <= 0 {
		window = collectDefaultWindowDays
	}

	// GBIF и Википедия друг от друга не зависят: наблюдения собираются
	// параллельно со статьёй, и сборка стоит самого медленного источника,
	// а не суммы.
	var (
		wg      sync.WaitGroup
		obs     Observations
		gbifMat *Material
	)
	obs = Observations{GBIFKey: e.GBIFKey, WindowDays: window}
	if e.GBIFKey != 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			obs, gbifMat = c.collectGBIF(ctx, sp, e.GBIFKey, window)
		}()
	}

	wiki, art, err := c.collectWikipedia(ctx, e)
	wg.Wait()
	if err != nil {
		return Dossier{}, fmt.Errorf("сборка досье %s (%d): %w", sp.SciName, sp.ID, err)
	}

	nameRu := ""
	if art.lang == "ru" {
		nameRu = collectStripQualifier(art.title)
	}
	// У малоизвестных видов русская статья часто названа латынью
	// («Lepilemur tymerlachsonorum»): это не русское название. Тогда, как и
	// для английской статьи, имя берётся из народных названий GBIF.
	if !collectHasCyrillic(nameRu) {
		nameRu = ""
	}
	if nameRu == "" && e.GBIFKey != 0 {
		// Его сбой не мешает выпуску: заголовок тогда будет латинским.
		names, err := tools.NewGBIF(c.gbifBase(), c.fetcher()).Vernacular(ctx, e.GBIFKey, "rus")
		if err == nil && len(names) > 0 {
			nameRu = collectCapitalize(names[0])
		}
	}

	mats := []Material{collectMDDMaterial(sp)}
	mats = append(mats, wiki...)
	if gbifMat != nil {
		mats = append(mats, *gbifMat)
	}
	for i := range mats {
		mats[i].ID = "S" + strconv.Itoa(i+1)
	}

	return Dossier{
		Pick:         p,
		Species:      sp,
		NameRu:       nameRu,
		Materials:    mats,
		Observations: obs,
		CollectedAt:  c.now(),
		Took:         time.Since(start),
	}, nil
}

func (c *WebCollector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *WebCollector) fetcher() *tools.Fetcher {
	if c.Fetcher == nil {
		return webSharedFetcher
	}
	return c.Fetcher
}

func (c *WebCollector) gbifBase() string { return webBase(c.GBIFBase, tools.DefaultGBIFBase) }

// --- MDD ---

// collectIUCNRu — расшифровка кодов МСОП: модель пишет по-русски, и
// «NT» без расшифровки она переведёт как попало.
var collectIUCNRu = map[string]string{
	"LC": "вызывающие наименьшие опасения",
	"NT": "близкие к уязвимому положению",
	"VU": "уязвимые",
	"EN": "вымирающие",
	"CR": "находящиеся на грани исчезновения",
	"EW": "исчезнувшие в дикой природе",
	"EX": "исчезнувшие",
	"DD": "недостаточно данных",
	"NE": "не оценивался",
}

// collectMDDMaterial — карточка вида из MDD. Метки полей по-русски, значения
// — как в MDD: модель должна видеть, что «Pallas, 1776» — автор описания, а
// не просто строку.
func collectMDDMaterial(sp mdd.Species) Material {
	var b strings.Builder
	line := func(label, value string) {
		if value = strings.TrimSpace(value); value != "" {
			fmt.Fprintf(&b, "%s: %s\n", label, value)
		}
	}
	list := func(label string, vs []string) { line(label, strings.Join(vs, ", ")) }

	line("Латинское название", sp.SciName)
	author := sp.Authority
	if author == "" && sp.Year > 0 {
		author = strconv.Itoa(sp.Year)
	}
	line("Автор и год описания", author)
	var tax []string
	for _, t := range []struct{ label, v string }{
		{"отряд", sp.Order}, {"семейство", sp.Family}, {"подсемейство", sp.Subfamily}, {"род", sp.Genus},
	} {
		if t.v != "" {
			tax = append(tax, t.label+" "+t.v)
		}
	}
	line("Систематика", strings.Join(tax, ", "))
	line("Английское название", sp.CommonName)
	list("Другие английские названия", sp.OtherCommonNames)
	if sp.IUCN != "" {
		status := sp.IUCN
		if ru, ok := collectIUCNRu[sp.IUCN]; ok {
			status += " (" + ru + ")"
		}
		line("Статус МСОП", status)
	}
	if sp.Extinct {
		line("Вымерший вид", "да")
	} else {
		line("Вымерший вид", "нет")
	}
	if sp.Domestic {
		line("Домашний вид", "да")
	}
	list("Страны обитания", sp.Countries)
	list("Страны обитания под вопросом", sp.CountriesUncertain)
	list("Континенты", sp.Continents)
	list("Биогеографические области", sp.Realms)
	line("Типовое местонахождение", tools.Truncate(sp.TypeLocality, collectNotesRunes))
	line("Заметки о распространении", tools.Truncate(sp.DistributionNotes, collectNotesRunes))
	line("Заметки о систематике", tools.Truncate(sp.TaxonomyNotes, collectNotesRunes))

	return Material{
		Kind:  KindMDD,
		Title: "MDD: " + sp.SciName,
		URL:   sp.URL(),
		Text:  strings.TrimSpace(b.String()),
		Tool:  "mdd",
	}
}

// --- Википедия ---

type collectArticle struct {
	lang, title string
}

// collectWikipedia читает статью и режет её на материалы. Язык и заголовок
// — из проверки пригодности. Если русская статья не прочиталась (удалена,
// переименована без редиректа, сбой), пробуется английская: выпуск по ней
// лучше, чем никакого. Не получилась ни одна — ошибка.
func (c *WebCollector) collectWikipedia(ctx context.Context, e Eligibility) ([]Material, collectArticle, error) {
	type try struct{ lang, title, base string }
	var tries []try
	switch e.WikiLang {
	case "ru":
		tries = append(tries, try{"ru", e.WikiTitle, webBase(c.RuWikiBase, webRuWikiBase)})
	case "en":
		title := e.EnTitle
		if title == "" {
			title = e.WikiTitle
		}
		tries = append(tries, try{"en", title, webBase(c.EnWikiBase, webEnWikiBase)})
	}
	if e.WikiLang == "ru" && e.EnTitle != "" {
		tries = append(tries, try{"en", e.EnTitle, webBase(c.EnWikiBase, webEnWikiBase)})
	}

	var errs []string
	for _, t := range tries {
		if strings.TrimSpace(t.title) == "" {
			continue
		}
		art, err := tools.NewWikipedia(t.base, c.fetcher()).Article(ctx, t.title)
		if err != nil {
			if ctx.Err() != nil {
				return nil, collectArticle{}, ctx.Err()
			}
			errs = append(errs, fmt.Sprintf("%s «%s»: %v", t.lang, t.title, err))
			continue
		}
		mats := collectArticleMaterials(t.lang, art, c.maxWikiRunes())
		if len(mats) == 0 {
			errs = append(errs, fmt.Sprintf("%s «%s»: статья пуста", t.lang, t.title))
			continue
		}
		return mats, collectArticle{lang: t.lang, title: art.Title}, nil
	}
	if len(errs) == 0 {
		return nil, collectArticle{}, fmt.Errorf("статьи в Википедии нет (проверка не нашла ни ru, ни en)")
	}
	return nil, collectArticle{}, fmt.Errorf("статья в Википедии не прочиталась: %s", strings.Join(errs, "; "))
}

func (c *WebCollector) maxWikiRunes() int {
	if c.MaxWikiRunes > 0 {
		return c.MaxWikiRunes
	}
	return collectDefaultWikiRunes
}

// collectTopic — тема раздела и ключевые слова заголовка (подстроки в
// нижнем регистре). Порядок тем — приоритет отбора: сначала то, из чего
// получаются «интересные факты» о самом животном, а распространение и
// охрана частично есть и в материалах MDD и GBIF.
type collectTopic struct {
	key   string
	words []string
}

var collectTopics = []collectTopic{
	{"appearance", []string{"внешний вид", "внешност", "описание", "морфолог", "анатоми", "строение",
		"description", "appearance", "characteristics", "morpholog", "anatomy"}},
	{"lifestyle", []string{"образ жизни", "поведение", "биология", "экология", "социальн",
		"behaviour", "behavior", "ecology", "biology", "social", "group organi", "life history"}},
	{"diet", []string{"питание", "пища", "рацион", "охота", "diet", "feeding", "food", "hunting", "foraging"}},
	{"breeding", []string{"размножение", "воспроизводство", "жизненный цикл", "reproduction", "breeding", "life cycle", "mating"}},
	{"habitat", []string{"распространение", "ареал", "среда обитания", "места обитания",
		"distribution", "habitat", "range"}},
	{"status", []string{"охран", "статус", "численность", "угроз", "conservation", "status", "threat", "population"}},
	{"extra", []string{"интересные факты", "в культуре", "культур", "человек", "этимологи", "название",
		"interesting", "trivia", "culture", "cultural", "relationship with humans", "interactions with humans",
		"human", "etymology", "in captivity", "в неволе"}},
}

// collectSkipPrefix — служебные разделы: фактов о животном в них нет, а
// место в бюджете они заняли бы. Сравнивается начало заголовка, а не
// подстрока: «Sources» — служебный раздел, а «Food sources» — питание.
var collectSkipPrefix = []string{
	"примечани", "литература", "ссылки", "см. также", "источники", "комментарии", "сноски", "галерея",
	"see also", "references", "external links", "notes", "further reading", "bibliography",
	"citations", "sources", "gallery",
}

// collectSkipWords — списки подвидов и синонимов где угодно в заголовке:
// перечень латинских названий модели фактов не даёт.
var collectSkipWords = []string{"подвид", "синоним", "subspecies", "synonym"}

func collectSkipped(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	for _, p := range collectSkipPrefix {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return collectHas(t, collectSkipWords)
}

func init() {
	// Кандидаты разделов карточки v17 — те же темы, что и у выпуска:
	// названия, накопленные на русских статьях, не должны разойтись с этим
	// списком.
	idx := map[string]int{}
	for i, t := range collectTopics {
		idx[t.key] = i
	}
	for _, t := range card.Topics {
		i, ok := idx[t.Key]
		if !ok {
			continue
		}
		for _, cand := range t.Candidates {
			collectTopics[i].words = append(collectTopics[i].words, strings.ToLower(cand))
		}
	}
}

func collectHas(title string, words []string) bool {
	t := strings.ToLower(title)
	for _, w := range words {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// collectSectionTopic — индекс темы раздела в collectTopics; -1 — не тема.
func collectSectionTopic(title string) int {
	for i, t := range collectTopics {
		if collectHas(title, t.words) {
			return i
		}
	}
	return -1
}

// collectPickSections отбирает до collectMaxSections разделов.
//
// Сначала — по одному разделу на тему в порядке приоритета тем. Среди
// разделов одной темы предпочитается более глубокий: у крупной статьи
// «Биология и экология» — контейнер на десятки тысяч знаков, а «Поведение»
// и «Охота и питание» — его подразделы; взяв подразделы, в бюджет влезет
// больше разных тем. Раздел, вложенный в уже взятый (или содержащий его),
// не берётся — иначе один текст попал бы в досье дважды. Оставшиеся места
// добиваются самыми длинными нетематическими разделами: у короткой статьи
// о малоизвестном грызуне разделы называются как попало.
//
// Возвращает индексы в порядке статьи: так материалы читаются связно.
func collectPickSections(secs []tools.Section) []int {
	parent := make([]int, len(secs)) // индекс раздела второго уровня, -1 — сам второго уровня
	skip := make([]bool, len(secs))
	cur := -1
	for i, s := range secs {
		if s.Level <= 2 {
			cur = i
			parent[i] = -1
		} else {
			parent[i] = cur
		}
		skip[i] = collectSkipped(s.Title) ||
			(parent[i] >= 0 && skip[parent[i]]) ||
			utf8.RuneCountInString(s.Text) < collectMinSectionRunes
	}
	// Уровни глубже третьего Section тоже отдаёт, но родителем считается
	// раздел второго уровня: перекрытие проверяется по нему.
	overlaps := func(i, j int) bool {
		return i == j || parent[i] == j || parent[j] == i
	}

	var chosen []int
	free := func(i int) bool {
		if skip[i] {
			return false
		}
		for _, j := range chosen {
			if overlaps(i, j) {
				return false
			}
		}
		return true
	}

	for ti := range collectTopics {
		if len(chosen) == collectMaxSections {
			break
		}
		best := -1
		for i, s := range secs {
			if !free(i) || collectSectionTopic(s.Title) != ti {
				continue
			}
			if best < 0 || s.Level > secs[best].Level {
				best = i
			}
		}
		if best >= 0 {
			chosen = append(chosen, best)
		}
	}

	if len(chosen) < collectMaxSections {
		rest := make([]int, 0, len(secs))
		for i := range secs {
			if free(i) {
				rest = append(rest, i)
			}
		}
		sort.SliceStable(rest, func(a, b int) bool {
			return utf8.RuneCountInString(secs[rest[a]].Text) > utf8.RuneCountInString(secs[rest[b]].Text)
		})
		for _, i := range rest {
			if len(chosen) == collectMaxSections {
				break
			}
			if free(i) {
				chosen = append(chosen, i)
			}
		}
	}
	sort.Ints(chosen)
	return chosen
}

// collectBudget делит бюджет между текстами «наливом»: короткий текст
// берёт сколько ему нужно, а недобранное делится между длинными. Ровная
// нарезка обрезала бы длинный раздел, пока короткий оставляет место
// пустым.
func collectBudget(lengths []int, total int) []int {
	out := make([]int, len(lengths))
	order := make([]int, len(lengths))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return lengths[order[a]] < lengths[order[b]] })
	left := total
	for k, i := range order {
		share := left / (len(order) - k)
		if lengths[i] < share {
			share = lengths[i]
		}
		out[i] = share
		left -= share
	}
	return out
}

// collectArticleMaterials — вступление и отобранные разделы статьи.
func collectArticleMaterials(lang string, art *tools.Article, budget int) []Material {
	prefix := fmt.Sprintf("Википедия (%s): %s", lang, art.Title)
	tool := "wikipedia_article (" + lang + ")"
	var mats []Material

	intro := strings.TrimSpace(art.Intro)
	introMax := budget / collectIntroShare
	if n := utf8.RuneCountInString(intro); n < introMax {
		introMax = n
	}
	if intro != "" {
		mats = append(mats, Material{
			Kind: KindWikipedia, Title: prefix, URL: art.URL,
			Text: tools.Truncate(intro, introMax), Tool: tool,
		})
	}

	idx := collectPickSections(art.Sections)
	lengths := make([]int, len(idx))
	for k, i := range idx {
		lengths[k] = utf8.RuneCountInString(art.Sections[i].Text)
	}
	shares := collectBudget(lengths, budget-introMax)
	for k, i := range idx {
		s := art.Sections[i]
		if shares[k] <= 0 {
			continue
		}
		mats = append(mats, Material{
			Kind:  KindWikipedia,
			Title: prefix + " — " + s.Title,
			URL:   art.URL + "#" + url.PathEscape(strings.ReplaceAll(s.Title, " ", "_")),
			Text:  tools.Truncate(s.Text, shares[k]),
			Tool:  tool,
		})
	}
	return mats
}

// collectStripQualifier — «Лев (животное)» → «Лев»: уточнение в скобках
// нужно Википедии для различения статей, а не читателю выпуска.
func collectStripQualifier(title string) string {
	title = strings.TrimSpace(title)
	if i := strings.LastIndex(title, " ("); i > 0 && strings.HasSuffix(title, ")") {
		return strings.TrimSpace(title[:i])
	}
	return title
}

func collectCapitalize(s string) string {
	s = strings.TrimSpace(s)
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// --- GBIF ---

type collectFacetResp struct {
	Count  int `json:"count"`
	Facets []struct {
		Field  string `json:"field"`
		Counts []struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"counts"`
	} `json:"facets"`
}

// collectFacet — число наблюдений и разбивка по странам (фасет country,
// до collectFacetLimit стран, GBIF отдаёт их по убыванию). extra —
// дополнительные параметры запроса (окно дат).
func (c *WebCollector) collectFacet(ctx context.Context, key int, extra url.Values, sp mdd.Species) (int, []CountryCount, error) {
	q := url.Values{}
	q.Set("taxonKey", strconv.Itoa(key))
	q.Set("limit", "0")
	q.Set("facet", "country")
	q.Set("facetLimit", strconv.Itoa(collectFacetLimit))
	for k, vs := range extra {
		q[k] = vs
	}
	var resp collectFacetResp
	if err := c.fetcher().GetJSON(ctx, c.gbifBase()+"/occurrence/search?"+q.Encode(), &resp); err != nil {
		return 0, nil, err
	}
	var out []CountryCount
	for _, f := range resp.Facets {
		if !strings.EqualFold(f.Field, "country") {
			continue
		}
		for _, fc := range f.Counts {
			code := strings.ToUpper(strings.TrimSpace(fc.Name))
			name, rng := countryClassify(code, sp)
			out = append(out, CountryCount{Code: code, Name: name, Count: fc.Count, Range: rng})
		}
	}
	// Порядок фасета GBIF обещает, но таблица по убыванию нужна и при
	// равных числах устойчивой — по коду.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Code < out[j].Code
	})
	return resp.Count, out, nil
}

// collectGBIF — наблюдения за всё время и за окно. Сбой общего запроса —
// досье без наблюдений (материала нет); сбой запроса за окно — Recent=0 и
// пометка в материале, чтобы модель не написала «за месяц наблюдений не
// было».
func (c *WebCollector) collectGBIF(ctx context.Context, sp mdd.Species, key, window int) (Observations, *Material) {
	obs := Observations{GBIFKey: key, WindowDays: window}
	total, byCountry, err := c.collectFacet(ctx, key, nil, sp)
	if err != nil {
		return obs, nil
	}
	obs.Total = total
	obs.ByCountry = byCountry
	for _, cc := range byCountry {
		if cc.Range == RangeOut {
			obs.OutOfRange = append(obs.OutOfRange, cc)
		}
	}

	to := c.now().UTC()
	from := to.AddDate(0, 0, -window)
	period := from.Format("2006-01-02") + "," + to.Format("2006-01-02")
	recent, recentBy, rerr := c.collectFacet(ctx, key, url.Values{"eventDate": {period}}, sp)
	if rerr == nil {
		obs.Recent = recent
		obs.RecentByCountry = recentBy
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Наблюдения вида в GBIF (ключ таксона %d). Записей за всё время: %d.\n", key, total)
	if rerr != nil {
		fmt.Fprintf(&b, "Записей за последние %d дней: неизвестно — запрос к GBIF не удался.\n", window)
	} else {
		fmt.Fprintf(&b, "Записей за последние %d дней (%s — %s, по дате наблюдения): %d.\n",
			window, from.Format("2006-01-02"), to.Format("2006-01-02"), recent)
	}
	if len(byCountry) > 0 {
		fmt.Fprintf(&b, "\nСтраны с наибольшим числом записей за всё время (сверка с ареалом по MDD):\n")
		collectWriteCountries(&b, byCountry)
	}
	if rerr == nil && len(recentBy) > 0 {
		fmt.Fprintf(&b, "\nСтраны за последние %d дней:\n", window)
		collectWriteCountries(&b, recentBy)
	}
	if len(obs.OutOfRange) > 0 {
		// Полный список, а не только попавшие в верхушку: одиночная запись
		// из чужой страны и есть то, что стоит пометить.
		out := make([]string, len(obs.OutOfRange))
		for i, cc := range obs.OutOfRange {
			out[i] = fmt.Sprintf("%s — %d", countryLabel(cc), cc.Count)
		}
		fmt.Fprintf(&b, "\nЗаписи из стран вне ареала MDD: %s. Обычно это зоопарки, интродукция или ошибки определения, а не дикие популяции.\n",
			strings.Join(out, "; "))
	}
	b.WriteString("Число записей — это оцифрованные наблюдения и музейные образцы, а не численность вида.")

	return obs, &Material{
		Kind:  KindGBIF,
		Title: "GBIF: наблюдения " + sp.SciName,
		URL:   tools.TaxonURL(key),
		Text:  b.String(),
		Tool:  "gbif occurrence facet=country",
	}
}

var collectRangeRu = map[string]string{
	RangeIn:        "в ареале MDD",
	RangeUncertain: "в ареале MDD под вопросом",
	RangeOut:       "вне ареала MDD",
	RangeUnknown:   "не сопоставлена с MDD",
}

func collectWriteCountries(b *strings.Builder, list []CountryCount) {
	for i, cc := range list {
		if i == collectTopCountries {
			fmt.Fprintf(b, "- и ещё %d стран(ы) с меньшим числом записей\n", len(list)-i)
			break
		}
		fmt.Fprintf(b, "- %s — %d (%s)\n", countryLabel(cc), cc.Count, collectRangeRu[cc.Range])
	}
}

// collectHasCyrillic — есть ли в строке кириллица.
func collectHasCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}
