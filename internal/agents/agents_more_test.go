package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

// Координатор для своего агента (подборка) открывает карточки тем же путём,
// что обычный ход, и собирает правки и расход в итог.
func TestCoordinatorOpensCardAndReadsSections(t *testing.T) {
	e := newEnv(t)
	co, err := NewCoordinator(context.Background(), e.d, Request{Features: e.fs}, e.safe)
	if err != nil {
		t.Fatal(err)
	}
	if co.Registry() == nil || !co.Registry().Has("read_wikipedia") || co.Features().Empty() {
		t.Fatal("реестр и механизмы координатора")
	}
	c, nf, err := co.OpenCard(context.Background(), "рысь")
	if err != nil || nf != nil || c == nil || c.Latin != "Lynx lynx" {
		t.Fatalf("карточка: %+v %+v %v", c, nf, err)
	}
	secs := co.ReadSections(context.Background(), *c, []string{"diet", "diet", "нет-такой-темы"})
	if len(secs) != 1 || secs[0].Status != card.SectionRead {
		t.Fatalf("разделы: %+v", secs)
	}
	got, ok := co.Card(c.ID)
	if !ok || got.Section("diet").Status != card.SectionRead {
		t.Fatalf("карточка в состоянии хода: %+v", got.Section("diet"))
	}
	if _, ok := co.Card("nope"); ok {
		t.Fatal("несуществующая карточка")
	}
	// Уже открытое животное второй раз не ищется.
	calls := e.fake.Calls()
	if again, _, _ := co.OpenCard(context.Background(), "  рысь "); again == nil || e.fake.Calls() != calls {
		t.Fatal("повторное открытие звало модель")
	}
	res := co.Result("collection", "собери", "готово", nil)
	if len(res.Added) != 2 || res.Added[0].Content != "собери" || res.Added[1].Content != "готово" {
		t.Fatalf("сообщения итога: %+v", res.Added)
	}
	if len(res.Deltas) < 3 || res.Stats.Steps == 0 || res.Route != "collection" || res.Effective.Empty() {
		t.Fatalf("итог: %d правок, %d шагов", len(res.Deltas), res.Stats.Steps)
	}
	own := []llm.Message{{Role: llm.RoleUser, Content: "x"}}
	if got := co.Result("r", "u", "t", own); len(got.Added) != 1 {
		t.Fatal("свои сообщения итога заменены")
	}
}

// Прогон своего агента учитывается в расходе хода.
func TestCoordinatorRunCountsStats(t *testing.T) {
	e := newEnv(t)
	e.d.Runner.LLM = &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) { return llmtest.Text("план"), nil }}
	co, err := NewCoordinator(context.Background(), e.d, Request{Features: e.fs}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := co.Run(context.Background(), agent.Spec{Name: "compiler", System: "Ты ведёшь подборку", AllowText: true, MaxSteps: 2},
		agent.Prepared{User: "собери сов", Features: e.fs})
	if err != nil || reply.Text != "план" {
		t.Fatalf("прогон: %q %v", reply.Text, err)
	}
	reply2, _ := co.Run(context.Background(), agent.Spec{Name: "compiler", System: "Ты ведёшь подборку", AllowText: true, MaxSteps: 2},
		agent.Prepared{User: "ещё", Features: e.fs})
	if got := co.Result("r", "u", "t", nil).Stats.Steps; got != reply.Stats.Steps+reply2.Stats.Steps {
		t.Fatalf("шаги двух прогонов: %d", got)
	}
	// Ошибка модели доходит до вызывающего, расход всё равно учтён.
	e.d.Runner.LLM = &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) { return llm.Response{}, errors.New("сеть") }}
	co2, _ := NewCoordinator(context.Background(), e.d, Request{Features: e.fs}, nil)
	if _, err := co2.Run(context.Background(), agent.Spec{Name: "compiler", System: "x", AllowText: true, MaxSteps: 1}, agent.Prepared{User: "u"}); err == nil {
		t.Fatal("ошибка модели потерялась")
	}
}

// Источники недоступны — координатор не создаётся; откат — пишется в журнал.
func TestCoordinatorSources(t *testing.T) {
	e := newEnv(t)
	d := e.d
	d.Sources = brokenSources{}
	if _, err := NewCoordinator(context.Background(), d, Request{Features: e.fs}, nil); err == nil || !strings.Contains(err.Error(), "сервер недоступен") {
		t.Fatalf("ошибка источников: %v", err)
	}
	reg, _, _, _ := e.d.Sources.For(context.Background(), e.fs)
	d.Sources = fallbackSources{reg: reg}
	co, err := NewCoordinator(context.Background(), d, Request{Features: e.fs.With("mcp", true)}, e.safe)
	if err != nil || co.Features().On("mcp") {
		t.Fatalf("откат: %v", err)
	}
	if len(e.events(agent.EventMechanism)) != 1 {
		t.Fatalf("откат в журнале: %+v", e.rec.Events)
	}
}

// Специалистам уходят только свод и профиль: память и история им не нужны.
func TestHeadBlocksKeepsCharterAndProfile(t *testing.T) {
	blocks := []features.Block{
		{Feature: features.Charter, Text: "свод"},
		{Feature: features.MemoryLong, Text: "память"},
		{Feature: features.Profile, Text: "профиль"},
		{Feature: features.Facts, Text: "факты"},
	}
	got := headBlocks(blocks)
	if len(got) != 2 || got[0].Text != "свод" || got[1].Text != "профиль" {
		t.Fatalf("блоки специалиста: %+v", got)
	}
	if headBlocks(nil) != nil {
		t.Fatal("пустой набор блоков")
	}
}

// Дерево кодом: без инструмента — карточка как есть; неудача GBIF —
// оговорка в карточке, а не ошибка хода.
func TestWithTreeFallbacks(t *testing.T) {
	c := card.Card{ID: "1", Name: "Рысь", TaxonKey: toolstest.KeyLynx}
	rec := &agent.Recorder{}
	got := withTree(context.Background(), card.NewTracker(), tools.MustRegistry(), c, rec)
	if len(got.Tree) != 0 || len(got.Notes) != 0 || len(rec.Events) != 0 {
		t.Fatalf("без taxon_tree: %+v", got)
	}
	broken := tools.Func{S: tools.Spec{Name: "taxon_tree"}, Fn: func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("GBIF не отвечает")
	}}
	got = withTree(context.Background(), card.NewTracker(), tools.MustRegistry(broken), c, rec)
	if len(got.Notes) != 1 || !strings.Contains(got.Notes[0], "GBIF не отвечает") {
		t.Fatalf("оговорка: %+v", got.Notes)
	}
	kinds := rec.Kinds()
	if len(kinds) != 2 || kinds[0] != agent.EventToolCall || kinds[1] != agent.EventToolError {
		t.Fatalf("журнал вызова кодом: %v", kinds)
	}
}

// Вызов кодом через MCP помечается в журнале так же, как вызов моделью, а
// идентификаторы вызовов не повторяются.
func TestCodeCallMarksViaAndUniqueIDs(t *testing.T) {
	tool := tools.Func{S: tools.Spec{Name: "taxon_tree", Via: tools.ViaMCP}, Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
		return `{"ok":true}`, nil
	}}
	rec := &agent.Recorder{}
	out, id1, err := codeCall(context.Background(), rec, tool, `{}`)
	if err != nil || out != `{"ok":true}` {
		t.Fatalf("вызов: %q %v", out, err)
	}
	_, id2, _ := codeCall(context.Background(), rec, tool, `{}`)
	if id1 == id2 || !strings.HasPrefix(id1, "code_taxon_tree_") {
		t.Fatalf("идентификаторы: %q %q", id1, id2)
	}
	if !strings.Contains(rec.Events[0].Title, "через MCP") || rec.Events[0].Via != tools.ViaMCP || rec.Events[1].Kind != agent.EventToolResult {
		t.Fatalf("журнал: %+v", rec.Events[:2])
	}
}

// Читателю раздела нужен read_wikipedia: без него — ошибка, а не паника.
func TestReadSectionNeedsReadTool(t *testing.T) {
	e := newEnv(t)
	topic, _ := card.TopicOf("diet")
	_, _, err := ReadSection(context.Background(), e.d, tools.MustRegistry(), card.Card{ID: "1", Article: "Рысь"}, topic, nil, e.fs, agent.Nop{})
	if err == nil || !strings.Contains(err.Error(), "read_wikipedia") {
		t.Fatalf("реестр без чтения: %v", err)
	}
	if _, err := Identify(context.Background(), e.d, tools.MustRegistry(), "рысь", nil, e.fs, agent.Nop{}); err == nil {
		t.Fatal("идентификатор без инструментов")
	}
}

// Ошибка модели у читателя раздела доходит с названием темы.
func TestReadSectionModelError(t *testing.T) {
	e := newEnv(t)
	e.d.Runner.LLM = &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) { return llm.Response{}, errors.New("таймаут") }}
	reg, _, _, _ := e.d.Sources.For(context.Background(), e.fs)
	topic, _ := card.TopicOf("habitat")
	_, _, err := ReadSection(context.Background(), e.d, reg, card.Card{ID: "1", Article: "Рысь"}, topic, nil, e.fs, agent.Nop{})
	if err == nil || !strings.Contains(err.Error(), topic.Title) || !strings.Contains(err.Error(), "таймаут") {
		t.Fatalf("ошибка раздела: %v", err)
	}
	if _, err := Identify(context.Background(), e.d, reg, "рысь", nil, e.fs, agent.Nop{}); err == nil || !strings.Contains(err.Error(), "идентификатор") {
		t.Fatalf("ошибка идентификатора: %v", err)
	}
}

// Без оглавления специалист получает подсказку открыть статью целиком.
func TestHeadingsOr(t *testing.T) {
	if got := headingsOr(nil); len(got) != 1 || !strings.Contains(got[0], "оглавление неизвестно") {
		t.Fatalf("пустое оглавление: %v", got)
	}
	if got := headingsOr([]string{"Питание"}); len(got) != 1 || got[0] != "Питание" {
		t.Fatalf("оглавление: %v", got)
	}
}
