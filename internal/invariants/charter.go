// Package invariants — свод инвариантов справочника (ФТ-29…ФТ-31): правила,
// которые агент не имеет права нарушать, процедура их изменения, страж и
// судья поверх готового ответа.
//
// Чем инвариант отличается от всего, что уже лежит в запросе. Профиль
// говорит, как разговаривать; память — что известно; состояние подборки —
// где мы сейчас. Всё это агент учитывает, но при конфликте уступает
// человеку: просьба сильнее профиля. Инвариант — наоборот: просьба его не
// отменяет. «Скажи всё-таки, какую таблетку дать коту» — не приоритетное
// указание, а конфликт с границей, и правильный ответ на него — отказ, а не
// исполнение.
//
// Отсюда два требования, которые и определили устройство пакета:
//
//  1. Инвариант нельзя отменить репликой в диалоге — иначе это обычное
//     предпочтение с громким названием.
//  2. Но изменить его всё-таки можно: нужна процедура — предложение, явное
//     согласие человека его словами, новая редакция свода, запись в
//     журнале. Предлагает модель, принимает код, решает человек.
//
// Пакет не знает ни про подборку, ни про профиль: свод стоит выше их.
package invariants

import (
	"fmt"
	"strings"
	"time"
)

// Kind — вид инварианта. Виды нарушаются по-разному: достоверность —
// ответом по памяти или подменой похожим, границы советов — инструкцией,
// тон — оценкой.
type Kind string

const (
	KindSources Kind = "sources" // откуда берутся сведения
	KindSafety  Kind = "safety"  // советы и безопасность
	KindTone    Kind = "tone"    // как говорить о животных
)

// Kinds — виды по порядку, для описания инструмента и ошибок.
var Kinds = []Kind{KindSources, KindSafety, KindTone}

// KindTitle — название вида для человека и для блока в запросе.
func KindTitle(k Kind) string {
	switch k {
	case KindSources:
		return "достоверность"
	case KindSafety:
		return "советы и безопасность"
	case KindTone:
		return "тон"
	}
	return string(k)
}

// Valid — вид из перечня. Неизвестный вид в файле — сломанный файл, а не
// повод молча подставить «прочее».
func (k Kind) Valid() bool {
	for _, x := range Kinds {
		if k == x {
			return true
		}
	}
	return false
}

// Status — жив ли инвариант. Снятый не удаляется из свода: в журнале должно
// быть видно, что ограничение снимали, когда и какими словами.
type Status string

const (
	StatusActive  Status = "active"
	StatusRetired Status = "retired"
)

// Invariant — одно правило свода.
//
// Because и Instead существуют ради отказа (ИП-8): «нельзя, это нарушает
// И-4» бесполезно, человек не понимает, почему, и не знает, что делать
// дальше. Обоснование и замена лежат рядом с правилом и попадают в запрос
// вместе с ним — агенту не нужно их выдумывать.
type Invariant struct {
	ID      string `json:"id"`
	Kind    Kind   `json:"kind"`
	Title   string `json:"title"`
	Rule    string `json:"rule"`
	Because string `json:"because,omitempty"`
	Instead string `json:"instead,omitempty"`

	// Markers — слова, по которым страж отбирает подозрительные фрагменты
	// ответа. Это не список запрещённых слов: «с дозировкой лекарств — к
	// ветеринару» — соблюдение, а не нарушение. Маркер даёт кандидата,
	// приговор выносит судья.
	Markers []string `json:"markers,omitempty"`
	// Except — что не считается нарушением, хотя выглядит похоже. Без
	// оговорок инвариант запрещает больше задуманного, а слишком широкое
	// ограничение обходят, а не соблюдают.
	Except []string `json:"except,omitempty"`

	Status Status     `json:"status"`
	Added  time.Time  `json:"added"`
	Edited *time.Time `json:"edited,omitempty"`
}

// Active — инвариант действует.
func (inv Invariant) Active() bool { return inv.Status != StatusRetired }

// Line — одна строка инварианта для чипа и журнала.
func (inv Invariant) Line() string {
	return fmt.Sprintf("[%s] %s — %s", inv.ID, inv.Title, inv.Rule)
}

// Action — что делает поправка со сводом.
type Action string

const (
	ActionAdd    Action = "add"    // новый инвариант
	ActionAmend  Action = "amend"  // новая редакция существующего
	ActionRetire Action = "retire" // снять ограничение
)

// ActionTitle — действие поправки по-русски.
func ActionTitle(a Action) string {
	switch a {
	case ActionAdd:
		return "добавить"
	case ActionAmend:
		return "изменить"
	case ActionRetire:
		return "снять"
	}
	return string(a)
}

// Valid — действие из перечня.
func (a Action) Valid() bool {
	switch a {
	case ActionAdd, ActionAmend, ActionRetire:
		return true
	}
	return false
}

// Amendment — предложенная поправка, ждущая решения человека. Лежит в
// своде, а не в диалоге: предложить могут в одном разговоре, а согласиться
// — в другом, на следующий день.
type Amendment struct {
	ID       string    `json:"id"`
	Action   Action    `json:"action"`
	ItemID   string    `json:"item_id,omitempty"`
	Proposed Invariant `json:"proposed"`
	Reason   string    `json:"reason"`
	Cost     string    `json:"cost,omitempty"`
	At       time.Time `json:"at"`
	// Turn — ход свода, на котором поправка предложена.
	Turn  int    `json:"turn"`
	RunID string `json:"run_id,omitempty"`
}

// Change — запись журнала свода: что изменилось и какими словами это
// подтвердил человек. Через полгода вопрос «кто разрешил советовать
// лекарства?» должен иметь ответ в файле.
type Change struct {
	At      time.Time `json:"at"`
	Version int       `json:"version"`
	Action  Action    `json:"action"`
	ItemID  string    `json:"item_id"`
	Title   string    `json:"title"`
	Summary string    `json:"summary"`
	Before  string    `json:"before,omitempty"`
	After   string    `json:"after,omitempty"`
	Quote   string    `json:"quote,omitempty"`
	RunID   string    `json:"run_id,omitempty"`
}

// Charter — свод справочника.
//
// Version — редакция: растёт только на принятой поправке и уходит в блок
// запроса. Turn — сквозной счётчик ходов свода через все диалоги: правило
// «принять не раньше следующего хода» не может опираться на номер хода
// диалога — предлагают в одном разговоре, принимают в другом.
type Charter struct {
	ID      string      `json:"id"`
	Title   string      `json:"title"`
	Version int         `json:"version"`
	Items   []Invariant `json:"items"`
	Pending []Amendment `json:"pending,omitempty"`
	Log     []Change    `json:"log,omitempty"`
	Turn    int         `json:"turn"`
	Created time.Time   `json:"created"`
	Updated time.Time   `json:"updated"`
}

// New — пустой свод.
func New(id, title string) Charter {
	now := time.Now()
	return Charter{ID: id, Title: title, Version: 1, Items: []Invariant{}, Created: now, Updated: now}
}

// ActiveItems — действующие инварианты в порядке файла. Порядок не
// переставляется: блок свода стоит в начале запроса и кэшируется, и
// перестановка рассыпала бы кэш префикса на ровном месте.
func (c Charter) ActiveItems() []Invariant {
	out := make([]Invariant, 0, len(c.Items))
	for _, inv := range c.Items {
		if inv.Active() {
			out = append(out, inv)
		}
	}
	return out
}

// Item — инвариант по идентификатору, включая снятый: на него может
// сослаться журнал или старая поправка.
func (c Charter) Item(id string) (Invariant, bool) {
	id = strings.TrimSpace(id)
	for _, inv := range c.Items {
		if strings.EqualFold(inv.ID, id) {
			return inv, true
		}
	}
	return Invariant{}, false
}

// Find — действующие инварианты по списку идентификаторов, в порядке свода
// и без повторов: модель может назвать один дважды или назвать снятый.
func (c Charter) Find(ids []string) []Invariant {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[strings.ToLower(strings.TrimSpace(id))] = true
	}
	var out []Invariant
	for _, inv := range c.ActiveItems() {
		if want[strings.ToLower(inv.ID)] {
			out = append(out, inv)
		}
	}
	return out
}

// Amendment — открытая поправка по идентификатору.
func (c Charter) Amendment(id string) (Amendment, bool) {
	for _, a := range c.Pending {
		if strings.EqualFold(a.ID, strings.TrimSpace(id)) {
			return a, true
		}
	}
	return Amendment{}, false
}

// Empty — действующих инвариантов нет.
func (c Charter) Empty() bool { return len(c.ActiveItems()) == 0 }

// Summary — свод одной строкой для пульта и журнала.
func (c Charter) Summary() string {
	s := fmt.Sprintf("редакция %d, действует %d", c.Version, len(c.ActiveItems()))
	if n := len(c.Pending); n > 0 {
		s += fmt.Sprintf(", открытых поправок: %d", n)
	}
	return s
}

// nextID — идентификатор нового инварианта, если модель его не назвала:
// «И-7» после «И-6». Номер не переиспользуется и после снятия.
func (c Charter) nextID() string {
	max := 0
	for _, inv := range c.Items {
		var n int
		if _, err := fmt.Sscanf(inv.ID, "И-%d", &n); err == nil && n > max {
			max = n
		}
	}
	for _, a := range c.Pending {
		var n int
		if _, err := fmt.Sscanf(a.Proposed.ID, "И-%d", &n); err == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("И-%d", max+1)
}
