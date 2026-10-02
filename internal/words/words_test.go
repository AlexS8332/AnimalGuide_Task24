package words

import "testing"

// Сверка цитаты — единственная защита от «пользователь же согласился».
// Поэтому проверяется с обеих сторон: настоящая цитата должна проходить,
// выдуманная — нет.

func TestQuoteFromReplyPasses(t *testing.T) {
	cases := []struct {
		name       string
		quote      string
		user       string
		keepShort  bool
		wantPassed bool
	}{
		{"дословно", "снимаем это ограничение", "да, снимаем это ограничение", true, true},
		{"падеж другой", "утверждаю план", "план утверждаю, начинай", true, true},
		{"короткое согласие", "да", "да", true, true},
		{"стоп", "стоп", "стоп, пауза", true, true},
		{"выдумано", "согласен снять ограничение", "а что там с планом на завтра?", true, false},
		{"пересказ своими словами", "пользователь разрешил монго", "ускорь отчёты, пожалуйста", true, false},
		{"пустая цитата", "", "да, конечно", true, false},
		{"знаки препинания не мешают", "«пиши короче»", "Пиши короче, без воды.", false, true},
		{"длинная цитата не из реплики", "отвечай списком и добавь примеры кода", "расскажи про рысь", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, n := InReply(tc.quote, tc.user, tc.keepShort)
			if tc.quote == "" && n != 0 {
				t.Fatalf("пустая цитата дала %d слов", n)
			}
			if ok != tc.wantPassed {
				t.Fatalf("сверка дала %v, ждали %v (слов %d)", ok, tc.wantPassed, n)
			}
		})
	}
}

// Короткое слово ищется целиком: иначе «да» нашлось бы в «надо», и любая
// реплика подтверждала бы что угодно.
func TestShortWordIsNotSubstring(t *testing.T) {
	if ok, _ := InReply("да", "надо подумать", true); ok {
		t.Fatal("«да» нашлось внутри «надо»")
	}
	if ok, _ := InReply("да", "да, давай", true); !ok {
		t.Fatal("настоящее «да» не нашлось")
	}
}

// Две политики различаются там, где это важно: короткая управляющая
// реплика подтверждает решение, но не годится в источник предпочтения.
func TestPoliciesDiffer(t *testing.T) {
	quote, user := "да, снимаем", "да, снимаем"
	if ok, _ := InReply(quote, user, true); !ok {
		t.Fatal("управляющая реплика не подтвердила решение")
	}
	if _, n := InReply("да", user, false); n != 0 {
		t.Fatalf("короткое слово попало в значимые: %d", n)
	}
}

func TestStemsAndSignificant(t *testing.T) {
	if got := Stems("Ёлка, Ёж"); len(got) != 2 || got[0] != "елка" || got[1] != "еж" {
		t.Fatalf("основы разобраны неверно: %q", got)
	}
	if got := Significant("да нет ограничение"); len(got) != 1 {
		t.Fatalf("короткие слова не отброшены: %q", got)
	}
}
