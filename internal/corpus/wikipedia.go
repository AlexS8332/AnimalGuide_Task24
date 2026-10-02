package corpus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// DefaultUserAgent — User-Agent снимка. Правила Wikimedia требуют от
// автоматических клиентов своего агента с понятным назначением; общий
// агент Go API режет ограничением частоты.
const DefaultUserAgent = "AnimalGuide-kb/21 (учебный проект; https://github.com/AlexS8332)"

// LicenseWikipedia и LicenseMDD — лицензии текстов в Doc.License.
const (
	LicenseWikipedia = "CC BY-SA 4.0"
	LicenseMDD       = "CC BY 4.0"
)

// fetchAttempts — сколько раз пробовать запрос при 429 и 5xx: Википедия
// под нагрузкой отвечает ими временно, и снимок из 30 статей не должен
// падать из-за одной такой секунды.
const fetchAttempts = 3

// fetchBackoff — пауза перед повтором; тесты её обнуляют.
var fetchBackoff = 2 * time.Second

// now — время снимка; тесты подменяют.
var now = time.Now

// maxResponse — предел ответа API: самая длинная статья корпуса — около
// 200 КБ текста, 16 МБ — заведомо больше любой.
const maxResponse = 16 << 20

// serviceSections — служебные разделы: в них нет текста о животном, только
// ссылки и библиография. Сравниваются после normalizeTitle. Отбрасываются
// вместе с подразделами («Примечания › Комментарии», «› Источники»).
var serviceSections = map[string]bool{
	"примечания":  true,
	"примечание":  true,
	"комментарии": true,
	"сноски":      true,
	"источники":   true,
	"литература":  true,
	"дополнительная литература": true,
	"рекомендуемая литература":  true,
	"библиография":              true,
	"ссылки":                    true,
	"внешние ссылки":            true,
	"см. также":                 true,
	"см также":                  true,
	"галерея":                   true,
	"фотогалерея":               true,
	"изображения":               true,
	"видео":                     true,
}

// headingRE — заголовок раздела в выдаче TextExtracts с
// exsectionformat=wiki: отдельная строка «== Заголовок ==», число знаков
// «=» — уровень.
var headingRE = regexp.MustCompile(`^(={2,6})\s*(.*?)\s*(={2,6})$`)

// mathRE — формула. TextExtracts разворачивает <math> в MathML-дерево:
// десяток строк с отступами по символу формулы, а в конце — LaTeX в
// «{\displaystyle …}». Блок целиком заменяется упрощённым LaTeX (см.
// latexToText): иначе в тексте «Зубная формула — I 3 3 C 1 1 …» по строке
// на цифру. Блок встроен в абзац, поэтому переводы строк вокруг него
// съедаются вместе с ним.
var mathRE = regexp.MustCompile(`\n(?:[ \t]*\n|[ \t]+[^\n]*\n)*?[ \t]*\{\\(?:displaystyle|textstyle)\s*([^\n]*)\}[ \t]*\n(?:[ \t]*\n)?`)

// wikiResponse — нужная часть ответа action=query, formatversion=2.
type wikiResponse struct {
	Error *struct {
		Code string `json:"code"`
		Info string `json:"info"`
	} `json:"error"`
	Query struct {
		Redirects []struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"redirects"`
		Pages []struct {
			PageID    int64  `json:"pageid"`
			Title     string `json:"title"`
			Missing   bool   `json:"missing"`
			Invalid   bool   `json:"invalid"`
			Extract   string `json:"extract"`
			Revisions []struct {
				RevID     int64  `json:"revid"`
				Timestamp string `json:"timestamp"`
			} `json:"revisions"`
			LastRevID int64 `json:"lastrevid"`
		} `json:"pages"`
	} `json:"query"`
}

// wikipedia — реализация Fetcher.Wikipedia: один запрос — и текст, и
// ревизия. Отдельными запросами текст и revid могли бы оказаться от разных
// правок, если статью отредактировали между ними.
func (f Fetcher) wikipedia(ctx context.Context, id, title string, species *Species) (Doc, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Doc{}, fmt.Errorf("%s: пустой заголовок", id)
	}
	base := strings.TrimRight(strings.TrimSpace(f.Base), "/")
	if base == "" {
		base = tools.DefaultWikipediaBase
	}

	q := url.Values{}
	q.Set("action", "query")
	q.Set("prop", "extracts|revisions|info")
	q.Set("explaintext", "1")
	q.Set("exsectionformat", "wiki")
	q.Set("redirects", "1")
	q.Set("rvprop", "ids|timestamp")
	q.Set("format", "json")
	q.Set("formatversion", "2")
	q.Set("titles", title)

	body, err := f.get(ctx, base+"/w/api.php?"+q.Encode())
	if err != nil {
		return Doc{}, fmt.Errorf("%s («%s»): %w", id, title, err)
	}
	var resp wikiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Doc{}, fmt.Errorf("%s («%s»): ответ не JSON: %w", id, title, err)
	}
	if resp.Error != nil {
		return Doc{}, fmt.Errorf("%s («%s»): API: %s: %s", id, title, resp.Error.Code, resp.Error.Info)
	}
	if len(resp.Query.Pages) != 1 {
		return Doc{}, fmt.Errorf("%s («%s»): API вернул %d страниц", id, title, len(resp.Query.Pages))
	}
	p := resp.Query.Pages[0]
	switch {
	case p.Missing:
		return Doc{}, fmt.Errorf("%s: статьи «%s» нет", id, title)
	case p.Invalid:
		return Doc{}, fmt.Errorf("%s: недопустимый заголовок «%s»", id, title)
	}
	var revID int64
	if len(p.Revisions) > 0 {
		revID = p.Revisions[0].RevID
	}
	if revID == 0 {
		revID = p.LastRevID
	}
	if revID == 0 {
		// Без ревизии снимок не воспроизвести и не атрибутировать.
		return Doc{}, fmt.Errorf("%s («%s»): в ответе нет revid", id, title)
	}

	intro, sections := parseExtract(p.Extract)
	if intro == "" && len(sections) == 0 {
		return Doc{}, fmt.Errorf("%s («%s»): пустой текст статьи", id, title)
	}
	return Doc{
		Schema:   Schema,
		ID:       id,
		Source:   SourceWikipedia,
		Title:    p.Title,
		URL:      base + "/wiki/" + url.PathEscape(strings.ReplaceAll(p.Title, " ", "_")),
		RevID:    revID,
		OldURL:   base + "/w/index.php?oldid=" + strconv.FormatInt(revID, 10),
		Fetched:  now().UTC().Format(time.RFC3339),
		License:  LicenseWikipedia,
		Species:  species,
		Intro:    intro,
		Sections: sections,
	}, nil
}

// get — GET с User-Agent и повтором при 429/5xx.
func (f Fetcher) get(ctx context.Context, u string) ([]byte, error) {
	client := f.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	ua := f.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	var lastErr error
	for attempt := 0; attempt < fetchAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(fetchBackoff * time.Duration(attempt)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		case resp.StatusCode != http.StatusOK:
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		case err != nil:
			return nil, err
		case len(body) > maxResponse:
			return nil, errors.New("ответ API больше 16 МБ")
		}
		return body, nil
	}
	return nil, lastErr
}

// parseExtract разбирает простой текст TextExtracts на вступление и
// разделы с СОБСТВЕННЫМИ телами. В отличие от tools.splitSections, текст
// раздела кончается на первом же следующем заголовке любого уровня, так
// что подраздел в тексте родителя не повторяется.
//
// Служебные разделы отбрасываются вместе со всеми своими подразделами;
// разделы без текста и без оставшихся подразделов — тоже (от них в чанках
// был бы только заголовок).
func parseExtract(extract string) (string, []Section) {
	text := cleanText(extract)

	var (
		intro    []string
		sections []Section
		cur      *Section
		body     []string
		path     []Section // стек заголовков: Title и Level
		skipFrom = 0       // >0 — пропускаем всё глубже этого уровня
	)
	flush := func() {
		if cur != nil {
			cur.Text = joinParagraphs(body)
			sections = append(sections, *cur)
		}
		cur, body = nil, nil
	}

	for _, line := range strings.Split(text, "\n") {
		m := headingRE.FindStringSubmatch(line)
		if m == nil || len(m[1]) != len(m[3]) || m[2] == "" {
			switch {
			case skipFrom > 0:
			case cur != nil:
				body = append(body, line)
			default:
				intro = append(intro, line)
			}
			continue
		}
		flush()
		level := len(m[1])
		title := m[2]
		if skipFrom > 0 && level > skipFrom {
			continue // подраздел служебного раздела
		}
		skipFrom = 0
		for len(path) > 0 && path[len(path)-1].Level >= level {
			path = path[:len(path)-1]
		}
		if serviceSections[normalizeTitle(title)] {
			skipFrom = level
			continue
		}
		path = append(path, Section{Title: title, Level: level})
		p := make([]string, len(path))
		for i, s := range path {
			p[i] = s.Title
		}
		cur = &Section{Path: p, Title: title, Level: level}
	}
	flush()
	return joinParagraphs(intro), pruneEmpty(sections)
}

// pruneEmpty убирает разделы без текста, у которых не осталось
// подразделов с текстом. Идём с конца: «следующий оставленный» к этому
// моменту уже известен, и раздел без текста остаётся, только если сразу за
// ним идёт его потомок (уровень глубже).
func pruneEmpty(list []Section) []Section {
	keep := make([]bool, len(list))
	nextLevel := 0 // уровень ближайшего оставленного раздела справа; 0 — нет
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Text != "" || (nextLevel > list[i].Level) {
			keep[i] = true
			nextLevel = list[i].Level
		}
	}
	out := make([]Section, 0, len(list))
	for i, s := range list {
		if keep[i] {
			out = append(out, s)
		}
	}
	return out
}

// normalizeTitle — заголовок для сверки со списком служебных: регистр,
// ё, пробелы.
func normalizeTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "ё", "е")
	return strings.Join(strings.Fields(s), " ")
}

// joinParagraphs — абзацы через один перевод строки, без пустых строк и
// пробелов по краям: пустые строки TextExtracts (по две-три вокруг
// заголовков и на месте таблиц и картинок) — шум для чанкера.
func joinParagraphs(lines []string) string {
	out := lines[:0:0]
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// cleanText — чистка простого текста до разбора на разделы:
//   - формулы (MathML + LaTeX) → одна строка упрощённого LaTeX;
//   - знаки ударения (U+0301, U+0300) снимаются: «Ману́л» иначе не
//     совпадёт с «Манул» ни в поиске по словам, ни в Find;
//   - неразрывный и прочие особые пробелы → обычный пробел, мягкий перенос
//     и символы нулевой ширины убираются;
//   - пробелы в конце строк и повторные пробелы внутри строки схлопываются.
func cleanText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = replaceMath(s)
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\u0301', '\u0300', '\u00ad', '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff':
			return -1
		case '\u00a0', '\u2002', '\u2003', '\u2009', '\u202f', '\t':
			return ' '
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.Join(lines, "\n")
}

// replaceMath заменяет блоки формул строкой (mathRE). Пробел после
// формулы ставится, только если дальше не знак препинания: «= 28.», а не
// «= 28 .».
func replaceMath(s string) string {
	locs := mathRE.FindAllStringSubmatchIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	prev := 0
	for _, l := range locs {
		b.WriteString(s[prev:l[0]])
		b.WriteString(" ")
		b.WriteString(latexToText(s[l[2]:l[3]]))
		if r, _ := utf8.DecodeRuneInString(s[l[1]:]); l[1] < len(s) && !strings.ContainsRune(".,;:!?)»", r) {
			b.WriteString(" ")
		}
		prev = l[1]
	}
	b.WriteString(s[prev:])
	return b.String()
}

var (
	latexFrac  = regexp.MustCompile(`\\frac\s*\{([^{}]*)\}\s*\{([^{}]*)\}`)
	latexCmd   = regexp.MustCompile(`\\([A-Za-z]+)`)
	latexWords = map[string]string{
		"times": "×", "cdot": "·", "approx": "≈", "pm": "±", "leq": "≤", "le": "≤",
		"geq": "≥", "ge": "≥", "neq": "≠", "sim": "~", "circ": "°", "degree": "°",
		"mu": "μ", "alpha": "α", "beta": "β", "gamma": "γ", "lambda": "λ", "pi": "π",
		"to": "→", "rightarrow": "→", "dots": "…", "ldots": "…",
		// Оформление без смысла.
		"mathrm": "", "text": "", "mathit": "", "mathbf": "", "left": "", "right": "",
		"displaystyle": "", "textstyle": "", "quad": " ", "qquad": " ",
	}
)

// latexToText — LaTeX формулы в читаемую строку: «\frac {3}{3}» → «3/3»,
// известные команды → символы, остальное — без обратной косой и скобок.
// Точное воспроизведение не нужно: в статьях о животных формулы — это
// зубные формулы и редкие отношения величин.
func latexToText(s string) string {
	for {
		next := latexFrac.ReplaceAllString(s, " $1/$2 ")
		if next == s {
			break
		}
		s = next
	}
	s = strings.ReplaceAll(s, `\,`, " ")
	s = strings.ReplaceAll(s, `\;`, " ")
	s = strings.ReplaceAll(s, `\ `, " ")
	s = latexCmd.ReplaceAllStringFunc(s, func(c string) string {
		if w, ok := latexWords[c[1:]]; ok {
			return " " + w + " "
		}
		return c[1:]
	})
	s = strings.NewReplacer("{", "", "}", "", "_", "").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}
