// Package collection — состояние подборки (ФТ-24, ФТ-28): этап, план,
// виды, сданные карточки, отчёт сверки, отказы. Отвечает на вопрос «где
// подборка сейчас»; что ей сейчас позволено — дело пакета lifecycle.
//
// Этапы planning → collecting → validation → done, пауза — флаг поверх
// этапа. Ожидаемое действие вычисляется из этапа, шага, паузы и открытого
// вопроса, а не хранится (ИП-6). Сданная работа — часть состояния, а не
// рассказ о ней: карточка вида лежит в файле подборки.
//
// Файл подборки — collections/<id>.json, тот же адрес, что у рабочей
// памяти: новый диалог продолжает подборку, ничего не копируя.
package collection

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
)

// Stage — этап подборки.
type Stage string

const (
	Planning   Stage = "planning"
	Collecting Stage = "collecting"
	Validation Stage = "validation"
	Done       Stage = "done"
)

// Stages — этапы по порядку.
var Stages = []Stage{Planning, Collecting, Validation, Done}

// Title — этап словами.
func (s Stage) Title() string {
	switch s {
	case Planning:
		return "план"
	case Collecting:
		return "сбор"
	case Validation:
		return "сверка"
	case Done:
		return "принята"
	}
	return string(s)
}

// Состояния вида в плане.
const (
	ItemPending = "pending"
	ItemActive  = "active"
	ItemDone    = "done"
)

// Item — вид в плане подборки.
type Item struct {
	N      int    `json:"n"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Result string `json:"result,omitempty"`
	Turn   int    `json:"turn,omitempty"`
	// Output — сданная карточка вида: без неё вид не закрыть (step.output).
	Output *Output `json:"output,omitempty"`
}

// Output — сданная работа по виду.
type Output struct {
	Card card.Card `json:"card"`
	Turn int       `json:"turn"`
	At   time.Time `json:"at"`
}

// Check — строка отчёта сверки по виду.
type Check struct {
	N      int      `json:"n"`
	OK     bool     `json:"ok"`
	Issues []string `json:"issues,omitempty"`
}

// Report — отчёт сверки: считает его код по сданным карточкам, а не модель
// словами (ИП-2).
type Report struct {
	Summary string    `json:"summary"`
	Checks  []Check   `json:"checks"`
	Turn    int       `json:"turn"`
	At      time.Time `json:"at"`
}

// Covers — есть ли в отчёте строка по виду.
func (r *Report) Covers(n int) bool {
	if r == nil {
		return false
	}
	for _, c := range r.Checks {
		if c.N == n {
			return true
		}
	}
	return false
}

// Failed — несошедшиеся строки.
func (r *Report) Failed() []Check {
	if r == nil {
		return nil
	}
	var out []Check
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// Denial — отказ, записанный в файл подборки (ФТ-27): что нельзя, почему,
// что доступно, что сделать.
type Denial struct {
	What      string    `json:"what"`
	Gate      string    `json:"gate"`
	Stage     Stage     `json:"stage"`
	Reason    string    `json:"reason"`
	Available string    `json:"available,omitempty"`
	Hint      string    `json:"hint,omitempty"`
	Turn      int       `json:"turn,omitempty"`
	Time      time.Time `json:"time"`
}

// MaxDenials — сколько отказов хранить.
const MaxDenials = 60

// Pause — пауза поверх этапа.
type Pause struct {
	At    time.Time `json:"at"`
	Turn  int       `json:"turn"`
	Quote string    `json:"quote,omitempty"`
}

// State — подборка.
type State struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Goal — для чего подборка; Sections — какие разделы читать у каждого
	// вида.
	Goal     string   `json:"goal,omitempty"`
	Sections []string `json:"sections,omitempty"`
	Stage    Stage    `json:"stage"`
	Paused   *Pause   `json:"paused,omitempty"`
	Items    []Item   `json:"items"`
	Current  int      `json:"current"`
	// Question — открытый вопрос справочника к человеку.
	Question     string   `json:"question,omitempty"`
	QuestionTurn int      `json:"questionTurn,omitempty"`
	Feedback     string   `json:"feedback,omitempty"`
	PlanTurn     int      `json:"planTurn,omitempty"`
	CheckTurn    int      `json:"checkTurn,omitempty"`
	Report       *Report  `json:"report,omitempty"`
	Denials      []Denial `json:"denials,omitempty"`
	// Turn — сквозной номер хода подборки через все её диалоги.
	Turn    int        `json:"turn"`
	Version int        `json:"version"`
	Created *time.Time `json:"created,omitempty"`
	Updated *time.Time `json:"updated,omitempty"`
	Log     []Change   `json:"log"`
}

// MaxLog — сколько переходов хранить.
const MaxLog = 200

// New — пустая подборка на этапе плана.
func New(id, title string) State {
	return State{ID: id, Title: title, Stage: Planning, Items: []Item{}, Log: []Change{}}
}

// IsPaused — стоит ли подборка на паузе.
func (s State) IsPaused() bool { return s.Paused != nil }

// Active — идёт ли подборка (не принята).
func (s State) Active() bool { return s.Stage != Done }

// Begin открывает ход подборки и возвращает его номер.
func Begin(s *State) int {
	s.Turn++
	return s.Turn
}

// Clone — глубокая копия.
func (s State) Clone() State {
	out := s
	out.Items = append([]Item(nil), s.Items...)
	out.Sections = append([]string(nil), s.Sections...)
	out.Log = append([]Change(nil), s.Log...)
	out.Denials = append([]Denial(nil), s.Denials...)
	if s.Paused != nil {
		p := *s.Paused
		out.Paused = &p
	}
	if s.Report != nil {
		r := *s.Report
		r.Checks = append([]Check(nil), s.Report.Checks...)
		out.Report = &r
	}
	for i := range out.Items {
		if out.Items[i].Output != nil {
			o := *out.Items[i].Output
			o.Card = o.Card.Clone()
			out.Items[i].Output = &o
		}
	}
	return out
}

// Deny записывает отказ в состояние.
func (s *State) Deny(d Denial) {
	if d.Time.IsZero() {
		d.Time = time.Now()
	}
	if d.Stage == "" {
		d.Stage = s.Stage
	}
	s.Denials = append(s.Denials, d)
	if len(s.Denials) > MaxDenials {
		s.Denials = s.Denials[len(s.Denials)-MaxDenials:]
	}
}

// Item — вид по номеру.
func (s *State) Item(n int) *Item {
	if n < 1 || n > len(s.Items) {
		return nil
	}
	return &s.Items[n-1]
}

// CurrentItem — вид, который сейчас собирается.
func (s State) CurrentItem() *Item {
	if s.Current < 1 || s.Current > len(s.Items) {
		return nil
	}
	return &s.Items[s.Current-1]
}

// DoneItems — сколько видов закрыто.
func (s State) DoneItems() int {
	n := 0
	for _, it := range s.Items {
		if it.Status == ItemDone {
			n++
		}
	}
	return n
}

// Point — где подборка: этап, вид, пауза.
type Point struct {
	Stage  Stage `json:"stage"`
	Item   int   `json:"item,omitempty"`
	Paused bool  `json:"paused,omitempty"`
}

// At — точка подборки сейчас.
func (s State) At() Point { return Point{Stage: s.Stage, Item: s.Current, Paused: s.IsPaused()} }

func (p Point) String() string {
	out := p.Stage.Title()
	if p.Item > 0 {
		out += fmt.Sprintf(", вид %d", p.Item)
	}
	if p.Paused {
		out += ", пауза"
	}
	return out
}

// Кто должен действовать.
const (
	ActorUser  = "user"
	ActorAgent = "agent"
	ActorNone  = "none"
)

// ActorTitle — действующее лицо словами.
func ActorTitle(a string) string {
	switch a {
	case ActorUser:
		return "человек"
	case ActorAgent:
		return "справочник"
	}
	return "никто"
}

// Expected — ожидаемое действие: вычисляется, а не хранится.
type Expected struct {
	Actor string `json:"actor"`
	Text  string `json:"text"`
}

// Expected — что должно произойти дальше.
func (s State) Expected() Expected {
	switch {
	case s.IsPaused():
		return Expected{ActorUser, "вернуться к подборке — сказать, что продолжаем"}
	case s.Stage == Done:
		return Expected{ActorNone, "ничего: подборка принята"}
	case s.Question != "":
		return Expected{ActorUser, "ответить на вопрос: «" + s.Question + "»"}
	case s.Stage == Planning && len(s.Items) == 0:
		return Expected{ActorAgent, "выяснить, что нужно, и составить план: какие виды войдут"}
	case s.Stage == Planning:
		return Expected{ActorUser, "утвердить план или попросить поправить"}
	case s.Stage == Collecting:
		if it := s.CurrentItem(); it != nil {
			return Expected{ActorAgent, fmt.Sprintf("собрать вид %d «%s»: сдать карточку и закрыть вид", it.N, it.Name)}
		}
		return Expected{ActorAgent, "собрать текущий вид"}
	case s.Stage == Validation:
		if s.Report == nil {
			return Expected{ActorAgent, "свести сверку по всем видам"}
		}
		return Expected{ActorUser, "принять подборку или вернуть вид на доработку"}
	}
	return Expected{ActorNone, ""}
}

// Summary — подборка одной строкой.
func (s State) Summary() string {
	parts := []string{"этап «" + s.Stage.Title() + "»"}
	if n := len(s.Items); n > 0 {
		parts = append(parts, fmt.Sprintf("собрано %d из %d", s.DoneItems(), n))
	}
	if s.IsPaused() {
		parts = append(parts, "на паузе")
	}
	if e := s.Expected(); e.Actor != ActorNone {
		parts = append(parts, "ждёт: "+ActorTitle(e.Actor))
	}
	return strings.Join(parts, ", ")
}

// Valid — известен ли этап.
func (s Stage) Valid() bool { return slices.Contains(Stages, s) }

// DefaultSections — разделы, которые читаются у каждого вида подборки,
// если план не назвал других.
var DefaultSections = []string{"habitat", "diet"}
