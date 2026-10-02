package card

import (
	"fmt"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Refusal — отказ завершающего инструмента. Отказ обязан учить (ИП-8):
// что нельзя, почему, что доступно сейчас и что сделать, чтобы стало можно.
// Уходит модели результатом вызова, в журнал — целиком.
type Refusal struct {
	What      string `json:"what"`
	Why       string `json:"why"`
	Available string `json:"available"`
	Do        string `json:"do"`
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("Не принято: %s. Почему: %s. Сейчас есть: %s. Что сделать: %s.", r.What, r.Why, r.Available, r.Do)
}

// CardDraft — аргументы submit_card: что модель считает карточкой.
type CardDraft struct {
	Name    string `json:"name_ru"`
	Latin   string `json:"latin"`
	Article string `json:"wiki_title"`
	Summary string `json:"summary"`
	// TreeRu — русские названия узлов дерева: единственное, что в дереве
	// берётся от модели (ФТ-8).
	TreeRu []struct {
		Name   string `json:"name"`
		NameRu string `json:"name_ru"`
	} `json:"tree_ru"`
}

// CardSchema — схема аргументов submit_card.
const CardSchema = `{
  "type": "object",
  "properties": {
    "name_ru": {"type": "string", "description": "Общепринятое русское название"},
    "latin": {"type": "string", "description": "Латинское название — ровно то, что подтвердил match_taxon (found = true)"},
    "wiki_title": {"type": "string", "description": "Заголовок прочитанной статьи Википедии, как его вернул read_wikipedia"},
    "summary": {"type": "string", "description": "Краткое описание, 2–4 предложения по вступлению статьи"},
    "tree_ru": {
      "type": "array",
      "description": "Русские названия таксонов из ответа match_taxon (царство, тип, класс, отряд, семейство, род), если уверен",
      "items": {"type": "object", "properties": {"name": {"type": "string", "description": "Латинское название узла"}, "name_ru": {"type": "string"}}, "required": ["name", "name_ru"]}
    }
  },
  "required": ["name_ru", "latin", "wiki_title", "summary"]
}`

// CheckCard проверяет карточку по трекеру и собирает её. Латынь принимается,
// только если match_taxon вернул found = true; статья — только прочитанная
// в этом прогоне. Ранг, ключ, ссылки и источники ставит программа.
func CheckCard(tr *Tracker, d CardDraft, query string) (Card, error) {
	d.Name = strings.TrimSpace(d.Name)
	d.Latin = strings.TrimSpace(d.Latin)
	d.Article = strings.TrimSpace(d.Article)
	d.Summary = strings.TrimSpace(d.Summary)

	var problems, todo []string
	m, matched := tr.Match(d.Latin)
	if !matched {
		problems = append(problems, fmt.Sprintf("латинское название «%s» не подтверждено GBIF", d.Latin))
		todo = append(todo, "вызови match_taxon с латынью из статьи; если found = false — сообщи report_not_found")
	}
	a, opened := tr.Article(d.Article)
	if !opened || a.CallID == "" {
		problems = append(problems, fmt.Sprintf("статья «%s» не открыта через read_wikipedia", d.Article))
		todo = append(todo, "открой статью read_wikipedia без section и возьми описание из вступления")
	}
	if d.Name == "" || d.Summary == "" {
		problems = append(problems, "name_ru и summary обязательны")
		todo = append(todo, "заполни name_ru и summary")
	}
	if len(problems) > 0 {
		return Card{}, &Refusal{
			What:      "карточка — " + strings.Join(problems, "; "),
			Why:       "латынь в карточке — только подтверждённая GBIF, описание — только из прочитанной статьи: связку «русское название → таксон» модель выдумывает правдоподобно",
			Available: tr.available(),
			Do:        strings.Join(todo, "; "),
		}
	}

	c := Card{
		ID: IDOf(m.Key), Name: d.Name, Latin: m.Canonical, Rank: m.Rank, RankRu: tools.RankRu[m.Rank],
		TaxonKey: m.Key, Query: strings.TrimSpace(query),
		Article: a.Title, ArticleURL: a.URL, Summary: d.Summary,
		Headings: cleanHeadings(a.Sections), Sections: EmptySections(),
		LatinWhy:   &Why{Tool: "match_taxon", CallID: m.CallID, Source: tools.TaxonURL(m.Key), SourceTitle: "GBIF: " + m.Canonical},
		SummaryWhy: &Why{Tool: "read_wikipedia", CallID: a.CallID, Source: a.URL, SourceTitle: "Википедия: " + a.Title},
	}
	c.AddSource(Source{Kind: "wikipedia", Title: "Википедия: " + a.Title, URL: a.URL})
	c.AddSource(Source{Kind: "gbif", Title: "GBIF: " + m.Canonical, URL: tools.TaxonURL(m.Key)})
	ru := map[string]string{}
	for _, n := range d.TreeRu {
		ru[strings.ToLower(strings.TrimSpace(n.Name))] = strings.TrimSpace(n.NameRu)
	}
	if nodes, callID, ok := tr.Tree(m.Key); ok {
		c.Tree, c.TreeWhy = nodes, &Why{Tool: "taxon_tree", CallID: callID, Source: tools.TaxonURL(m.Key), SourceTitle: "GBIF"}
	}
	c.ApplyTreeRu(ru)
	c.remember(ru)
	return c, nil
}

// pendingRu — русские названия узлов, пришедшие раньше самого дерева: дерево
// может достроить программа после приёма карточки.
func (c *Card) remember(ru map[string]string) {
	if len(ru) == 0 {
		return
	}
	for name, v := range ru {
		if v != "" {
			c.treeRu = append(c.treeRu, [2]string{name, v})
		}
	}
}

// SetTree ставит дерево из ответа taxon_tree и накладывает русские названия,
// названные моделью при сдаче карточки.
func (c *Card) SetTree(nodes []Node, why *Why) {
	c.Tree, c.TreeWhy = nodes, why
	ru := map[string]string{}
	for _, p := range c.treeRu {
		ru[p[0]] = p[1]
	}
	c.ApplyTreeRu(ru)
}

// ApplyTreeRu накладывает русские названия на узлы дерева по латыни. Узел
// самого животного получает русское название карточки.
func (c *Card) ApplyTreeRu(ru map[string]string) {
	for i := range c.Tree {
		n := &c.Tree[i]
		if v := ru[strings.ToLower(n.Name)]; v != "" && n.NameRu == "" {
			n.NameRu = v
		}
		if n.Key == c.TaxonKey && n.NameRu == "" {
			n.NameRu = c.Name
		}
	}
}

// cleanHeadings — оглавление без отступов подразделов.
func cleanHeadings(list []string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// available — что трекер уже видел, одной строкой: часть отказа «что есть
// сейчас».
func (t *Tracker) available() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var parts []string
	var latins, arts []string
	for _, m := range t.matched {
		latins = append(latins, m.Canonical)
	}
	for _, a := range t.articles {
		if a.CallID != "" {
			arts = append(arts, "«"+a.Title+"»")
		}
	}
	if len(latins) > 0 {
		parts = append(parts, "подтверждено GBIF: "+strings.Join(latins, ", "))
	}
	if len(arts) > 0 {
		parts = append(parts, "открыты статьи: "+strings.Join(arts, ", "))
	}
	if len(parts) == 0 {
		return "в этом прогоне ещё ничего не проверено"
	}
	return strings.Join(parts, "; ")
}

// SectionDraft — аргументы submit_section.
type SectionDraft struct {
	Found   bool   `json:"found"`
	Heading string `json:"section"`
	Text    string `json:"text"`
}

// SectionSchema — схема аргументов submit_section.
const SectionSchema = `{
  "type": "object",
  "properties": {
    "found": {"type": "boolean", "description": "Нашлись ли в статье сведения по теме"},
    "section": {"type": "string", "description": "Раздел статьи, из которого взят пересказ, как его вернул read_wikipedia; «вступление», если сведения во вступлении"},
    "text": {"type": "string", "description": "Пересказ по прочитанному тексту; при found = false — почему сведений нет"}
  },
  "required": ["found", "section", "text"]
}`

// CheckSection проверяет пересказ раздела: принимается, только если раздел
// читали через read_wikipedia в этом прогоне (ФТ-8). «Сведений нет»
// принимается, если статью хотя бы открывали — иначе это не отсутствие
// сведений, а отсутствие работы.
func CheckSection(tr *Tracker, c *Card, topic Topic, d SectionDraft) (Section, error) {
	d.Text = strings.TrimSpace(d.Text)
	d.Heading = strings.TrimSpace(d.Heading)
	out := Section{Key: topic.Key, Title: topic.Title}
	if d.Text == "" {
		return out, &Refusal{What: "пустой раздел", Why: "text обязателен и при found = false",
			Available: tr.readLine(c.Article), Do: "перескажи прочитанное или объясни, почему сведений нет"}
	}
	if !d.Found {
		a, ok := tr.Article(c.Article)
		if !ok {
			return out, &Refusal{What: "«сведений нет» без чтения статьи",
				Why:       "отсутствие сведений тоже проверяется: статью «" + c.Article + "» в этом прогоне не открывали",
				Available: tr.readLine(c.Article),
				Do:        "открой статью read_wikipedia и посмотри подходящие разделы"}
		}
		out.Status, out.Reason = SectionNone, d.Text
		out.Why = &Why{Tool: "read_wikipedia", CallID: a.CallID, Source: c.ArticleURL, SourceTitle: "Википедия: " + c.Article}
		return out, nil
	}
	s, ok := tr.SectionRead(c.Article, d.Heading)
	if !ok {
		return out, &Refusal{What: fmt.Sprintf("пересказ раздела «%s»", d.Heading),
			Why:       "пересказ принимается только по разделу, прочитанному через read_wikipedia в этом прогоне",
			Available: tr.readLine(c.Article),
			Do:        fmt.Sprintf("вызови read_wikipedia с title «%s» и section «%s» либо сдай found = false", c.Article, d.Heading)}
	}
	out.Status, out.Heading, out.Text = SectionRead, s.Heading, d.Text
	out.Why = &Why{Tool: "read_wikipedia", CallID: s.CallID, Source: c.ArticleURL, SourceTitle: "Википедия: " + c.Article + " — " + s.Heading}
	return out, nil
}

func (t *Tracker) readLine(title string) string {
	read := t.ReadHeadings(title)
	intro := t.IntroRead(title)
	var parts []string
	if intro {
		parts = append(parts, "вступление")
	}
	parts = append(parts, read...)
	if len(parts) == 0 {
		return "из статьи «" + title + "» ничего не прочитано"
	}
	return "прочитано из «" + title + "»: " + strings.Join(parts, ", ")
}

// Aspects — строки сравнения по умолчанию (С-4).
var Aspects = []string{"Размеры", "Ареал", "Питание", "Статус охраны"}

// NoData — текст пустой ячейки сравнения: не догадка, а честное отсутствие.
const NoData = "сведений нет"

// Ref — животное в сравнении.
type Ref struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Latin      string `json:"latin"`
	Article    string `json:"article"`
	ArticleURL string `json:"articleUrl"`
}

// RefOf — ссылка на карточку.
func RefOf(c Card) Ref {
	return Ref{ID: c.ID, Name: c.Name, Latin: c.Latin, Article: c.Article, ArticleURL: c.ArticleURL}
}

// Cell — ячейка сравнения.
type Cell struct {
	Text      string `json:"text"`
	Confirmed bool   `json:"confirmed"`
	Heading   string `json:"heading,omitempty"`
	Why       *Why   `json:"why,omitempty"`
}

// Row — строка сравнения.
type Row struct {
	Aspect string `json:"aspect"`
	A      Cell   `json:"a"`
	B      Cell   `json:"b"`
}

// Comparison — таблица сравнения двух животных по общим разделам.
type Comparison struct {
	A     Ref      `json:"a"`
	B     Ref      `json:"b"`
	Rows  []Row    `json:"rows"`
	Notes []string `json:"notes,omitempty"`
}

// ComparisonDraft — аргументы submit_comparison.
type ComparisonDraft struct {
	Rows []struct {
		Aspect   string `json:"aspect"`
		A        string `json:"a"`
		ASection string `json:"a_section"`
		B        string `json:"b"`
		BSection string `json:"b_section"`
	} `json:"rows"`
}

// ComparisonSchema — схема аргументов submit_comparison.
const ComparisonSchema = `{
  "type": "object",
  "properties": {
    "rows": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "aspect": {"type": "string", "description": "Что сравнивается: Размеры, Ареал, Питание, Статус охраны"},
          "a": {"type": "string", "description": "Факт о первом животном по прочитанному разделу; «сведений нет», если его нет"},
          "a_section": {"type": "string", "description": "Раздел статьи первого животного, откуда факт (как вернул read_wikipedia; «вступление» — для вступления)"},
          "b": {"type": "string", "description": "Факт о втором животном; «сведений нет», если его нет"},
          "b_section": {"type": "string", "description": "Раздел статьи второго животного, откуда факт"}
        },
        "required": ["aspect", "a", "a_section", "b", "b_section"]
      }
    }
  },
  "required": ["rows"]
}`

// CheckComparison проверяет таблицу: факт в ячейке остаётся, только если
// его раздел читали в этом прогоне; иначе ячейка — «сведений нет», а не
// догадка (ФТ-8, С-4). Таблица, в которой не подтверждено ничего, не
// принимается вовсе: это не отсутствие сведений, а отсутствие чтения.
func CheckComparison(tr *Tracker, a, b Card, d ComparisonDraft) (Comparison, error) {
	out := Comparison{A: RefOf(a), B: RefOf(b)}
	confirmed := 0
	cell := func(c Card, text, heading string) Cell {
		text = strings.TrimSpace(text)
		if text == "" || isNoData(text) {
			return Cell{Text: NoData}
		}
		s, ok := tr.SectionRead(c.Article, heading)
		if !ok {
			out.Notes = append(out.Notes, fmt.Sprintf("%s: «%s» — раздел «%s» не читался, факт снят", c.Name, tools.Truncate(text, 60), heading))
			return Cell{Text: NoData}
		}
		confirmed++
		return Cell{Text: text, Confirmed: true, Heading: s.Heading,
			Why: &Why{Tool: "read_wikipedia", CallID: s.CallID, Source: c.ArticleURL, SourceTitle: "Википедия: " + c.Article + " — " + s.Heading}}
	}
	for _, r := range d.Rows {
		aspect := strings.TrimSpace(r.Aspect)
		if aspect == "" {
			continue
		}
		out.Rows = append(out.Rows, Row{Aspect: aspect, A: cell(a, r.A, r.ASection), B: cell(b, r.B, r.BSection)})
	}
	if len(out.Rows) == 0 || confirmed == 0 {
		return Comparison{}, &Refusal{
			What:      "сравнение без единого подтверждённого факта",
			Why:       "строка сравнения держится на прочитанных разделах обоих животных",
			Available: tr.readLine(a.Article) + "; " + tr.readLine(b.Article),
			Do:        "прочитай нужные разделы обеих статей read_wikipedia и сдай таблицу снова",
		}
	}
	return out, nil
}

func isNoData(s string) bool {
	s = strings.ToLower(strings.Trim(strings.TrimSpace(s), ".!"))
	return s == NoData || s == "нет сведений" || s == "нет данных" || s == "—" || s == "-"
}

// NotFound — «такого животного нет» (И-3 свода: похожее не подставлять).
type NotFound struct {
	Query  string `json:"query"`
	Reason string `json:"reason"`
	// Gate — отказал привратник, до дорогих шагов.
	Gate bool `json:"gate,omitempty"`
}

// NotFoundSchema — схема report_not_found.
const NotFoundSchema = `{
  "type": "object",
  "properties": {
    "reason": {"type": "string", "description": "Коротко: почему сведений нет — не животное, название выдумано или искажено, статьи именно о нём нет"}
  },
  "required": ["reason"]
}`

// NeighborsOf — соседи узла из трекера.
func NeighborsOf(tr *Tracker, nodeKey int, nodeName string) (Neighbors, bool) {
	list, total, callID, ok := tr.Children(nodeKey)
	if !ok {
		return Neighbors{}, false
	}
	return Neighbors{NodeKey: nodeKey, NodeName: nodeName, Children: list, Total: total,
		Why: &Why{Tool: "taxon_children", CallID: callID, Source: tools.TaxonURL(nodeKey), SourceTitle: "GBIF: " + nodeName}}, true
}
