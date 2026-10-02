// Package facts — карточка фактов ветки диалога: отдельные записи «ключ —
// значение», каждая правится независимо (ФТ-16). Вместе с окном последних
// сообщений она заменяет модели начало истории: то, что ушло из окна, не
// пропадает, а остаётся строкой.
//
// Карточка принадлежит ветке: при ветвлении она копируется снимком точки
// сохранения, дальше у каждой ветки своя (ФТ-19). Пишет её извлекатель —
// тем же единственным запросом на ход, что память и профиль: свой запрос
// сломал бы бюджет в четыре запроса на ход.
package facts

import (
	"fmt"
	"strings"
	"time"
)

// Потолки карточки.
const (
	// DefaultMaxFacts — потолок числа записей: карточка должна быть дешевле
	// истории, которую она заменяет.
	DefaultMaxFacts = 24
	// DefaultMaxValueRunes — потолок длины значения: факт — строка, а не
	// абзац рассуждений.
	DefaultMaxValueRunes = 200
)

// Entry — один факт. Since и Turn показывают, когда он появился и когда
// последний раз менялся.
type Entry struct {
	Key     string     `json:"key"`
	Value   string     `json:"value"`
	Since   int        `json:"since"`
	Turn    int        `json:"turn"`
	Updated *time.Time `json:"updated,omitempty"`
}

// State — карточка фактов.
type State struct {
	Entries []Entry `json:"entries"`
	// Version — сколько раз карточка менялась.
	Version int `json:"version"`
}

// Empty — в карточке нет ни одного факта.
func (s State) Empty() bool { return len(s.Entries) == 0 }

// Runes — размер карточки в символах.
func (s State) Runes() int {
	n := 0
	for _, e := range s.Entries {
		n += len([]rune(e.Key)) + len([]rune(e.Value))
	}
	return n
}

// Clone — глубокая копия: правки в одной ветке не доходят до другой.
func (s State) Clone() State {
	out := s
	out.Entries = make([]Entry, len(s.Entries))
	copy(out.Entries, s.Entries)
	return out
}

// Get — значение факта по ключу.
func (s State) Get(key string) (string, bool) {
	for _, e := range s.Entries {
		if equalKey(e.Key, key) {
			return e.Value, true
		}
	}
	return "", false
}

// Has — есть ли факт.
func (s State) Has(key string) bool {
	_, ok := s.Get(key)
	return ok
}

// Set записывает факт: по существующему ключу значение замещается, новый
// ключ дописывается в конец. Возвращает, изменилось ли что-нибудь.
func (s *State) Set(key, value string, turn int) bool {
	key = strings.TrimSpace(key)
	value = clip(strings.TrimSpace(value), DefaultMaxValueRunes)
	if key == "" || value == "" {
		return false
	}
	now := time.Now()
	for i, e := range s.Entries {
		if equalKey(e.Key, key) {
			if e.Value == value {
				return false
			}
			s.Entries[i].Value, s.Entries[i].Turn, s.Entries[i].Updated = value, turn, &now
			s.Version++
			return true
		}
	}
	s.Entries = append(s.Entries, Entry{Key: key, Value: value, Since: turn, Turn: turn, Updated: &now})
	s.trim(DefaultMaxFacts)
	s.Version++
	return true
}

// Delete убирает факт и говорит, был ли он.
func (s *State) Delete(key string) bool {
	for i, e := range s.Entries {
		if equalKey(e.Key, strings.TrimSpace(key)) {
			s.Entries = append(s.Entries[:i], s.Entries[i+1:]...)
			s.Version++
			return true
		}
	}
	return false
}

// trim удерживает карточку в пределах потолка: выпадает самый давно не
// обновлявшийся факт — карточка про то, где разговор сейчас.
func (s *State) trim(max int) int {
	dropped := 0
	for max > 0 && len(s.Entries) > max {
		oldest := 0
		for i, e := range s.Entries {
			if e.Turn < s.Entries[oldest].Turn {
				oldest = i
			}
		}
		s.Entries = append(s.Entries[:oldest], s.Entries[oldest+1:]...)
		dropped++
	}
	return dropped
}

func equalKey(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// Render — карточка текстом: по строке на факт.
func (s State) Render() string {
	var b strings.Builder
	for _, e := range s.Entries {
		fmt.Fprintf(&b, "- %s: %s\n", e.Key, e.Value)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Prompt — карточка в том виде, в каком её получает модель. Пояснение нужно
// не меньше самих фактов: без него модель принимает карточку за реплику
// пользователя (П-3: каждый блок объясняет себя сам).
func (s State) Prompt() string {
	if s.Empty() {
		return ""
	}
	return "Карточка фактов этого разговора — что пользователь сообщил раньше и о чём вы договорились. Начало разговора дословно не показано: вместо него эта карточка, дословно — только последние сообщения. Считай факты частью диалога; саму карточку не пересказывай.\n\n" + s.Render()
}
