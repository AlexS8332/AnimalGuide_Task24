package agentstest_test

import (
	"context"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents/agentstest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

// Подставная модель ведёт настоящих агентов по подставным источникам от
// начала до конца: так проверяется, что сценарий агентов добросовестен —
// иначе на нём держались бы ложные зелёные тесты.
func deps(t *testing.T, b *agentstest.Brain) agents.Deps {
	t.Helper()
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)
	reg := tools.MustRegistry(tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL)...)
	return agents.Deps{Runner: agent.Runner{LLM: &llmtest.Fake{Fn: b.Chat}, Model: llm.DefaultModel},
		Features: features.Catalog(), Sources: agents.Local{Registry: reg}}
}

func run(t *testing.T, d agents.Deps, req agents.Request) agents.Result {
	t.Helper()
	if req.Features.Empty() {
		req.Features = features.Catalog().Defaults()
	}
	res, err := agents.Run(context.Background(), d, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Карточка, раздел и сравнение — полный путь сценариев идентификатора,
// читателя и сравнивающего.
func TestBrainDrivesAllSpecialists(t *testing.T) {
	b := &agentstest.Brain{}
	d := deps(t, b)
	open := run(t, d, agents.Request{Kind: agents.KindOpen, Name: "рысь"})
	st := card.Fold(open.Deltas)
	c := st.CurrentCard()
	if c == nil || c.Latin != "Lynx lynx" {
		t.Fatalf("карточка: %+v", c)
	}
	sec := run(t, d, agents.Request{Kind: agents.KindSection, CardID: c.ID, Topic: "breeding", Deltas: open.Deltas})
	st = card.Fold(append(open.Deltas, sec.Deltas...))
	if s := st.Card(c.ID).Section("breeding"); s.Status != card.SectionRead {
		t.Fatalf("размножение: %+v", s)
	}
	cmp := run(t, d, agents.Request{Kind: agents.KindCompare, A: "рысь", B: "манул"})
	if len(card.Fold(cmp.Deltas).Comparisons) != 1 || b.Calls("comparer") == 0 || b.Calls("section") == 0 {
		t.Fatalf("сравнение: %d вызовов сравнивающего", b.Calls("comparer"))
	}
}

// Статьи нет — сценарий сдаёт «сведений нет», а не похожее.
func TestBrainReportsNotFound(t *testing.T) {
	b := &agentstest.Brain{}
	d := deps(t, b)
	res := run(t, d, agents.Request{Kind: agents.KindOpen, Name: "единорог"})
	if st := card.Fold(res.Deltas); len(st.Cards) != 0 || len(st.NotFound) != 1 {
		t.Fatalf("единорог: %+v", st)
	}
}

// Выдуманная латынь: после отказа трекера сценарий исправляется.
func TestBrainFakeLatinThenFixed(t *testing.T) {
	b := &agentstest.Brain{FakeLatin: "Lynx striatus"}
	d := deps(t, b)
	res := run(t, d, agents.Request{Kind: agents.KindOpen, Name: "рысь"})
	st := card.Fold(res.Deltas)
	if c := st.CurrentCard(); c == nil || c.Latin != "Lynx lynx" {
		t.Fatalf("карточка после отказа: %+v", c)
	}
}
