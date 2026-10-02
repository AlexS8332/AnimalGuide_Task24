// Package lifecycle — что подборке сейчас позволено (ФТ-25…ФТ-27): права
// этапа, предусловия переходов и отказ, который учит.
//
// Запрет держится отсутствием инструмента, а не текстом (ИП-7): на плане
// сдавать карточку попросту нечем. Таблица переходов (collection) говорит,
// какие переходы бывают; предусловия — какие из них заслужены, и проверяет
// их код по сделанному, а не промпт.
//
// Инструмент переходов принадлежит этому пакету, потому что зависит от прав.
package lifecycle

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/collection"
)

// Инструменты подборки.
const (
	ToolPlan     = "plan"
	ToolApprove  = "approve"
	ToolAsk      = "ask"
	ToolDeliver  = "deliver"
	ToolStepDone = "step_done"
	ToolReplan   = "replan"
	ToolValidate = "validate"
	ToolAccept   = "accept"
	ToolReject   = "reject"
	ToolPause    = "pause"
	ToolResume   = "resume"
)

// Предусловия (ФТ-26).
const (
	GateStage      = "stage"
	GateTool       = "tool.locked"
	GatePlanSeen   = "plan.seen"
	GateResultSeen = "result.seen"
	GateStepOutput = "step.output"
	GateStepOne    = "step.one"
	GateReport     = "validation.done"
	GateReportOK   = "validation.ok"
	GateConsent    = "user.consent"
)

// sourceByStage — инструменты источников по этапам (ФТ-25).
var sourceByStage = map[collection.Stage][]string{
	collection.Planning:   {"search_wikipedia"},
	collection.Collecting: {"search_wikipedia", "read_wikipedia", "match_taxon", "taxon_tree", "taxon_children", "vernacular_names"},
	collection.Validation: {"read_wikipedia", "match_taxon"},
}

// Locked — закрытый инструмент: почему и когда появится (П-4).
type Locked struct {
	Tool string `json:"tool"`
	Why  string `json:"why"`
	When string `json:"when"`
}

// Grant — права состояния: инструменты подборки, инструменты источников и
// закрытое с объяснением.
type Grant struct {
	Stage   collection.Stage `json:"stage"`
	Paused  bool             `json:"paused"`
	Tools   []string         `json:"tools"`
	Sources []string         `json:"sources"`
	Locked  []Locked         `json:"locked,omitempty"`
}

// Has — выдан ли инструмент (подборки или источника).
func (g Grant) Has(tool string) bool {
	return slices.Contains(g.Tools, tool) || slices.Contains(g.Sources, tool)
}

// Of — права ровно этого состояния.
func Of(s collection.State) Grant {
	g := Grant{Stage: s.Stage, Paused: s.IsPaused()}
	lock := func(tool, why, when string) { g.Locked = append(g.Locked, Locked{tool, why, when}) }
	if s.IsPaused() {
		g.Tools = []string{ToolResume}
		lock(ToolDeliver, "подборка на паузе", "после того как человек к ней вернётся")
		lock(ToolStepDone, "подборка на паузе", "после того как человек к ней вернётся")
		return g
	}
	switch s.Stage {
	case collection.Planning:
		g.Tools = []string{ToolPlan, ToolAsk, ToolPause}
		if len(s.Items) > 0 {
			g.Tools = append(g.Tools, ToolApprove)
		}
		lock(ToolDeliver, "план ещё не утверждён — собирать нечего", "на этапе сбора, после того как человек утвердит план")
		lock(ToolValidate, "сверять нечего: виды ещё не собраны", "на этапе сверки, когда закрыт последний вид")
		lock(ToolAccept, "принимать нечего", "на этапе сверки, когда сверка сведена")
	case collection.Collecting:
		g.Tools = []string{ToolAsk, ToolReplan, ToolPause}
		if s.CurrentItem() != nil {
			g.Tools = append(g.Tools, ToolDeliver, ToolStepDone)
		}
		lock(ToolValidate, "не все виды собраны", "на этапе сверки, когда закрыт последний вид")
		lock(ToolAccept, "подборка ещё собирается", "на этапе сверки, после сверки по всем видам")
		lock(ToolPlan, "план уже утверждён", "если человек попросит пересмотреть план (replan)")
	case collection.Validation:
		g.Tools = []string{ToolValidate, ToolAccept, ToolReject, ToolReplan, ToolPause}
		lock(ToolDeliver, "на сверке новые карточки не собирают", "если человек вернёт вид на доработку (reject)")
		lock(ToolStepDone, "все виды закрыты", "если человек вернёт вид на доработку")
	case collection.Done:
		lock(ToolDeliver, "подборка принята", "никогда: доработка — это новая подборка")
		lock(ToolAccept, "подборка уже принята", "никогда")
	}
	g.Sources = append([]string(nil), sourceByStage[s.Stage]...)
	return g
}

// Turn — права хода: права всех состояний, достижимых за один переход
// (ФТ-25). Набор инструментов собирается до ответа модели, а состояние
// внутри хода успевает уйти вперёд: человек утверждает план, и справочник
// тем же ответом берётся за первый вид. Выданный инструмент — не
// разрешение: каждый вызов проверяется по состоянию на момент вызова.
func Turn(s collection.State) Grant {
	g := Of(s)
	merge := func(o Grant) {
		for _, t := range o.Tools {
			if !slices.Contains(g.Tools, t) {
				g.Tools = append(g.Tools, t)
			}
		}
		for _, t := range o.Sources {
			if !slices.Contains(g.Sources, t) {
				g.Sources = append(g.Sources, t)
			}
		}
	}
	base := s.Clone()
	if base.IsPaused() {
		base.Paused = nil
		merge(Of(base))
	}
	switch base.Stage {
	case collection.Planning:
		if len(base.Items) > 0 {
			next := base.Clone()
			next.Stage, next.Current = collection.Collecting, 1
			merge(Of(next))
		}
	case collection.Collecting:
		open := 0
		for _, it := range base.Items {
			if it.Status != collection.ItemDone {
				open++
			}
		}
		if open <= 1 {
			next := base.Clone()
			next.Stage = collection.Validation
			merge(Of(next))
		}
	case collection.Validation:
		next := base.Clone()
		next.Stage = collection.Collecting
		next.Current = 1
		merge(Of(next))
	}
	var locked []Locked
	for _, l := range g.Locked {
		if !g.Has(l.Tool) {
			locked = append(locked, l)
		}
	}
	g.Locked = locked
	return g
}

// Full — контрольная дорожка: все инструменты сразу, без прав этапа.
func Full(s collection.State) Grant {
	return Grant{Stage: s.Stage, Paused: s.IsPaused(),
		Tools:   []string{ToolPlan, ToolApprove, ToolAsk, ToolDeliver, ToolStepDone, ToolReplan, ToolValidate, ToolAccept, ToolReject, ToolPause, ToolResume},
		Sources: sourceByStage[collection.Collecting]}
}

// Denial — отказ из четырёх частей (ФТ-27, ИП-8): что нельзя, почему, что
// доступно сейчас, что сделать, чтобы стало можно.
type Denial struct {
	What      string           `json:"what"`
	Gate      string           `json:"gate"`
	Stage     collection.Stage `json:"stage"`
	Reason    string           `json:"reason"`
	Available []string         `json:"available"`
	Hint      string           `json:"hint"`
}

func (d Denial) Error() string {
	avail := "ничего"
	if len(d.Available) > 0 {
		avail = strings.Join(d.Available, ", ")
	}
	hint := d.Hint
	if hint == "" {
		hint = "дождаться реплики человека"
	}
	return fmt.Sprintf("Нельзя: %s. Почему: %s. Сейчас доступно: %s. Чтобы стало можно: %s.", d.What, d.Reason, avail, hint)
}

// Record — отказ для файла подборки.
func (d Denial) Record(turn int) collection.Denial {
	return collection.Denial{What: d.What, Gate: d.Gate, Stage: d.Stage, Reason: d.Reason,
		Available: strings.Join(d.Available, ", "), Hint: d.Hint, Turn: turn}
}

// Ctx — обстоятельства проверки: ход подборки, источник, цитата.
type Ctx struct {
	Turn   int
	Manual bool
	Quote  string
}

// gate — предусловие события.
type gate struct {
	event  string
	name   string
	why    func(s collection.State) string
	hint   func(s collection.State) string
	ok     func(s collection.State, c Ctx) bool
	manual bool // проверяется и для кнопки
}

var gates = []gate{
	consent(ToolApprove, "утверждения плана", "попроси утвердить план прямо и дождись ответа: утверждает человек, а не ты за него"),
	consent(ToolAccept, "приёмки подборки", "попроси принять подборку прямо и дождись ответа"),
	consent(ToolPause, "просьбы остановиться", "на паузу подборку ставит человек; если он просит о другом — сделай это"),
	consent(ToolResume, "возвращения к подборке", "с паузы подборку снимает человек"),
	consent(ToolReject, "возврата на доработку", "вернуть вид может только человек своими словами"),
	consent(ToolReplan, "пересмотра плана", "план пересматривают по просьбе человека"),
	{event: ToolApprove, name: GatePlanSeen,
		why: func(collection.State) string {
			return "план составлен на этом же ходу — человек его ещё не видел"
		},
		hint: func(collection.State) string {
			return "покажи план и дождись ответа следующим ходом"
		},
		ok: func(s collection.State, c Ctx) bool { return c.Turn <= 0 || s.PlanTurn < c.Turn }},
	{event: ToolStepDone, name: GateStepOutput, manual: true,
		why: func(s collection.State) string {
			if it := s.CurrentItem(); it != nil {
				return fmt.Sprintf("карточка вида %d «%s» не сдана: рассказать о виде — не то же, что собрать карточку", it.N, it.Name)
			}
			return "карточка вида не сдана"
		},
		hint: func(s collection.State) string {
			n := 0
			if it := s.CurrentItem(); it != nil {
				n = it.N
			}
			return fmt.Sprintf("сначала сдай карточку: %s(n=%d), и только потом закрывай вид", ToolDeliver, n)
		},
		ok: func(s collection.State, _ Ctx) bool { it := s.CurrentItem(); return it != nil && it.Output != nil }},
	{event: ToolStepDone, name: GateStepOne,
		why: func(collection.State) string {
			return "в этом ходу вид уже закрывали: за один ответ — один вид"
		},
		hint: func(collection.State) string {
			return "покажи этот вид и дождись следующей реплики; следующий вид — следующим ходом"
		},
		ok: func(s collection.State, c Ctx) bool {
			if c.Turn <= 0 {
				return true
			}
			for _, ch := range s.Log {
				if ch.Event == collection.EvStepDone && !ch.Rejected && ch.Turn == c.Turn {
					return false
				}
			}
			return true
		}},
	{event: ToolDeliver, name: GateStepOne,
		why: func(collection.State) string {
			return "в этом ходу вид уже собран: за один ответ — один вид"
		},
		hint: func(collection.State) string {
			return "покажи собранный вид и дождись следующей реплики"
		},
		ok: func(s collection.State, c Ctx) bool {
			if c.Turn <= 0 {
				return true
			}
			for _, it := range s.Items {
				if it.Output != nil && it.Output.Turn == c.Turn && it.Status == collection.ItemDone {
					return false
				}
			}
			return true
		}},
	{event: ToolAccept, name: GateResultSeen,
		why: func(collection.State) string {
			return "подборка пришла на сверку на этом же ходу — человек её ещё не видел"
		},
		hint: func(collection.State) string {
			return "покажи сверку и дождись ответа следующим ходом"
		},
		ok: func(s collection.State, c Ctx) bool { return c.Turn <= 0 || s.CheckTurn < c.Turn }},
	{event: ToolAccept, name: GateReport, manual: true,
		why: func(s collection.State) string {
			if s.Report == nil {
				return "сверка не проведена: отчёта нет"
			}
			return "отчёт сверки не покрывает все виды"
		},
		hint: func(collection.State) string {
			return "сначала сведи сверку: " + ToolValidate + "(summary=…)"
		},
		ok: func(s collection.State, _ Ctx) bool {
			if s.Report == nil {
				return false
			}
			for _, it := range s.Items {
				if !s.Report.Covers(it.N) {
					return false
				}
			}
			return true
		}},
	{event: ToolAccept, name: GateReportOK, manual: true,
		why: func(s collection.State) string {
			var parts []string
			for _, c := range s.Report.Failed() {
				parts = append(parts, fmt.Sprintf("вид %d: %s", c.N, strings.Join(c.Issues, "; ")))
			}
			return "сверка не сошлась — " + strings.Join(parts, "; ")
		},
		hint: func(collection.State) string {
			return "несошедшееся не принимают, а дорабатывают: человек возвращает вид (reject), справочник собирает его заново"
		},
		ok: func(s collection.State, _ Ctx) bool { return len(s.Report.Failed()) == 0 }},
}

func consent(event, what, hint string) gate {
	return gate{event: event, name: GateConsent,
		why: func(collection.State) string {
			return "в цитате нет " + what + ": сказанное человеком значит другое"
		},
		hint: func(collection.State) string { return hint },
		ok:   func(_ collection.State, c Ctx) bool { return Decided(event, c.Quote) }}
}

// Check — можно ли вызвать инструмент сейчас: выдан ли он правами этапа и
// выполнены ли предусловия. nil — можно.
func Check(s collection.State, tool string, c Ctx) *Denial {
	g := Of(s)
	if !g.Has(tool) {
		d := &Denial{What: tool, Gate: GateTool, Stage: s.Stage, Available: g.Tools,
			Reason: "на этапе «" + s.Stage.Title() + "» этого инструмента нет"}
		for _, l := range Turn(s).Locked {
			if l.Tool == tool {
				d.Reason, d.Hint = l.Why, "он появится "+l.When
			}
		}
		for _, l := range g.Locked {
			if l.Tool == tool {
				d.Reason, d.Hint = l.Why, "он появится "+l.When
			}
		}
		if d.Hint == "" {
			d.Hint = stageHint(s)
		}
		return d
	}
	for _, gt := range gates {
		if gt.event != tool || (c.Manual && !gt.manual) {
			continue
		}
		if !gt.ok(s, c) {
			return &Denial{What: tool, Gate: gt.name, Stage: s.Stage, Reason: gt.why(s), Hint: gt.hint(s), Available: g.Tools}
		}
	}
	return nil
}

func stageHint(s collection.State) string {
	e := s.Expected()
	if e.Actor == collection.ActorNone {
		return ""
	}
	return "сейчас ожидается: " + collection.ActorTitle(e.Actor) + " — " + e.Text
}

// decisionWords — слова решения человека. Цитата доказывает, что слова были
// сказаны, но не что они значат (урок упражнения 15: «план не нужен, я тебе
// доверяю» подставлялось согласием). Решение должно читаться в словах.
var decisionWords = map[string][]string{
	ToolApprove: {"утвержда", "утвердил", "утверждено", "согласен", "согласна", "принима", "одобря", "поехали", "годится",
		"подходит", "устраивает", "начинай", "собирай", "да", "ага", "ок", "окей", "давай", "давайте", "верно", "хорошо"},
	ToolAccept: {"принима", "принял", "приняла", "согласен", "согласна", "годится", "подходит", "устраивает", "доволен",
		"довольна", "закрывай", "заверша", "готово", "да", "ага", "ок", "окей", "отлично", "хорошо"},
	ToolPause:  {"пауза", "паузу", "паузе", "стоп", "останов", "приостанов", "отлож", "подожд", "перерыв", "позже", "завтра", "хватит"},
	ToolResume: {"продолж", "вернемся", "вернуться", "возвращаемся", "возобнов", "поехали", "дальше", "работаем", "снимай", "сними"},
	ToolReject: {"доработ", "переделай", "переделать", "поправ", "исправ", "верни", "вернуть", "не принима", "не годится", "не подходит"},
	ToolReplan: {"план", "перепланир", "пересмотр", "поменя", "измени", "заново", "замени", "добавь", "убери"},
}

// Decided — читается ли решение в словах человека. Отрицание рядом
// отменяет слово: «не утверждаю» — не согласие.
func Decided(event, quote string) bool {
	ws, ok := decisionWords[event]
	if !ok {
		return true
	}
	folded := " " + fold(quote) + " "
	for _, w := range ws {
		i := strings.Index(folded, " "+w)
		for i >= 0 {
			if !negatedBefore(folded[:i+1]) {
				return true
			}
			next := strings.Index(folded[i+1:], " "+w)
			if next < 0 {
				break
			}
			i += next + 1
		}
	}
	return false
}

func negatedBefore(head string) bool {
	ws := strings.Fields(head)
	for i := len(ws) - 1; i >= 0 && i >= len(ws)-2; i-- {
		if ws[i] == "не" || ws[i] == "нет" || ws[i] == "без" {
			return true
		}
	}
	return false
}

func fold(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r == 'ё':
			b.WriteRune('е')
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
