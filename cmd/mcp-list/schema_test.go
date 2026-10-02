package main

import (
	"slices"
	"strings"
	"testing"
)

func TestSchemaPropsKeepsOrderAndTypes(t *testing.T) {
	schema := `{
	  "type": "object",
	  "properties": {
	    "title":   {"type": "string", "description": "заголовок"},
	    "limit":   {"type": "integer"},
	    "fields":  {"type": ["array", "null"], "items": {"type": "string"}},
	    "diet":    {"type": "string", "enum": ["хищник", "травоядное"]}
	  },
	  "required": ["title"]
	}`

	props, err := schemaProps([]byte(schema))
	if err != nil {
		t.Fatalf("схема не разобралась: %v", err)
	}

	names := make([]string, 0, len(props))
	for _, p := range props {
		names = append(names, p.Name)
	}
	if want := []string{"title", "limit", "fields", "diet"}; !slices.Equal(names, want) {
		t.Errorf("порядок свойств %v, ожидался %v", names, want)
	}

	byName := make(map[string]prop, len(props))
	for _, p := range props {
		byName[p.Name] = p
	}
	if p := byName["title"]; !p.Required || p.Type != "string" || p.Description != "заголовок" {
		t.Errorf("title: %+v", p)
	}
	if p := byName["limit"]; p.Required || p.Type != "integer" {
		t.Errorf("limit: %+v", p)
	}
	// Схема необязательного массива — ["array","null"]; null в выводе
	// лишний, его роль играет отсутствие пометки «обязательный».
	if p := byName["fields"]; p.Type != "array[string]" {
		t.Errorf("fields: тип %q", p.Type)
	}
	if p := byName["diet"]; !slices.Equal(p.Enum, []string{"хищник", "травоядное"}) {
		t.Errorf("diet: перечисление %v", p.Enum)
	}
}

func TestSchemaPropsEmptyObject(t *testing.T) {
	props, err := schemaProps([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatalf("схема без свойств не разобралась: %v", err)
	}
	if len(props) != 0 {
		t.Errorf("получили %d свойств", len(props))
	}
	if _, err := schemaProps([]byte(`не json`)); err == nil {
		t.Error("мусор вместо схемы разобрался без ошибки")
	}
}

func TestClipByLinesAndRunes(t *testing.T) {
	short := "одна\nдве"
	if got := clip(short, 5, 100); got != short {
		t.Errorf("короткий текст обрезан: %q", got)
	}

	long := strings.Repeat("строка\n", 30)
	got := clip(long, 3, 1000)
	if strings.Count(got, "\n") != 3 {
		t.Errorf("обрезали до %d строк: %q", strings.Count(got, "\n"), got)
	}
	if !strings.Contains(got, "обрезано") {
		t.Error("нет пометки об обрезке")
	}

	// Длинная строка без переносов: ограничение по строкам её не берёт.
	oneLine := strings.Repeat("я", 500)
	got = clip(oneLine, 10, 100)
	if r := []rune(strings.Split(got, "\n")[0]); len(r) != 100 {
		t.Errorf("обрезали до %d знаков", len(r))
	}
}

func TestWrapByWords(t *testing.T) {
	lines := wrap("серый журавль гнездится на труднодоступных болотах", 20)
	for _, line := range lines {
		if len([]rune(line)) > 20 {
			t.Errorf("строка длиннее 20 знаков: %q", line)
		}
	}
	if strings.Join(lines, " ") != "серый журавль гнездится на труднодоступных болотах" {
		t.Errorf("перенос потерял слова: %v", lines)
	}
	if wrap("   ", 20) != nil {
		t.Error("пустая строка превратилась в строку вывода")
	}
}

func TestTypeName(t *testing.T) {
	cases := map[string]string{
		`"string"`:            "string",
		`["array","null"]`:    "array",
		`["string","number"]`: "string|number",
		`["null"]`:            "null",
		`12`:                  "any",
	}
	for in, want := range cases {
		if got := typeName([]byte(in)); got != want {
			t.Errorf("%s: получили %q, ожидали %q", in, got, want)
		}
	}
}
