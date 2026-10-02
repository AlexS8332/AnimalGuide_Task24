// Package words — сверка цитаты с репликой пользователя.
//
// Приём один и тот же в трёх местах приложения, и везде он решает одну
// задачу: модель сообщает, что пользователь что-то сказал — разрешил
// изменить профиль, утвердил план, согласился снять ограничение, — и код
// обязан проверить, что это правда. Без проверки модель «вспоминает»
// согласие, которого не было; в прошлом задании она так объявляла паузу,
// которой не ставила.
//
// Точного совпадения не требуем: модель нормализует падежи и выкидывает
// слова-паразиты, и требование дословности сделало бы проверку
// бесполезной — её начали бы обходить, цитируя одно слово. Считаем по
// словам: большинство слов цитаты должно найтись в реплике.
//
// Политики две, и различие не косметическое:
//
//	Significant — служебные короткие слова отбрасываются. Годится там, где
//	     цитата длинная и содержательная: «отвечай короче, без воды».
//	Stems — короткие слова сохраняются. Годится там, где вся реплика может
//	     быть коротким словом: «да», «стоп», «снимаем». Отбрось их — и
//	     подтверждать решения станет нечем.
package words

import (
	"strings"
	"unicode"
)

// Stems — слова строки, приведённые к сравнимому виду: нижний регистр,
// «ё» как «е», у слов длиннее четырёх букв отрезан хвост в две буквы,
// чтобы падеж не мешал («утвержда» найдётся и в «утверждаю», и в
// «утверждаем»). Короткие слова остаются целиком.
func Stems(s string) []string {
	var out []string
	for _, w := range split(s) {
		r := []rune(strings.ReplaceAll(w, "ё", "е"))
		if len(r) > 4 {
			r = r[:len(r)-2]
		}
		out = append(out, string(r))
	}
	return out
}

// Significant — то же, но без коротких слов: они служебные и находятся в
// любой реплике, а значит подтверждают что угодно.
func Significant(s string) []string {
	var out []string
	for _, w := range split(s) {
		r := []rune(w)
		if len(r) < 4 {
			continue
		}
		out = append(out, string(r[:len(r)-2]))
	}
	return out
}

func split(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// InReply — большинство слов цитаты есть в реплике.
//
// keepShort выбирает политику: true — короткие слова считаются наравне с
// остальными (управляющие реплики коротки), false — отбрасываются.
// Пустая цитата — не «совпадение по нулю слов», а отсутствие
// подтверждения; такой случай вызывающий код разбирает сам, потому что
// объяснять его пользователю надо разными словами в разных местах.
func InReply(quote, user string, keepShort bool) (ok bool, words int) {
	var list []string
	if keepShort {
		list = Stems(quote)
	} else {
		list = Significant(quote)
	}
	if len(list) == 0 {
		return false, 0
	}

	low := strings.ToLower(user)
	if !keepShort {
		hits := 0
		for _, w := range list {
			if strings.Contains(low, w) {
				hits++
			}
		}
		return hits*2 >= len(list), len(list)
	}

	// Короткое слово ищется целиком («да» не должно найтись в «надо»),
	// длинное — подстрокой.
	have := make(map[string]bool, 16)
	for _, w := range Stems(user) {
		have[w] = true
	}
	low = strings.ReplaceAll(low, "ё", "е")
	hits := 0
	for _, w := range list {
		if have[w] || (len([]rune(w)) > 3 && strings.Contains(low, w)) {
			hits++
		}
	}
	return hits*2 >= len(list), len(list)
}
