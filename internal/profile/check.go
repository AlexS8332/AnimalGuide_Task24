package profile

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Проверка соблюдения профиля (ФТ-23): по тексту ответа, кодом, для полей,
// видных в тексте — длина, форма, обращение, латынь, эмодзи. У проверки
// три исхода: «соблюдено», «нарушено» и «не определить» — короткий ответ
// без местоимений ничего не говорит об обращении, и засчитывать его в
// соблюдение так же нечестно, как в нарушение. «Не определить» в
// знаменатель не идёт.
//
// Пороги длины мягкие и меряют то, что попросили: «коротко» — это до
// четырёх предложений, а не «не больше 60 слов» (урок упражнения 12:
// проверка, штрафующая за послушание, врёт правдоподобнее агента).

// Check — результат одной проверки.
type Check struct {
	Field string `json:"field"`
	Title string `json:"title"`
	Want  string `json:"want"`
	Got   string `json:"got"`
	OK    bool   `json:"ok"`
	NA    bool   `json:"na"`
}

// Rate — сколько проверок соблюдено из определимых.
func Rate(checks []Check) (ok, total int) {
	for _, c := range checks {
		if c.NA {
			continue
		}
		total++
		if c.OK {
			ok++
		}
	}
	return ok, total
}

// Note — итог проверок одной строкой.
func Note(checks []Check) string {
	ok, total := Rate(checks)
	if total == 0 {
		return "проверить нечего"
	}
	var broken []string
	for _, c := range checks {
		if !c.NA && !c.OK {
			broken = append(broken, fmt.Sprintf("%s: %s вместо «%s»", c.Title, c.Got, c.Want))
		}
	}
	head := fmt.Sprintf("профиль соблюдён в %d проверках из %d", ok, total)
	if len(broken) == 0 {
		return head
	}
	return head + "; нарушено — " + strings.Join(broken, ", ")
}

// Checks — проверки ответа по анкете. Незаполненные поля не проверяются:
// требований нет — и нарушить нечего.
func Checks(p Profile, reply string) []Check {
	text := strings.TrimSpace(reply)
	if text == "" {
		return nil
	}
	m := Measure(text)
	var out []Check
	for _, f := range fields {
		if !f.Checked {
			continue
		}
		v, ok := p.Values[f.Key]
		if !ok {
			continue
		}
		c := Check{Field: f.Key, Title: f.Title, Want: labelOf(f, v.Value)}
		switch f.Key {
		case FieldLength:
			c = checkLength(c, v.Value, m)
		case FieldForm:
			c = checkForm(c, v.Value, m)
		case FieldAddress:
			c = checkAddress(c, v.Value, m)
		case FieldLatin:
			c = checkLatin(c, v.Value, m)
		case FieldEmoji:
			c = checkEmoji(c, v.Value, m)
		}
		out = append(out, c)
	}
	return out
}

func checkLength(c Check, value string, m Shape) Check {
	c.Got = plural(m.Sentences, "предложение", "предложения", "предложений") + " на " + plural(m.Words, "слово", "слова", "слов")
	switch value {
	case "tiny":
		c.OK = m.Sentences <= 2 && m.Words <= 45
	case "short":
		c.OK = m.Sentences <= 4 && m.Words <= 110
	case "normal":
		c.OK = m.Words <= 230
	case "long":
		c.OK = m.Words >= 90
	}
	return c
}

func checkForm(c Check, value string, m Shape) Check {
	switch value {
	case "list":
		c.OK, c.Got = m.Bullets >= 2, fmt.Sprintf("пунктов списка: %d", m.Bullets)
	case "prose":
		c.OK = m.Bullets == 0 && m.Headings == 0 && m.Tables == 0
		c.Got = "сплошной текст"
		if !c.OK {
			c.Got = fmt.Sprintf("пунктов %d, заголовков %d, строк таблицы %d", m.Bullets, m.Headings, m.Tables)
		}
	case "table":
		// «Где уместно»: в ответе, где сравнивать нечего, таблицы и не нужно.
		if m.Tables == 0 {
			c.NA, c.Got = true, "таблицы нет, сравнения в ответе, возможно, тоже"
			return c
		}
		c.OK, c.Got = true, fmt.Sprintf("строк таблицы: %d", m.Tables)
	}
	return c
}

func checkAddress(c Check, value string, m Shape) Check {
	if m.Informal == 0 && m.Formal == 0 {
		c.NA, c.Got = true, "обращения в ответе нет"
		return c
	}
	c.Got = "на «" + m.Address + "»"
	c.OK = (value == "ty" && m.Informal > 0 && m.Formal == 0) || (value == "vy" && m.Formal > 0 && m.Informal == 0)
	if m.Informal > 0 && m.Formal > 0 {
		c.Got = "и на «ты», и на «вы»"
	}
	return c
}

func checkLatin(c Check, value string, m Shape) Check {
	c.Got = plural(m.Latin, "латинское слово", "латинских слова", "латинских слов")
	switch value {
	case "hide":
		c.OK = m.Latin == 0
	case "caption":
		if m.Latin == 0 {
			c.NA, c.Got = true, "латыни нет — таксон, возможно, и не называли"
			return c
		}
		c.OK = m.Latin <= 4
	case "full":
		if m.Latin == 0 {
			c.NA, c.Got = true, "латыни нет — таксон, возможно, и не называли"
			return c
		}
		c.OK = true
	}
	return c
}

func checkEmoji(c Check, value string, m Shape) Check {
	switch value {
	case "no":
		c.OK, c.Got = !m.Emoji, "эмодзи нет"
		if m.Emoji {
			c.Got = "в ответе есть эмодзи"
		}
	case "some":
		if !m.Emoji {
			c.NA, c.Got = true, "эмодзи нет, но они и не обязательны"
			return c
		}
		c.OK, c.Got = true, "эмодзи есть"
	}
	return c
}

// Shape — измеримые свойства ответа: по ним сравнивают дорожки, не
// спрашивая мнения ни у человека, ни у второй модели.
type Shape struct {
	Words     int    `json:"words"`
	Sentences int    `json:"sentences"`
	Bullets   int    `json:"bullets"`
	Headings  int    `json:"headings"`
	Tables    int    `json:"tables"`
	Emoji     bool   `json:"emoji"`
	Latin     int    `json:"latin"`
	Informal  int    `json:"informal"`
	Formal    int    `json:"formal"`
	Address   string `json:"address,omitempty"`
}

var (
	bulletLine  = regexp.MustCompile(`(?m)^\s*([-*•]\s+|\d+[.)]\s+)`)
	headingLine = regexp.MustCompile(`(?m)^\s*#{1,6}\s+`)
	tableLine   = regexp.MustCompile(`(?m)^\s*\|.*\|\s*$`)
	sentenceEnd = regexp.MustCompile(`[.!?…]+(\s|$)`)
	linkRe      = regexp.MustCompile(`https?://\S+|\]\([^)]*\)`)
	latinWord   = regexp.MustCompile(`\b[A-Za-z]{3,}\b`)
)

// Формы местоимений — отдельными словами: «вы» есть внутри сотни слов.
var (
	youInformal = set("ты", "тебя", "тебе", "тобой", "твой", "твоя", "твоё", "твое", "твои", "твоего", "твоей", "твоему", "твоим", "твоих", "твою")
	youFormal   = set("вы", "вас", "вам", "вами", "ваш", "ваша", "ваше", "ваши", "вашего", "вашей", "вашему", "вашим", "ваших", "вашу")
)

func set(ws ...string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

// Measure — свойства ответа.
func Measure(reply string) Shape {
	plain := linkRe.ReplaceAllString(reply, " ")
	ws := strings.FieldsFunc(strings.ToLower(plain), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	s := Shape{
		Words:    len(ws),
		Bullets:  len(bulletLine.FindAllString(plain, -1)),
		Headings: len(headingLine.FindAllString(plain, -1)),
		Tables:   len(tableLine.FindAllString(plain, -1)),
		Emoji:    hasEmoji(reply),
		Latin:    len(latinWord.FindAllString(plain, -1)),
	}
	s.Sentences = len(sentenceEnd.FindAllString(plain, -1))
	if s.Sentences == 0 && s.Words > 0 {
		s.Sentences = 1
	}
	for _, w := range ws {
		switch {
		case youInformal[w]:
			s.Informal++
		case youFormal[w]:
			s.Formal++
		}
	}
	switch {
	case s.Informal > s.Formal:
		s.Address = "ты"
	case s.Formal > s.Informal:
		s.Address = "вы"
	case s.Formal > 0:
		s.Address = "вперемешку"
	}
	return s
}

func hasEmoji(s string) bool {
	for _, r := range s {
		switch {
		case r >= 0x1F300 && r <= 0x1FAFF, r >= 0x2600 && r <= 0x27BF, r >= 0x1F000 && r <= 0x1F0FF:
			return true
		}
	}
	return false
}

func plural(n int, one, few, many string) string {
	m10, m100 := n%10, n%100
	switch {
	case m10 == 1 && m100 != 11:
		return fmt.Sprintf("%d %s", n, one)
	case m10 >= 2 && m10 <= 4 && (m100 < 10 || m100 >= 20):
		return fmt.Sprintf("%d %s", n, few)
	}
	return fmt.Sprintf("%d %s", n, many)
}
