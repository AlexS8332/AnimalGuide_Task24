package collection

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/words"
)

// События подборки.
const (
	EvPlan     = "plan"
	EvApprove  = "approve"
	EvAsk      = "ask"
	EvStepDone = "step_done"
	EvAccept   = "accept"
	EvReject   = "reject"
	EvReplan   = "replan"
	EvPause    = "pause"
	EvResume   = "resume"
)

// Rule — строка таблицы переходов: из каких этапов событие бывает, кто его
// инициирует и куда ведёт.
type Rule struct {
	Event  string  `json:"event"`
	Actor  string  `json:"actor"`
	From   []Stage `json:"from"`
	To     Stage   `json:"to,omitempty"`
	Also   Stage   `json:"also,omitempty"`
	Title  string  `json:"title"`
	Paused bool    `json:"paused,omitempty"`
}

// Rules — таблица переходов.
var Rules = []Rule{
	{Event: EvPlan, Actor: ActorAgent, From: []Stage{Planning}, Title: "составить или поправить план подборки"},
	{Event: EvApprove, Actor: ActorUser, From: []Stage{Planning}, To: Collecting, Title: "утвердить план"},
	{Event: EvAsk, Actor: ActorAgent, From: []Stage{Planning, Collecting}, Title: "спросить человека и ждать ответа"},
	{Event: EvStepDone, Actor: ActorAgent, From: []Stage{Collecting}, Also: Validation, Title: "закрыть текущий вид; после последнего — на сверку"},
	{Event: EvAccept, Actor: ActorUser, From: []Stage{Validation}, To: Done, Title: "принять подборку"},
	{Event: EvReject, Actor: ActorUser, From: []Stage{Validation}, To: Collecting, Title: "вернуть вид на доработку"},
	{Event: EvReplan, Actor: ActorUser, From: []Stage{Collecting, Validation}, To: Planning, Title: "пересмотреть план"},
	{Event: EvPause, Actor: ActorUser, From: []Stage{Planning, Collecting, Validation}, Title: "поставить на паузу"},
	{Event: EvResume, Actor: ActorUser, From: []Stage{Planning, Collecting, Validation}, Paused: true, Title: "снять с паузы"},
}

// RuleOf — строка таблицы по событию.
func RuleOf(event string) (Rule, bool) {
	for _, r := range Rules {
		if r.Event == event {
			return r, true
		}
	}
	return Rule{}, false
}

// Allowed — события, которые таблица допускает сейчас.
func (s State) Allowed() []string {
	var out []string
	for _, r := range Rules {
		if r.Paused != s.IsPaused() {
			continue
		}
		if r.Event == EvApprove && len(s.Items) == 0 {
			continue
		}
		if slices.Contains(r.From, s.Stage) {
			out = append(out, r.Event)
		}
	}
	return out
}

// Потолки.
const (
	MaxItems       = 10
	MaxItemRunes   = 80
	MaxResultRunes = 400
	MaxNoteRunes   = 400
)

// Op — событие, предложенное моделью или кнопкой.
type Op struct {
	Event    string   `json:"event"`
	Goal     string   `json:"goal,omitempty"`
	Items    []string `json:"species,omitempty"`
	Sections []string `json:"sections,omitempty"`
	Item     int      `json:"n,omitempty"`
	Result   string   `json:"result,omitempty"`
	Question string   `json:"question,omitempty"`
	Note     string   `json:"note,omitempty"`
	Quote    string   `json:"quote,omitempty"`
}

// Источники события.
const (
	SourceModel  = "model"
	SourceManual = "manual"
)

// Ctx — обстоятельства события.
type Ctx struct {
	User   string
	Turn   int
	Source string
	Now    time.Time
}

// Change — принятый или отклонённый переход.
type Change struct {
	Event    string    `json:"event"`
	Source   string    `json:"source"`
	Turn     int       `json:"turn,omitempty"`
	Time     time.Time `json:"time"`
	From     Point     `json:"from"`
	To       Point     `json:"to"`
	Note     string    `json:"note,omitempty"`
	Quote    string    `json:"quote,omitempty"`
	Rejected bool      `json:"rejected,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

// Title — переход словами.
func (c Change) Title() string {
	name := EventTitle(c.Event)
	if c.Rejected {
		return fmt.Sprintf("%s — отклонено: %s", name, c.Reason)
	}
	if c.From == c.To {
		return fmt.Sprintf("%s (%s)", name, c.To)
	}
	return fmt.Sprintf("%s: %s → %s", name, c.From, c.To)
}

// EventTitle — событие словами.
func EventTitle(event string) string {
	switch event {
	case EvPlan:
		return "план подборки"
	case EvApprove:
		return "план утверждён"
	case EvAsk:
		return "вопрос человеку"
	case EvStepDone:
		return "вид закрыт"
	case EvAccept:
		return "подборка принята"
	case EvReject:
		return "возврат на доработку"
	case EvReplan:
		return "пересмотр плана"
	case EvPause:
		return "пауза"
	case EvResume:
		return "продолжение"
	}
	return event
}

// Apply — переход по таблице. Здесь проверяется только «бывает ли такое
// событие отсюда» и корректность данных; «заслужен ли переход» (права этапа
// и предусловия) — дело lifecycle.
func Apply(s *State, op Op, c Ctx) Change {
	if c.Now.IsZero() {
		c.Now = time.Now()
	}
	if c.Source == "" {
		c.Source = SourceModel
	}
	op.Event = strings.TrimSpace(op.Event)
	ch := Change{Event: op.Event, Source: c.Source, Turn: c.Turn, Time: c.Now, From: s.At(), Quote: strings.TrimSpace(op.Quote)}
	reject := func(format string, args ...any) Change {
		ch.Rejected, ch.Reason, ch.To = true, fmt.Sprintf(format, args...), ch.From
		return ch
	}
	rule, ok := RuleOf(op.Event)
	switch {
	case !ok:
		return reject("такого события нет; допустимы: %s", strings.Join(s.Allowed(), ", "))
	case s.Stage == Done:
		return reject("подборка принята — переходов из «принята» нет")
	case rule.Paused != s.IsPaused() && s.IsPaused():
		return reject("подборка на паузе: сначала resume")
	case rule.Paused != s.IsPaused():
		return reject("подборка не на паузе")
	case !slices.Contains(rule.From, s.Stage):
		return reject("с этапа «%s» такого перехода нет; допустимы: %s", s.Stage.Title(), strings.Join(s.Allowed(), ", "))
	}
	if rule.Actor == ActorUser && c.Source != SourceManual {
		if reason := CheckQuote(ch.Quote, c.User); reason != "" {
			return reject("%s", reason)
		}
	}
	next := s.Clone()
	var err error
	switch op.Event {
	case EvPlan:
		err = next.plan(op, c.Turn)
		ch.Note = next.planNote()
	case EvApprove:
		if len(next.Items) == 0 {
			return reject("утверждать нечего: плана ещё нет")
		}
		next.Stage, next.Question = Collecting, ""
		next.activate(firstOpen(next.Items))
	case EvAsk:
		q := clip(op.Question, MaxNoteRunes)
		if q == "" {
			return reject("вопрос пустой")
		}
		next.Question, next.QuestionTurn = q, c.Turn
		ch.Note = q
	case EvStepDone:
		err = next.stepDone(op, c.Turn)
		ch.Note = clip(op.Result, MaxResultRunes)
	case EvAccept:
		next.Stage, next.Feedback = Done, ""
	case EvReject:
		err = next.sendBack(op)
		ch.Note = next.Feedback
	case EvReplan:
		next.Stage, next.Current, next.Question, next.PlanTurn, next.Report = Planning, 0, "", c.Turn, nil
		for i := range next.Items {
			if next.Items[i].Status == ItemActive {
				next.Items[i].Status = ItemPending
			}
		}
		ch.Note = clip(op.Note, MaxNoteRunes)
	case EvPause:
		next.Paused = &Pause{At: c.Now, Turn: c.Turn, Quote: ch.Quote}
	case EvResume:
		next.Paused = nil
	}
	if err != nil {
		return reject("%s", err.Error())
	}
	ch.To = next.At()
	next.touch(c.Now)
	next.Log = append(next.Log, ch)
	if len(next.Log) > MaxLog {
		next.Log = next.Log[len(next.Log)-MaxLog:]
	}
	*s = next
	return ch
}

func (s *State) touch(now time.Time) {
	s.Version++
	s.Updated = &now
	if s.Created == nil {
		s.Created = &now
	}
}

func (s *State) plan(op Op, turn int) error {
	goal := clip(op.Goal, MaxNoteRunes)
	if goal == "" && s.Goal == "" {
		return fmt.Errorf("нужна цель подборки (goal): для чего она")
	}
	var items []Item
	seen := map[string]bool{}
	for _, name := range op.Items {
		name = clip(name, MaxItemRunes)
		key := strings.ToLower(name)
		if name == "" || seen[key] {
			continue
		}
		seen[key] = true
		items = append(items, Item{N: len(items) + 1, Name: name, Status: ItemPending})
	}
	switch {
	case len(items) == 0:
		return fmt.Errorf("в плане нет видов")
	case len(items) > MaxItems:
		return fmt.Errorf("видов %d, а потолок — %d", len(items), MaxItems)
	}
	if goal != "" {
		s.Goal = goal
	}
	var secs []string
	for _, k := range op.Sections {
		if t, ok := card.TopicOf(k); ok && !slices.Contains(secs, t.Key) {
			secs = append(secs, t.Key)
		}
	}
	if len(secs) == 0 {
		secs = append(secs, DefaultSections...)
	}
	s.Sections = secs
	s.Items, s.Current, s.Question, s.PlanTurn, s.Report = items, 0, "", turn, nil
	return nil
}

func (s *State) stepDone(op Op, turn int) error {
	it := s.CurrentItem()
	if it == nil {
		return fmt.Errorf("текущего вида нет")
	}
	if op.Item != 0 && op.Item != it.N {
		return fmt.Errorf("вид %d не текущий: текущий — %d «%s», виды идут по порядку", op.Item, it.N, it.Name)
	}
	result := clip(op.Result, MaxResultRunes)
	if result == "" {
		return fmt.Errorf("нужен итог по виду (result) — одной-двумя фразами")
	}
	it.Status, it.Result, it.Turn = ItemDone, result, turn
	s.Question = ""
	if n := firstOpen(s.Items); n > 0 {
		s.activate(n)
		return nil
	}
	s.Current, s.Stage, s.CheckTurn = 0, Validation, turn
	return nil
}

func (s *State) sendBack(op Op) error {
	n := op.Item
	if n == 0 {
		if f := s.Report.Failed(); len(f) > 0 {
			n = f[0].N
		} else {
			n = len(s.Items)
		}
	}
	it := s.Item(n)
	if it == nil {
		return fmt.Errorf("вида %d в плане нет: видов %d", n, len(s.Items))
	}
	note := clip(op.Note, MaxNoteRunes)
	if note == "" {
		return fmt.Errorf("нужно замечание (note): что доработать")
	}
	it.Status, it.Output, it.Result = ItemPending, nil, ""
	s.Report, s.Stage, s.Feedback = nil, Collecting, note
	s.activate(n)
	return nil
}

func (s *State) activate(n int) {
	s.Current = n
	if it := s.Item(n); it != nil {
		it.Status = ItemActive
	}
}

func firstOpen(items []Item) int {
	for _, it := range items {
		if it.Status != ItemDone {
			return it.N
		}
	}
	return 0
}

func (s State) planNote() string {
	names := make([]string, 0, len(s.Items))
	for _, it := range s.Items {
		names = append(names, fmt.Sprintf("%d. %s", it.N, it.Name))
	}
	return strings.Join(names, "; ")
}

// Deliver — сдать карточку текущего вида. Это работа, а не переход: этап не
// меняется, меняется то, чем вид можно будет закрыть.
func Deliver(s *State, n int, c card.Card, turn int) error {
	it := s.CurrentItem()
	if it == nil {
		return fmt.Errorf("текущего вида нет")
	}
	if n != 0 && n != it.N {
		return fmt.Errorf("вид %d не текущий: текущий — %d «%s»", n, it.N, it.Name)
	}
	now := time.Now()
	it.Output = &Output{Card: c.Clone(), Turn: turn, At: now}
	s.touch(now)
	return nil
}

// Verify — отчёт сверки по сданным карточкам, посчитанный кодом: у каждого
// вида есть карточка, латынь подтверждена GBIF, нужные разделы прочитаны.
func Verify(s State, summary string, turn int) Report {
	r := Report{Summary: clip(summary, MaxNoteRunes*2), Turn: turn, At: time.Now()}
	for _, it := range s.Items {
		c := Check{N: it.N, OK: true}
		switch {
		case it.Output == nil:
			c.Issues = append(c.Issues, "карточка не сдана")
		default:
			cd := it.Output.Card
			if cd.TaxonKey == 0 || cd.Unverified {
				c.Issues = append(c.Issues, "латынь не подтверждена GBIF")
			}
			for _, key := range s.Sections {
				if sec := cd.Section(key); sec == nil || (sec.Status != card.SectionRead && sec.Status != card.SectionNone) {
					t, _ := card.TopicOf(key)
					c.Issues = append(c.Issues, "раздел «"+t.Title+"» не прочитан")
				}
			}
		}
		c.OK = len(c.Issues) == 0
		r.Checks = append(r.Checks, c)
	}
	return r
}

// SetReport записывает отчёт сверки.
func SetReport(s *State, r Report) {
	s.Report = &r
	s.touch(time.Now())
}

// EndTurn — вопрос, заданный раньше, после удачного хода считается
// отвеченным: единственная правка состояния, которую делает код.
func EndTurn(s *State, turn int) bool {
	if s.Question == "" || s.IsPaused() || s.QuestionTurn >= turn {
		return false
	}
	s.Question, s.QuestionTurn = "", 0
	s.touch(time.Now())
	return true
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > max {
		return strings.TrimSpace(string(r[:max])) + "…"
	}
	return s
}

// CheckQuote — события человека принимаются только с его словами из этой
// реплики. Пустая строка — всё в порядке.
func CheckQuote(quote, user string) string {
	if strings.TrimSpace(quote) == "" {
		return "нет цитаты: событие человека принимается только с его словами из этой реплики"
	}
	ok, n := words.InReply(quote, user, true)
	if n == 0 {
		return "в цитате нет слов"
	}
	if !ok {
		return "цитаты нет в реплике человека"
	}
	return ""
}
