package invariants

import (
	"strings"
	"unicode"
)

// Страж: первая, дешёвая ступень проверки ответа (ФТ-30).
//
// У инварианта есть слова, которые в нарушающем ответе почти наверняка
// встретятся: совет с лекарством не обходится без «дозировки» или
// «таблетки», инструкция с опасным зверем — без «отпугнуть» или «если
// нападёт». Код ищет эти слова в готовом ответе до того, как ответ ушёл
// человеку.
//
// Приговор страж не выносит: «с лечением — только к ветеринару» —
// образцовое соблюдение, и слово там то же. Совпадение маркера значит
// «посмотри сюда»; решает судья, и только по отобранным предложениям.
// Гонять судью по каждому ответу целиком дорого, по трём предложениям из
// двадцати — почти бесплатно.

// Hit — найденный маркер: где и какой.
type Hit struct {
	Invariant string `json:"invariant"`
	Marker    string `json:"marker"`
	Fragment  string `json:"fragment"`
	Cleared   bool   `json:"cleared,omitempty"`
	Why       string `json:"why,omitempty"` // почему снят без судьи
}

// Screening — итог просмотра ответа.
type Screening struct {
	Hits []Hit `json:"hits,omitempty"`
}

// Suspect — попадания, которые страж снять не смог: их разбирает судья.
func (s Screening) Suspect() []Hit {
	var out []Hit
	for _, h := range s.Hits {
		if !h.Cleared {
			out = append(out, h)
		}
	}
	return out
}

// Touched — инварианты с неснятыми попаданиями, без повторов, в порядке
// попаданий.
func (s Screening) Touched() []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range s.Suspect() {
		if !seen[h.Invariant] {
			seen[h.Invariant] = true
			out = append(out, h.Invariant)
		}
	}
	return out
}

// Clean — подозрительного не нашлось.
func (s Screening) Clean() bool { return len(s.Suspect()) == 0 }

// Слова, которыми ответ говорит о правиле или отсылает к специалисту. Если
// предложение построено как отказ и ничего не советует, судью звать незачем.
var talkAboutRules = []string{
	"свод", "инвариант", "не даю", "не могу", "не буду", "не стану", "не советую", "не подскажу",
	"нельзя советовать", "не мне решать", "обратитесь", "обратиться", "к специалист",
}

// Слова совета. Их наличие возвращает предложение судье даже при разговоре
// о правилах: «советовать не могу, но дайте полтаблетки» — обход, а не
// соблюдение.
var offerWords = []string{
	"советую", "рекомендую", "попробуйте", "сделайте", "дайте", "давайте", "дать ", "можно давать",
	"нужно", "следует", "стоит ", "лучше всего", "возьмите", "подойдите", "принимайте",
}

// Screen — просмотр текста по маркерам действующих инвариантов: снятое
// правило больше ничего не запрещает.
func Screen(c Charter, text string) Screening {
	if strings.TrimSpace(text) == "" {
		return Screening{}
	}
	sentences := split(text)
	var out Screening
	for _, inv := range c.ActiveItems() {
		for _, marker := range inv.Markers {
			m := fold(marker)
			if m == "" {
				continue
			}
			for _, s := range sentences {
				if !strings.Contains(" "+fold(s), " "+m) {
					continue
				}
				hit := Hit{Invariant: inv.ID, Marker: marker, Fragment: trim(s, 240)}
				if why := clears(inv, s); why != "" {
					hit.Cleared, hit.Why = true, why
				}
				out.Hits = append(out.Hits, hit)
				break // одного предложения на маркер довольно
			}
		}
	}
	return out
}

// clears — можно ли снять попадание без судьи: предложение называет сам
// инвариант, подпадает под оговорку или построено как отказ и ничего не
// советует. Остальное — к судье: дешевле лишняя проверка, чем пропущенное
// нарушение.
func clears(inv Invariant, sentence string) string {
	low := fold(sentence)
	if strings.Contains(strings.ToLower(sentence), strings.ToLower(inv.ID)) {
		return "предложение ссылается на инвариант по имени"
	}
	for _, e := range inv.Except {
		if e = fold(e); e != "" && strings.Contains(low, e) {
			return "случай описан в оговорках инварианта: " + e
		}
	}
	if hasAny(" "+low+" ", offerWords) {
		return ""
	}
	if hasAny(low, talkAboutRules) {
		return "предложение говорит о правиле, а не советует"
	}
	return ""
}

func hasAny(low string, list []string) bool {
	for _, w := range list {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// split — текст на предложения; переводы строки тоже режут: пункты списка —
// те же предложения без точек.
func split(text string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			out = append(out, s)
		}
		b.Reset()
	}
	for _, r := range text {
		b.WriteRune(r)
		switch r {
		case '.', '!', '?', '\n', ';':
			flush()
		}
	}
	flush()
	return out
}

// fold — текст для поиска: нижний регистр, «ё» как «е», знаки как пробелы.
func fold(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, "ё", "е"), "Ё", "е")) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' {
			b.WriteRune(r)
			continue
		}
		b.WriteRune(' ')
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func trim(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max])) + "…"
}
