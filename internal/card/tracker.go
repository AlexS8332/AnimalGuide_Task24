package card

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Tracker запоминает, что агент реально проверил через инструменты. По нему
// завершающий инструмент отклоняет карточку с латынью, которую никто не
// сверял, и раздел, который никто не читал: ответ по памяти модели не
// проходит, даже если выглядит правдоподобно.
//
// У каждого запомненного факта есть CallID — вызов, из ответа которого он
// взят: так карточка получает «почему так» (ФТ-12).
type Tracker struct {
	mu       sync.Mutex
	matched  map[string]match     // латынь в нижнем регистре → сверка
	trees    map[int]tree         // ключ таксона → дерево
	articles map[string]article   // заголовок статьи в нижнем регистре → статья
	aliases  map[string]string    // перенаправление в нижнем регистре → заголовок
	sections map[string][]section // заголовок статьи в нижнем регистре → прочитанные разделы
	children map[int]kids         // ключ узла → вложенные таксоны
	calls    int
}

type match struct {
	Key       int
	Canonical string
	Rank      string
	CallID    string
	Taxonomy  map[string]string // ранг → латынь из ответа match_taxon
}

type tree struct {
	Nodes  []Node
	CallID string
}

type article struct {
	Title    string
	URL      string
	Intro    string
	Sections []string
	CallID   string
}

type section struct {
	Heading string
	Text    string
	CallID  string
}

type kids struct {
	Children []Neighbor
	Total    int
	CallID   string
}

// NewTracker — пустой трекер.
func NewTracker() *Tracker {
	return &Tracker{
		matched:  map[string]match{},
		trees:    map[int]tree{},
		articles: map[string]article{},
		aliases:  map[string]string{},
		sections: map[string][]section{},
		children: map[int]kids{},
	}
}

// Observe оборачивает инструмент: результат уходит модели как есть, а копия
// разбирается и запоминается вместе с идентификатором вызова.
func (t *Tracker) Observe(tool tools.Tool) tools.Tool {
	name := tool.Spec().Name
	return tools.Wrap(tool, func(ctx context.Context, args json.RawMessage, next tools.CallFunc) (string, error) {
		out, err := next(ctx, args)
		if err == nil {
			t.Record(name, tools.CallID(ctx), out)
		}
		return out, err
	})
}

// ObserveAll — Observe для списка.
func (t *Tracker) ObserveAll(ts []tools.Tool) []tools.Tool {
	out := make([]tools.Tool, len(ts))
	for i, tool := range ts {
		out[i] = t.Observe(tool)
	}
	return out
}

// Calls — сколько ответов инструментов разобрано.
func (t *Tracker) Calls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// Record разбирает ответ инструмента. Пометка «данные источника» снимается:
// трекер работает с данными, а не с обёрткой.
func (t *Tracker) Record(name, callID, out string) {
	out, _ = tools.Unwrap(out)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++

	switch name {
	case "match_taxon":
		var m struct {
			Found      bool   `json:"found"`
			UsageKey   int    `json:"usage_key"`
			Canonical  string `json:"canonical_name"`
			Scientific string `json:"scientific_name"`
			Rank       string `json:"rank"`
			Kingdom    string `json:"kingdom"`
			Phylum     string `json:"phylum"`
			Class      string `json:"class"`
			Order      string `json:"order"`
			Family     string `json:"family"`
			Genus      string `json:"genus"`
		}
		if json.Unmarshal([]byte(out), &m) == nil && m.Found && m.Canonical != "" {
			got := match{Key: m.UsageKey, Canonical: m.Canonical, Rank: m.Rank, CallID: callID,
				Taxonomy: map[string]string{"KINGDOM": m.Kingdom, "PHYLUM": m.Phylum, "CLASS": m.Class,
					"ORDER": m.Order, "FAMILY": m.Family, "GENUS": m.Genus}}
			t.matched[strings.ToLower(m.Canonical)] = got
			// Модель то и дело сдаёт латынь с автором и годом — так, как её
			// вернул тот же ответ в scientific_name. Это та же сверка, и
			// отказ ради неё стоил бы лишнего запроса к модели.
			if sci := strings.ToLower(strings.TrimSpace(m.Scientific)); sci != "" {
				t.matched[sci] = got
			}
		}
	case "taxon_tree":
		var r struct {
			UsageKey int               `json:"usage_key"`
			Tree     []tools.TaxonNode `json:"tree"`
		}
		if json.Unmarshal([]byte(out), &r) == nil && len(r.Tree) > 0 {
			nodes := make([]Node, 0, len(r.Tree))
			for _, n := range r.Tree {
				nodes = append(nodes, Node{Rank: n.Rank, RankRu: n.RankRu, Name: n.Name, Key: n.Key})
			}
			t.trees[r.UsageKey] = tree{Nodes: nodes, CallID: callID}
		}
	case "taxon_children":
		var r struct {
			UsageKey int           `json:"usage_key"`
			Children []tools.Child `json:"children"`
			Total    int           `json:"total"`
		}
		if json.Unmarshal([]byte(out), &r) == nil {
			list := make([]Neighbor, 0, len(r.Children))
			for _, c := range r.Children {
				list = append(list, Neighbor{Key: c.Key, Name: c.Name, NameRu: c.NameRu, Rank: c.Rank, RankRu: c.RankRu})
			}
			t.children[r.UsageKey] = kids{Children: list, Total: r.Total, CallID: callID}
		}
	case "read_wikipedia":
		var r struct {
			Title    string   `json:"title"`
			URL      string   `json:"url"`
			From     string   `json:"redirected_from"`
			Intro    string   `json:"intro"`
			Section  string   `json:"section"`
			Found    *bool    `json:"found"`
			Text     string   `json:"text"`
			Sections []string `json:"sections"`
		}
		if json.Unmarshal([]byte(out), &r) != nil || r.Title == "" {
			return
		}
		key := strings.ToLower(r.Title)
		a := t.articles[key]
		a.Title, a.URL = r.Title, r.URL
		if r.From != "" {
			t.aliases[strings.ToLower(r.From)] = key
		}
		if len(r.Sections) > 0 {
			a.Sections = r.Sections
		}
		if r.Section == "" {
			// Статья открыта без раздела: прочитано вступление.
			a.Intro, a.CallID = r.Intro, callID
		}
		t.articles[key] = a
		if r.Section != "" && (r.Found == nil || *r.Found) {
			t.sections[key] = append(t.sections[key], section{Heading: r.Section, Text: r.Text, CallID: callID})
		}
	}
}

// Replay засевает трекер тем, что уже было прочитано в этой ветке диалога:
// ответы инструментов прошлых ходов берутся из истории полностью, до
// сокращения. Вызов и ответ связываются по tool_call_id.
func (t *Tracker) Replay(history []llm.Message) {
	names := map[string]string{}
	for _, m := range history {
		for _, c := range m.ToolCalls {
			names[c.ID] = c.Function.Name
		}
		if m.Role == llm.RoleTool {
			if name, ok := names[m.ToolCallID]; ok {
				t.Record(name, m.ToolCallID, m.Content)
			}
		}
	}
}

// Match — сверка латыни, если она была и нашла таксон.
func (t *Tracker) Match(latin string) (match, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.matched[strings.ToLower(strings.TrimSpace(latin))]
	if !ok {
		m, ok = t.matched[strings.ToLower(CanonicalLatin(latin))]
	}
	return m, ok
}

// CanonicalLatin — латынь без автора и года: «Lynx lynx (Linnaeus, 1758)» →
// «Lynx lynx», «Grus Brisson, 1760» → «Grus». Род с заглавной и следом
// эпитеты строчными; всё, что после первого слова с заглавной, цифры или
// скобки, — авторство. Сверка остаётся сверкой: ключ берётся из ответа
// match_taxon, а не из этой строки.
func CanonicalLatin(latin string) string {
	f := strings.Fields(latin)
	if len(f) == 0 {
		return ""
	}
	out := []string{strings.Trim(f[0], ",")}
	for _, w := range f[1:] {
		if w == "" || strings.ContainsAny(w, "(),0123456789&") || strings.ToLower(w) != w {
			break
		}
		out = append(out, w)
	}
	return strings.Join(out, " ")
}

// Tree — дерево таксона, если его запрашивали.
func (t *Tracker) Tree(key int) ([]Node, string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr, ok := t.trees[key]
	return append([]Node(nil), tr.Nodes...), tr.CallID, ok
}

// Children — соседи узла, если их запрашивали.
func (t *Tracker) Children(key int) ([]Neighbor, int, string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k, ok := t.children[key]
	return append([]Neighbor(nil), k.Children...), k.Total, k.CallID, ok
}

func (t *Tracker) articleLocked(title string) (article, string, bool) {
	key := strings.ToLower(strings.TrimSpace(title))
	if to, ok := t.aliases[key]; ok {
		key = to
	}
	a, ok := t.articles[key]
	return a, key, ok
}

// Article — статья, если её открывали: заголовок, адрес, оглавление и
// вызов, которым прочитано вступление.
func (t *Tracker) Article(title string) (article, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a, _, ok := t.articleLocked(title)
	return a, ok
}

// IntroRead — прочитано ли вступление статьи (read_wikipedia без раздела).
func (t *Tracker) IntroRead(title string) bool {
	a, ok := t.Article(title)
	return ok && a.CallID != ""
}

// SectionRead — был ли прочитан раздел статьи. Названия сравниваются по
// вхождению в обе стороны: инструмент ищет раздел по подстроке, и модель
// может назвать «Распространение», прочитав «Распространение и места
// обитания». «Вступление» и пустое название означают вступление статьи.
func (t *Tracker) SectionRead(title, heading string) (section, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a, key, ok := t.articleLocked(title)
	if !ok {
		return section{}, false
	}
	want := strings.ToLower(strings.TrimSpace(heading))
	if want == "" || strings.Contains(want, "вступлени") || want == "intro" || want == key {
		if a.CallID == "" {
			return section{}, false
		}
		return section{Heading: "вступление", Text: a.Intro, CallID: a.CallID}, true
	}
	list := t.sections[key]
	for i := len(list) - 1; i >= 0; i-- {
		have := strings.ToLower(list[i].Heading)
		if have == want {
			return list[i], true
		}
	}
	for i := len(list) - 1; i >= 0; i-- {
		have := strings.ToLower(list[i].Heading)
		if strings.Contains(have, want) || strings.Contains(want, have) {
			return list[i], true
		}
	}
	return section{}, false
}

// ReadHeadings — прочитанные разделы статьи.
func (t *Tracker) ReadHeadings(title string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, key, ok := t.articleLocked(title)
	if !ok {
		return nil
	}
	var out []string
	for _, s := range t.sections[key] {
		out = append(out, s.Heading)
	}
	return out
}
