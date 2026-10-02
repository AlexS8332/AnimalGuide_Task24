package corpus

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// normRune — одна руна нормализованного текста и откуда она взялась в
// исходном: [start, end) в рунах. У схлопнутой пробельной
// последовательности end — конец всей последовательности.
type normRune struct {
	r          rune
	start, end int
}

// foldRune — посимвольная часть нормализации. Отображение руна → руна
// (unicode.ToLower, а не strings.ToLower), поэтому смещения переводятся
// обратно без потерь.
func foldRune(r rune) rune {
	r = unicode.ToLower(r)
	switch r {
	case 'ё':
		return 'е'
	case '«', '»', '“', '”', '„':
		return '"'
	case '—', '–', '−', '‐', '‑':
		// Длинное и среднее тире по контракту; минус и неразрывный дефис
		// туда же — в статьях они стоят вперемешку с дефисом в числах.
		return '-'
	}
	return r
}

// normalizeMap нормализует текст и запоминает для каждой руны результата её
// место в исходном. Пробельная последовательность (включая неразрывный
// пробел и переводы строк) становится одним пробелом.
func normalizeMap(s string) []normRune {
	out := make([]normRune, 0, len(s))
	i := 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			if n := len(out); n > 0 && out[n-1].r == ' ' && out[n-1].end == i {
				out[n-1].end = i + 1
			} else {
				out = append(out, normRune{r: ' ', start: i, end: i + 1})
			}
		} else {
			out = append(out, normRune{r: foldRune(r), start: i, end: i + 1})
		}
		i++
	}
	return out
}

// normalize — реализация Normalize: нормализованный текст без пробелов по
// краям.
func normalize(s string) string {
	m := normalizeMap(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, n := range m {
		b.WriteRune(n.r)
	}
	return strings.TrimSpace(b.String())
}

// find — реализация Find. Фрагмент нормализуется целиком (с обрезкой
// пробелов по краям), текст — с картой смещений; найденное место
// переводится обратно в руны исходного текста.
func find(text, fragment string) (int, int) {
	frag := normalize(fragment)
	if frag == "" {
		return -1, 0
	}
	m := normalizeMap(text)
	var b strings.Builder
	b.Grow(len(text))
	for _, n := range m {
		b.WriteRune(n.r)
	}
	norm := b.String()
	at := strings.Index(norm, frag)
	if at < 0 {
		return -1, 0
	}
	first := utf8.RuneCountInString(norm[:at])
	last := first + utf8.RuneCountInString(frag) - 1
	start := m[first].start
	return start, m[last].end - start
}
