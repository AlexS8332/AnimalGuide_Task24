// Package charter — свод справочника и страж как хук хода (этап 7). До
// хода хук кладёт свод первым блоком запроса и даёт агенту инструменты
// invariant_check и invariant_amend; после хода страж проверяет готовый
// ответ: код отбирает фрагменты по словам свода, судья решает, нарушен ли
// он, и нарушающий ответ до человека не доходит (ФТ-30, ФТ-43).
//
// Судья и страж — обвязка хода, а не инструменты агента (ИП-2): модель
// их не видит и вызвать не может.
package charter

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/invariants"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
)

// Name — имя хука: под ним лежат итог хода и сведения для пульта.
const Name = "charter"

// Статусы стража в итоге хода.
const (
	GuardOff       = "off"       // механизм выключен: ответ ушёл без проверки
	GuardClean     = "clean"     // подозрительного не нашлось, судья не звался
	GuardPassed    = "passed"    // судья смотрел и нарушений не нашёл
	GuardRefused   = "refused"   // нарушение: ответ заменён отказом кода
	GuardUnchecked = "unchecked" // судья не ответил: ход остался непроверенным
	GuardEmpty     = "empty"     // проверять нечего: ответа текстом нет
)

// Hook — свод и страж.
type Hook struct {
	Store *invariants.Store
	Judge invariants.Judge
	// ID — свод; пусто — свод справочника.
	ID string
}

func (h *Hook) Name() string { return Name }

func (h *Hook) id() string {
	if h.ID != "" {
		return h.ID
	}
	return invariants.GuideID
}

// Result — итог хода для чипов под ответом.
type Result struct {
	// Enabled — свод шёл блоком; иначе правила ушли абзацем промпта.
	Enabled    bool                     `json:"enabled"`
	Version    int                      `json:"version"`
	Checks     []invariants.CheckRecord `json:"checks,omitempty"`
	Amendments []invariants.Result      `json:"amendments,omitempty"`
	Guard      Guard                    `json:"guard"`
}

// Guard — что сделал страж.
type Guard struct {
	Status string            `json:"status"`
	Review invariants.Review `json:"review"`
	// Original — начало заменённого ответа: что именно не дошло до
	// человека. Целиком — в журнале хода.
	Original string `json:"original,omitempty"`
}

type state struct {
	on  bool
	rec *invariants.Recorder
}

// turnStates — идущие ходы: After видит тот же регистратор, что инструменты.
var turnStates sync.Map

// Before — блок свода и инструменты, либо абзац правил при выключенном
// механизме.
func (h *Hook) Before(ctx context.Context, t *runs.Turn) error {
	if !t.Features.On(features.Charter) {
		c, err := h.Store.Get(h.id())
		if err != nil {
			return err
		}
		t.Request.Rules = join(t.Request.Rules, invariants.PlainRules(c))
		turnStates.Store(t.ID, &state{})
		t.Em.Log(agent.Event{Agent: Name, Kind: agent.EventMechanism, Mechanism: string(features.Charter),
			Title:  "свод выключен — те же правила абзацем системного промпта",
			Detail: "Без блока свода, сверки invariant_check и процедуры поправки (ФТ-48)."})
		return nil
	}
	c, err := h.Store.Begin(h.id())
	if err != nil {
		return err
	}
	rec := &invariants.Recorder{
		OnCheck: func(r invariants.CheckRecord) {
			title := "свод: сверка — нарушений по словам нет"
			if len(r.Suspect) > 0 {
				title = "свод: сверка — похоже на нарушение " + strings.Join(r.Suspect, ", ")
			}
			t.Em.Log(agent.Event{Agent: Name, Kind: agent.EventMechanism, Mechanism: string(features.Charter),
				Title: title, Detail: "Ответ: " + r.Answer, Data: r})
		},
		OnResult: func(r invariants.Result, now invariants.Charter) {
			title := "свод: " + invariants.EventTitle(r.Event) + " — " + r.Summary
			if r.Rejected {
				title = "свод: " + r.Event + " не принято — " + r.Reason
			}
			t.Em.Log(agent.Event{Agent: Name, Kind: agent.EventMechanism, Mechanism: string(features.Charter),
				Title: title, Detail: "Теперь: " + now.Summary(), Data: r})
		},
	}
	t.AddBlock(features.Block{Feature: features.Charter, Title: "свод инвариантов", Text: invariants.Prompt(c)})
	t.Request.Tools = append(t.Request.Tools, invariants.CheckTool(h.Store, h.id(), rec),
		invariants.AmendTool(h.Store, h.id(), t.Request.Text, t.Conv.ID, rec))
	turnStates.Store(t.ID, &state{on: true, rec: rec})
	return nil
}

func join(a, b string) string {
	switch {
	case strings.TrimSpace(a) == "":
		return b
	case strings.TrimSpace(b) == "":
		return a
	}
	return a + "\n\n" + b
}

// After — страж и итог хода.
func (h *Hook) After(ctx context.Context, t *runs.Turn) error {
	v, _ := turnStates.LoadAndDelete(t.ID)
	st, _ := v.(*state)
	if st == nil || t.Result == nil {
		return nil
	}
	// Страж смотрит по своду после правок хода: принятая поправка действует
	// сразу.
	c, err := h.Store.Get(h.id())
	if err != nil {
		return err
	}
	out := Result{Enabled: st.on, Version: c.Version}
	if st.rec != nil {
		out.Checks, out.Amendments = st.rec.Checks(), st.rec.Results()
	}
	if t.Features.On(features.Guard) {
		out.Guard = h.guard(ctx, t, c)
	} else {
		out.Guard = Guard{Status: GuardOff}
		t.Em.Log(agent.Event{Agent: "guard", Kind: agent.EventMechanism, Mechanism: string(features.Guard),
			Title: "страж выключен — ответ ушёл без проверки по своду"})
	}
	t.Extra(Name, out)
	return nil
}

// guard — проверка готового ответа. Работает независимо от того, что
// пришло из источника (ФТ-43): проверяется текст, который увидит человек.
func (h *Hook) guard(ctx context.Context, t *runs.Turn, c invariants.Charter) Guard {
	res := t.Result
	text := strings.TrimSpace(res.Text)
	if text == "" {
		return Guard{Status: GuardEmpty}
	}
	user := res.User
	if user == "" {
		user = t.Request.Text
	}
	scr := invariants.Screen(c, text)
	rev := h.Judge.Check(ctx, c, scr, user, text)
	g := Guard{Review: rev}
	ev := agent.Event{Agent: "guard", Kind: agent.EventMechanism, Mechanism: string(features.Guard), Data: rev}
	if rev.Called {
		t.Meter = t.Meter.Add(history.Meter{Calls: 1, Usage: rev.Usage, Cost: rev.Cost, Seconds: rev.Seconds})
		usage, cost := rev.Usage, rev.Cost
		ev.Usage, ev.Cost, ev.Seconds = &usage, &cost, rev.Seconds
	}
	switch {
	case rev.Err != "":
		g.Status = GuardUnchecked
		ev.Title = "страж: ответ не проверен — " + rev.Err
		ev.Detail = "Ответ ушёл как есть: сбой судьи не превращается в отказ человеку."
	case !rev.OK():
		g.Status = GuardRefused
		g.Original = trim(text, 600)
		refusal := invariants.Refusal(c, rev.Broken)
		replace(res, refusal)
		ids := make([]string, 0, len(rev.Broken))
		for _, v := range rev.Broken {
			ids = append(ids, v.Invariant)
		}
		ev.Title = "страж: нарушен свод (" + strings.Join(ids, ", ") + ") — ответ заменён отказом"
		ev.Detail = "Не дошло до человека:\n" + text
	case len(scr.Hits) == 0:
		g.Status = GuardClean
		ev.Title = "страж: подозрительного по словам свода нет"
	default:
		g.Status = GuardPassed
		ev.Title = fmt.Sprintf("страж: %d фрагм. проверено — нарушений нет", len(scr.Hits))
		if !rev.Called {
			ev.Title = "страж: все совпадения сняты кодом — судья не звался"
		}
	}
	t.Em.Log(ev)
	return g
}

// replace — нарушающий ответ заменяется и в тексте хода, и в сообщениях,
// которые лягут в историю: иначе следующий ход увидел бы нарушение как
// собственный прошлый ответ и повторил бы его.
func replace(res *agents.Result, text string) {
	res.Text = text
	for i := len(res.Added) - 1; i >= 0; i-- {
		m := res.Added[i]
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) == 0 {
			res.Added[i].Content = text
			return
		}
	}
	res.Added = append(res.Added, llm.Message{Role: llm.RoleAssistant, Content: text})
}

func trim(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}

// View — свод для пульта.
type View struct {
	Charter bool                   `json:"charter"`
	Guard   bool                   `json:"guard"`
	Title   string                 `json:"title"`
	Version int                    `json:"version"`
	Summary string                 `json:"summary"`
	Items   []invariants.Invariant `json:"items"`
	Pending []invariants.Amendment `json:"pending,omitempty"`
	Path    string                 `json:"path"`
	Error   string                 `json:"error,omitempty"`
}

// Describe — сведения для пульта (runs.Describer).
func (h *Hook) Describe(c *history.Conversation) any {
	ch, err := h.Store.Get(h.id())
	v := View{Charter: c.Features.On(features.Charter), Guard: c.Features.On(features.Guard), Title: ch.Title,
		Version: ch.Version, Summary: ch.Summary(), Items: ch.Items, Pending: ch.Pending, Path: h.Store.DisplayPath(h.id())}
	if err != nil {
		v.Error = err.Error()
	}
	return v
}
