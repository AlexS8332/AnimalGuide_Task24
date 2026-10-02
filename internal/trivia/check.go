package trivia

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

const (
	webEnWikiBase = "https://en.wikipedia.org"
	webRuWikiBase = "https://ru.wikipedia.org"
)

// WebChecker — Checker на живых источниках: английская Википедия (статья и
// межъязыковая ссылка на русскую) и GBIF (ключ таксона и число наблюдений).
//
// Статья ищется через англовики, а не через русскую: у англовики почти для
// всех млекопитающих есть редирект с латинского названия, а у русской —
// нет, зато из английской статьи межъязыковая ссылка ведёт на русскую.
// Так одним запросом находятся обе.
type WebChecker struct {
	Fetcher    *tools.Fetcher
	EnWikiBase string // пусто — https://en.wikipedia.org
	RuWikiBase string // пусто — https://ru.wikipedia.org; только для WikiURL, запросов туда нет
	GBIFBase   string // пусто — tools.DefaultGBIFBase
	Now        func() time.Time
}

var _ Checker = (*WebChecker)(nil)

// NewWebChecker — проверка с боевыми адресами источников.
func NewWebChecker(f *tools.Fetcher) *WebChecker {
	return &WebChecker{Fetcher: f}
}

// Check проверяет пригодность вида.
//
// Порядок: домашний (без сети) → GBIF → статья. Замер TestLiveCheck на 152
// случайных видах настоящего MDD (порог 50): GBIF отсекает 53 (35 %: 39
// «мало наблюдений», 14 «не знает названия»), статьи нет у 16 (10 %), и
// из этих 16 у 15 отказывает и GBIF — это недавно описанные или выделенные
// виды, о которых почти ничего нет. Значит, статья-первой почти никого не отсекла бы сама, а
// GBIF-первым отсекается треть кандидатов. Отказ GBIF ещё и дешёвый: вида
// нет — один запрос match (~65 мс), тогда как статья при промахе по
// латинскому названию — два запроса к вики (~250 мс). Проходящий вид
// платит одинаково при любом порядке.
func (c *WebChecker) Check(ctx context.Context, sp mdd.Species, minOccurrences int) (Eligibility, error) {
	e := Eligibility{SpeciesID: sp.ID, SciName: sp.SciName}
	if sp.Domestic {
		// Домашний вид не отбирается независимо от статей и наблюдений:
		// выпуск про кошку или корову — не то, ради чего демон.
		return c.webDone(e, ReasonDomestic), nil
	}

	for _, step := range []func(context.Context, mdd.Species, int, *Eligibility) (string, error){
		c.webCheckGBIF,
		c.webCheckArticle,
	} {
		reason, err := step(ctx, sp, minOccurrences, &e)
		if err != nil {
			// Сбой сети — не повод отвергать вид надолго: ReasonCheckFailed
			// по контракту не кэшируется, вид будет проверен в другой раз.
			failed := c.webDone(Eligibility{SpeciesID: sp.ID, SciName: sp.SciName}, ReasonCheckFailed)
			return failed, fmt.Errorf("проверка вида %s (%d): %w", sp.SciName, sp.ID, err)
		}
		if reason != ReasonOK {
			return c.webDone(e, reason), nil
		}
	}
	return c.webDone(e, ReasonOK), nil
}

func (c *WebChecker) webDone(e Eligibility, reason string) Eligibility {
	e.Reason = reason
	e.OK = reason == ReasonOK
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	e.CheckedAt = now()
	return e
}

// webSharedFetcher — на случай WebChecker{} без Fetcher: подставлять свой в
// поле нельзя, Check может идти из нескольких горутин.
var webSharedFetcher = tools.NewFetcher()

func (c *WebChecker) webFetcher() *tools.Fetcher {
	if c.Fetcher == nil {
		return webSharedFetcher
	}
	return c.Fetcher
}

func webBase(base, def string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return def
	}
	return base
}

// webCheckArticle ищет статью: сначала по латинскому названию, потом по
// английскому. Латинское надёжнее: английское бывает общим для нескольких
// животных («Mountain beaver»), и тогда найдётся не та статья или
// неоднозначность.
func (c *WebChecker) webCheckArticle(ctx context.Context, sp mdd.Species, _ int, e *Eligibility) (string, error) {
	for _, title := range []string{sp.SciName, sp.CommonName} {
		title = strings.TrimSpace(title)
		if title == "" {
			continue
		}
		page, found, err := c.webEnPage(ctx, title)
		if err != nil {
			return "", err
		}
		if !found {
			continue
		}
		e.EnTitle = page.Title
		if page.RuTitle != "" {
			e.WikiLang = "ru"
			e.WikiTitle = page.RuTitle
			e.WikiURL = webBase(c.RuWikiBase, webRuWikiBase) + "/wiki/" + webPathTitle(page.RuTitle)
		} else {
			e.WikiLang = "en"
			e.WikiTitle = page.Title
			e.WikiURL = page.URL
		}
		return ReasonOK, nil
	}
	return ReasonNoArticle, nil
}

type webPage struct {
	Title   string
	URL     string
	RuTitle string
}

// webEnPage — страница англовики по заголовку с раскрытием редиректов.
// Страница-неоднозначность статьёй о виде не считается: на живом API такие
// попадаются по английским названиям (у одного «Pygmy …» бывает несколько
// носителей), а по такой странице факты о виде не собрать.
func (c *WebChecker) webEnPage(ctx context.Context, title string) (webPage, bool, error) {
	base := webBase(c.EnWikiBase, webEnWikiBase)
	q := url.Values{}
	q.Set("action", "query")
	q.Set("format", "json")
	q.Set("formatversion", "2")
	q.Set("titles", title)
	q.Set("redirects", "1")
	q.Set("prop", "langlinks|info|pageprops")
	q.Set("lllang", "ru")
	q.Set("inprop", "url")
	q.Set("ppprop", "disambiguation")

	var resp struct {
		Query struct {
			Pages []struct {
				Title     string `json:"title"`
				Missing   bool   `json:"missing"`
				Invalid   bool   `json:"invalid"`
				FullURL   string `json:"fullurl"`
				Langlinks []struct {
					Lang  string `json:"lang"`
					Title string `json:"title"`
				} `json:"langlinks"`
				PageProps map[string]any `json:"pageprops"`
			} `json:"pages"`
		} `json:"query"`
	}
	if err := c.webFetcher().GetJSON(ctx, base+"/w/api.php?"+q.Encode(), &resp); err != nil {
		return webPage{}, false, fmt.Errorf("статья в Википедии «%s»: %w", title, err)
	}
	if len(resp.Query.Pages) == 0 {
		return webPage{}, false, nil
	}
	p := resp.Query.Pages[0]
	if p.Missing || p.Invalid || p.Title == "" {
		return webPage{}, false, nil
	}
	if _, ok := p.PageProps["disambiguation"]; ok {
		return webPage{}, false, nil
	}
	page := webPage{Title: p.Title, URL: p.FullURL}
	if page.URL == "" {
		page.URL = base + "/wiki/" + webPathTitle(p.Title)
	}
	for _, l := range p.Langlinks {
		if l.Lang == "ru" && strings.TrimSpace(l.Title) != "" {
			page.RuTitle = l.Title
			break
		}
	}
	return page, true, nil
}

func webPathTitle(title string) string {
	return url.PathEscape(strings.ReplaceAll(title, " ", "_"))
}

// webCheckGBIF — ключ таксона и число наблюдений.
//
// Засчитывается только точное совпадение на уровне вида. Нечёткое (FUZZY)
// GBIF даёт, когда такого названия у него нет, и подсовывает похожее —
// нередко соседний вид того же рода, а чужие наблюдения нам не нужны.
// HIGHERRANK — совпал только род: вид GBIF не знает.
func (c *WebChecker) webCheckGBIF(ctx context.Context, sp mdd.Species, minOccurrences int, e *Eligibility) (string, error) {
	base := webBase(c.GBIFBase, tools.DefaultGBIFBase)
	q := url.Values{}
	q.Set("name", sp.SciName)
	q.Set("kingdom", "Animalia")
	q.Set("class", "Mammalia")
	q.Set("rank", "SPECIES")
	q.Set("verbose", "false")

	var m struct {
		UsageKey         int    `json:"usageKey"`
		AcceptedUsageKey int    `json:"acceptedUsageKey"`
		Rank             string `json:"rank"`
		MatchType        string `json:"matchType"`
	}
	if err := c.webFetcher().GetJSON(ctx, base+"/species/match?"+q.Encode(), &m); err != nil {
		return "", fmt.Errorf("сверка с GBIF: %w", err)
	}
	if m.MatchType != "EXACT" || m.UsageKey == 0 || !webSpeciesRank(m.Rank) {
		return ReasonNoGBIF, nil
	}
	// Для синонима GBIF ведёт наблюдения под принятым названием: MDD и GBIF
	// нередко расходятся в роде (Otocolobus manul у MDD — Felis manul у
	// GBIF), и по ключу синонима наблюдений почти нет.
	key := m.UsageKey
	if m.AcceptedUsageKey != 0 {
		key = m.AcceptedUsageKey
	}
	e.GBIFKey = key

	var occ struct {
		Count int `json:"count"`
	}
	u := fmt.Sprintf("%s/occurrence/search?taxonKey=%d&limit=0", base, key)
	if err := c.webFetcher().GetJSON(ctx, u, &occ); err != nil {
		return "", fmt.Errorf("наблюдения в GBIF: %w", err)
	}
	e.Occurrences = occ.Count
	if occ.Count < minOccurrences {
		return ReasonFewRecords, nil
	}
	return ReasonOK, nil
}

// webSpeciesRank — вид или ниже. Подвид допустим: у синонима принятым
// названием GBIF бывает подвид (вид MDD, который GBIF считает подвидом), и
// его наблюдения — как раз наблюдения этого вида.
func webSpeciesRank(rank string) bool {
	switch rank {
	case "SPECIES", "SUBSPECIES", "VARIETY", "FORM":
		return true
	}
	return false
}
