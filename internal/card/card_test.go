package card

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

// world — трекер поверх подставных источников и функция вызова инструмента
// «как будто модель»: с идентификатором вызова в контексте.
type world struct {
	tr    *Tracker
	tools map[string]tools.Tool
	n     int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)
	w := &world{tr: NewTracker(), tools: map[string]tools.Tool{}}
	for _, tool := range w.tr.ObserveAll(tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL)) {
		w.tools[tool.Spec().Name] = tool
	}
	return w
}

func (w *world) call(t *testing.T, name, args string) string {
	t.Helper()
	w.n++
	ctx := tools.WithCallID(context.Background(), "call_"+name+"_"+string(rune('0'+w.n)))
	out, err := w.tools[name].Call(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

func draft(name, latin, article, summary string) CardDraft {
	return CardDraft{Name: name, Latin: latin, Article: article, Summary: summary}
}

func refusal(t *testing.T, err error) *Refusal {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("ждали отказ, получили %v", err)
	}
	if r.What == "" || r.Why == "" || r.Available == "" || r.Do == "" {
		t.Fatalf("отказ без одной из четырёх частей: %+v", r)
	}
	return r
}

func TestCardRejectedWithoutMatch(t *testing.T) {
	w := newWorld(t)
	w.call(t, "read_wikipedia", `{"title":"Рысь"}`)
	_, err := CheckCard(w.tr, draft("Рысь", "Lynx lynx", "Обыкновенная рысь", "Кошка."), "рысь")
	r := refusal(t, err)
	if !strings.Contains(r.What, "не подтверждено GBIF") || !strings.Contains(r.Do, "match_taxon") {
		t.Fatalf("отказ не про латынь: %v", r)
	}
	if !strings.Contains(r.Available, "открыты статьи") {
		t.Fatalf("отказ не говорит, что уже есть: %v", r)
	}
}

func TestCardRejectedWithFuzzyLatin(t *testing.T) {
	w := newWorld(t)
	w.call(t, "read_wikipedia", `{"title":"Рысь"}`)
	w.call(t, "match_taxon", `{"scientific_name":"Lynx striatus"}`) // HIGHERRANK: не найдено
	_, err := CheckCard(w.tr, draft("Полосатая рысь", "Lynx striatus", "Обыкновенная рысь", "x"), "")
	refusal(t, err)
}

func TestCardRejectedWithoutArticle(t *testing.T) {
	w := newWorld(t)
	w.call(t, "match_taxon", `{"scientific_name":"Lynx lynx"}`)
	_, err := CheckCard(w.tr, draft("Рысь", "Lynx lynx", "Обыкновенная рысь", "Кошка."), "")
	r := refusal(t, err)
	if !strings.Contains(r.What, "не открыта") || !strings.Contains(r.Available, "подтверждено GBIF: Lynx lynx") {
		t.Fatalf("отказ: %v", r)
	}
	// Раздел без вступления — тоже не «открыта»: описание берётся из вступления.
	w.call(t, "read_wikipedia", `{"title":"Обыкновенная рысь","section":"Питание"}`)
	_, err = CheckCard(w.tr, draft("Рысь", "Lynx lynx", "Обыкновенная рысь", "Кошка."), "")
	refusal(t, err)
}

func TestCardRejectedWhenEmpty(t *testing.T) {
	w := newWorld(t)
	_, err := CheckCard(w.tr, CardDraft{}, "")
	r := refusal(t, err)
	if !strings.Contains(r.Available, "ничего не проверено") {
		t.Fatalf("пустой трекер: %v", r)
	}
}

func TestCardAcceptedAndBuiltByProgram(t *testing.T) {
	w := newWorld(t)
	w.call(t, "read_wikipedia", `{"title":"Рысь"}`) // перенаправление
	w.call(t, "match_taxon", `{"scientific_name":"Lynx lynx"}`)
	d := draft(" Обыкновенная рысь ", "lynx LYNX", "Рысь", "Кошка лесов Евразии.")
	d.TreeRu = append(d.TreeRu, struct {
		Name   string `json:"name"`
		NameRu string `json:"name_ru"`
	}{"Felidae", "Кошачьи"})
	c, err := CheckCard(w.tr, d, "рысь")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "2435240" || c.Latin != "Lynx lynx" || c.Rank != "SPECIES" || c.RankRu != "вид" {
		t.Fatalf("латынь и ранг — из GBIF, а не от модели: %+v", c)
	}
	if c.Article != "Обыкновенная рысь" || !strings.Contains(c.ArticleURL, "/wiki/") {
		t.Fatalf("статья по перенаправлению: %q %q", c.Article, c.ArticleURL)
	}
	if len(c.Sources) != 2 || c.Sources[0].Kind != "wikipedia" || c.Sources[1].URL != tools.TaxonURL(toolstest.KeyLynx) {
		t.Fatalf("источники ставит программа: %+v", c.Sources)
	}
	if c.LatinWhy == nil || c.LatinWhy.Tool != "match_taxon" || c.LatinWhy.CallID == "" {
		t.Fatalf("у латыни нет «почему так»: %+v", c.LatinWhy)
	}
	if c.SummaryWhy == nil || c.SummaryWhy.CallID == "" {
		t.Fatalf("у описания нет «почему так»: %+v", c.SummaryWhy)
	}
	if len(c.Sections) != len(Topics) || c.Sections[0].Status != SectionUnread {
		t.Fatalf("разделы новой карточки: %+v", c.Sections)
	}
	if len(c.Headings) != 6 || c.Headings[2] != "Подвиды" {
		t.Fatalf("оглавление без отступов: %q", c.Headings)
	}
	// Дерева ещё нет — программа достраивает его после приёма.
	if len(c.Tree) != 0 {
		t.Fatalf("дерево до taxon_tree: %+v", c.Tree)
	}
	w.call(t, "taxon_tree", `{"usage_key":2435240}`)
	nodes, callID, ok := w.tr.Tree(toolstest.KeyLynx)
	if !ok {
		t.Fatal("дерево не запомнено")
	}
	c.SetTree(nodes, &Why{Tool: "taxon_tree", CallID: callID})
	var family, self *Node
	for i := range c.Tree {
		switch c.Tree[i].Name {
		case "Felidae":
			family = &c.Tree[i]
		case "Lynx lynx":
			self = &c.Tree[i]
		}
	}
	if family == nil || family.NameRu != "Кошачьи" || family.Key != toolstest.KeyFelidae {
		t.Fatalf("русское название узла от модели, ключ — от GBIF: %+v", family)
	}
	if self == nil || self.NameRu != "Обыкновенная рысь" {
		t.Fatalf("узел самого животного: %+v", self)
	}
}

func TestCardUsesTreeIfAlreadyRead(t *testing.T) {
	w := newWorld(t)
	w.call(t, "read_wikipedia", `{"title":"Манул"}`)
	w.call(t, "match_taxon", `{"scientific_name":"Otocolobus manul"}`)
	w.call(t, "taxon_tree", `{"usage_key":2435270}`)
	c, err := CheckCard(w.tr, draft("Манул", "Otocolobus manul", "Манул", "Степная кошка."), "манул")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tree) != 5 || c.TreeWhy == nil || c.TreeWhy.Tool != "taxon_tree" {
		t.Fatalf("дерево: %+v", c.Tree)
	}
}

func lynxCard(t *testing.T, w *world) Card {
	t.Helper()
	w.call(t, "read_wikipedia", `{"title":"Рысь"}`)
	w.call(t, "match_taxon", `{"scientific_name":"Lynx lynx"}`)
	c, err := CheckCard(w.tr, draft("Рысь", "Lynx lynx", "Обыкновенная рысь", "Кошка."), "рысь")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSectionRejectedWithoutReading(t *testing.T) {
	w := newWorld(t)
	c := lynxCard(t, w)
	diet, _ := TopicOf("diet")
	_, err := CheckSection(w.tr, &c, diet, SectionDraft{Found: true, Heading: "Питание", Text: "Ест зайцев."})
	r := refusal(t, err)
	if !strings.Contains(r.Do, "read_wikipedia") || !strings.Contains(r.Available, "вступление") {
		t.Fatalf("отказ: %v", r)
	}
}

func TestSectionAcceptedBySubstringBothWays(t *testing.T) {
	w := newWorld(t)
	c := lynxCard(t, w)
	w.call(t, "read_wikipedia", `{"title":"Обыкновенная рысь","section":"питание"}`)
	diet, _ := TopicOf("Питание")
	s, err := CheckSection(w.tr, &c, diet, SectionDraft{Found: true, Heading: "Питание", Text: "Охотится на зайцев."})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != SectionRead || s.Heading != "Образ жизни, поведение и питание" || s.Why == nil || s.Why.CallID == "" {
		t.Fatalf("раздел: %+v", s)
	}
	// Вступление принимается, если его открывали.
	s, err = CheckSection(w.tr, &c, diet, SectionDraft{Found: true, Heading: "вступление", Text: "Кошка."})
	if err != nil || s.Heading != "вступление" {
		t.Fatalf("вступление: %+v %v", s, err)
	}
}

func TestSectionNotFoundNeedsOpenedArticle(t *testing.T) {
	w := newWorld(t)
	c := Card{Article: "Манул", ArticleURL: "u"}
	status, _ := TopicOf("status")
	_, err := CheckSection(w.tr, &c, status, SectionDraft{Found: false, Heading: "", Text: "нет раздела"})
	refusal(t, err)

	w.call(t, "read_wikipedia", `{"title":"Манул"}`)
	s, err := CheckSection(w.tr, &c, status, SectionDraft{Found: false, Text: "раздела о размножении нет"})
	if err != nil || s.Status != SectionNone || s.Reason == "" {
		t.Fatalf("сведений нет: %+v %v", s, err)
	}
	if _, err := CheckSection(w.tr, &c, status, SectionDraft{Found: true, Heading: "x", Text: " "}); err == nil {
		t.Fatal("пустой текст принят")
	}
}

func TestReplayRebuildsFromHistory(t *testing.T) {
	envelope := tools.Envelope("match_taxon", `{"found":true,"usage_key":2435240,"canonical_name":"Lynx lynx","rank":"SPECIES"}`)
	history := []llm.Message{
		{Role: llm.RoleUser, Content: "рысь"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Function: llm.FunctionCall{Name: "match_taxon"}},
			{ID: "c2", Function: llm.FunctionCall{Name: "read_wikipedia"}}}},
		{Role: llm.RoleTool, ToolCallID: "c1", Content: envelope},
		{Role: llm.RoleTool, ToolCallID: "c2", Content: `{"title":"Обыкновенная рысь","url":"u","intro":"x","sections":["Питание"]}`},
		{Role: llm.RoleTool, ToolCallID: "orphan", Content: `{}`},
	}
	tr := NewTracker()
	tr.Replay(history)
	m, ok := tr.Match("Lynx lynx")
	if !ok || m.CallID != "c1" || m.Key != toolstest.KeyLynx {
		t.Fatalf("сверка из обёрнутого ответа: %+v %v", m, ok)
	}
	if !tr.IntroRead("Обыкновенная рысь") || tr.Calls() != 2 {
		t.Fatalf("статья из истории: %d", tr.Calls())
	}
}

func TestNeighbors(t *testing.T) {
	w := newWorld(t)
	if _, ok := NeighborsOf(w.tr, toolstest.KeyFelidae, "Felidae"); ok {
		t.Fatal("соседи без вызова")
	}
	w.call(t, "taxon_children", `{"usage_key":9703}`)
	n, ok := NeighborsOf(w.tr, toolstest.KeyFelidae, "Felidae")
	if !ok || len(n.Children) != 3 || n.Why == nil || n.Why.CallID == "" || n.Children[0].NameRu != "Рыси" {
		t.Fatalf("соседи: %+v", n)
	}
}

func TestComparisonReplacesUnreadWithNoData(t *testing.T) {
	w := newWorld(t)
	lynx := lynxCard(t, w)
	w.call(t, "read_wikipedia", `{"title":"Манул"}`)
	w.call(t, "match_taxon", `{"scientific_name":"Otocolobus manul"}`)
	manul, _ := CheckCard(w.tr, draft("Манул", "Otocolobus manul", "Манул", "Кошка."), "")
	w.call(t, "read_wikipedia", `{"title":"Обыкновенная рысь","section":"Внешний вид"}`)
	w.call(t, "read_wikipedia", `{"title":"Манул","section":"Описание"}`)

	var d ComparisonDraft
	json.Unmarshal([]byte(`{"rows":[
		{"aspect":"Размеры","a":"80–130 см","a_section":"Внешний вид","b":"52–65 см","b_section":"Описание"},
		{"aspect":"Питание","a":"зайцы","a_section":"Питание","b":"пищухи","b_section":"Питание"},
		{"aspect":"Статус охраны","a":"сведений нет","a_section":"","b":"Красная книга","b_section":"Охранный статус"},
		{"aspect":"","a":"x","a_section":"","b":"y","b_section":""}]}`), &d)
	cmp, err := CheckComparison(w.tr, lynx, manul, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.Rows) != 3 {
		t.Fatalf("строк %d", len(cmp.Rows))
	}
	if !cmp.Rows[0].A.Confirmed || !cmp.Rows[0].B.Confirmed || cmp.Rows[0].A.Why == nil {
		t.Fatalf("размеры подтверждены: %+v", cmp.Rows[0])
	}
	if cmp.Rows[1].A.Text != NoData || cmp.Rows[1].B.Text != NoData {
		t.Fatalf("непрочитанное питание должно стать «сведений нет»: %+v", cmp.Rows[1])
	}
	if cmp.Rows[2].A.Text != NoData || cmp.Rows[2].B.Confirmed {
		t.Fatalf("статус: %+v", cmp.Rows[2])
	}
	if len(cmp.Notes) != 3 {
		t.Fatalf("оговорки о снятых фактах: %v", cmp.Notes)
	}
	md := ComparisonMarkdown(cmp)
	if !strings.Contains(md, "| Размеры | 80–130 см | 52–65 см |") || !strings.Contains(md, "## Источники") {
		t.Fatalf("выгрузка сравнения:\n%s", md)
	}
}

func TestComparisonWithNothingConfirmedIsRefused(t *testing.T) {
	w := newWorld(t)
	var d ComparisonDraft
	json.Unmarshal([]byte(`{"rows":[{"aspect":"Размеры","a":"большая","a_section":"x","b":"маленький","b_section":"y"}]}`), &d)
	_, err := CheckComparison(w.tr, Card{Name: "a", Article: "A"}, Card{Name: "b", Article: "B"}, d)
	refusal(t, err)
	_, err = CheckComparison(w.tr, Card{}, Card{}, ComparisonDraft{})
	refusal(t, err)
}

func TestFoldBuildsStateFromDeltas(t *testing.T) {
	lynx := Card{ID: "1", Name: "Обыкновенная рысь", Latin: "Lynx lynx", Article: "Обыкновенная рысь", Query: "рысь",
		Sections: EmptySections(), LatinWhy: &Why{Tool: "match_taxon", CallID: "c1"}}
	manul := Card{ID: "2", Name: "Манул", Latin: "Otocolobus manul", Article: "Манул", Sections: EmptySections()}
	diet := Section{Key: "diet", Title: "Питание", Status: SectionRead, Text: "зайцы", Why: &Why{CallID: "c9"}}
	deltas := []Delta{
		{Kind: DeltaCard, Card: &lynx, Turn: "t1"},
		{Kind: DeltaTree, CardID: "1", Tree: []Node{{Name: "Lynx", Key: 5}}, TreeWhy: &Why{Tool: "taxon_tree"}, Turn: "t1"},
		{Kind: DeltaSection, CardID: "1", Section: &diet, Turn: "t2"},
		{Kind: DeltaCard, Card: &manul, Turn: "t3"},
		{Kind: DeltaNeighbors, CardID: "1", Neighbors: &Neighbors{NodeKey: 5, Children: []Neighbor{{Name: "x"}}}, Turn: "t4"},
		{Kind: DeltaNeighbors, CardID: "1", Neighbors: &Neighbors{NodeKey: 5, Children: []Neighbor{{Name: "y"}}}, Turn: "t5"},
		{Kind: DeltaNotFound, NotFound: &NotFound{Query: "шурундук", Gate: true}},
		{Kind: DeltaComparison, Comparison: &Comparison{A: RefOf(lynx), B: RefOf(manul)}},
		{Kind: DeltaSection, CardID: "missing", Section: &diet},
		{Kind: DeltaCard}, // пустая правка не роняет свёртку
	}
	s := Fold(deltas)
	if len(s.Cards) != 2 || s.Current != "2" || len(s.NotFound) != 1 || len(s.Comparisons) != 1 {
		t.Fatalf("состояние: %+v", s)
	}
	c := s.Card("1")
	if c.Section("diet").Status != SectionRead || c.Section("diet").Why.Turn != "t2" {
		t.Fatalf("раздел и ход «почему так»: %+v", c.Section("diet"))
	}
	if c.LatinWhy.Turn != "t1" || len(c.Tree) != 1 || c.TreeWhy.Turn != "t1" {
		t.Fatalf("ход у латыни и дерева: %+v %+v", c.LatinWhy, c.TreeWhy)
	}
	if len(c.Neighbors) != 1 || c.Neighbors[0].Children[0].Name != "y" {
		t.Fatalf("соседи узла заменяются, а не копятся: %+v", c.Neighbors)
	}
	// Повторная карточка того же таксона не забывает прочитанное (ФТ-13).
	again := lynx
	again.Sections = EmptySections()
	s = Fold(append(deltas, Delta{Kind: DeltaCard, Card: &again}))
	if !s.Card("1").Done("diet") || len(s.Card("1").Tree) != 1 || s.Current != "1" {
		t.Fatalf("повторное открытие стёрло прочитанное: %+v", s.Card("1"))
	}
	// Входные правки не изменились: свёртка работает с копиями.
	if lynx.Sections[1].Status != SectionUnread {
		t.Fatal("свёртка правит исходную карточку")
	}
}

func TestFindAndBrief(t *testing.T) {
	s := Fold([]Delta{
		{Kind: DeltaCard, Card: &Card{ID: "1", Name: "Обыкновенная рысь", Latin: "Lynx lynx", Article: "Обыкновенная рысь", Query: "рысь",
			Sections: []Section{{Key: "diet", Title: "Питание", Status: SectionRead}}}},
		{Kind: DeltaCard, Card: &Card{ID: "2", Name: "Ёж обыкновенный", Latin: "Erinaceus europaeus", Article: "Обыкновенный ёж"}},
	})
	for q, want := range map[string]string{"рысь": "1", "LYNX LYNX": "1", "еж обыкновенный": "2", "обыкновенный ёж": "2", "Ёж": "2"} {
		if c := s.Find(q); c == nil || c.ID != want {
			t.Errorf("Find(%q) = %+v", q, c)
		}
	}
	if s.Find("манул") != nil || s.Find(" ") != nil {
		t.Error("найдено то, чего нет")
	}
	brief := s.Brief()
	if !strings.Contains(brief, "Ёж обыкновенный (Erinaceus europaeus)") || !strings.Contains(brief, "← текущая") ||
		!strings.Contains(brief, "прочитаны разделы: Питание") {
		t.Fatalf("Brief:\n%s", brief)
	}
	if (&State{}).Brief() != "" || (&State{}).CurrentCard() != nil {
		t.Fatal("пустое состояние")
	}
	if s.CurrentCard().ID != "2" {
		t.Fatal("CurrentCard")
	}
}

func TestMarkdownHasSourcesAndSections(t *testing.T) {
	c := Card{Name: "Рысь", Latin: "Lynx lynx", Rank: "SPECIES", Summary: "Кошка.",
		Tree:     []Node{{Rank: "FAMILY", RankRu: "семейство", Name: "Felidae", NameRu: "Кошачьи"}, {Rank: "TRIBE", Name: "X"}},
		Sections: []Section{{Title: "Питание", Status: SectionRead, Text: "Зайцы.", Heading: "Питание"}, {Title: "Статус охраны", Status: SectionNone, Reason: "раздела нет"}, {Title: "Ареал", Status: SectionUnread}},
		Notes:    []string{"оговорка"},
		Sources:  []Source{{Title: "Википедия: Рысь", URL: "https://ru.wikipedia.org/wiki/Рысь"}}}
	md := Markdown(c)
	for _, want := range []string{"# Рысь", "*Lynx lynx* — species", "Кошачьи (Felidae)", "## Питание", "Сведений нет: раздела нет",
		"## Оговорки", "[Википедия: Рысь](https://ru.wikipedia.org/wiki/Рысь)", "tribe: X"} {
		if !strings.Contains(md, want) {
			t.Errorf("в выгрузке нет %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "## Ареал") {
		t.Error("непрочитанный раздел попал в выгрузку")
	}
}

func TestSmallHelpers(t *testing.T) {
	for s, want := range map[string]string{SectionRead: "прочитан", SectionNone: "сведений нет", SectionReading: "читается", "": "не прочитан"} {
		if SectionStatusTitle(s) != want {
			t.Errorf("%q → %q", s, SectionStatusTitle(s))
		}
	}
	if _, ok := TopicOf("нет такого"); ok {
		t.Error("TopicOf")
	}
	c := Card{Name: "Манул"}
	if c.Title() != "Манул" || c.Section("x") != nil {
		t.Error("Title/Section")
	}
	c.AddSource(Source{})
	c.AddSource(Source{URL: "a"})
	c.AddSource(Source{URL: "a"})
	if len(c.Sources) != 1 {
		t.Error("AddSource")
	}
	cl := (Card{}).Clone()
	if cl.Sections == nil || cl.Sources == nil {
		t.Error("Clone пустой карточки отдаёт nil-срезы")
	}
	for _, s := range []string{"Сведений нет.", "нет данных", "—"} {
		if !isNoData(s) {
			t.Errorf("isNoData(%q)", s)
		}
	}
	if (&Refusal{What: "a", Why: "b", Available: "c", Do: "d"}).Error() != "Не принято: a. Почему: b. Сейчас есть: c. Что сделать: d." {
		t.Error("Refusal.Error")
	}
}

// Латынь с автором и годом — та же сверка: отказ ради неё стоил бы лишнего
// запроса к модели, а в карточку всё равно идёт canonical_name из GBIF.
// Непроверенный вид с тем же родом не проходит.
func TestCardAcceptsLatinWithAuthorship(t *testing.T) {
	w := newWorld(t)
	w.call(t, "read_wikipedia", `{"title":"Обыкновенная рысь"}`)
	w.call(t, "match_taxon", `{"scientific_name":"Lynx lynx"}`)
	for _, latin := range []string{"Lynx lynx (Linnaeus, 1758)", "Lynx lynx Linnaeus, 1758"} {
		c, err := CheckCard(w.tr, draft("Обыкновенная рысь", latin, "Обыкновенная рысь", "Кошка."), "рысь")
		if err != nil || c.Latin != "Lynx lynx" {
			t.Fatalf("%q: %v, латынь %q", latin, err, c.Latin)
		}
	}
	for _, latin := range []string{"Lynx pardinus (Temminck, 1827)", "Lynx lynx dinniki"} {
		if _, err := CheckCard(w.tr, draft("Рысь", latin, "Обыкновенная рысь", "Кошка."), "рысь"); err == nil {
			t.Fatalf("%q принята без сверки", latin)
		}
	}
	for in, want := range map[string]string{"Grus Brisson, 1760": "Grus", "Otocolobus manul (Pallas, 1776)": "Otocolobus manul",
		"Canis lupus familiaris": "Canis lupus familiaris", "  ": ""} {
		if got := CanonicalLatin(in); got != want {
			t.Errorf("CanonicalLatin(%q) = %q, ждали %q", in, got, want)
		}
	}
}
