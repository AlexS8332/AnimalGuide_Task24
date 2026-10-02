package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents/agentstest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

type env struct {
	d     Deps
	b     *agentstest.Brain
	fake  *llmtest.Fake
	wiki  *toolstest.Wiki
	gbif  *toolstest.GBIF
	fs    features.Set
	rec   *agent.Recorder
	safe  *agent.Safe
	cards []card.Delta
}

func newEnv(t *testing.T) *env {
	t.Helper()
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)
	b := &agentstest.Brain{}
	fake := &llmtest.Fake{Fn: b.Chat}
	reg := tools.MustRegistry(tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL)...)
	rec := &agent.Recorder{}
	e := &env{
		d: Deps{Runner: agent.Runner{LLM: fake, Model: llm.DefaultModel}, Features: features.Catalog(), Sources: Local{Registry: reg}},
		b: b, fake: fake, wiki: wiki, gbif: gbif, fs: features.Catalog().Defaults(), rec: rec, safe: &agent.Safe{E: rec},
	}
	return e
}

func (e *env) run(t *testing.T, req Request) Result {
	t.Helper()
	if req.Features.Empty() {
		req.Features = e.fs
	}
	req.Deltas = append(req.Deltas, e.cards...)
	res, err := Run(context.Background(), e.d, req, e.safe)
	if err != nil {
		t.Fatalf("ход %+v: %v", req, err)
	}
	for i := range res.Deltas {
		res.Deltas[i].Turn = "t"
	}
	e.cards = append(e.cards, res.Deltas...)
	return res
}

func (e *env) state() card.State { return card.Fold(e.cards) }

func (e *env) events(kind string) []agent.Event {
	var out []agent.Event
	for _, ev := range e.rec.Events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func TestClassify(t *testing.T) {
	st := card.Fold([]card.Delta{{Kind: card.DeltaCard, Card: &card.Card{ID: "1", Name: "Обыкновенная рысь"}}})
	cases := []struct{ text, kind, a, b string }{
		{"рысь", KindOpen, "рысь", ""},
		{"Малая выхухоль", KindOpen, "Малая выхухоль", ""},
		{"Lynx lynx", KindOpen, "Lynx lynx", ""},
		{"«шурундук пятнистый»", KindOpen, "шурундук пятнистый", ""},
		{"Сравни рысь и манула", KindCompare, "рысь", "манула"},
		{"сравните её с манулом", KindCompare, "Обыкновенная рысь", "манулом"},
		{"а чем она питается?", KindMessage, "", ""},
		{"Расскажи про рысь и где она живёт", KindMessage, "", ""},
		{"Меня зовут Алекс", KindMessage, "", ""},
		{"Собери подборку: хищники тайги", KindMessage, "", ""},
		{"", KindMessage, "", ""},
		{"очень длинное название из многих слов подряд здесь", KindMessage, "", ""},
	}
	for _, c := range cases {
		kind, a, b := Classify(c.text, st)
		if kind != c.kind || a != c.a || b != c.b {
			t.Errorf("Classify(%q) = %s %q %q, ждали %s %q %q", c.text, kind, a, b, c.kind, c.a, c.b)
		}
	}
	// «Её» без текущей карточки — не сравнение.
	if kind, _, _ := Classify("сравни её с манулом", card.State{}); kind == KindCompare {
		t.Error("местоимение без карточки стало сравнением")
	}
}

func TestOpenCardHappyPath(t *testing.T) {
	e := newEnv(t)
	res := e.run(t, Request{Kind: KindMessage, Text: "рысь"})
	if res.Route != RouteCard {
		t.Fatalf("маршрут %s", res.Route)
	}
	st := e.state()
	c := st.CurrentCard()
	if c == nil || c.Latin != "Lynx lynx" || c.TaxonKey != toolstest.KeyLynx || c.Article != "Обыкновенная рысь" {
		t.Fatalf("карточка: %+v", c)
	}
	if len(c.Tree) != 5 || c.TreeWhy == nil || !strings.HasPrefix(c.TreeWhy.CallID, "code_taxon_tree") {
		t.Fatalf("дерево достроено кодом: %+v %+v", c.Tree, c.TreeWhy)
	}
	var felidae string
	for _, n := range c.Tree {
		if n.Name == "Felidae" {
			felidae = n.NameRu
		}
	}
	if felidae != "Кошачьи" {
		t.Fatalf("русские названия узлов от модели: %+v", c.Tree)
	}
	if !strings.Contains(res.Text, "Lynx lynx") || len(res.Added) != 2 || res.Added[0].Content != "рысь" {
		t.Fatalf("ответ: %q, добавлено %+v", res.Text, res.Added)
	}
	// Привратник 1 запрос + идентификатор 3 (чтение, сверка, сдача): поиск
	// по названию делает код до первого запроса, русские названия приходят
	// вместе со сверкой.
	if e.b.Calls("gatekeeper") != 1 || e.b.Calls("identifier") != 3 || res.Stats.Steps != 4 {
		t.Fatalf("запросов: привратник %d, идентификатор %d, шагов в ходе %d",
			e.b.Calls("gatekeeper"), e.b.Calls("identifier"), res.Stats.Steps)
	}
	if len(e.rec.Updates) == 0 || e.rec.Updates[0].Kind != "card" {
		t.Fatalf("карточка не опубликована частью хода: %+v", e.rec.Updates)
	}
	// Повторное «рысь» — без единого запроса к модели.
	calls := e.fake.Calls()
	res = e.run(t, Request{Kind: KindMessage, Text: "Рысь"})
	if e.fake.Calls() != calls || res.Route != RouteCard {
		t.Fatalf("повторное открытие делало запросы: %d → %d", calls, e.fake.Calls())
	}
}

// Поиск по названию делает код до первого запроса к идентификатору, а
// match_taxon приносит русские названия таксона: vernacular_names у
// идентификатора нет, а вызовы кодом видны в журнале.
func TestIdentifierPreloadsSearchAndVernacular(t *testing.T) {
	e := newEnv(t)
	var first llm.Request
	var sawNames bool
	e.fake.Fn = func(req llm.Request) (llm.Response, error) {
		if strings.Contains(req.Messages[0].Content, "агент-идентификатор") {
			if first.Messages == nil {
				first = req
			}
			if strings.Contains(agentstest.LastReply(req, "match_taxon"), `"vernacular_rus":["Обыкновенная рысь"`) {
				sawNames = true
			}
		}
		return e.b.Chat(req)
	}
	e.run(t, Request{Kind: KindOpen, Name: "рысь"})
	if agentstest.LastReply(first, "search_wikipedia") == "" {
		t.Fatal("первый запрос идентификатора — без ответа поиска")
	}
	if llmtest.HasTool(first, "vernacular_names") || !llmtest.HasTool(first, "match_taxon") {
		t.Fatal("набор инструментов идентификатора: vernacular_names отдельно не нужен")
	}
	if !sawNames {
		t.Fatal("ответ match_taxon без русских названий таксона")
	}
	var pre, vern bool
	for _, ev := range e.events(agent.EventToolCall) {
		pre = pre || (ev.Agent == "identifier" && ev.Tool == "search_wikipedia" && strings.HasPrefix(ev.CallID, "pre_"))
		vern = vern || (ev.Agent == "coordinator" && ev.Tool == "vernacular_names")
	}
	if !pre || !vern {
		t.Fatalf("журнал: поиск кодом %v, названия кодом %v", pre, vern)
	}
	st := e.state()
	if c := st.CurrentCard(); c == nil || c.Latin != "Lynx lynx" {
		t.Fatalf("карточка: %+v", c)
	}
}

func TestGatekeeperStopsBeforeExpensiveSteps(t *testing.T) {
	e := newEnv(t)
	e.b.GateNo = []string{"шурундук"}
	res := e.run(t, Request{Kind: KindOpen, Name: "шурундук пятнистый"})
	st := e.state()
	if len(st.NotFound) != 1 || !st.NotFound[0].Gate || e.b.Calls("identifier") != 0 {
		t.Fatalf("привратник: %+v, идентификатор звали %d раз", st.NotFound, e.b.Calls("identifier"))
	}
	if !strings.Contains(res.Text, "Сведений") || !strings.Contains(res.Text, "Похожее животное подставлять не буду") {
		t.Fatalf("ответ: %s", res.Text)
	}
}

// Название, о котором ветка уже решала, привратник второй раз не судит:
// его отказ повторяется без запросов, а после отказа идентификатора
// повторная попытка идёт сразу к идентификатору.
func TestGatekeeperNotAskedTwiceForSameName(t *testing.T) {
	e := newEnv(t)
	e.b.GateNo = []string{"шурундук"}
	e.run(t, Request{Kind: KindOpen, Name: "шурундук пятнистый"})
	calls := e.fake.Calls()
	res := e.run(t, Request{Kind: KindOpen, Name: "Шурундук пятнистый"})
	if e.fake.Calls() != calls || !strings.Contains(res.Text, "Сведений") {
		t.Fatalf("повторный отказ стоил запросов: %d → %d, ответ %q", calls, e.fake.Calls(), res.Text)
	}

	e.run(t, Request{Kind: KindOpen, Name: "полосатый манул"}) // привратник пропустил, идентификатор не нашёл
	gate, ident := e.b.Calls("gatekeeper"), e.b.Calls("identifier")
	e.run(t, Request{Kind: KindOpen, Name: "полосатый манул"})
	if e.b.Calls("gatekeeper") != gate || e.b.Calls("identifier") == ident {
		t.Fatalf("повтор после отказа идентификатора: привратник %d → %d, идентификатор %d → %d",
			gate, e.b.Calls("gatekeeper"), ident, e.b.Calls("identifier"))
	}
}

func TestSimilarButDifferentIsNotFound(t *testing.T) {
	e := newEnv(t)
	e.run(t, Request{Kind: KindOpen, Name: "полосатый манул"})
	st := e.state()
	if len(st.Cards) != 0 || len(st.NotFound) != 1 || st.NotFound[0].Gate {
		t.Fatalf("похожее подставлено: %+v", st)
	}
}

func TestGatekeeperOffMakesNoRequests(t *testing.T) {
	e := newEnv(t)
	e.fs = e.fs.With(features.Gatekeeper, false)
	e.run(t, Request{Kind: KindOpen, Name: "рысь"})
	if e.b.Calls("gatekeeper") != 0 {
		t.Fatal("выключенный привратник делал запросы")
	}
	found := false
	for _, ev := range e.events(agent.EventMechanism) {
		if ev.Mechanism == string(features.Gatekeeper) && strings.Contains(ev.Title, "выключен") {
			found = true
		}
	}
	if !found {
		t.Fatal("выключенный механизм — молчаливая дыра (ФТ-48)")
	}
}

func TestUnverifiedLatinIsRefusedThenFixed(t *testing.T) {
	e := newEnv(t)
	e.b.FakeLatin = "Lynx striatus"
	e.run(t, Request{Kind: KindOpen, Name: "рысь"})
	st := e.state()
	if c := st.CurrentCard(); c == nil || c.Latin != "Lynx lynx" {
		t.Fatalf("после отказа карточка должна быть с подтверждённой латынью: %+v", c)
	}
	rejected := 0
	for _, ev := range e.events(agent.EventToolError) {
		if ev.Final && ev.Rejected && strings.Contains(ev.Detail, "не подтверждено GBIF") {
			rejected++
		}
	}
	if rejected != 1 {
		t.Fatalf("отказов по трекеру %d", rejected)
	}
}

func TestTrackerOffAcceptsTextAndMarksUnverified(t *testing.T) {
	e := newEnv(t)
	e.b.TextOnly = true
	e.fs = e.fs.With(features.Tracker, false)
	e.run(t, Request{Kind: KindOpen, Name: "рысь"})
	st := e.state()
	c := st.CurrentCard()
	if c == nil || !c.Unverified || len(c.Notes) == 0 {
		t.Fatalf("контрольная дорожка: карточка без пометки: %+v", c)
	}
}

func TestProtocolErrorFailsTurnWithTracker(t *testing.T) {
	e := newEnv(t)
	e.b.TextOnly = true
	_, err := Run(context.Background(), e.d, Request{Kind: KindOpen, Name: "рысь", Features: e.fs}, e.safe)
	if !errors.Is(err, agent.ErrProtocol) {
		t.Fatalf("текст вместо завершающего инструмента должен ронять ход: %v", err)
	}
}

func openLynx(t *testing.T, e *env) card.Card {
	t.Helper()
	e.run(t, Request{Kind: KindOpen, Name: "рысь"})
	st := e.state()
	return *st.CurrentCard()
}

func TestSectionClickReadsOnceThenUsesSaved(t *testing.T) {
	e := newEnv(t)
	c := openLynx(t, e)
	res := e.run(t, Request{Kind: KindSection, CardID: c.ID, Topic: "diet"})
	st := e.state()
	s := st.Card(c.ID).Section("diet")
	if s.Status != card.SectionRead || s.Heading != "Образ жизни, поведение и питание" || s.Why == nil || s.Why.Turn == "" {
		t.Fatalf("раздел: %+v", s)
	}
	if !strings.Contains(res.Text, "зайцев") || res.Route != RouteSection {
		t.Fatalf("ответ: %q", res.Text)
	}
	calls := e.fake.Calls()
	e.run(t, Request{Kind: KindSection, CardID: c.ID, Topic: "diet"})
	if e.fake.Calls() != calls {
		t.Fatal("прочитанный раздел перечитан (ФТ-13)")
	}
	// Раздела нет в статье — «сведений нет», а не выдумка.
	e.run(t, Request{Kind: KindSection, CardID: c.ID, Topic: "lifestyle"})
	st = e.state()
	if got := st.Card(c.ID).Section("lifestyle"); got.Status != card.SectionRead && got.Status != card.SectionNone {
		t.Fatalf("образ жизни: %+v", got)
	}
}

func TestSectionRequestErrors(t *testing.T) {
	e := newEnv(t)
	if _, err := Run(context.Background(), e.d, Request{Kind: KindSection, CardID: "nope", Topic: "diet", Features: e.fs}, nil); err == nil {
		t.Fatal("раздел несуществующей карточки")
	}
	if _, err := Run(context.Background(), e.d, Request{Kind: KindNode, CardID: "nope", Features: e.fs}, nil); err == nil {
		t.Fatal("узел несуществующей карточки")
	}
}

func TestSpecialistErrorBecomesNoteNotFailure(t *testing.T) {
	e := newEnv(t)
	c := openLynx(t, e)
	e.fake.Fn = func(req llm.Request) (llm.Response, error) {
		if strings.Contains(req.Messages[0].Content, "агент-специалист") {
			return llm.Response{}, errors.New("модель недоступна")
		}
		return e.b.Chat(req)
	}
	res := e.run(t, Request{Kind: KindSection, CardID: c.ID, Topic: "habitat"})
	if !strings.Contains(res.Text, "не справился") {
		t.Fatalf("ответ: %q", res.Text)
	}
	st := e.state()
	if st.Card(c.ID).Section("habitat").Status != card.SectionUnread {
		t.Fatal("раздел неудачника отмечен прочитанным")
	}
}

func TestNodeClickNeedsNoModel(t *testing.T) {
	e := newEnv(t)
	c := openLynx(t, e)
	calls := e.fake.Calls()
	res := e.run(t, Request{Kind: KindNode, CardID: c.ID, NodeKey: toolstest.KeyFelidae, NodeName: "Felidae"})
	if e.fake.Calls() != calls {
		t.Fatal("клик по узлу звал модель")
	}
	st := e.state()
	n := st.Card(c.ID).Neighbors
	if len(n) != 1 || len(n[0].Children) != 3 || !strings.Contains(res.Text, "Рыси (Lynx)") {
		t.Fatalf("соседи: %+v, ответ %q", n, res.Text)
	}
	// Второй клик по тому же узлу — без запросов к источнику.
	before := e.gbif.Calls.Load()
	e.run(t, Request{Kind: KindNode, CardID: c.ID, NodeKey: toolstest.KeyFelidae, NodeName: "Felidae"})
	if e.gbif.Calls.Load() != before {
		t.Fatal("соседи узла запрошены повторно")
	}
}

func TestCompareOpensMissingCardsAndMarksNoData(t *testing.T) {
	e := newEnv(t)
	res := e.run(t, Request{Kind: KindMessage, Text: "Сравни рысь и манул"})
	if res.Route != RouteCompare {
		t.Fatalf("маршрут %s", res.Route)
	}
	st := e.state()
	if len(st.Cards) != 2 || len(st.Comparisons) != 1 {
		t.Fatalf("состояние: %+v", st)
	}
	cmp := st.Comparisons[0]
	if cmp.Rows[0].A.Text != "леса Евразии" || !cmp.Rows[0].A.Confirmed {
		t.Fatalf("ареал: %+v", cmp.Rows[0])
	}
	// «Внешний вид» рыси сравнивающий не читал — факт снят.
	if cmp.Rows[1].A.Text != card.NoData || cmp.Rows[1].B.Text != card.NoData {
		t.Fatalf("размеры: %+v", cmp.Rows[1])
	}
	if !strings.Contains(res.Text, "Сравнение: Обыкновенная рысь и Манул") {
		t.Fatalf("ответ: %q", res.Text)
	}
}

func TestCompareWithMissingAnimal(t *testing.T) {
	e := newEnv(t)
	e.b.GateNo = []string{"шурундук"}
	res := e.run(t, Request{Kind: KindCompare, A: "рысь", B: "шурундук"})
	if !strings.Contains(res.Text, "Сравнить не получилось") || e.b.Calls("comparer") != 0 {
		t.Fatalf("ответ %q, сравнивающий %d", res.Text, e.b.Calls("comparer"))
	}
	res = e.run(t, Request{Kind: KindCompare, A: "рысь", B: "Рысь"})
	if !strings.Contains(res.Text, "одно и то же") {
		t.Fatalf("сравнение с самим собой: %q", res.Text)
	}
}

func TestLeadOpensCardWithParallelSections(t *testing.T) {
	e := newEnv(t)
	e.b.LeadScript = func(req llm.Request, step int) llm.Response {
		if step == 0 {
			return llmtest.ToolCall("open_card", `{"name":"рысь","sections":["habitat","diet","habitat","bogus"]}`)
		}
		return llmtest.Text("Рысь живёт в лесах и охотится на зайцев.")
	}
	res := e.run(t, Request{Kind: KindMessage, Text: "Расскажи про рысь: где живёт и чем питается"})
	if res.Route != RouteLead || res.Text == "" {
		t.Fatalf("ход: %+v", res)
	}
	st := e.state()
	c := st.CurrentCard()
	if c.Section("habitat").Status != card.SectionRead || c.Section("diet").Status != card.SectionRead {
		t.Fatalf("разделы: %+v", c.Sections)
	}
	// Два специалиста по одному запросу: подходящий раздел прочитал код.
	if e.b.Calls("section") != 2 {
		t.Fatalf("запросов специалистов %d", e.b.Calls("section"))
	}
	// Ход ведущего лежит в истории с вызовами и ответами инструментов.
	if len(res.Added) != 4 || res.Added[1].ToolCalls[0].Function.Name != "open_card" || res.Added[2].ToolCallID == "" {
		t.Fatalf("история хода: %+v", res.Added)
	}
	// Расход хода — сумма всех агентов.
	if res.Stats.Steps < 2+1+3+2 {
		t.Fatalf("шагов в ходе %d", res.Stats.Steps)
	}
}

func TestLeadReadsSectionOfCurrentCardByPronoun(t *testing.T) {
	e := newEnv(t)
	openLynx(t, e)
	var sawBrief bool
	e.b.LeadScript = func(req llm.Request, step int) llm.Response {
		for _, m := range req.Messages {
			if m.Role == llm.RoleSystem && strings.Contains(m.Content, "← текущая") {
				sawBrief = true
			}
		}
		if step == 0 {
			return llmtest.ToolCall("read_card_section", `{"animal":"она","topics":["status"]}`)
		}
		return llmtest.Text("Вид вызывает наименьшие опасения.")
	}
	e.run(t, Request{Kind: KindMessage, Text: "а она редкая?"})
	if !sawBrief {
		t.Fatal("ведущий не получил карточки разговора")
	}
	st := e.state()
	if st.CurrentCard().Section("status").Status != card.SectionRead {
		t.Fatalf("раздел не дописан в карточку: %+v", st.CurrentCard().Section("status"))
	}
}

func TestLeadToolErrors(t *testing.T) {
	e := newEnv(t)
	e.b.LeadScript = func(req llm.Request, step int) llm.Response {
		switch step {
		case 0:
			return llmtest.ToolCall("read_card_section", `{"animal":"манул","topics":["diet"]}`)
		case 1:
			return llmtest.ToolCall("open_card", `{"name":"шурундук"}`)
		case 2:
			return llmtest.ToolCall("open_card", `{bad`)
		}
		return llmtest.Text("Сведений нет.")
	}
	e.b.GateNo = []string{"шурундук"}
	res := e.run(t, Request{Kind: KindMessage, Text: "что ест манул и кто такой шурундук?"})
	outs := []string{}
	for _, m := range res.Added {
		if m.Role == llm.RoleTool {
			data, _ := tools.Unwrap(m.Content)
			outs = append(outs, data)
		}
	}
	if len(outs) != 3 || !strings.Contains(outs[0], "сначала open_card") || !strings.Contains(outs[1], `"not_found":true`) || !strings.Contains(outs[2], "error") {
		t.Fatalf("ответы инструментов ведущего: %v", outs)
	}
}

type brokenSources struct{}

func (brokenSources) For(context.Context, features.Set) (*tools.Registry, features.Set, string, error) {
	return nil, features.Set{}, "", errors.New("сервер недоступен")
}

type fallbackSources struct{ reg *tools.Registry }

func (f fallbackSources) For(_ context.Context, fs features.Set) (*tools.Registry, features.Set, string, error) {
	return f.reg, fs.With("mcp", false), "MCP недоступен — ход идёт в процессе", nil
}

func TestSourcesFailureAndFallback(t *testing.T) {
	e := newEnv(t)
	d := e.d
	d.Sources = brokenSources{}
	if _, err := Run(context.Background(), d, Request{Kind: KindOpen, Name: "рысь", Features: e.fs}, nil); err == nil {
		t.Fatal("ошибка источников потерялась")
	}
	reg, _, _, _ := e.d.Sources.For(context.Background(), e.fs)
	d.Sources = fallbackSources{reg: reg}
	res, err := Run(context.Background(), d, Request{Kind: KindOpen, Name: "рысь", Features: e.fs.With("mcp", true)}, e.safe)
	if err != nil || res.Effective.On("mcp") {
		t.Fatalf("откат: %v %+v", err, res.Effective)
	}
	found := false
	for _, ev := range e.events(agent.EventMechanism) {
		if strings.Contains(ev.Title, "MCP недоступен") {
			found = true
		}
	}
	if !found {
		t.Fatal("откат не записан в журнал (ФТ-48)")
	}
	// Реестр без нужных инструментов — ошибка, а не паника.
	d.Sources = Local{Registry: tools.MustRegistry()}
	if _, err := Run(context.Background(), d, Request{Kind: KindOpen, Name: "манул", Features: e.fs.With(features.Gatekeeper, false)}, nil); err == nil {
		t.Fatal("пустой реестр")
	}
}

func TestWithCards(t *testing.T) {
	reg := features.Catalog()
	st := card.Fold([]card.Delta{{Kind: card.DeltaCard, Card: &card.Card{ID: "1", Name: "Манул", Article: "Манул"}}})
	blocks := []features.Block{{Feature: features.Charter, Text: "свод"}, {Feature: features.Facts, Text: "- имя: Алекс"}}
	out := withCards(reg, blocks, st, reg.Defaults())
	if len(out) != 2 || !strings.Contains(out[1].Text, "Манул") || !strings.Contains(out[1].Text, "имя: Алекс") {
		t.Fatalf("блоки: %+v", out)
	}
	out = withCards(reg, blocks[:1], st, reg.Defaults())
	if len(out) != 2 || out[1].Feature != features.Facts {
		t.Fatalf("новый блок карточек: %+v", out)
	}
	off := reg.Defaults().With(features.Facts, false)
	if out := withCards(reg, blocks[:1], st, off); len(out) != 1 {
		t.Fatalf("при выключенной карточке фактов блока быть не должно: %+v", out)
	}
	if out := withCards(nil, nil, card.State{}, reg.Defaults()); out != nil {
		t.Fatalf("пустое состояние: %+v", out)
	}
}

func TestParseVerdict(t *testing.T) {
	cases := map[string]Verdict{
		"Strix aluco | ДА":        {OK: true, Latin: "Strix aluco"},
		"- | НЕТ":                 {OK: false},
		"ДА":                      {OK: true},
		"  Meles meles  |  да.  ": {OK: true, Latin: "Meles meles"},
		"Desmana moschata | НЕТ":  {OK: false, Latin: "Desmana moschata"},
		"не знаю":                 {OK: false},
	}
	for text, want := range cases {
		got := ParseVerdict(text)
		if got.OK != want.OK || got.Latin != want.Latin {
			t.Errorf("ParseVerdict(%q) = %+v", text, got)
		}
	}
}

func TestTopicsIn(t *testing.T) {
	cases := map[string]string{
		"где живёт и чем питается": "diet,habitat",
		"она в Красной книге?":     "status",
		"как размножается":         "breeding",
		"расскажи про образ жизни": "lifestyle",
		"просто привет":            "",
	}
	for text, want := range cases {
		if got := strings.Join(TopicsIn(text), ","); got != want {
			t.Errorf("TopicsIn(%q) = %q, ждали %q", text, got, want)
		}
	}
}

func TestReplies(t *testing.T) {
	if !strings.Contains(sectionReply(card.Section{Status: card.SectionNone, Reason: "нет"}), "Сведений нет") ||
		!strings.Contains(sectionReply(card.Section{Status: card.SectionUnread, Reason: "x"}), "не прочитан") {
		t.Error("sectionReply")
	}
	if !strings.Contains(neighborsReply(card.Neighbors{NodeName: "X"}), "не нашлось") {
		t.Error("neighborsReply")
	}
	if orText(" ", "b") != "b" || orText("a", "b") != "a" {
		t.Error("orText")
	}
	if headingsOr(nil)[0] == "" {
		t.Error("headingsOr")
	}
	b, _ := json.Marshal(briefCard(card.Card{Name: "x"}, []card.Section{{Title: "Питание", Status: card.SectionNone, Reason: "нет"}}))
	if !strings.Contains(string(b), "сведений нет") {
		t.Errorf("briefCard: %s", b)
	}
}

func TestPlainSectionParsing(t *testing.T) {
	tr := card.NewTracker()
	c := card.Card{Article: "Манул"}
	topic, _ := card.TopicOf("diet")
	if s := plainSection(tr, c, topic, `{"found":false,"section":"","text":"нет"}`); s.Status != card.SectionNone {
		t.Errorf("не найдено: %+v", s)
	}
	if s := plainSection(tr, c, topic, `{"found":true,"section":"Питание","text":"пищухи"}`); s.Status != card.SectionRead || s.Reason == "" {
		t.Errorf("не читалось, но принято с пометкой: %+v", s)
	}
	if s := plainSection(tr, c, topic, `просто текст`); s.Reason == "" {
		t.Errorf("не по форме: %+v", s)
	}
}

func TestParsePlainCard(t *testing.T) {
	tr := card.NewTracker()
	if _, nf := parsePlain(tr, `{"not_found":"выдумка"}`, "x"); nf == nil || nf.Reason != "выдумка" {
		t.Error("not_found")
	}
	if _, nf := parsePlain(tr, `мусор`, "x"); nf == nil {
		t.Error("мусор")
	}
	tr.Record("match_taxon", "c1", `{"found":true,"usage_key":5,"canonical_name":"Lynx lynx","rank":"SPECIES"}`)
	tr.Record("read_wikipedia", "c2", `{"title":"Рысь","url":"u","intro":"x"}`)
	c, _ := parsePlain(tr, `ответ: {"name_ru":"Рысь","latin":"lynx lynx","wiki_title":"Рысь","summary":"s"}`, "рысь")
	if c == nil || c.Unverified || c.TaxonKey != 5 || len(c.Sources) != 2 {
		t.Errorf("подтверждённая карточка контрольной дорожки: %+v", c)
	}
}

// Абзац правил выключенного механизма дописывается к системному промпту;
// пустой — промпт не трогается ни на байт.
func TestRequestSystemRules(t *testing.T) {
	if (Request{}).System("база") != "база" || (Request{Rules: "  "}).System("база") != "база" {
		t.Fatal("пустые правила изменили промпт")
	}
	if got := (Request{Rules: " правила \n"}).System("база"); got != "база\n\nправила" {
		t.Fatalf("%q", got)
	}
}
