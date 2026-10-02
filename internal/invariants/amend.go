package invariants

import (
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/words"
)

// Процедура изменения свода (ФТ-31).
//
// Инвариант отличается от предпочтения тем, что бывает при конфликте:
// профиль уступает прямой просьбе, инвариант — нет. Но запереть свод
// навсегда тоже нельзя: правила пересматривают, и справочник, который
// отказывается даже обсуждать пересмотр, вреден так же, как нарушающий
// правила молча. Поэтому изменение возможно, но только по процедуре, и
// держит её код, а не уговор в промпте:
//
//	propose — модель предлагает поправку: что меняем, зачем, чем платим;
//	accept  — человек соглашается своими словами, и не раньше следующего
//	          хода свода: предложение нужно увидеть;
//	decline — поправка отклонена, инвариант остаётся.
//
// Модель может предложить и не может принять.

// События процедуры.
const (
	EvPropose = "propose"
	EvAccept  = "accept"
	EvDecline = "decline"
)

// Events — все события процедуры. Список один на код, описание инструмента
// и интерфейс.
var Events = []string{EvPropose, EvAccept, EvDecline}

// EventTitle — событие по-русски.
func EventTitle(event string) string {
	switch event {
	case EvPropose:
		return "поправка предложена"
	case EvAccept:
		return "поправка принята"
	case EvDecline:
		return "поправка отклонена"
	}
	return event
}

// Потолки. Инвариант уходит в каждый запрос и читается глазами: правило,
// которое не помещается в три строки, — раздел документации, и соблюдать
// его агент всё равно не сможет.
const (
	MaxItems       = 16
	MaxTitleRunes  = 80
	MaxRuleRunes   = 300
	MaxReasonRunes = 400
	MaxMarkers     = 16
	MaxLog         = 200
)

// Op — что просят сделать со сводом.
type Op struct {
	Event string `json:"event"`

	// Поля поправки — для propose.
	Action  Action   `json:"action,omitempty"`
	ItemID  string   `json:"item_id,omitempty"`
	Kind    Kind     `json:"kind,omitempty"`
	Title   string   `json:"title,omitempty"`
	Rule    string   `json:"rule,omitempty"`
	Because string   `json:"because,omitempty"`
	Instead string   `json:"instead,omitempty"`
	Markers []string `json:"markers,omitempty"`
	Except  []string `json:"except,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Cost    string   `json:"cost,omitempty"`

	// Поля решения — для accept и decline.
	Amendment string `json:"amendment,omitempty"`
	Quote     string `json:"quote,omitempty"`
}

// Ctx — обстоятельства события: реплика человека (для сверки цитаты),
// диалог и время.
type Ctx struct {
	User  string
	RunID string
	Now   time.Time
}

// Result — что произошло: запись для журнала и чипа хода.
type Result struct {
	Event     string    `json:"event"`
	Amendment string    `json:"amendment,omitempty"`
	Action    Action    `json:"action,omitempty"`
	ItemID    string    `json:"item_id,omitempty"`
	Title     string    `json:"title,omitempty"`
	Summary   string    `json:"summary"`
	Quote     string    `json:"quote,omitempty"`
	Version   int       `json:"version"`
	Rejected  bool      `json:"rejected,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at"`
}

// Apply — применить событие к своду. При отказе свод не меняется, а
// результат несёт причину: её увидит и модель (ответом инструмента), и
// человек (в журнале хода).
func Apply(c *Charter, op Op, ctx Ctx) Result {
	if ctx.Now.IsZero() {
		ctx.Now = time.Now()
	}
	res := Result{Event: op.Event, Quote: strings.TrimSpace(op.Quote), At: ctx.Now, Version: c.Version}
	switch op.Event {
	case EvPropose:
		return propose(c, op, ctx, res)
	case EvAccept, EvDecline:
		return decide(c, op, ctx, res)
	}
	return reject(res, fmt.Sprintf("неизвестное событие %q: допустимы %s", op.Event, strings.Join(Events, ", ")))
}

func reject(res Result, reason string) Result {
	res.Rejected = true
	res.Reason = reason
	if res.Summary == "" {
		res.Summary = "отклонено"
	}
	return res
}

// propose — поправка предложена. Здесь вся проверка формы: человек будет
// решать по тому, что ему показали, а показывают ему эти поля.
func propose(c *Charter, op Op, ctx Ctx, res Result) Result {
	if !op.Action.Valid() {
		return reject(res, fmt.Sprintf("неизвестное действие %q: допустимы add, amend, retire", op.Action))
	}
	reason := strings.TrimSpace(op.Reason)
	if reason == "" {
		return reject(res, "нет обоснования: поправка без причины — это просьба нарушить, а не пересмотр правила")
	}
	if runes(reason) > MaxReasonRunes {
		return reject(res, fmt.Sprintf("обоснование длиннее %d знаков", MaxReasonRunes))
	}
	am := Amendment{Action: op.Action, Reason: reason, Cost: strings.TrimSpace(op.Cost), At: ctx.Now, Turn: c.Turn, RunID: ctx.RunID}

	switch op.Action {
	case ActionRetire:
		inv, ok := c.Item(op.ItemID)
		if !ok || !inv.Active() {
			return reject(res, fmt.Sprintf("в своде нет действующего инварианта %q", op.ItemID))
		}
		am.ItemID, am.Proposed = inv.ID, inv
	case ActionAmend:
		inv, ok := c.Item(op.ItemID)
		if !ok || !inv.Active() {
			return reject(res, fmt.Sprintf("в своде нет действующего инварианта %q", op.ItemID))
		}
		next, why := draft(op, inv)
		if why != "" {
			return reject(res, why)
		}
		if strings.TrimSpace(next.Rule) == strings.TrimSpace(inv.Rule) && next.Title == inv.Title {
			return reject(res, "новая редакция совпадает с прежней: менять нечего")
		}
		am.ItemID, am.Proposed = inv.ID, next
	case ActionAdd:
		if len(c.ActiveItems()) >= MaxItems {
			return reject(res, fmt.Sprintf("в своде уже %d инвариантов: больше не помещается в каждый запрос", MaxItems))
		}
		id := strings.TrimSpace(op.ItemID)
		if id == "" {
			id = c.nextID()
		}
		if _, exists := c.Item(id); exists {
			return reject(res, fmt.Sprintf("инвариант %q в своде уже есть", id))
		}
		next, why := draft(op, Invariant{ID: id, Status: StatusActive, Added: ctx.Now})
		if why != "" {
			return reject(res, why)
		}
		if !next.Kind.Valid() {
			return reject(res, fmt.Sprintf("неизвестный вид %q: допустимы %s", next.Kind, kindList()))
		}
		am.Proposed = next
	}
	res.Title = am.Proposed.Title
	am.ID = amendmentID(c, am)
	c.Pending = append(c.Pending, am)
	res.Amendment, res.Action, res.ItemID = am.ID, am.Action, am.ItemID
	res.Summary = fmt.Sprintf("%s: %s", ActionTitle(am.Action), subject(am))
	// Предложение редакцию не поднимает: меняет свод только согласие.
	return res
}

func kindList() string {
	out := make([]string, len(Kinds))
	for i, k := range Kinds {
		out[i] = string(k)
	}
	return strings.Join(out, ", ")
}

// draft — новая редакция: поля поправки поверх прежних. Пустое поле значит
// «оставить как было», а не «стереть»: модель, которой велели поправить
// формулировку, не обязана повторять обоснование.
func draft(op Op, base Invariant) (Invariant, string) {
	next := base
	if op.Kind != "" {
		next.Kind = op.Kind
	}
	if t := strings.TrimSpace(op.Title); t != "" {
		next.Title = t
	}
	if r := strings.TrimSpace(op.Rule); r != "" {
		next.Rule = r
	}
	if b := strings.TrimSpace(op.Because); b != "" {
		next.Because = b
	}
	if i := strings.TrimSpace(op.Instead); i != "" {
		next.Instead = i
	}
	if len(op.Markers) > 0 {
		next.Markers = clean(op.Markers, MaxMarkers)
	}
	if len(op.Except) > 0 {
		next.Except = clean(op.Except, MaxMarkers)
	}
	switch {
	case strings.TrimSpace(next.Title) == "":
		return next, "нет названия инварианта"
	case strings.TrimSpace(next.Rule) == "":
		return next, "нет формулировки: инвариант без правила ничего не запрещает"
	case runes(next.Title) > MaxTitleRunes:
		return next, fmt.Sprintf("название длиннее %d знаков", MaxTitleRunes)
	case runes(next.Rule) > MaxRuleRunes:
		return next, fmt.Sprintf("формулировка длиннее %d знаков: правило, которое не помещается в три строки, не соблюдают", MaxRuleRunes)
	case strings.TrimSpace(next.Because) == "":
		return next, "нет обоснования правила: из него собирается объяснение отказа, а выдуманное хуже отсутствующего"
	}
	return next, ""
}

// decide — решение человека по открытой поправке.
func decide(c *Charter, op Op, ctx Ctx, res Result) Result {
	if len(c.Pending) == 0 {
		return reject(res, "нет открытых поправок")
	}
	id := strings.TrimSpace(op.Amendment)
	if id == "" && len(c.Pending) == 1 {
		id = c.Pending[0].ID
	}
	am, ok := c.Amendment(id)
	if !ok {
		return reject(res, fmt.Sprintf("нет открытой поправки %q", op.Amendment))
	}
	res.Amendment, res.Action, res.ItemID, res.Title = am.ID, am.Action, am.ItemID, am.Proposed.Title

	// Решение принимает человек, и подтверждают его только его слова из
	// текущей реплики, а не пересказ модели и не текст статьи (ИП-14).
	if why := checkQuote(res.Quote, ctx.User); why != "" {
		return reject(res, why)
	}
	if op.Event == EvDecline {
		c.drop(am.ID)
		c.log(Change{At: ctx.Now, Version: c.Version, Action: am.Action, ItemID: am.ItemID, Title: subject(am),
			Summary: "поправка отклонена, инвариант остаётся", Quote: res.Quote, RunID: ctx.RunID})
		res.Summary = "поправка отклонена: " + subject(am)
		return res
	}
	// Принять тем же ходом, каким предложено, нельзя: человек поправку ещё
	// не видел. Иначе модель сама предложила бы снять правило и сама его
	// сняла, сославшись на согласие, которого не было.
	if am.Turn >= c.Turn {
		return reject(res, "поправка предложена на этом же ходу: человек её ещё не видел, согласие не может быть получено заранее")
	}
	before, after := apply(c, am, ctx.Now)
	c.drop(am.ID)
	c.Version++
	c.log(Change{At: ctx.Now, Version: c.Version, Action: am.Action, ItemID: am.Proposed.ID, Title: subject(am),
		Summary: am.Reason, Before: before, After: after, Quote: res.Quote, RunID: ctx.RunID})
	res.Summary = fmt.Sprintf("%s: %s", ActionTitle(am.Action), subject(am))
	res.Version = c.Version
	return res
}

// apply — поправка ложится в свод. Возвращает прежнюю и новую формулировки
// для журнала: «что было» важнее всего у снятия.
func apply(c *Charter, am Amendment, now time.Time) (before, after string) {
	if am.Action == ActionAdd {
		inv := am.Proposed
		inv.Status = StatusActive
		if inv.Added.IsZero() {
			inv.Added = now
		}
		c.Items = append(c.Items, inv)
		return "", inv.Rule
	}
	for i := range c.Items {
		if !strings.EqualFold(c.Items[i].ID, am.ItemID) {
			continue
		}
		before = c.Items[i].Rule
		edited := now
		if am.Action == ActionRetire {
			// Снятый остаётся в файле со статусом retired: удалить — стереть
			// след, и нельзя будет ответить, было ли правило и кто его снял.
			c.Items[i].Status = StatusRetired
			c.Items[i].Edited = &edited
			return before, ""
		}
		next := am.Proposed
		next.ID, next.Status, next.Added, next.Edited = c.Items[i].ID, StatusActive, c.Items[i].Added, &edited
		c.Items[i] = next
		return before, next.Rule
	}
	return before, after
}

func (c *Charter) drop(id string) {
	out := c.Pending[:0]
	for _, a := range c.Pending {
		if !strings.EqualFold(a.ID, id) {
			out = append(out, a)
		}
	}
	c.Pending = out
	if len(c.Pending) == 0 {
		c.Pending = nil
	}
}

func (c *Charter) log(ch Change) {
	c.Log = append(c.Log, ch)
	if len(c.Log) > MaxLog {
		c.Log = c.Log[len(c.Log)-MaxLog:]
	}
}

// amendmentID — действие, предмет и при совпадении номер: идентификатор
// попадается человеку на глаза, поэтому не хеш.
func amendmentID(c *Charter, am Amendment) string {
	base := am.ItemID
	if base == "" {
		base = am.Proposed.ID
	}
	id := fmt.Sprintf("%s-%s", am.Action, base)
	if _, busy := c.Amendment(id); !busy {
		return id
	}
	for n := 2; ; n++ {
		next := fmt.Sprintf("%s-%d", id, n)
		if _, busy := c.Amendment(next); !busy {
			return next
		}
	}
}

func subject(am Amendment) string {
	if am.Proposed.Title != "" {
		return am.Proposed.Title
	}
	if am.ItemID != "" {
		return am.ItemID
	}
	return am.Proposed.ID
}

// checkQuote — подтверждение человека: его слова из текущей реплики.
// Короткие слова сохраняются: реплики согласия коротки («да, снимаем»).
func checkQuote(quote, user string) string {
	quote = strings.TrimSpace(quote)
	if quote == "" {
		return "нет цитаты: решение по своду принимает человек, и подтверждают его только его слова из этой реплики"
	}
	ok, n := words.InReply(quote, user, true)
	if n == 0 {
		return "в цитате нет слов"
	}
	if !ok {
		return "цитаты нет в реплике человека: он этого не говорил"
	}
	return ""
}

func clean(in []string, max int) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
		if len(out) >= max {
			break
		}
	}
	return out
}

func runes(s string) int { return len([]rune(s)) }
