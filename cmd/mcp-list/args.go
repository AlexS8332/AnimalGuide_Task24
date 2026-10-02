package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Разбор аргументов, которые человек набирает руками.
//
// JSON правилен, но неудобен для набора: кавычки вокруг каждого ключа,
// фигурные скобки, запятые. Поэтому принимаются две записи — либо
// готовый JSON-объект, либо пары `ключ=значение`:
//
//	{"animal":"рысь"}
//	animal=рысь
//	class=птицы limit=3
//	animals=lynx,amur-tiger fields=family,diet
//	id=platypus name="Утконос" weight_kg.min=0.7 weight_kg.max=2.4
//
// Точка в ключе делает вложенный объект, запятая — список, значение в
// кавычках остаётся строкой целиком.
func parseArgs(s string) (map[string]any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}

	if strings.HasPrefix(s, "{") {
		var args map[string]any
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			return nil, fmt.Errorf("JSON не разобрался: %w", err)
		}
		return args, nil
	}

	tokens, err := splitTokens(s)
	if err != nil {
		return nil, err
	}

	args := make(map[string]any, len(tokens))
	for _, token := range tokens {
		key, value, ok := strings.Cut(token, "=")
		if !ok {
			return nil, fmt.Errorf("«%s» не похоже на аргумент: нужен вид ключ=значение", token)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("пустое имя аргумента в «%s»", token)
		}
		if err := assign(args, strings.Split(key, "."), parseValue(value)); err != nil {
			return nil, err
		}
	}
	return args, nil
}

// assign кладёт значение по пути ключей, создавая вложенные объекты.
func assign(args map[string]any, path []string, value any) error {
	last := len(path) - 1
	for _, key := range path[:last] {
		next, ok := args[key]
		if !ok {
			nested := map[string]any{}
			args[key] = nested
			args = nested
			continue
		}
		nested, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("ключ %q задан и как значение, и как объект", key)
		}
		args = nested
	}
	args[path[last]] = value
	return nil
}

// parseValue угадывает тип значения. Угадывание здесь уместно: схему
// инструмента человек перед глазами не держит, а сервер всё равно
// проверит результат по ней и скажет, если тип не тот.
func parseValue(s string) any {
	if quoted, ok := unquote(s); ok {
		return quoted
	}
	if strings.Contains(s, ",") {
		parts := strings.Split(s, ",")
		list := make([]any, 0, len(parts))
		for _, p := range parts {
			list = append(list, parseValue(strings.TrimSpace(p)))
		}
		return list
	}
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n
	}
	return s
}

func unquote(s string) (string, bool) {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1], true
	}
	return s, false
}

// splitTokens режет строку по пробелам, не трогая пробелы внутри
// кавычек: «name="Малая панда"» — один аргумент, а не два.
func splitTokens(s string) ([]string, error) {
	var (
		tokens []string
		token  strings.Builder
		quote  rune
	)
	for _, r := range s {
		switch {
		case quote != 0:
			token.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
			token.WriteRune(r)
		case r == ' ' || r == '\t':
			if token.Len() > 0 {
				tokens = append(tokens, token.String())
				token.Reset()
			}
		default:
			token.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("незакрытая кавычка")
	}
	if token.Len() > 0 {
		tokens = append(tokens, token.String())
	}
	return tokens, nil
}
