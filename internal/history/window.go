package history

import (
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// DefaultWindow — сколько последних сообщений уходит модели дословно (ФТ-16).
const DefaultWindow = 8

// DefaultKeepToolRunes — до скольких символов сокращать ответы инструментов
// прошлых ходов.
const DefaultKeepToolRunes = 600

// compactNote — чем помечено сокращение: модель должна знать, что текста
// было больше и что его можно прочитать снова.
const compactNote = " …[сокращено: полный ответ был раньше в этом разговоре, при необходимости вызови инструмент снова]"

// Compact сокращает ответы инструментов прошлых ходов до keep символов.
// Сокращение детерминированно: одинаковая история даёт одинаковый запрос, и
// кэш префикса не ломается. Пометка «данные источника» сохраняется —
// режутся данные внутри неё. keep <= 0 — не сокращать.
func Compact(ms []llm.Message, keep int) []llm.Message {
	if keep <= 0 {
		return ms
	}
	out := make([]llm.Message, len(ms))
	for i, m := range ms {
		if m.Role == llm.RoleTool {
			m.Content = tools.Rewrap(m.Content, func(s string) string {
				if r := []rune(s); len(r) > keep {
					return string(r[:keep]) + compactNote
				}
				return s
			})
		}
		out[i] = m
	}
	return out
}

// Window — последние n сообщений, начиная с реплики пользователя: ответ
// инструмента без вызова, который его породил, API отвергает, и окно не
// может начинаться с середины хода. n <= 0 — вся история.
func Window(ms []llm.Message, n int) []llm.Message {
	if n <= 0 || len(ms) <= n {
		return ms
	}
	start := len(ms) - n
	for start < len(ms) && ms[start].Role != llm.RoleUser {
		start++
	}
	return ms[start:]
}
