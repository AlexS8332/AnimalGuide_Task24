// Package persona — человек вокруг хода: профиль, долговременная и рабочая
// память и карточка фактов ветки (этап 5). Это хук менеджера ходов: до хода
// он зовёт извлекатель и готовит блоки, после хода проверяет, соблюдён ли
// профиль, и дописывает прочитанное в долговременную память.
//
// Решение об обвязке принимает арифметика, а не модель (ИП-2): извлекатель
// — не инструмент агента, модель его не видит.
package persona

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/extract"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/memory"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/profile"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
)

// Name — имя хука: под ним лежат итоги в ходе и сведения в диалоге.
const Name = "persona"

// Hook — человек вокруг хода.
type Hook struct {
	Memory    *memory.Store
	Profiles  *profile.Store
	Extractor extract.Extractor
}

func (h *Hook) Name() string { return Name }

// Reserved — чей ключ, если не памяти: анкета профиля, карточка животного
// или код (ИП-4: каждое хранилище объявляет свои зарезервированные ключи).
func Reserved(key string) string {
	switch {
	case profile.Reserved(key):
		return "анкета профиля"
	case card.Reserved(key):
		return "карточка животного"
	case memory.EqualKey(key, memory.KeyRead), memory.EqualKey(key, memory.KeyBookmarks):
		return "код приложения"
	}
	return ""
}

// owner — чей профиль и долговременная память ведутся в диалоге: у продукта
// один собеседник на диалог (первый в списке).
func owner(t *runs.Turn) (string, string) {
	if len(t.Owners) == 0 {
		return "", ""
	}
	return t.Owners[0], t.Conv.TitleOf(t.Owners[0])
}

// state — адресаты хода.
type state struct {
	prof     profile.Profile
	long     memory.Card
	work     memory.Card
	onceProf profile.Profile
}

// Before — извлекатель и блоки.
func (h *Hook) Before(ctx context.Context, t *runs.Turn) error {
	fs := t.Features
	id, title := owner(t)
	collection, ctitle := t.Conv.Collection, t.Conv.CollectionTitle
	if t.Collection != "" {
		collection, ctitle = t.Collection, t.CollectionTitle
	}
	st := &state{prof: profile.New(id, title), long: memory.NewCard(memory.LayerLong, id, title),
		work: memory.NewCard(memory.LayerWork, collection, ctitle)}
	var firstErr error
	if fs.On(features.Profile) && id != "" {
		p, err := h.Profiles.Get(id, title)
		if err != nil {
			firstErr = err
		}
		st.prof = p
	}
	if id != "" || collection != "" {
		longID, workID := "", ""
		if fs.On(features.MemoryLong) {
			longID = id
		}
		if fs.On(features.MemoryWork) {
			workID = collection
		}
		long, work, err := h.Memory.Update(longID, title, workID, ctitle, func(*memory.Card, *memory.Card) error { return nil })
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if longID != "" {
			st.long = long
		}
		if workID != "" {
			st.work = work
		}
	}
	st.onceProf = st.prof

	if h.extractNeeded(t) {
		h.extract(ctx, t, st, id, title, collection, ctitle)
	}

	if fs.On(features.Profile) {
		t.AddBlock(features.Block{Feature: features.Profile, Title: "профиль собеседника", Text: st.onceProf.Prompt()})
	}
	if fs.On(features.MemoryLong) {
		t.AddBlock(features.Block{Feature: features.MemoryLong, Title: "долговременная память", Text: st.long.Prompt()})
	}
	if fs.On(features.MemoryWork) {
		t.AddBlock(features.Block{Feature: features.MemoryWork, Title: "рабочая память подборки", Text: st.work.Prompt()})
	}
	turnStates.Store(t.ID, st)
	return firstErr
}

// turnStates — адресаты идущих ходов: After проверяет ответ по тому профилю,
// с которым ход ушёл модели (с разовыми правками), а не по файлу.
var turnStates sync.Map

// extractNeeded — есть ли что извлекать. Клики (раздел, узел, сравнение по
// кнопке) новых слов человека не несут; реплика из одного названия —
// тоже: предпочтений в «рысь» нет. Выключенный извлекатель — не дыра: об
// этом событие в журнале (ФТ-48).
func (h *Hook) extractNeeded(t *runs.Turn) bool {
	if t.Request.Kind != agents.KindMessage && t.Request.Kind != "" {
		return false
	}
	text := strings.TrimSpace(t.Request.Text)
	if text == "" {
		return false
	}
	if !t.Features.On(features.Extract) {
		t.Em.Log(agent.Event{Agent: "extract", Kind: agent.EventMechanism, Mechanism: string(features.Extract),
			Title:  "извлекатель выключен — память, профиль и карточка фактов по реплике не меняются",
			Detail: "Правки возможны только руками в окнах «Память целиком» и «Картотека профилей»."})
		return false
	}
	if kind, _, _ := agents.Classify(text, card.State{}); kind == agents.KindOpen {
		return false
	}
	// Вопрос о животном без слов о человеке, форме ответа и подборке:
	// правок из него не бывает, а тема хода и так видна в карточках пути.
	// Решает арифметика, а не модель (ИП-2); в журнале — почему не звали.
	if extract.Nothing(text) {
		t.Em.Log(agent.Event{Agent: "extract", Kind: agent.EventMechanism, Mechanism: string(features.Extract),
			Title:  "извлекатель не звался: вопрос о животном, извлекать нечего",
			Detail: "В реплике нет слов о человеке, форме ответа, подборке и решений — правкам памяти, профиля и карточки фактов взяться неоткуда."})
		return false
	}
	return true
}

func (h *Hook) extract(ctx context.Context, t *runs.Turn, st *state, id, title, collection, ctitle string) {
	fs := t.Features
	in := extract.Input{
		Targets: extract.Targets{
			Profile: fs.On(features.Profile) && id != "",
			Long:    fs.On(features.MemoryLong) && id != "",
			Work:    fs.On(features.MemoryWork) && collection != "",
			Facts:   fs.On(features.Facts),
		},
		Profile: st.prof, Long: st.long, Work: st.work, Facts: t.Facts,
		History: t.History, User: t.Request.Text, Turn: t.Number, Reserved: Reserved,
	}
	upd, err := h.Extractor.Run(ctx, in)
	if upd.Called {
		t.Meter = t.Meter.Add(history.Meter{Calls: 1, Usage: upd.Usage, Cost: upd.Cost, Seconds: upd.Seconds})
	}
	usage, cost := upd.Usage, upd.Cost
	if err != nil {
		t.Em.Log(agent.Event{Agent: "extract", Kind: agent.EventMechanism, Mechanism: string(features.Extract),
			Title: "память и профиль не обновлены: " + err.Error(), Detail: "Ход идёт с прежними; следующий ход попробует снова.",
			Usage: &usage, Cost: &cost, Seconds: upd.Seconds})
		return
	}
	// Запись в файлы — под замком хранилищ; разовые правки профиля в файл
	// не пишутся, но в запрос этого хода уходят.
	if in.Targets.Long || in.Targets.Work {
		longID, workID := "", ""
		if in.Targets.Long {
			longID = id
		}
		if in.Targets.Work {
			workID = collection
		}
		long, work, werr := h.Memory.Update(longID, title, workID, ctitle, func(l, w *memory.Card) error {
			if l != nil && upd.Long.Version != st.long.Version {
				*l = upd.Long
			}
			if w != nil && upd.Work.Version != st.work.Version {
				*w = upd.Work
			}
			return nil
		})
		if werr != nil {
			t.Em.Log(agent.Event{Agent: "extract", Kind: agent.EventNote, Title: "память не записана: " + werr.Error()})
		} else {
			if longID != "" {
				st.long = long
			}
			if workID != "" {
				st.work = work
			}
		}
	}
	if in.Targets.Profile && upd.Profile.Version != st.prof.Version {
		if err := h.Profiles.Save(upd.Profile); err != nil {
			t.Em.Log(agent.Event{Agent: "extract", Kind: agent.EventNote, Title: "профиль не записан: " + err.Error()})
		} else {
			st.prof = upd.Profile
		}
	}
	st.onceProf = st.prof.With(upd.ProfileChanges.Once())
	if in.Targets.Facts {
		t.Facts = upd.Facts
	}

	title2 := "извлекатель: " + upd.MemoryChanges.Summary()
	if !upd.Changed() {
		title2 = "извлекатель: новых сведений и предпочтений нет"
	}
	t.Em.Log(agent.Event{Agent: "extract", Kind: agent.EventMechanism, Mechanism: string(features.Extract),
		Title: title2, Detail: detail(upd), Usage: &usage, Cost: &cost, Seconds: upd.Seconds, Data: upd})
	if len(upd.ProfileChanges) > 0 {
		t.Em.Log(agent.Event{Agent: "extract", Kind: agent.EventMechanism, Mechanism: string(features.Profile),
			Title: "профиль: " + upd.ProfileChanges.Summary(), Data: upd.ProfileChanges})
	}
	if len(upd.MemoryChanges) > 0 {
		t.Extra("memory", upd.MemoryChanges)
	}
	if len(upd.ProfileChanges) > 0 {
		t.Extra("profile", upd.ProfileChanges)
	}
	if len(upd.FactChanges) > 0 {
		t.Extra("facts", upd.FactChanges)
	}
}

func detail(u extract.Update) string {
	var b strings.Builder
	for _, c := range u.MemoryChanges {
		fmt.Fprintf(&b, "память %s: %s «%s» = %q %s\n", c.Layer, c.Op, c.Key, c.Value, c.Reason)
	}
	for _, c := range u.ProfileChanges {
		b.WriteString("профиль: " + c.String())
		if c.Quote != "" {
			b.WriteString(" (цитата: «" + c.Quote + "»)")
		}
		b.WriteByte('\n')
	}
	for _, c := range u.FactChanges {
		fmt.Fprintf(&b, "факты: %s «%s» = %q %s\n", c.Op, c.Key, c.Value, c.Reason)
	}
	return b.String()
}

// After — соблюдение профиля по тексту ответа (ФТ-23) и «уже читал».
func (h *Hook) After(ctx context.Context, t *runs.Turn) error {
	v, _ := turnStates.LoadAndDelete(t.ID)
	st, _ := v.(*state)
	if st == nil || t.Result == nil {
		return nil
	}
	res := t.Result
	if t.Features.On(features.Profile) {
		text := checkedText(res)
		if checks := profile.Checks(st.onceProf, text); len(checks) > 0 {
			t.Extra("checks", checks)
			t.Em.Log(agent.Event{Agent: "profile", Kind: agent.EventMechanism, Mechanism: string(features.Profile),
				Title: profile.Note(checks), Data: checks})
		}
	}
	// «Уже читал» ведёт код: открытая карточка — это прочитанное животное.
	id, title := owner(t)
	if id == "" || !t.Features.On(features.MemoryLong) {
		return nil
	}
	var added []string
	for _, d := range res.Deltas {
		if d.Kind != card.DeltaCard || d.Card == nil {
			continue
		}
		if _, ok, err := h.Memory.AddToList(id, title, memory.KeyRead, d.Card.Name, t.Number); err == nil && ok {
			added = append(added, d.Card.Name)
		}
	}
	if len(added) > 0 {
		t.Extra("read", added)
	}
	return nil
}

// checkedText — что проверять на соблюдение профиля: текст, который
// написала модель. У карточки это её описание, у кликов по дереву и у
// таблицы сравнения — нечего.
func checkedText(res *agents.Result) string {
	switch res.Route {
	case agents.RouteLead, agents.RouteSection, "collection":
		return res.Text
	case agents.RouteCard:
		for _, d := range res.Deltas {
			if d.Kind == card.DeltaCard && d.Card != nil {
				return d.Card.Summary
			}
		}
	}
	return ""
}

// View — человек диалога для пульта: профиль, слои памяти.
type View struct {
	Owner   string            `json:"owner"`
	Profile profile.Profile   `json:"profile"`
	Long    memory.Card       `json:"long"`
	Work    memory.Card       `json:"work"`
	Paths   map[string]string `json:"paths"`
	Error   string            `json:"error,omitempty"`
}

// Describe — сведения для пульта (runs.Describer).
func (h *Hook) Describe(c *history.Conversation) any {
	v := View{Paths: map[string]string{}}
	if len(c.Owners) > 0 {
		v.Owner = c.Owners[0]
	}
	title := c.TitleOf(v.Owner)
	var err error
	if v.Owner != "" {
		if v.Profile, err = h.Profiles.Get(v.Owner, title); err != nil {
			v.Error = err.Error()
		}
		if v.Long, err = h.Memory.Card(memory.LayerLong, v.Owner, title); err != nil {
			v.Error = err.Error()
		}
		v.Paths["long"] = h.Memory.DisplayPath(memory.LayerLong, v.Owner)
		v.Paths["profile"] = h.Profiles.DisplayPath(v.Owner)
	}
	if c.Collection != "" {
		if v.Work, err = h.Memory.Card(memory.LayerWork, c.Collection, c.CollectionTitle); err != nil {
			v.Error = err.Error()
		}
		v.Paths["work"] = h.Memory.DisplayPath(memory.LayerWork, c.Collection)
	}
	return v
}
