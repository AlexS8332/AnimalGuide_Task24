// Package card — карточка животного как структура, а не текст ответа
// (ФТ-10): русское название, латынь с рангом, дерево классификации, краткое
// описание, разделы с состоянием, оговорки и источники.
//
// Пакет не знает ни про HTTP, ни про модель: он знает про трекер реальных
// вызовов инструментов. Всё, что попадает в карточку, проходит проверку по
// трекеру (ИП-1), а источники ставит программа, а не модель (ФТ-11).
//
// Сама карточка нигде не хранится: ход записывает правку (Delta), а
// карточка выводится свёрткой правок пути ветки (ИП-6). Поэтому в каждой
// ветке диалога она своя и правильная.
package card

import (
	"strconv"
	"strings"
)

// Состояния раздела карточки (ФТ-10).
const (
	SectionUnread  = "unread"
	SectionReading = "reading"
	SectionRead    = "read"
	SectionNone    = "none"
)

// SectionStatusTitle — состояние раздела словами.
func SectionStatusTitle(s string) string {
	switch s {
	case SectionReading:
		return "читается"
	case SectionRead:
		return "прочитан"
	case SectionNone:
		return "сведений нет"
	default:
		return "не прочитан"
	}
}

// Why — «почему так» у факта карточки (ФТ-12): откуда факт пришёл —
// ссылка на источник и на событие журнала с вызовом инструмента.
type Why struct {
	Tool        string `json:"tool"`
	CallID      string `json:"callId,omitempty"`
	Turn        string `json:"turn,omitempty"`
	Source      string `json:"source,omitempty"`
	SourceTitle string `json:"sourceTitle,omitempty"`
}

// Node — узел дерева классификации. Латынь, ранг и ключ — из ответа GBIF,
// от модели — только русское название (ФТ-8).
type Node struct {
	Rank   string `json:"rank"`
	RankRu string `json:"rankRu,omitempty"`
	Name   string `json:"name"`
	NameRu string `json:"nameRu,omitempty"`
	Key    int    `json:"key"`
}

// Section — раздел карточки.
type Section struct {
	// Key — постоянное имя раздела: habitat, diet, lifestyle, breeding,
	// status. По нему интерфейс и сравнение находят «тот же» раздел у
	// разных животных.
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status string `json:"status"`
	// Heading — раздел статьи, из которого взят пересказ, как его вернул
	// read_wikipedia.
	Heading string `json:"heading,omitempty"`
	Text    string `json:"text,omitempty"`
	// Reason — почему сведений нет.
	Reason string `json:"reason,omitempty"`
	Why    *Why   `json:"why,omitempty"`
}

// Source — источник карточки.
type Source struct {
	Kind  string `json:"kind"` // wikipedia | gbif
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Card — карточка животного.
type Card struct {
	// ID — ключ карточки: ключ таксона GBIF строкой. Одно животное — одна
	// карточка, как бы его ни назвали.
	ID       string `json:"id"`
	Name     string `json:"name"`
	Latin    string `json:"latin"`
	Rank     string `json:"rank,omitempty"`
	RankRu   string `json:"rankRu,omitempty"`
	TaxonKey int    `json:"taxonKey"`
	// Query — как пользователь назвал животное.
	Query      string `json:"query,omitempty"`
	Article    string `json:"article"`
	ArticleURL string `json:"articleUrl"`
	Summary    string `json:"summary"`
	// Headings — оглавление статьи: из него специалист выбирает раздел.
	Headings []string  `json:"headings,omitempty"`
	Tree     []Node    `json:"tree,omitempty"`
	Sections []Section `json:"sections"`
	Notes    []string  `json:"notes,omitempty"`
	Sources  []Source  `json:"sources"`
	// Why по полям: откуда латынь, описание и дерево.
	LatinWhy   *Why `json:"latinWhy,omitempty"`
	SummaryWhy *Why `json:"summaryWhy,omitempty"`
	TreeWhy    *Why `json:"treeWhy,omitempty"`
	// Neighbors — соседние таксоны по узлам дерева, открытые кликом (С-3).
	Neighbors []Neighbors `json:"neighbors,omitempty"`
	// Unverified — карточка принята без проверки по трекеру (контрольная
	// дорожка) и что-то в ней не подтверждено источником. Стенд считает
	// такие карточки: их должно быть ноль на основной дорожке.
	Unverified bool `json:"unverified,omitempty"`

	// treeRu — русские названия узлов, названные моделью при сдаче, пока
	// дерева ещё нет: его достраивает программа тем же ходом.
	treeRu [][2]string
}

// Neighbors — вложенные таксоны одного узла дерева.
type Neighbors struct {
	NodeKey  int        `json:"nodeKey"`
	NodeName string     `json:"nodeName"`
	Children []Neighbor `json:"children"`
	Total    int        `json:"total"`
	Why      *Why       `json:"why,omitempty"`
}

// Neighbor — сосед по дереву.
type Neighbor struct {
	Key    int    `json:"key"`
	Name   string `json:"name"`
	NameRu string `json:"nameRu,omitempty"`
	Rank   string `json:"rank"`
	RankRu string `json:"rankRu,omitempty"`
}

// Topic — раздел карточки по умолчанию: тема и названия разделов статьи, в
// которых она обычно лежит.
type Topic struct {
	Key        string   `json:"key"`
	Title      string   `json:"title"`
	Candidates []string `json:"candidates"`
}

// Topics — разделы карточки, свёрнутые и подписанные «прочитать» (С-1).
var Topics = []Topic{
	{Key: "habitat", Title: "Ареал", Candidates: []string{"Распространение", "Ареал", "Среда обитания", "Места обитания", "Распространение и среда обитания"}},
	{Key: "diet", Title: "Питание", Candidates: []string{"Питание", "Пища", "Рацион", "Образ жизни и питание", "Образ жизни, поведение и питание"}},
	{Key: "lifestyle", Title: "Образ жизни", Candidates: []string{"Образ жизни", "Поведение", "Биология", "Образ жизни и поведение"}},
	{Key: "breeding", Title: "Размножение", Candidates: []string{"Размножение", "Размножение и развитие", "Воспроизводство", "Жизненный цикл"}},
	{Key: "status", Title: "Статус охраны", Candidates: []string{"Охранный статус", "Статус охраны", "Охрана", "Численность", "Статус популяции", "Статус и охрана"}},
}

// TopicOf — раздел по ключу или по названию.
func TopicOf(s string) (Topic, bool) {
	want := strings.ToLower(strings.TrimSpace(s))
	for _, t := range Topics {
		if t.Key == want || strings.ToLower(t.Title) == want {
			return t, true
		}
	}
	return Topic{}, false
}

// EmptySections — разделы новой карточки: все не прочитаны.
func EmptySections() []Section {
	out := make([]Section, len(Topics))
	for i, t := range Topics {
		out[i] = Section{Key: t.Key, Title: t.Title, Status: SectionUnread}
	}
	return out
}

// Section — раздел карточки по ключу.
func (c *Card) Section(key string) *Section {
	for i := range c.Sections {
		if c.Sections[i].Key == key {
			return &c.Sections[i]
		}
	}
	return nil
}

// AddSource добавляет источник без повторов.
func (c *Card) AddSource(s Source) {
	if s.URL == "" && s.Title == "" {
		return
	}
	for _, have := range c.Sources {
		if have.URL == s.URL {
			return
		}
	}
	c.Sources = append(c.Sources, s)
}

// Clone — глубокая копия: карточка отдаётся наружу, пока ход её дописывает.
func (c Card) Clone() Card {
	out := c
	out.Headings = append([]string(nil), c.Headings...)
	out.Tree = append([]Node(nil), c.Tree...)
	out.Sections = append([]Section(nil), c.Sections...)
	out.Notes = append([]string(nil), c.Notes...)
	out.Sources = append([]Source(nil), c.Sources...)
	out.Neighbors = make([]Neighbors, len(c.Neighbors))
	for i, n := range c.Neighbors {
		n.Children = append([]Neighbor(nil), n.Children...)
		out.Neighbors[i] = n
	}
	if out.Sections == nil {
		out.Sections = []Section{}
	}
	if out.Sources == nil {
		out.Sources = []Source{}
	}
	return out
}

// Done — раздел уже прочитан или о нём известно, что сведений нет:
// такой не перечитывают (ФТ-13).
func (c *Card) Done(key string) bool {
	s := c.Section(key)
	return s != nil && (s.Status == SectionRead || s.Status == SectionNone)
}

// reservedKeys — сведения о животных, которыми ведает карточка. Их источник
// — трекер вызовов, а не реплики, поэтому ни память, ни карточка фактов их
// не пишут (И-1 свода, ИП-4, ИП-14).
var reservedKeys = []string{"латынь", "латинское название", "ареал", "питание", "рацион", "размер", "размеры",
	"вес", "масса", "длина тела", "статус охраны", "охранный статус", "классификация", "семейство", "отряд",
	"род", "вид", "размножение", "образ жизни", "среда обитания"}

// Reserved — ведает ли ключом карточка животного.
func Reserved(key string) bool {
	k := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "ё", "е")
	for _, r := range reservedKeys {
		if k == r || strings.HasPrefix(k, r+" ") {
			return true
		}
	}
	return false
}

// IDOf — ключ карточки по ключу таксона.
func IDOf(taxonKey int) string { return strconv.Itoa(taxonKey) }

// Title — как назвать карточку человеку: русское название с латынью.
func (c *Card) Title() string {
	if c.Latin == "" {
		return c.Name
	}
	return c.Name + " (" + c.Latin + ")"
}
