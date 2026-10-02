package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/collection"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Recorder копит итог хода подборки: переходы, отказы, сданную работу.
type Recorder struct {
	mu      sync.Mutex
	changes []collection.Change
	denials []collection.Denial
	works   []string
	last    *collection.State
}

func (r *Recorder) change(c collection.Change, st collection.State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changes = append(r.changes, c)
	r.last = &st
}

func (r *Recorder) deny(d collection.Denial, st collection.State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.denials = append(r.denials, d)
	r.last = &st
}

func (r *Recorder) work(w string, st collection.State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.works = append(r.works, w)
	r.last = &st
}

// Result — итог хода подборки для журнала и ленты.
type Result struct {
	Changes []collection.Change `json:"changes,omitempty"`
	Denials []collection.Denial `json:"denials,omitempty"`
	Works   []string            `json:"works,omitempty"`
	Before  collection.Point    `json:"before"`
	After   collection.Point    `json:"after"`
	Gated   bool                `json:"gated"`
	Grant   Grant               `json:"grant"`
}

// Result — итог по накопленному.
func (r *Recorder) Result(before collection.State, gated bool, g Grant) Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := Result{Changes: append([]collection.Change(nil), r.changes...), Denials: append([]collection.Denial(nil), r.denials...),
		Works: append([]string(nil), r.works...), Before: before.At(), After: before.At(), Gated: gated, Grant: g}
	if r.last != nil {
		res.After = r.last.At()
	}
	return res
}

// Hooks — события для журнала.
type Hooks struct {
	OnChange func(collection.Change, collection.State)
	OnDenial func(Denial, collection.State)
	OnWork   func(string, collection.State)
}

// Deps — всё, что нужно инструментам подборки на ход.
type Deps struct {
	Store *collection.Store
	ID    string
	Title string
	// User — реплика человека: из неё берутся цитаты событий человека.
	User string
	Turn int
	Rec  *Recorder
	Hooks
	// Full — контрольная дорожка: права этапа и предусловия выключены.
	Full bool
	// Deliver собирает карточку вида: координатор кодом (привратник,
	// идентификатор, разделы плана).
	Deliver func(ctx context.Context, name string, sections []string) (card.Card, error)
}

// apply — правка подборки под замком с проверкой прав и предусловий на
// момент вызова. Отказ пишется в файл подборки (ФТ-27).
func (d Deps) apply(tool string, quote string, fn func(st *collection.State) error) (collection.State, error) {
	var denial *Denial
	var inner error
	st, err := d.Store.Update(d.ID, d.Title, func(st *collection.State) bool {
		if !d.Full {
			denial = Check(*st, tool, Ctx{Turn: st.Turn, Quote: quote})
		}
		if denial == nil {
			inner = fn(st)
			if inner == nil {
				return true
			}
			var dn *Denial
			if asDenial(inner, &dn) {
				denial = dn
			} else {
				return false
			}
		}
		st.Deny(denial.Record(st.Turn))
		return true
	})
	if err != nil {
		return st, err
	}
	if denial != nil {
		if d.Rec != nil {
			d.Rec.deny(denial.Record(st.Turn), st)
		}
		if d.OnDenial != nil {
			d.OnDenial(*denial, st)
		}
		return st, fmt.Errorf("%s Сейчас: %s", denial.Error(), st.Summary())
	}
	return st, inner
}

func asDenial(err error, out **Denial) bool {
	if d, ok := err.(*Denial); ok {
		*out = d
		return true
	}
	return false
}

// transition — переход автомата от имени модели.
func (d Deps) transition(tool string, op collection.Op) (string, error) {
	var ch collection.Change
	st, err := d.apply(tool, op.Quote, func(st *collection.State) error {
		ch = collection.Apply(st, op, collection.Ctx{User: d.User, Turn: st.Turn, Source: collection.SourceModel})
		if ch.Rejected {
			return &Denial{What: tool, Gate: GateStage, Stage: st.Stage, Reason: ch.Reason, Available: Of(*st).Tools, Hint: stageHint(*st)}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if d.Rec != nil {
		d.Rec.change(ch, st)
	}
	if d.OnChange != nil {
		d.OnChange(ch, st)
	}
	return stateReply(st)
}

func stateReply(st collection.State) (string, error) {
	e := st.Expected()
	return tools.Result(map[string]any{"ok": true, "stage": st.Stage, "item": st.Current, "paused": st.IsPaused(),
		"expected": collection.ActorTitle(e.Actor) + " — " + e.Text, "tools": Turn(st).Tools})
}

var (
	quoteProp = `"quote": {"type": "string", "description": "Слова человека из его ТЕКУЩЕЙ реплики, в которых читается это решение"}`
)

func tool(name, desc, params string, fn tools.CallFunc) tools.Tool {
	return tools.Func{S: tools.Spec{Name: name, Description: desc, Parameters: json.RawMessage(params)}, Fn: fn}
}

// Tools — инструменты подборки на ход: по правам хода или все сразу на
// контрольной дорожке.
func Tools(d Deps, st collection.State) []tools.Tool {
	g := Turn(st)
	if d.Full {
		g = Full(st)
	}
	var out []tools.Tool
	for _, name := range g.Tools {
		if t := d.toolByName(name); t != nil {
			out = append(out, t)
		}
	}
	return out
}

// Guard оборачивает инструмент источника проверкой прав на момент вызова:
// на ход он выдан, потому что этап может смениться, но вызвать его можно,
// только если этап уже сменился.
func Guard(d Deps, t tools.Tool) tools.Tool {
	name := t.Spec().Name
	return tools.Wrap(t, func(ctx context.Context, args json.RawMessage, next tools.CallFunc) (string, error) {
		if d.Full {
			return next(ctx, args)
		}
		st, err := d.Store.Get(d.ID, d.Title)
		if err != nil {
			return "", err
		}
		if Of(st).Has(name) {
			return next(ctx, args)
		}
		_, err = d.apply(name, "", func(*collection.State) error { return nil })
		if err == nil {
			return next(ctx, args)
		}
		return "", err
	})
}

func (d Deps) toolByName(name string) tools.Tool {
	switch name {
	case ToolPlan:
		return tool(ToolPlan, "Составить или поправить план подборки: цель, виды (по-русски, как их называют люди), разделы, которые читать у каждого вида. Только на этапе плана.",
			`{"type":"object","properties":{"goal":{"type":"string","description":"Для чего подборка, одной-двумя фразами"},"species":{"type":"array","items":{"type":"string"},"description":"Виды по порядку, 2–10"},"sections":{"type":"array","items":{"type":"string","enum":["habitat","diet","lifestyle","breeding","status"]},"description":"Какие разделы читать у каждого вида; по умолчанию ареал и питание"}},"required":["goal","species"]}`,
			func(_ context.Context, args json.RawMessage) (string, error) {
				var op collection.Op
				if err := tools.ParseArgs(args, &op); err != nil {
					return "", err
				}
				op.Event = collection.EvPlan
				return d.transition(ToolPlan, op)
			})
	case ToolApprove:
		return tool(ToolApprove, "Записать, что человек утвердил план. Вызывай, только если человек сам сказал это в текущей реплике, и передай его слова в quote. Утверждение не тем же ходом, в котором план составлен.",
			`{"type":"object","properties":{`+quoteProp+`},"required":["quote"]}`,
			d.userEvent(ToolApprove, collection.EvApprove))
	case ToolAsk:
		return tool(ToolAsk, "Задать человеку вопрос по подборке и ждать ответа.",
			`{"type":"object","properties":{"question":{"type":"string"}},"required":["question"]}`,
			func(_ context.Context, args json.RawMessage) (string, error) {
				var op collection.Op
				if err := tools.ParseArgs(args, &op); err != nil {
					return "", err
				}
				op.Event = collection.EvAsk
				return d.transition(ToolAsk, op)
			})
	case ToolDeliver:
		return tool(ToolDeliver, "Собрать и сдать карточку текущего вида: программа найдёт статью, подтвердит латынь по GBIF и прочитает разделы плана. Карточка ложится в подборку; без неё вид не закрыть.",
			`{"type":"object","properties":{"n":{"type":"integer","description":"Номер текущего вида"}},"required":["n"]}`,
			d.deliver)
	case ToolStepDone:
		return tool(ToolStepDone, "Закрыть текущий вид после того, как его карточка сдана. За один ответ — один вид; после последнего подборка уходит на сверку.",
			`{"type":"object","properties":{"n":{"type":"integer"},"result":{"type":"string","description":"Что вошло в карточку, одной-двумя фразами"}},"required":["n","result"]}`,
			func(_ context.Context, args json.RawMessage) (string, error) {
				var op collection.Op
				if err := tools.ParseArgs(args, &op); err != nil {
					return "", err
				}
				op.Event = collection.EvStepDone
				return d.transition(ToolStepDone, op)
			})
	case ToolReplan:
		return tool(ToolReplan, "Вернуть подборку на этап плана по просьбе человека; его слова — в quote.",
			`{"type":"object","properties":{"note":{"type":"string","description":"Что поменять"},`+quoteProp+`},"required":["quote"]}`,
			d.userEvent(ToolReplan, collection.EvReplan))
	case ToolValidate:
		return tool(ToolValidate, "Свести сверку: программа проверит по сданным карточкам, у всех ли видов подтверждена латынь и прочитаны разделы плана. Твоё — короткий итог для человека.",
			`{"type":"object","properties":{"summary":{"type":"string","description":"Итог сверки для человека"}},"required":["summary"]}`,
			d.validate)
	case ToolAccept:
		return tool(ToolAccept, "Записать, что человек принял подборку. Только его словами из текущей реплики; только после сверки, которая сошлась.",
			`{"type":"object","properties":{`+quoteProp+`},"required":["quote"]}`,
			d.userEvent(ToolAccept, collection.EvAccept))
	case ToolReject:
		return tool(ToolReject, "Вернуть вид на доработку по просьбе человека.",
			`{"type":"object","properties":{"n":{"type":"integer"},"note":{"type":"string","description":"Что доработать"},`+quoteProp+`},"required":["note","quote"]}`,
			d.userEvent(ToolReject, collection.EvReject))
	case ToolPause:
		return tool(ToolPause, "Поставить подборку на паузу по просьбе человека: этап и вид сохранятся.",
			`{"type":"object","properties":{`+quoteProp+`},"required":["quote"]}`,
			d.userEvent(ToolPause, collection.EvPause))
	case ToolResume:
		return tool(ToolResume, "Снять подборку с паузы, когда человек сам к ней вернулся.",
			`{"type":"object","properties":{`+quoteProp+`},"required":["quote"]}`,
			d.userEvent(ToolResume, collection.EvResume))
	}
	return nil
}

func (d Deps) userEvent(name, event string) tools.CallFunc {
	return func(_ context.Context, args json.RawMessage) (string, error) {
		var op collection.Op
		if err := tools.ParseArgs(args, &op); err != nil {
			return "", err
		}
		op.Event = event
		return d.transition(name, op)
	}
}

// deliver — сдача карточки. Права и предусловия проверяются до сборки, а
// не после: собирать карточку, которую нельзя сдать, — деньги на ветер.
func (d Deps) deliver(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		N int `json:"n"`
	}
	if err := tools.ParseArgs(args, &in); err != nil {
		return "", err
	}
	var item collection.Item
	var sections []string
	if _, err := d.apply(ToolDeliver, "", func(st *collection.State) error {
		it := st.CurrentItem()
		if it == nil {
			return &Denial{What: ToolDeliver, Gate: GateStage, Stage: st.Stage, Reason: "текущего вида нет", Available: Of(*st).Tools, Hint: stageHint(*st)}
		}
		if in.N != 0 && in.N != it.N {
			return &Denial{What: ToolDeliver, Gate: GateStage, Stage: st.Stage, Available: Of(*st).Tools,
				Reason: fmt.Sprintf("вид %d не текущий: текущий — %d «%s»", in.N, it.N, it.Name), Hint: fmt.Sprintf("сдай вид %d", it.N)}
		}
		item, sections = *it, st.Sections
		return errNoWrite
	}); err != nil && err != errNoWrite {
		return "", err
	}
	if d.Deliver == nil {
		return "", fmt.Errorf("сборка карточек не подключена")
	}
	c, err := d.Deliver(ctx, item.Name, sections)
	if err != nil {
		return "", fmt.Errorf("карточка вида «%s» не собрана: %v", item.Name, err)
	}
	st, err := d.apply(ToolDeliver, "", func(st *collection.State) error { return collection.Deliver(st, item.N, c, st.Turn) })
	if err != nil {
		return "", err
	}
	title := fmt.Sprintf("сдана карточка вида %d: %s", item.N, c.Title())
	if d.Rec != nil {
		d.Rec.work(title, st)
	}
	if d.OnWork != nil {
		d.OnWork(title, st)
	}
	var read []string
	for _, s := range c.Sections {
		if s.Status == card.SectionRead {
			read = append(read, s.Title+": "+s.Text)
		}
	}
	return tools.Result(map[string]any{"ok": true, "item": item.N, "card": c.Title(), "summary": c.Summary,
		"sections": read, "notes": c.Notes, "next": "покажи карточку человеку и закрой вид: " + ToolStepDone})
}

// errNoWrite — проверка прошла, писать нечего.
var errNoWrite = fmt.Errorf("без записи")

func (d Deps) validate(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Summary string `json:"summary"`
	}
	if err := tools.ParseArgs(args, &in); err != nil {
		return "", err
	}
	var r collection.Report
	st, err := d.apply(ToolValidate, "", func(st *collection.State) error {
		r = collection.Verify(*st, in.Summary, st.Turn)
		collection.SetReport(st, r)
		return nil
	})
	if err != nil {
		return "", err
	}
	title := fmt.Sprintf("сверка: сошлось %d из %d видов", len(r.Checks)-len(r.Failed()), len(r.Checks))
	if d.Rec != nil {
		d.Rec.work(title, st)
	}
	if d.OnWork != nil {
		d.OnWork(title, st)
	}
	return tools.Result(map[string]any{"ok": true, "report": r, "next": "покажи сверку человеку и дождись его решения"})
}

// Prompt — блок состояния подборки (П-4): его ведёт код, а не модель;
// правила только текущего этапа — что выдано, чего нет, когда появится.
func Prompt(st collection.State, gated bool) string {
	var b strings.Builder
	b.WriteString("Состояние подборки — его ведёт код, а не ты. Двигать подборку можно только инструментами; словами этап не меняется.\n")
	fmt.Fprintf(&b, "Подборка «%s». %s.\n", st.Title, st.Summary())
	if st.Goal != "" {
		b.WriteString("Цель: " + st.Goal + "\n")
	}
	if len(st.Items) > 0 && len(st.Sections) > 0 {
		b.WriteString("Разделы у каждого вида: " + sectionTitles(st.Sections) + "\n")
	}
	for _, it := range st.Items {
		mark := map[string]string{collection.ItemDone: "✓", collection.ItemActive: "→", collection.ItemPending: "·"}[it.Status]
		line := fmt.Sprintf("%s %d. %s", mark, it.N, it.Name)
		if it.Output != nil {
			line += " — карточка сдана"
		}
		b.WriteString(line + "\n")
	}
	if st.Feedback != "" {
		b.WriteString("Замечание человека: " + st.Feedback + "\n")
	}
	e := st.Expected()
	b.WriteString("Ожидается: " + collection.ActorTitle(e.Actor) + " — " + e.Text + ".\n")
	if !gated {
		return strings.TrimRight(b.String(), "\n")
	}
	g := Of(st)
	b.WriteString("Выдано сейчас: " + strings.Join(append(append([]string{}, g.Tools...), g.Sources...), ", ") + ".\n")
	for _, l := range g.Locked {
		fmt.Fprintf(&b, "Нет %s: %s; появится %s.\n", l.Tool, l.Why, l.When)
	}
	return strings.TrimRight(b.String(), "\n")
}

// sectionTitles — разделы плана словами: в новом диалоге модель видит план
// только отсюда и иначе пересказывает разделы по памяти.
func sectionTitles(keys []string) string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if t, ok := card.TopicOf(k); ok {
			k = strings.ToLower(t.Title)
		}
		out = append(out, k)
	}
	return strings.Join(out, ", ")
}

// PlainRules — те же правила словами для контрольной дорожки: формулировки
// совпадают дословно с тем, что держат права и предусловия (ИП-13).
const PlainRules = `Порядок работы над подборкой: план → утверждение человеком → сбор по одному виду за ответ → сверка → приём.
- План утверждает человек, и не тем же ходом, в котором он составлен.
- Вид не закрывают без сданной карточки; за один ответ — один вид.
- Подборку не принимают без сверки по всем видам; несошедшийся пункт закрывает приём.
- Согласие человека должно читаться в его словах.`

// Describe — права этапа для интерфейса.
func Describe(st collection.State, gated bool) Grant {
	if !gated {
		return Full(st)
	}
	return Turn(st)
}
