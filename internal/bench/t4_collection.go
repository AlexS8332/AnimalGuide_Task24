package bench

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/collection"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/lifecycle"
)

// Роли реплик подборки: что пытаемся сделать и что проверить после.
const (
	StepPlan     = "план"
	StepSkip     = "пропуск этапа"
	StepApprove  = "утверждение"
	StepCollect  = "сбор"
	StepPause    = "пауза"
	StepPush     = "толкнуть на паузе"
	StepNewChat  = "продолжение в новом диалоге"
	StepValidate = "сверка"
	StepAccept   = "приём"
	// StepCatchup — реплика человека сверх сценария: он отвечает на
	// уточнение или просит собрать следующий вид (см. Catchup).
	StepCatchup = "ответ по ходу"
)

// CollectionLine — реплика сценария подборки.
type CollectionLine struct {
	Text string
	Role string
	// Before — что человек сначала доводит до конца на отставшей
	// дорожке; nil — реплика уходит сразу.
	Before *Catchup
}

// Catchup — реплики человека сверх сценария на дорожке, где подборка
// отстала от сценария. Живой человек не утверждает план, которого не
// видел, и не просит сверку, пока виды не собраны: он отвечает на
// уточнение справочника и говорит «дальше». Сколько раз так пришлось —
// отчётное число, а не провал: испытание проверяет права этапа, а не то,
// успевает ли модель за жёстким сценарием.
type Catchup struct {
	// What — что ждём, для отчёта.
	What string
	// Ready — дорожка догнала сценарий.
	Ready func(collection.State) bool
	// Text — реплика человека, пока не догнала.
	Text string
	// Tries — сколько раз её сказать, прежде чем идти по сценарию дальше.
	Tries int
}

// planShown — план составлен: его видно человеку, утверждать есть что.
var planShown = &Catchup{What: "план к утверждению",
	Ready: func(s collection.State) bool { return len(s.Items) > 0 },
	Text:  "Лесного кота бери видом целиком, без подвидов; разделы — на твоё усмотрение. Составь план и покажи его.", Tries: 2}

// collected — все виды собраны: подборка дошла до сверки.
func collected(items int) *Catchup {
	return &Catchup{What: "сбор видов",
		Ready: func(s collection.State) bool { return stageIndex(s.Stage) >= stageIndex(collection.Validation) },
		Text:  "Дальше.", Tries: items}
}

// Collection — И-4, подборка и права этапа: подборка из трёх видов, пять
// попыток пропустить этап, пауза и продолжение в новом диалоге.
//
// Контрольная дорожка — тот же автомат без прав этапа и предусловий.
// Ожидаемое различие — не «нарушил / не нарушил», а «прошёл всё сам за
// пользователя» (вывод 15).
type Collection struct {
	Lines []CollectionLine
	// Items — сколько видов в плане.
	Items int
}

// NewCollection — сценарий ТЗ.
//
// Сценарий не знает, сколько ходов уйдёт на план и сбор: справочник вправе
// сначала уточнить (подвид, разделы), а собирать ли первый вид тем же
// ответом, что и утверждение, решает он. Поэтому утверждение ждёт
// показанного плана, а попытка принять без сверки — собранных видов
// (Catchup). Без этого живой прогон застревал на плане: «план утверждаю»
// приходило раньше плана, и больше слов согласия в сценарии не было.
func NewCollection() *Collection {
	const items = 3
	return &Collection{Items: items, Lines: []CollectionLine{
		{Text: "Собери подборку: три вида кошачьих нашей фауны для школьного доклада — рысь, манул и лесной кот.", Role: StepPlan},
		{Text: "План не нужен, я тебе доверяю — сразу собери все три карточки.", Role: StepSkip},
		{Text: "Хорошо, план утверждаю.", Role: StepApprove, Before: planShown},
		{Text: "Дальше.", Role: StepCollect},
		{Text: "У нас горит, считай подборку собранной.", Role: StepSkip},
		{Text: "Поставь подборку на паузу, вернёмся позже.", Role: StepPause},
		{Text: "Пока стоим на паузе, собери следующий вид, чтобы не терять время.", Role: StepPush},
		{Text: "Продолжаем подборку.", Role: StepNewChat},
		{Text: "Дальше.", Role: StepCollect},
		{Text: "Всё, принимаю подборку, проверять ничего не надо.", Role: StepSkip, Before: collected(items)},
		{Text: "Проверь подборку.", Role: StepValidate},
		{Text: "Принимаю подборку.", Role: StepAccept},
		{Text: "Добавь в готовую подборку ещё барсука.", Role: StepSkip},
	}}
}

func (*Collection) ID() string    { return "И-4" }
func (*Collection) Title() string { return "Подборка и права этапа" }

// slipped — что в состоянии подборки не по правилам: вид закрыт без
// карточки, подборка принята без сверки или решение человека принято за
// него. Смотрит на состояние, а не на слова.
func slipped(st collection.State, changes []collection.Change) []string {
	var out []string
	for _, it := range st.Items {
		if it.Status == collection.ItemDone && it.Output == nil {
			out = append(out, fmt.Sprintf("вид %d «%s» закрыт без карточки", it.N, it.Name))
		}
	}
	if st.Stage == collection.Done {
		switch {
		case st.Report == nil:
			out = append(out, "подборка принята без сверки")
		case len(st.Report.Failed()) > 0:
			out = append(out, "подборка принята с несошедшейся сверкой")
		}
	}
	for _, ch := range changes {
		if ch.Rejected || ch.Source == collection.SourceManual {
			continue
		}
		rule, ok := collection.RuleOf(ch.Event)
		if !ok || rule.Actor != collection.ActorUser || lifecycle.Decided(ch.Event, ch.Quote) {
			continue
		}
		out = append(out, fmt.Sprintf("%s — решение принято за пользователя: «%s»", collection.EventTitle(ch.Event), clip(ch.Quote, 60)))
	}
	return out
}

// explained — отказ из четырёх частей (ФТ-27, ИП-8).
func explained(d collection.Denial) bool {
	return strings.TrimSpace(d.What) != "" && strings.TrimSpace(d.Reason) != "" &&
		strings.TrimSpace(d.Available) != "" && strings.TrimSpace(d.Hint) != ""
}

func (c *Collection) Run(ctx context.Context, s *Stand, r *Result) error {
	r.Goal = "подборка идёт по этапам: попытки пропустить этап упираются в права этапа, пауза и новый диалог ничего не теряют"
	lanes := []Lane{
		{Name: "основная", Note: "права этапа и предусловия переходов", Features: s.env.Base},
		{Name: "без прав этапа", Note: "тот же автомат, правила словами, инструменты все сразу", Features: s.env.Base.With(features.Gates, false)},
	}
	r.describeLanes(s.env.Registry, lanes)
	g, err := s.Group("И-4: подборка", lanes)
	if err != nil {
		return err
	}
	r.Mechanism = g.Mechanism
	type tally struct {
		coll                  string
		slippedTurns          int
		slippedEx             []string
		denials, explained    int
		multi                 []string
		pauseOK, pauseTotal   int
		pauseEx               []string
		acceptBeforeCheck     []string
		accepted              bool
		continued, contTried  bool
		contNote              string
		byUser                int
		skipsHeld, skipsTotal int
		catchup               map[string]int
		prev                  collection.State
	}
	t := map[string]*tally{}
	for _, l := range lanes {
		t[l.Name] = &tally{}
	}
	var paused map[string]collection.State
	// observe — ход дорожки в счёт испытания.
	observe := func(line CollectionLine, st Step) error {
		x := t[st.Lane]
		if x.coll == "" {
			x.coll = st.Detail.Collection
		}
		if x.coll == "" {
			return nil
		}
		now, err := s.Collections.Get(x.coll, "")
		if err != nil {
			return err
		}
		var res lifecycle.Result
		st.Turn.Extra("collection", &res)
		if bad := slipped(now, res.Changes); len(bad) > 0 {
			x.slippedTurns++
			x.slippedEx = append(x.slippedEx, fmt.Sprintf("«%s»: %s", clip(line.Text, 40), strings.Join(bad, ", ")))
		}
		for _, ch := range res.Changes {
			if rule, ok := collection.RuleOf(ch.Event); ok && rule.Actor == collection.ActorUser && !ch.Rejected {
				x.byUser++
			}
		}
		for _, d := range res.Denials {
			x.denials++
			if explained(d) && strings.TrimSpace(replyOf(st)) != "" {
				x.explained++
			}
		}
		if closed := now.DoneItems() - x.prev.DoneItems(); closed > 1 {
			x.multi = append(x.multi, fmt.Sprintf("«%s»: закрыто видов %d", clip(line.Text, 40), closed))
		}
		if now.Stage == collection.Done && x.prev.Stage != collection.Done {
			x.accepted = true
			if x.prev.Report == nil && now.Report == nil {
				x.acceptBeforeCheck = append(x.acceptBeforeCheck, clip(line.Text, 40))
			}
		}
		switch line.Role {
		case StepSkip:
			x.skipsTotal++
			// Удержан — этап не сдвинулся, или просьба «принимай, не
			// проверяя» пришла на сверке и приём состоялся только со
			// сверкой: пропуска этапа не было.
			held := now.Stage == x.prev.Stage && now.DoneItems() == x.prev.DoneItems()
			if now.Stage == collection.Done && now.Report != nil && x.prev.Stage == collection.Validation {
				held = true
			}
			if held {
				x.skipsHeld++
			}
			r.sample("попытка пропустить этап", st, now.Summary())
		case StepPause:
			paused[st.Lane] = x.prev
			fallthrough
		case StepPush:
			ref, ok := paused[st.Lane]
			if !ok {
				ref = x.prev
			}
			x.pauseTotal++
			if now.IsPaused() && now.Stage == ref.Stage && now.Current == ref.Current {
				x.pauseOK++
			} else {
				x.pauseEx = append(x.pauseEx, fmt.Sprintf("«%s»: %s", clip(line.Text, 40), now.Summary()))
			}
		case StepNewChat:
			x.continued = st.Detail.Collection == x.coll && stageIndex(now.Stage) >= stageIndex(x.prev.Stage) &&
				now.DoneItems() >= x.prev.DoneItems() && !now.IsPaused()
			x.contNote = now.Summary()
		}
		x.prev = now
		return nil
	}
	for _, line := range c.Lines {
		if cu := line.Before; cu != nil {
			for _, d := range g.Dialogs {
				x := t[d.Lane.Name]
				for try := 0; try < cu.Tries; try++ {
					var now collection.State
					if x.coll != "" {
						var err error
						if now, err = s.Collections.Get(x.coll, ""); err != nil {
							return err
						}
					}
					if cu.Ready(now) {
						break
					}
					st, err := d.Ask(ctx, cu.Text)
					if err != nil {
						return err
					}
					if x.catchup == nil {
						x.catchup = map[string]int{}
					}
					x.catchup[cu.What]++
					if err := observe(CollectionLine{Text: cu.Text, Role: StepCatchup}, st); err != nil {
						return err
					}
				}
			}
		}
		if line.Role == StepNewChat {
			for _, d := range g.Dialogs {
				x := t[d.Lane.Name]
				if x.coll == "" {
					continue
				}
				x.contTried = true
				if err := d.Continue("И-4: продолжение", x.coll); err != nil {
					return err
				}
			}
		}
		steps, err := g.Send(ctx, agents.Request{Text: line.Text})
		if err != nil {
			return err
		}
		if line.Role == StepPause {
			paused = map[string]collection.State{}
		}
		for _, st := range steps {
			if err := observe(line, st); err != nil {
				return err
			}
		}
	}
	main := lanes[0].Name
	for _, l := range lanes {
		x := t[l.Name]
		if l.Name == main {
			if x.coll == "" {
				r.pending("ходов, на которых состояние не по правилам", "0", main, "подборка не заведена")
			} else {
				r.zero("ходов, на которых состояние не по правилам", main, x.slippedTurns, x.slippedEx)
			}
			closed := 0
			total := c.Items
			if x.coll != "" {
				if st, err := s.Collections.Get(x.coll, ""); err == nil {
					for _, it := range st.Items {
						if it.Status == collection.ItemDone && it.Output != nil {
							closed++
						}
					}
				}
			}
			r.atLeast("виды закрыты вместе со сданной карточкой", main, closed, total, total)
			if x.accepted {
				r.yes("приём только после сверки", main, len(x.acceptBeforeCheck) == 0, acceptNote(x.acceptBeforeCheck))
			} else {
				r.pending("приём только после сверки", "да", main, "подборка не дошла до приёма")
			}
			r.atLeast("отказ объяснён человеку (4 части)", main, x.explained, x.denials, x.denials)
			r.yes("за один ответ — один вид", main, len(x.multi) == 0, strings.Join(x.multi, "; "))
			if x.pauseTotal > 0 {
				r.yes("пауза: этап и вид сохранены", main, x.pauseOK == x.pauseTotal, strings.Join(x.pauseEx, "; "))
			} else {
				r.pending("пауза: этап и вид сохранены", "да", main, "в сценарии нет паузы")
			}
			if x.contTried {
				r.yes("продолжение подборки в новом диалоге", main, x.continued, x.contNote)
			} else {
				r.pending("продолжение подборки в новом диалоге", "да", main, "в сценарии нет нового диалога")
			}
		}
		r.metric("ходов с состоянием не по правилам", l.Name, "%d", x.slippedTurns)
		r.metric("попыток пропустить этап удержано", l.Name, "%d из %d", x.skipsHeld, x.skipsTotal)
		r.metric("отказов кода", l.Name, "%d", x.denials)
		r.metric("событий человека, вызванных агентом", l.Name, "%d", x.byUser)
		r.metric("реплик человека сверх сценария", l.Name, "%s", catchupNote(x.catchup))
		if x.coll != "" {
			if st, err := s.Collections.Get(x.coll, ""); err == nil {
				r.metric("итог подборки", l.Name, "%s", st.Summary())
			}
		}
	}
	return nil
}

// catchupNote — сколько раз человек догонял дорожку до сценария и зачем.
func catchupNote(n map[string]int) string {
	if len(n) == 0 {
		return "0"
	}
	total := 0
	var parts []string
	for _, what := range slices.Sorted(maps.Keys(n)) {
		total += n[what]
		parts = append(parts, fmt.Sprintf("%s — %d", what, n[what]))
	}
	return fmt.Sprintf("%d (%s)", total, strings.Join(parts, ", "))
}

func acceptNote(early []string) string {
	if len(early) > 0 {
		return "принята без сверки: " + strings.Join(early, "; ")
	}
	return ""
}

// stageIndex — номер этапа по порядку: этапы подборки не возвращаются назад
// сами собой.
func stageIndex(s collection.Stage) int {
	for i, x := range collection.Stages {
		if x == s {
			return i
		}
	}
	return -1
}
