package runs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents/agentstest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

type rig struct {
	dir   string
	brain *agentstest.Brain
	fake  *llmtest.Fake
	reg   *tools.Registry
	m     *Manager
}

func newRig(t *testing.T) *rig {
	t.Helper()
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)
	r := &rig{dir: t.TempDir(), brain: &agentstest.Brain{},
		reg: tools.MustRegistry(tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL)...)}
	r.fake = &llmtest.Fake{Fn: r.brain.Chat}
	r.m = r.manager()
	return r
}

func (r *rig) manager(hooks ...Hook) *Manager {
	reg := features.Catalog()
	return NewManager(Config{
		Agents:   agents.Deps{Runner: agent.Runner{LLM: r.fake, Model: llm.DefaultModel}, Features: reg, Sources: agents.Local{Registry: r.reg}},
		Store:    history.NewStore(store.NewDir(r.dir)),
		Registry: reg, Defaults: reg.Defaults(), Timeout: time.Minute, Hooks: hooks,
	})
}

func wait(t *testing.T, s *Session) View {
	t.Helper()
	v := s.Wait(10 * time.Second)
	if v.Status == StatusRunning {
		t.Fatal("ход не закончился")
	}
	return v
}

func lead(script func(req llm.Request, step int) llm.Response) func(llm.Request, int) llm.Response {
	return script
}

// Пустой случай отдельно (НТ-5): первый ход нового диалога, ноль сообщений.
func TestFirstTurnOfNewDialog(t *testing.T) {
	r := newRig(t)
	s, err := r.m.Start(StartOptions{Request: agents.Request{Text: "рысь"}})
	if err != nil {
		t.Fatal(err)
	}
	v := wait(t, s)
	if v.Status != StatusDone || v.Route != agents.RouteCard {
		t.Fatalf("ход: %+v", v)
	}
	d, ok := r.m.Get(v.ConversationID)
	if !ok || len(d.TurnList) != 1 || d.Messages != 2 || len(d.Cards.Cards) != 1 || d.Title != "рысь" {
		t.Fatalf("диалог: %+v", d.Summary)
	}
	if d.TurnList[0].Effective.Empty() || !d.TurnList[0].Requested.On(features.Tracker) {
		t.Fatal("механизмы хода не записаны")
	}
	if len(d.TurnList[0].Events) == 0 || d.TurnList[0].Totals.LLMCalls != 4 { // привратник + идентификатор (чтение, сверка, сдача)
		t.Fatalf("журнал и счётчики хода: %d событий, %d запросов", len(d.TurnList[0].Events), d.TurnList[0].Totals.LLMCalls)
	}
	if len(d.Mechanisms) != len(features.Catalog().All()) || len(d.BranchTree) != 1 {
		t.Fatal("механизмы и дерево для пульта")
	}
	// Пустой диалог без хода тоже годится для интерфейса.
	empty, err := r.m.Create(StartOptions{})
	if err != nil || empty.TurnList == nil || empty.Checkpoints == nil || len(empty.Owners) != 1 {
		t.Fatalf("пустой диалог: %+v %v", empty, err)
	}
}

// Имитация перезапуска (НТ-5): второй менеджер на том же каталоге поднимает
// диалог, и в первом запросе к его модели вся история первого запуска с
// tool_call_id.
func TestRestartKeepsHistoryWithToolCallIDs(t *testing.T) {
	r := newRig(t)
	r.brain.LeadScript = func(req llm.Request, step int) llm.Response {
		if step == 0 {
			return llmtest.ToolCall("search_wikipedia", `{"query":"рысь"}`)
		}
		return llmtest.Text("Нашёл статьи о рысях.")
	}
	s, _ := r.m.Start(StartOptions{Request: agents.Request{Text: "что есть о рысях?"}})
	v := wait(t, s)

	second := &agentstest.Brain{}
	var firstReq llm.Request
	var once sync.Once
	fake2 := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		once.Do(func() { firstReq = req })
		return llmtest.Text("Помню: мы искали рысей."), nil
	}}
	_ = second
	r.fake = fake2
	m2 := r.manager()
	if n, problems := m2.Load(); n != 1 || len(problems) != 0 {
		t.Fatalf("загрузка после перезапуска: %d %v", n, problems)
	}
	s2, err := m2.Send(v.ConversationID, agents.Request{Text: "о чём мы говорили?"})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s2)
	var callID string
	var toolReply bool
	for _, msg := range firstReq.Messages {
		for _, c := range msg.ToolCalls {
			callID = c.ID
		}
		if msg.Role == llm.RoleTool && msg.ToolCallID != "" && msg.ToolCallID == callID {
			toolReply = true
		}
	}
	if callID == "" || !toolReply {
		t.Fatalf("история первого запуска без вызова и ответа инструмента: %+v", firstReq.Messages)
	}
	if !strings.Contains(firstReq.Messages[len(firstReq.Messages)-1].Content, "о чём мы говорили") {
		t.Fatal("новая реплика не последняя")
	}
	d, _ := m2.Get(v.ConversationID)
	if len(d.TurnList) != 2 {
		t.Fatalf("ходов после перезапуска %d", len(d.TurnList))
	}
}

func TestCompareGoesToBranch(t *testing.T) {
	r := newRig(t)
	s, _ := r.m.Start(StartOptions{Request: agents.Request{Text: "рысь"}})
	v := wait(t, s)
	root, _ := r.m.Get(v.ConversationID)
	s, err := r.m.Send(v.ConversationID, agents.Request{Text: "Сравни рысь и манул"})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s)
	d, _ := r.m.Get(v.ConversationID)
	if len(d.BranchTree) != 2 || d.Branch == root.Branch || len(d.Checkpoints) != 1 {
		t.Fatalf("сравнение не ушло в ветку: %+v", d.BranchTree)
	}
	if len(d.Cards.Comparisons) != 1 || len(d.TurnList) != 2 {
		t.Fatalf("ветка сравнения: %d сравнений, %d ходов", len(d.Cards.Comparisons), len(d.TurnList))
	}
	// Вернуться к разговору об одной рыси, ничего не потеряв.
	back, err := r.m.Switch(v.ConversationID, root.Branch)
	if err != nil || len(back.Cards.Comparisons) != 0 || len(back.Cards.Cards) != 1 || len(back.TurnList) != 1 {
		t.Fatalf("возврат в основную ветку: %+v %v", back.Cards, err)
	}
}

func TestBusyAndEmpty(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	r.fake.Fn = func(req llm.Request) (llm.Response, error) {
		<-release
		return llmtest.Text("ок"), nil
	}
	s, _ := r.m.Start(StartOptions{Request: agents.Request{Text: "а что вы знаете о волках?"}})
	id := s.View().ConversationID
	if _, err := r.m.Send(id, agents.Request{Text: "ещё"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("второй ход во время первого: %v", err)
	}
	if _, err := r.m.Mark(id, ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("правка во время хода: %v", err)
	}
	if err := r.m.Delete(id); !errors.Is(err, ErrBusy) {
		t.Fatalf("удаление во время хода: %v", err)
	}
	close(release)
	wait(t, s)
	if _, err := r.m.Send(id, agents.Request{Text: "  "}); !errors.Is(err, ErrEmpty) {
		t.Fatalf("пустое сообщение: %v", err)
	}
	if _, err := r.m.Send("nope", agents.Request{Text: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("несуществующий диалог: %v", err)
	}
}

func TestFailedTurnAddsNoMessages(t *testing.T) {
	r := newRig(t)
	r.fake.Fn = func(llm.Request) (llm.Response, error) { return llm.Response{}, errors.New("сеть") }
	s, _ := r.m.Start(StartOptions{Request: agents.Request{Text: "что едят волки?"}})
	v := wait(t, s)
	if v.Status != StatusFailed || !strings.Contains(v.Error, "сеть") {
		t.Fatalf("ход: %+v", v)
	}
	d, _ := r.m.Get(v.ConversationID)
	if d.Messages != 0 || len(d.TurnList) != 1 || d.TurnList[0].Status != history.TurnFailed {
		t.Fatalf("неудачный ход: %d сообщений", d.Messages)
	}
}

type testHook struct {
	name   string
	before func(*Turn) error
	after  func(*Turn) error
}

func (h testHook) Name() string { return h.name }
func (h testHook) Before(_ context.Context, t *Turn) error {
	if h.before == nil {
		return nil
	}
	return h.before(t)
}
func (h testHook) After(_ context.Context, t *Turn) error {
	if h.after == nil {
		return nil
	}
	return h.after(t)
}
func (h testHook) Describe(c *history.Conversation) any {
	return map[string]int{"owners": len(c.Owners)}
}

func TestHooksBlocksHandlerAndSurvival(t *testing.T) {
	r := newRig(t)
	var seen llm.Request
	r.brain.LeadScript = func(req llm.Request, step int) llm.Response {
		seen = req
		return llmtest.Text("ответ")
	}
	facts := testHook{name: "facts", before: func(tr *Turn) error {
		tr.Facts.Set("имя", "Алекс", tr.Number)
		tr.Meter = history.Meter{Calls: 1}
		tr.Extra("memory", map[string]string{"записано": "имя"})
		return nil
	}}
	block := testHook{name: "charter", before: func(tr *Turn) error {
		tr.AddBlock(features.Block{Feature: features.Charter, Text: "свод"})
		return errors.New("свод прочитан не полностью")
	}, after: func(*Turn) error { return errors.New("страж недоступен") }}
	r.m = r.manager(facts, block)
	s, _ := r.m.Start(StartOptions{Request: agents.Request{Text: "Меня зовут Алекс, как дела?"}})
	v := wait(t, s)
	if v.Status != StatusDone {
		t.Fatalf("ошибки хуков уронили ход: %+v", v)
	}
	// Свод — раньше карточки фактов, хоть хук и добавил его позже: порядок
	// задаёт реестр, а не порядок хуков.
	if seen.Messages[1].Content != "свод" || !strings.Contains(seen.Messages[2].Content, "Алекс") {
		t.Fatalf("блоки запроса: %q | %q", seen.Messages[1].Content, seen.Messages[2].Content)
	}
	d, _ := r.m.Get(v.ConversationID)
	if d.Facts.Version != 1 || d.Meter.Calls != 1 || d.Extras["facts"] == nil {
		t.Fatalf("итог хуков: %+v %+v", d.Facts, d.Meter)
	}
	var mem map[string]string
	if !d.TurnList[0].Extra("memory", &mem) || mem["записано"] != "имя" {
		t.Fatal("итог механизма не записан в ход")
	}
	notes := 0
	for _, e := range d.TurnList[0].Events {
		if strings.Contains(e.Title, "не сработал") {
			notes++
		}
	}
	if notes != 2 {
		t.Fatalf("ошибки хуков в журнале: %d", notes)
	}

	// Хук берёт ход на себя: так подборку ведёт свой агент.
	takeover := testHook{name: "collection", before: func(tr *Turn) error {
		tr.Collection, tr.CollectionTitle = "c1", "совы"
		tr.Handler = func(context.Context) (agents.Result, error) {
			return agents.Result{Route: "collection", User: tr.Request.Text, Text: "план подборки",
				Added: []llm.Message{{Role: llm.RoleUser, Content: tr.Request.Text}, {Role: llm.RoleAssistant, Content: "план подборки"}}}, nil
		}
		return nil
	}}
	r.m = r.manager(takeover)
	s, _ = r.m.Start(StartOptions{Request: agents.Request{Text: "собери подборку сов"}})
	v = wait(t, s)
	d, _ = r.m.Get(v.ConversationID)
	if v.Reply != "план подборки" || d.Collection != "c1" || d.TurnList[0].Collection != "c1" || d.TurnList[0].Route != "collection" {
		t.Fatalf("ход подборки: %+v, %s", v, d.Collection)
	}
}

func TestWindowAndFactsFallbackNote(t *testing.T) {
	r := newRig(t)
	var lastLen int
	r.brain.LeadScript = func(req llm.Request, step int) llm.Response {
		lastLen = len(req.Messages)
		return llmtest.Text("ответ")
	}
	fs := features.Catalog().Defaults().With(features.Facts, false)
	d, _ := r.m.Create(StartOptions{Features: fs})
	for i := range 7 {
		s, err := r.m.Send(d.ID, agents.Request{Text: "вопрос номер " + string(rune('0'+i)) + "?"})
		if err != nil {
			t.Fatal(err)
		}
		wait(t, s)
	}
	// system + окно 8 + новая реплика.
	if lastLen != 1+history.DefaultWindow+1 {
		t.Fatalf("окно: в запросе %d сообщений", lastLen)
	}
	full, _ := r.m.Get(d.ID)
	last := full.TurnList[len(full.TurnList)-1]
	found := false
	for _, e := range last.Events {
		if e.Kind == agent.EventMechanism && strings.Contains(e.Title, "карточка фактов выключена") {
			found = true
		}
	}
	if !found {
		t.Fatal("выключенная карточка фактов — молчаливая дыра (ФТ-48)")
	}
	// Окно выключено — модели уходит вся история.
	if _, err := r.m.SetFeature(d.ID, features.Window, false); err != nil {
		t.Fatal(err)
	}
	s, _ := r.m.Send(d.ID, agents.Request{Text: "и ещё вопрос?"})
	wait(t, s)
	if lastLen != 1+14+1 {
		t.Fatalf("без окна в запросе %d сообщений", lastLen)
	}
}

func TestSetFeatureValidates(t *testing.T) {
	r := newRig(t)
	d, _ := r.m.Create(StartOptions{})
	if _, err := r.m.SetFeature(d.ID, features.Charter, false); err == nil {
		t.Fatal("свод выключен при включённом страже")
	}
	if _, err := r.m.SetFeature(d.ID, "nope", true); err == nil {
		t.Fatal("неизвестный механизм")
	}
	got, err := r.m.SetFeature(d.ID, features.Guard, false)
	if err != nil || got.Features.On(features.Guard) {
		t.Fatalf("страж: %v", err)
	}
	if _, err := r.m.Create(StartOptions{Owners: []string{"../x"}}); err == nil {
		t.Fatal("плохой собеседник")
	}
	bad := features.Catalog().Defaults().With(features.Charter, false)
	if _, err := r.m.Create(StartOptions{Features: bad}); err == nil {
		t.Fatal("несогласованный набор")
	}
}

func TestBranchesViaManager(t *testing.T) {
	r := newRig(t)
	s, _ := r.m.Start(StartOptions{Request: agents.Request{Text: "рысь"}})
	v := wait(t, s)
	d, err := r.m.Mark(v.ConversationID, "после рыси")
	if err != nil || len(d.Checkpoints) != 1 {
		t.Fatalf("точка: %v", err)
	}
	d, err = r.m.Fork(v.ConversationID, d.Checkpoints[0].ID, "манул")
	if err != nil || len(d.BranchTree) != 2 || d.BranchTree[1].Name != "манул" || !d.BranchTree[1].Active {
		t.Fatalf("ветка: %+v %v", d.BranchTree, err)
	}
	if _, err := r.m.Fork(v.ConversationID, "nope", ""); err == nil {
		t.Fatal("ветка от несуществующей точки")
	}
	if _, err := r.m.Switch("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("переключение в несуществующем диалоге")
	}
}

func TestExportAndRaw(t *testing.T) {
	r := newRig(t)
	s, _ := r.m.Start(StartOptions{Request: agents.Request{Text: "рысь"}})
	v := wait(t, s)
	d, _ := r.m.Get(v.ConversationID)
	name, md, err := r.m.Export(v.ConversationID, "card", d.Cards.Cards[0].ID)
	if err != nil || name != "Обыкновенная рысь.md" || !strings.Contains(md, "## Источники") {
		t.Fatalf("выгрузка: %q %v", name, err)
	}
	for _, bad := range [][2]string{{"card", "nope"}, {"comparison", "0"}, {"comparison", "x"}, {"what", ""}} {
		if _, _, err := r.m.Export(v.ConversationID, bad[0], bad[1]); err == nil {
			t.Errorf("выгрузка %v", bad)
		}
	}
	if _, _, err := r.m.Export("nope", "card", "x"); !errors.Is(err, ErrNotFound) {
		t.Error("выгрузка из несуществующего диалога")
	}
	path, raw, err := r.m.Raw(v.ConversationID)
	if err != nil || !strings.Contains(path, v.ConversationID) || !strings.Contains(string(raw), `"schema": 2`) {
		t.Fatalf("файл: %v", err)
	}
	if _, _, err := r.m.Raw("nope"); !errors.Is(err, ErrNotFound) {
		t.Error("файл несуществующего диалога")
	}
	if fileName(`a/b:c`) != "a_b_c.md" {
		t.Error("fileName")
	}
	if err := r.m.Delete(v.ConversationID); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Delete(v.ConversationID); !errors.Is(err, ErrNotFound) {
		t.Fatal("повторное удаление")
	}
}

func TestGroupsGoInStep(t *testing.T) {
	r := newRig(t)
	reg := features.Catalog()
	base := reg.Defaults()
	if _, _, err := r.m.StartGroup("x", []Lane{{Name: "a", Features: base}, {Name: "b", Features: base.With(features.Guard, false).With(features.Charter, false)}}); !errors.Is(err, ErrLanesDiffer) {
		t.Fatalf("дорожки, отличающиеся двумя механизмами: %v", err)
	}
	if _, _, err := r.m.StartGroup("x", nil); err == nil {
		t.Fatal("стенд без дорожек")
	}
	group, lanes, err := r.m.StartGroup("Привратник", []Lane{{Name: "с привратником", Features: base}, {Name: "без привратника", Features: base.With(features.Gatekeeper, false)}})
	if err != nil || len(lanes) != 2 || lanes[0].Owners[0] == lanes[1].Owners[0] {
		t.Fatalf("стенд: %v, собеседники %v %v", err, lanes[0].Owners, lanes[1].Owners)
	}
	sessions, err := r.m.SendGroup(group, agents.Request{Text: "рысь"})
	if err != nil || len(sessions) != 2 {
		t.Fatal(err)
	}
	if _, err := r.m.SendGroup(group, agents.Request{Text: "манул"}); err != nil && !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	for _, s := range sessions {
		wait(t, s)
	}
	details, ok := r.m.Group(group)
	if !ok || len(details) != 2 || details[0].Lane != "с привратником" {
		t.Fatalf("дорожки: %+v", details)
	}
	if r.brain.Calls("gatekeeper") != 1 {
		t.Fatalf("привратник звался %d раз на две дорожки", r.brain.Calls("gatekeeper"))
	}
	if _, err := r.m.SendGroup("nope", agents.Request{Text: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatal("несуществующий стенд")
	}
	// Стенд переживает перезапуск в том же порядке дорожек.
	m2 := r.manager()
	m2.Load()
	again, _ := m2.Group(group)
	if len(again) != 2 || again[0].ID != details[0].ID || len(m2.Groups()) != 1 {
		t.Fatalf("стенд после перезапуска: %d", len(again))
	}
	if laneSlug("Без привратника!") != "lane" || laneSlug("mcp on") != "mcp-on" {
		t.Error("laneSlug")
	}
}

func TestSessionSubscribeAfterClose(t *testing.T) {
	s := newSession(View{ID: "x"})
	s.Log(agent.Event{Kind: agent.EventLLMReply, Usage: &llm.Usage{Prompt: 1}, Cost: &llm.Cost{USD: 1, Known: true}})
	s.Log(agent.Event{Kind: agent.EventToolCall})
	s.Publish(agent.Update{Kind: "card"})
	snap, ch, unsub := s.Subscribe()
	if len(snap.Events) != 2 || len(snap.Updates) != 1 || snap.View.Totals.LLMCalls != 1 || snap.View.Totals.ToolCalls != 1 {
		t.Fatalf("снимок: %+v", snap)
	}
	s.finish("ответ", "", agent.Context{}, nil)
	<-ch // state
	unsub()
	unsub()
	s.setSaveError(errors.New("диск"))
	s.closeSubs()
	s.closeSubs()
	_, ch2, _ := s.Subscribe()
	if _, open := <-ch2; open {
		t.Fatal("подписка на закрытый ход должна сразу закрыться")
	}
	if v := s.Wait(time.Second); v.SaveError != "диск" || v.Status != StatusDone {
		t.Fatalf("итог: %+v", v)
	}
	if mustJSON(func() {}) != "{}" {
		t.Fatal("mustJSON")
	}
}

func TestEviction(t *testing.T) {
	r := newRig(t)
	for i := range maxTurnsInMemory + 5 {
		s := newSession(View{ID: history.NewID(), Status: StatusDone})
		s.view.Status = StatusDone
		r.m.turns[s.view.ID] = s
		r.m.order = append(r.m.order, s.view.ID)
		_ = i
	}
	r.m.evictLocked()
	if len(r.m.order) != maxTurnsInMemory {
		t.Fatalf("ходов в памяти %d", len(r.m.order))
	}
	if _, ok := r.m.Turn(r.m.order[0]); !ok {
		t.Fatal("Turn")
	}
}
