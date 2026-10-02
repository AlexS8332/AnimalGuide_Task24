package main

import (
	"encoding/json"
	"testing"
)

// Аргументы набирает человек, поэтому разбор проверяется придирчиво:
// ошибка здесь выглядит как «сервер не понял вызов».
func TestParseArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // ожидаемое в виде JSON
	}{
		{"пусто", "   ", "null"},
		{"готовый JSON", `{"animal":"рысь"}`, `{"animal":"рысь"}`},
		{"пара", "animal=рысь", `{"animal":"рысь"}`},
		{"несколько пар", "class=птицы limit=3", `{"class":"птицы","limit":3}`},
		{"дробное число", "min_weight_kg=0.7", `{"min_weight_kg":0.7}`},
		{"булево", "verbose=true", `{"verbose":true}`},
		{"список", "animals=lynx,amur-tiger", `{"animals":["lynx","amur-tiger"]}`},
		{"строка в кавычках", `name="Малая панда"`, `{"name":"Малая панда"}`},
		{"кавычки сохраняют запятую", `description="один, два"`, `{"description":"один, два"}`},
		{"вложенный объект", "weight_kg.min=0.7 weight_kg.max=2.4", `{"weight_kg":{"max":2.4,"min":0.7}}`},
		{"пустое значение", "section=", `{"section":""}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseArgs(c.in)
			if err != nil {
				t.Fatalf("разбор не прошёл: %v", err)
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("результат не сериализуется: %v", err)
			}
			if string(raw) != c.want {
				t.Errorf("получили %s, ожидали %s", raw, c.want)
			}
		})
	}
}

func TestParseArgsErrors(t *testing.T) {
	cases := map[string]string{
		"без знака равенства": "animal рысь",
		"пустой ключ":         "=рысь",
		"незакрытая кавычка":  `name="Малая панда`,
		"битый JSON":          `{"animal":`,
		"ключ и объект разом": "weight=1 weight.min=2",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseArgs(in); err == nil {
				t.Errorf("разбор «%s» прошёл без ошибки", in)
			}
		})
	}
}

func TestSplitTokensKeepsQuotedSpaces(t *testing.T) {
	tokens, err := splitTokens(`id=platypus name="Утконос обыкновенный" class=млекопитающие`)
	if err != nil {
		t.Fatalf("разбор не прошёл: %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("получили %d токенов: %q", len(tokens), tokens)
	}
	if tokens[1] != `name="Утконос обыкновенный"` {
		t.Errorf("кавычки не удержали пробел: %q", tokens[1])
	}
}
