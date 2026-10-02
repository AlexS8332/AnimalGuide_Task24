package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Разбор JSON Schema инструмента ради одной строки на аргумент:
// «имя, тип, обязателен ли, зачем нужен». Схема приезжает от сервера как
// произвольный JSON, поэтому разбираем её как данные, а не как известную
// структуру: чужой сервер вправе прислать что угодно.

// prop — один аргумент инструмента.
type prop struct {
	Name        string
	Type        string
	Description string
	Required    bool
	Enum        []string
}

// schemaProps возвращает аргументы объекта-схемы в том порядке, в каком
// они записаны в схеме: порядок — решение сервера, и разбор не должен
// его терять, даже если печатать мы будем по-своему.
func schemaProps(raw json.RawMessage) ([]prop, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var head struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("схема не разобралась: %w", err)
	}
	if len(head.Properties) == 0 {
		return nil, nil
	}

	required := make(map[string]bool, len(head.Required))
	for _, name := range head.Required {
		required[name] = true
	}

	names, err := objectKeys(head.Properties)
	if err != nil {
		return nil, err
	}

	var byName map[string]json.RawMessage
	if err := json.Unmarshal(head.Properties, &byName); err != nil {
		return nil, fmt.Errorf("свойства схемы не разобрались: %w", err)
	}

	props := make([]prop, 0, len(names))
	for _, name := range names {
		p := prop{Name: name, Required: required[name]}
		var body struct {
			Type        json.RawMessage `json:"type"`
			Description string          `json:"description"`
			Enum        []any           `json:"enum"`
			Items       *struct {
				Type json.RawMessage `json:"type"`
			} `json:"items"`
			Properties json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(byName[name], &body); err != nil {
			return nil, fmt.Errorf("свойство %q не разобралось: %w", name, err)
		}
		p.Type = typeName(body.Type)
		if body.Items != nil {
			p.Type += "[" + typeName(body.Items.Type) + "]"
		}
		// У вложенного объекта показываем имена полей: вручную их
		// задают как weight_kg.min=0.7, и знать их надо заранее.
		if len(body.Properties) > 0 {
			if keys, err := objectKeys(body.Properties); err == nil && len(keys) > 0 {
				p.Type += "{" + strings.Join(keys, ",") + "}"
			}
		}
		p.Description = body.Description
		for _, v := range body.Enum {
			p.Enum = append(p.Enum, fmt.Sprint(v))
		}
		props = append(props, p)
	}
	return props, nil
}

// objectKeys читает ключи JSON-объекта по порядку. encoding/json отдаёт
// объект как map и порядок теряет, поэтому идём потоком токенов.
func objectKeys(raw json.RawMessage) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("ожидался объект, получено %v", tok)
	}

	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("ожидалось имя свойства, получено %v", tok)
		}
		keys = append(keys, key)

		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// typeName приводит поле type к строке: в JSON Schema оно бывает и
// строкой, и списком («строка или null»).
func typeName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "any"
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return one
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil && len(many) > 0 {
		// Схема необязательного массива или указателя выглядит как
		// ["array", "null"]. Слово null в списке аргументов ничего не
		// добавляет: то же самое говорит отсутствие пометки «обязательный».
		kept := make([]string, 0, len(many))
		for _, t := range many {
			if t != "null" {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			return "null"
		}
		return strings.Join(kept, "|")
	}
	return "any"
}

// indentJSON форматирует JSON для показа человеку. Если это не JSON,
// возвращаем как есть: инструмент вправе ответить обычным текстом.
func indentJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(s), "", "  "); err != nil {
		return s
	}
	return buf.String()
}

// clip обрезает ответ инструмента: читать его в терминале целиком
// невозможно, а флаг -full вернёт всё.
// Длина считается и в строках, и в знаках: у read_wikipedia весь текст
// раздела приезжает одной строкой JSON, и ограничение по строкам его
// не укоротит.
func clip(s string, maxLines, maxRunes int) string {
	cut := false

	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		lines, cut = lines[:maxLines], true
	}
	out := strings.Join(lines, "\n")

	if r := []rune(out); len(r) > maxRunes {
		out, cut = string(r[:maxRunes]), true
	}
	if !cut {
		return s
	}
	return out + "\n… обрезано (флаг -full покажет целиком)"
}
