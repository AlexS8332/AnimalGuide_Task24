package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

const (
	DefaultGBIFBase = "https://api.gbif.org/v1"

	// Сколько соседних таксонов отдавать по умолчанию и предельно: узел
	// «Кошачьи» — это десятки родов, а в интерфейсе нужен обозримый список.
	defaultChildren = 12
	maxChildren     = 40
	// Сколько запросов народных названий идёт одновременно.
	vernacularParallel = 6
)

// GBIF — инструменты таксономической базы GBIF: сверка латинского названия,
// дерево классификации, соседние таксоны и народные названия. Ключ не нужен.
type GBIF struct {
	Base string
	f    *Fetcher
}

func NewGBIF(base string, f *Fetcher) *GBIF {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = DefaultGBIFBase
	}
	return &GBIF{Base: base, f: f}
}

// ID — имя источника.
func (g *GBIF) ID() string { return "gbif" }

// Tools — все четыре инструмента GBIF.
func (g *GBIF) Tools() []Tool {
	return []Tool{g.matchTool(), g.treeTool(), g.childrenTool(), g.vernacularTool()}
}

// RankRu — русские названия рангов. Модель их знает, но здесь они нужны
// программе: дерево классификации собирается из ответа GBIF без модели.
var RankRu = map[string]string{
	"KINGDOM":    "царство",
	"PHYLUM":     "тип",
	"CLASS":      "класс",
	"ORDER":      "отряд",
	"FAMILY":     "семейство",
	"GENUS":      "род",
	"SPECIES":    "вид",
	"SUBSPECIES": "подвид",
	"SUBFAMILY":  "подсемейство",
	"SUBORDER":   "подотряд",
	"SUBCLASS":   "подкласс",
	"SUBPHYLUM":  "подтип",
	"SUBGENUS":   "подрод",
	"TRIBE":      "триба",
	"VARIETY":    "разновидность",
}

// Match — результат сверки названия.
type Match struct {
	Found      bool   `json:"found"`
	UsageKey   int    `json:"usage_key,omitempty"`
	Canonical  string `json:"canonical_name,omitempty"`
	Scientific string `json:"scientific_name,omitempty"`
	Rank       string `json:"rank,omitempty"`
	Status     string `json:"status,omitempty"`
	Confidence int    `json:"confidence"`
	MatchType  string `json:"match_type"`
	Kingdom    string `json:"kingdom,omitempty"`
	Phylum     string `json:"phylum,omitempty"`
	Class      string `json:"class,omitempty"`
	Order      string `json:"order,omitempty"`
	Family     string `json:"family,omitempty"`
	Genus      string `json:"genus,omitempty"`
	Species    string `json:"species,omitempty"`
	Note       string `json:"note,omitempty"`
}

func (g *GBIF) matchTool() Tool {
	return Func{
		S: Spec{
			Name: "match_taxon",
			Description: "Сверка латинского (научного) названия с таксономической базой GBIF. " +
				"Возвращает found, ранг, статус, уверенность и положение в системе. " +
				"Проверяет только существование таксона с таким латинским названием, " +
				"но не связь между русским названием и этим таксоном.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "scientific_name": {"type": "string", "description": "Латинское название, например Lynx lynx"}
  },
  "required": ["scientific_name"]
}`),
			Untrusted: true,
			Via:       ViaLocal,
		},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Name string `json:"scientific_name"`
			}
			if err := ParseArgs(args, &in); err != nil {
				return "", err
			}
			in.Name = strings.TrimSpace(in.Name)
			if in.Name == "" {
				return "", fmt.Errorf("scientific_name пуст")
			}
			m, err := g.Match(ctx, in.Name)
			if err != nil {
				return "", err
			}
			return Result(m)
		},
	}
}

// Match сверяет название. Нечёткое совпадение (FUZZY) и совпадение только
// по роду (HIGHERRANK) не считаются найденным таксоном: это как раз те
// случаи, где база «подсказывает похожее», а нам похожее не нужно.
func (g *GBIF) Match(ctx context.Context, name string) (Match, error) {
	q := url.Values{}
	q.Set("name", name)

	var resp struct {
		UsageKey       int    `json:"usageKey"`
		ScientificName string `json:"scientificName"`
		CanonicalName  string `json:"canonicalName"`
		Rank           string `json:"rank"`
		Status         string `json:"status"`
		Confidence     int    `json:"confidence"`
		MatchType      string `json:"matchType"`
		Kingdom        string `json:"kingdom"`
		Phylum         string `json:"phylum"`
		Class          string `json:"class"`
		Order          string `json:"order"`
		Family         string `json:"family"`
		Genus          string `json:"genus"`
		Species        string `json:"species"`
	}
	if err := g.f.GetJSON(ctx, g.Base+"/species/match?"+q.Encode(), &resp); err != nil {
		return Match{}, fmt.Errorf("сверка с GBIF: %w", err)
	}

	m := Match{
		Confidence: resp.Confidence,
		MatchType:  resp.MatchType,
	}
	switch resp.MatchType {
	case "EXACT":
		m.Found = true
	case "FUZZY":
		m.Note = "совпадение нечёткое: в базе есть похожее название, но не это"
	case "HIGHERRANK":
		m.Note = "совпал только более высокий ранг (например, род); такого вида в базе нет"
	default:
		m.Note = "таксон с таким названием в базе не найден"
	}
	if resp.MatchType != "NONE" {
		m.UsageKey = resp.UsageKey
		m.Canonical = resp.CanonicalName
		m.Scientific = resp.ScientificName
		m.Rank = resp.Rank
		m.Status = resp.Status
		m.Kingdom = resp.Kingdom
		m.Phylum = resp.Phylum
		m.Class = resp.Class
		m.Order = resp.Order
		m.Family = resp.Family
		m.Genus = resp.Genus
		m.Species = resp.Species
	}
	return m, nil
}

// TaxonNode — узел дерева классификации.
type TaxonNode struct {
	Rank   string `json:"rank"`
	RankRu string `json:"rank_ru"`
	Name   string `json:"name"`
	Key    int    `json:"key"`
}

func (g *GBIF) treeTool() Tool {
	return Func{
		S: Spec{
			Name: "taxon_tree",
			Description: "Дерево классификации таксона из GBIF от царства до самого таксона. " +
				"Нужен usage_key из результата match_taxon.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "usage_key": {"type": "integer", "description": "Ключ таксона в GBIF (usage_key из match_taxon)"}
  },
  "required": ["usage_key"]
}`),
			Untrusted: true,
			Via:       ViaLocal,
		},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Key int `json:"usage_key"`
			}
			if err := ParseArgs(args, &in); err != nil {
				return "", err
			}
			if in.Key <= 0 {
				return "", fmt.Errorf("usage_key должен быть положительным числом")
			}
			tree, err := g.Tree(ctx, in.Key)
			if err != nil {
				return "", err
			}
			return Result(map[string]any{"usage_key": in.Key, "tree": tree})
		},
	}
}

// Tree собирает цепочку родителей и сам таксон.
func (g *GBIF) Tree(ctx context.Context, key int) ([]TaxonNode, error) {
	type taxon struct {
		Key           int    `json:"key"`
		Rank          string `json:"rank"`
		CanonicalName string `json:"canonicalName"`
	}

	var parents []taxon
	if err := g.f.GetJSON(ctx, fmt.Sprintf("%s/species/%d/parents", g.Base, key), &parents); err != nil {
		return nil, fmt.Errorf("родители таксона: %w", err)
	}
	var self taxon
	if err := g.f.GetJSON(ctx, fmt.Sprintf("%s/species/%d", g.Base, key), &self); err != nil {
		return nil, fmt.Errorf("таксон: %w", err)
	}
	if self.Key == 0 {
		return nil, fmt.Errorf("таксона с ключом %d в GBIF нет", key)
	}

	tree := make([]TaxonNode, 0, len(parents)+1)
	for _, p := range append(parents, self) {
		tree = append(tree, TaxonNode{
			Rank:   p.Rank,
			RankRu: RankRu[p.Rank],
			Name:   p.CanonicalName,
			Key:    p.Key,
		})
	}
	return tree, nil
}

// Child — таксон, вложенный в узел дерева: сосед животного по классификации.
type Child struct {
	Key    int    `json:"key"`
	Name   string `json:"name"`
	Rank   string `json:"rank"`
	RankRu string `json:"rank_ru,omitempty"`
	NameRu string `json:"name_ru,omitempty"`
}

func (g *GBIF) childrenTool() Tool {
	return Func{
		S: Spec{
			Name: "taxon_children",
			Description: "Таксоны, непосредственно входящие в узел дерева классификации GBIF " +
				"(например, роды семейства Кошачьи), с русскими народными названиями, если они есть в GBIF. " +
				"Нужен ключ узла из taxon_tree.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "usage_key": {"type": "integer", "description": "Ключ узла дерева в GBIF (key из taxon_tree)"},
    "limit": {"type": "integer", "description": "Сколько таксонов вернуть, по умолчанию 12, не больше 40"}
  },
  "required": ["usage_key"]
}`),
			Untrusted: true,
			Via:       ViaLocal,
		},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Key   int `json:"usage_key"`
				Limit int `json:"limit"`
			}
			if err := ParseArgs(args, &in); err != nil {
				return "", err
			}
			if in.Key <= 0 {
				return "", fmt.Errorf("usage_key должен быть положительным числом")
			}
			children, total, err := g.Children(ctx, in.Key, in.Limit)
			if err != nil {
				return "", err
			}
			return Result(map[string]any{"usage_key": in.Key, "children": children, "total": total})
		},
	}
}

// Children — вложенные таксоны узла с русскими названиями. Принятые
// (ACCEPTED) идут, синонимы отбрасываются: в дереве они выглядели бы
// соседями самим себе. Русские названия собираются параллельно и не больше
// чем по одному запросу на таксон; их отсутствие ошибкой не считается.
func (g *GBIF) Children(ctx context.Context, key, limit int) ([]Child, int, error) {
	if limit <= 0 {
		limit = defaultChildren
	}
	if limit > maxChildren {
		limit = maxChildren
	}
	var resp struct {
		Count   int `json:"count"`
		Results []struct {
			Key            int    `json:"key"`
			CanonicalName  string `json:"canonicalName"`
			ScientificName string `json:"scientificName"`
			Rank           string `json:"rank"`
			Status         string `json:"taxonomicStatus"`
		} `json:"results"`
	}
	u := fmt.Sprintf("%s/species/%d/children?limit=%d", g.Base, key, limit*2)
	if err := g.f.GetJSON(ctx, u, &resp); err != nil {
		return nil, 0, fmt.Errorf("вложенные таксоны: %w", err)
	}
	children := make([]Child, 0, limit)
	for _, r := range resp.Results {
		if r.Status != "" && r.Status != "ACCEPTED" {
			continue
		}
		name := r.CanonicalName
		if name == "" {
			name = r.ScientificName
		}
		if name == "" || r.Key == 0 {
			continue
		}
		children = append(children, Child{Key: r.Key, Name: name, Rank: r.Rank, RankRu: RankRu[r.Rank]})
		if len(children) == limit {
			break
		}
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, vernacularParallel)
	for i := range children {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if names, err := g.Vernacular(ctx, children[i].Key, "rus"); err == nil && len(names) > 0 {
				children[i].NameRu = names[0]
			}
		}(i)
	}
	wg.Wait()
	total := resp.Count
	if total < len(children) {
		total = len(children)
	}
	return children, total, nil
}

func (g *GBIF) vernacularTool() Tool {
	return Func{
		S: Spec{
			Name: "vernacular_names",
			Description: "Народные (обиходные) названия таксона на заданном языке по данным GBIF. " +
				"По умолчанию русский (rus). Позволяет проверить, что русское название " +
				"действительно относится к этому таксону.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "usage_key": {"type": "integer", "description": "Ключ таксона в GBIF"},
    "language": {"type": "string", "description": "Код языка ISO 639-3, по умолчанию rus"}
  },
  "required": ["usage_key"]
}`),
			Untrusted: true,
			Via:       ViaLocal,
		},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Key      int    `json:"usage_key"`
				Language string `json:"language"`
			}
			if err := ParseArgs(args, &in); err != nil {
				return "", err
			}
			if in.Key <= 0 {
				return "", fmt.Errorf("usage_key должен быть положительным числом")
			}
			lang := strings.ToLower(strings.TrimSpace(in.Language))
			if lang == "" {
				lang = "rus"
			}
			names, err := g.Vernacular(ctx, in.Key, lang)
			if err != nil {
				return "", err
			}
			return Result(map[string]any{
				"usage_key": in.Key,
				"language":  lang,
				"names":     names,
			})
		},
	}
}

// Vernacular возвращает народные названия без повторов, в порядке появления.
func (g *GBIF) Vernacular(ctx context.Context, key int, lang string) ([]string, error) {
	var resp struct {
		Results []struct {
			Name     string `json:"vernacularName"`
			Language string `json:"language"`
		} `json:"results"`
	}
	u := fmt.Sprintf("%s/species/%d/vernacularNames?limit=200", g.Base, key)
	if err := g.f.GetJSON(ctx, u, &resp); err != nil {
		return nil, fmt.Errorf("народные названия: %w", err)
	}

	seen := make(map[string]bool)
	names := make([]string, 0)
	for _, r := range resp.Results {
		if !strings.EqualFold(r.Language, lang) {
			continue
		}
		name := strings.TrimSpace(r.Name)
		lower := strings.ToLower(name)
		if name == "" || seen[lower] {
			continue
		}
		seen[lower] = true
		names = append(names, name)
	}
	return names, nil
}

// TaxonURL — страница таксона на сайте GBIF: её ссылку в карточку ставит
// программа, а не модель (ФТ-11).
func TaxonURL(key int) string {
	return fmt.Sprintf("https://www.gbif.org/species/%d", key)
}
