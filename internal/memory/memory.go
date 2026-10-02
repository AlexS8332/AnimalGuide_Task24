// Package memory — рабочая и долговременная память (ФТ-17). Краткосрочная
// живёт в файле диалога и сюда не попадает.
//
// Слои различаются не важностью, а адресом:
//
//	долговременная — memory/long/<человек>.json — интересы, закладки,
//	                 «уже читал»; не пропадает;
//	рабочая        — memory/work/<подборка>.json — что собрано по подборке;
//	                 пропадает со сменой подборки.
//
// Забыть при смене подборки — не операция, а свойство: диалог просто
// начинает ссылаться на другой файл (ИП-5). Один ключ живёт в одном слое
// (ИП-4), а ключи, которыми ведает анкета профиля или карточка животного,
// память не пишет вовсе: каждое хранилище объявляет свои зарезервированные
// ключи, и правила раскладки их уважают.
package memory

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Слои.
const (
	LayerLong = "long"
	LayerWork = "work"
)

// Title — слой словами.
func Title(layer string) string {
	switch layer {
	case LayerLong:
		return "долговременная"
	case LayerWork:
		return "рабочая"
	}
	return layer
}

// TitleIn — слой в предложном падеже: «в долговременной».
func TitleIn(layer string) string {
	switch layer {
	case LayerLong:
		return "в долговременной"
	case LayerWork:
		return "в рабочей"
	}
	return "в слое " + layer
}

// Ключи долговременной памяти, которые ведёт код, а не извлекатель:
// закладки ставит человек кнопкой, «уже читал» — карточки, открытые в
// разговорах. Извлекатель их не пишет (ИП-4).
const (
	KeyBookmarks = "закладки"
	KeyRead      = "уже читал"
)

// Потолки слоёв и записи.
const (
	DefaultMaxLong       = 20
	DefaultMaxWork       = 24
	DefaultMaxValueRunes = 240
	// maxListItems — сколько названий держать в списочных записях
	// («уже читал», «закладки»): старые выпадают первыми.
	maxListItems = 30
)

// Entry — запись слоя.
type Entry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Since — ход, на котором запись появилась, Turn — ход последней правки.
	Since   int        `json:"since"`
	Turn    int        `json:"turn"`
	Source  string     `json:"source,omitempty"`
	Updated *time.Time `json:"updated,omitempty"`
}

// Card — слой одного владельца: человека или подборки.
type Card struct {
	Layer   string     `json:"layer"`
	ID      string     `json:"id"`
	Title   string     `json:"title,omitempty"`
	Entries []Entry    `json:"entries"`
	Version int        `json:"version"`
	Updated *time.Time `json:"updated,omitempty"`
}

// NewCard — пустой слой владельца.
func NewCard(layer, id, title string) Card {
	return Card{Layer: layer, ID: id, Title: title, Entries: []Entry{}}
}

// Empty — в слое нет записей.
func (c Card) Empty() bool { return len(c.Entries) == 0 }

// Runes — размер слоя в символах.
func (c Card) Runes() int {
	n := 0
	for _, e := range c.Entries {
		n += len([]rune(e.Key)) + len([]rune(e.Value))
	}
	return n
}

// Clone — глубокая копия.
func (c Card) Clone() Card {
	out := c
	out.Entries = append([]Entry{}, c.Entries...)
	return out
}

// Get — значение по ключу.
func (c Card) Get(key string) (string, bool) {
	for _, e := range c.Entries {
		if EqualKey(e.Key, key) {
			return e.Value, true
		}
	}
	return "", false
}

// Set записывает значение: по существующему ключу — замещает. Возвращает
// прежнее значение и было ли изменение.
func (c *Card) Set(key, value string, turn int, source string) (string, bool) {
	key, value = cleanKey(key), clip(strings.TrimSpace(value), DefaultMaxValueRunes)
	if key == "" || value == "" {
		return "", false
	}
	now := time.Now()
	for i, e := range c.Entries {
		if EqualKey(e.Key, key) {
			if e.Value == value {
				return e.Value, false
			}
			old := e.Value
			c.Entries[i].Value, c.Entries[i].Turn, c.Entries[i].Source, c.Entries[i].Updated = value, turn, source, &now
			c.touch()
			return old, true
		}
	}
	c.Entries = append(c.Entries, Entry{Key: key, Value: value, Since: turn, Turn: turn, Source: source, Updated: &now})
	c.touch()
	return "", true
}

// Delete убирает запись и говорит, была ли она.
func (c *Card) Delete(key string) (string, bool) {
	for i, e := range c.Entries {
		if EqualKey(e.Key, key) {
			c.Entries = append(c.Entries[:i], c.Entries[i+1:]...)
			c.touch()
			return e.Value, true
		}
	}
	return "", false
}

// AddToList дописывает название в списочную запись («закладки», «уже
// читал») без повторов; самые старые выпадают по потолку.
func (c *Card) AddToList(key, item string, turn int) bool {
	item = strings.TrimSpace(item)
	if item == "" {
		return false
	}
	cur, _ := c.Get(key)
	var items []string
	for _, x := range strings.Split(cur, ";") {
		if x = strings.TrimSpace(x); x != "" && !strings.EqualFold(x, item) {
			items = append(items, x)
		}
	}
	items = append(items, item)
	if len(items) > maxListItems {
		items = items[len(items)-maxListItems:]
	}
	next := strings.Join(items, "; ")
	if next == cur {
		return false
	}
	// Списки длиннее обычной записи: потолок значения для них не режет
	// хвост, а выбрасывает старые названия.
	for len([]rune(next)) > DefaultMaxValueRunes*4 && len(items) > 1 {
		items = items[1:]
		next = strings.Join(items, "; ")
	}
	now := time.Now()
	for i, e := range c.Entries {
		if EqualKey(e.Key, key) {
			c.Entries[i].Value, c.Entries[i].Turn, c.Entries[i].Source, c.Entries[i].Updated = next, turn, SourceCode, &now
			c.touch()
			return true
		}
	}
	c.Entries = append(c.Entries, Entry{Key: key, Value: next, Since: turn, Turn: turn, Source: SourceCode, Updated: &now})
	c.touch()
	return true
}

// Источники записи.
const (
	SourceExtract = "extract"
	SourceHuman   = "human"
	SourceCode    = "code"
)

func (c *Card) touch() {
	now := time.Now()
	c.Version++
	c.Updated = &now
}

// Trim удерживает слой в пределах потолка: выпадают самые давно не
// обновлявшиеся записи, кроме списков, которые ведёт код. Возвращает
// выброшенные ключи — их видно в журнале.
func (c *Card) Trim(max int) []string {
	var dropped []string
	for max > 0 && len(c.Entries) > max {
		oldest := -1
		for i, e := range c.Entries {
			if e.Source == SourceCode {
				continue
			}
			if oldest < 0 || e.Turn < c.Entries[oldest].Turn {
				oldest = i
			}
		}
		if oldest < 0 {
			break
		}
		dropped = append(dropped, c.Entries[oldest].Key)
		c.Entries = append(c.Entries[:oldest], c.Entries[oldest+1:]...)
	}
	if len(dropped) > 0 {
		c.touch()
	}
	return dropped
}

// Render — записи текстом, по строке.
func (c Card) Render() string {
	if c.Empty() {
		return "(пусто)"
	}
	var b strings.Builder
	for _, e := range c.Entries {
		fmt.Fprintf(&b, "- %s: %s\n", e.Key, e.Value)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Prompt — слой блоком запроса. Каждый блок объясняет себя сам и называется
// по-своему (П-3): одинаковое пояснение на два блока не годится — модель
// перестаёт их различать.
func (c Card) Prompt() string {
	if c.Empty() {
		return ""
	}
	switch c.Layer {
	case LayerLong:
		return "Известное о человеке вообще, между разговорами (долговременная память): его интересы, закладки, что он уже читал. Это сведения, а не правила ответа; не пересказывай их без повода.\n\n" + c.Render()
	case LayerWork:
		head := "Что уже собрано по текущей подборке (рабочая память)"
		if c.Title != "" {
			head += " «" + c.Title + "»"
		}
		return head + ". Она живёт, пока идёт подборка.\n\n" + c.Render()
	}
	return c.Render()
}

// EqualKey — ключи без учёта регистра, краевых пробелов и «ё».
func EqualKey(a, b string) bool { return normKey(a) == normKey(b) }

func normKey(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "ё", "е")
}

func cleanKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// Change — правка памяти за ход: что, куда и почему. Отклонённые правки
// тоже правки: по ним видно, где правило поправило модель (ФТ-18).
type Change struct {
	Op     string `json:"op"`
	Layer  string `json:"layer"`
	From   string `json:"from,omitempty"`
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Old    string `json:"old,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Виды правок.
const (
	OpSet    = "set"
	OpDelete = "delete"
	OpMove   = "move"
	OpSkip   = "skip"
)

// Changes — правки хода.
type Changes []Change

// Applied — правки, которые что-то изменили.
func (cs Changes) Applied() Changes {
	var out Changes
	for _, c := range cs {
		if c.Op != OpSkip {
			out = append(out, c)
		}
	}
	return out
}

// Summary — правки одной строкой для журнала.
func (cs Changes) Summary() string {
	var parts []string
	for _, c := range cs {
		switch c.Op {
		case OpSet:
			parts = append(parts, fmt.Sprintf("%s «%s»", TitleIn(c.Layer), c.Key))
		case OpDelete:
			parts = append(parts, fmt.Sprintf("удалено «%s» %s", c.Key, TitleIn(c.Layer)))
		case OpMove:
			parts = append(parts, fmt.Sprintf("«%s» перенесено %s", c.Key, TitleIn(c.Layer)))
		case OpSkip:
			parts = append(parts, fmt.Sprintf("отклонено «%s»", c.Key))
		}
	}
	if len(parts) == 0 {
		return "без правок"
	}
	return strings.Join(parts, ", ")
}

// SortedKeys — ключи слоя по алфавиту (для отчёта и тестов).
func (c Card) SortedKeys() []string {
	out := make([]string, len(c.Entries))
	for i, e := range c.Entries {
		out[i] = e.Key
	}
	sort.Strings(out)
	return out
}
