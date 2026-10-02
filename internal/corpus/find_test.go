package corpus

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Ёж и ЁЛКА", "еж и елка"},
		{"«Красная книга» “МСОП” „x“", `"красная книга" "мсоп" "x"`},
		{"50—72 см, 2–5 кг", "50-72 см, 2-5 кг"},
		{"  много \t пробелов\n\nи строк  ", "много пробелов и строк"},
		{"неразрывный" + string(rune(0xa0)) + "пробел", "неразрывный пробел"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, ждали %q", c.in, got, c.want)
		}
	}
}

func TestFind(t *testing.T) {
	text := "Манул — хищник.\nМех манула «самый пушистый»  среди   кошачьих; весит 2—5 кг. Ёмкость."
	runes := []rune(text)
	cases := []struct {
		frag string
		want string // ожидаемый кусок ИСХОДНОГО текста; "" — не найдено
	}{
		{"манул — хищник", "Манул — хищник"},
		{"хищник. мех", "хищник.\nМех"},                                           // перевод строки = пробел
		{`"самый пушистый" среди кошачьих`, "«самый пушистый»  среди   кошачьих"}, // кавычки и пробелы
		{"весит 2-5 кг", "весит 2—5 кг"},                                          // тире
		{"емкость", "Ёмкость"},                                                    // ё и регистр
		{"  МЕХ манула  ", "Мех манула"},                                          // пробелы по краям фрагмента
		{"кошачьих; весит", "кошачьих; весит"},
		{"барс", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		start, n := Find(text, c.frag)
		if c.want == "" {
			if start != -1 {
				t.Errorf("Find(%q) = %d,%d, ждали -1", c.frag, start, n)
			}
			continue
		}
		if start < 0 {
			t.Errorf("Find(%q) не нашёл", c.frag)
			continue
		}
		if got := string(runes[start : start+n]); got != c.want {
			t.Errorf("Find(%q) = [%d,%d) %q, ждали %q", c.frag, start, start+n, got, c.want)
		}
	}
}

func TestFindOffsetsInRunesAfterCollapsedSpace(t *testing.T) {
	// Смещение после схлопнутых пробелов и многобайтных рун — в рунах
	// исходного текста, а не в байтах и не в нормализованном тексте.
	text := "А    Б\n\n\nВ ёж"
	start, n := Find(text, "в еж")
	if start != 9 || n != 4 {
		t.Fatalf("Find = %d,%d, ждали 9,4", start, n)
	}
}
