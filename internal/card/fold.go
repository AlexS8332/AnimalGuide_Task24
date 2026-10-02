package card

import "strings"

// Виды правок карточки.
const (
	DeltaCard       = "card"
	DeltaNotFound   = "notfound"
	DeltaSection    = "section"
	DeltaNeighbors  = "neighbors"
	DeltaComparison = "comparison"
	DeltaTree       = "tree"
)

// Delta — что ход сделал с карточками. Ход хранит правки, а не карточки:
// карточка выводится свёрткой правок пути ветки (ИП-6), и в каждой ветке
// она своя.
type Delta struct {
	Kind       string      `json:"kind"`
	CardID     string      `json:"cardId,omitempty"`
	Card       *Card       `json:"card,omitempty"`
	Section    *Section    `json:"section,omitempty"`
	Neighbors  *Neighbors  `json:"neighbors,omitempty"`
	Comparison *Comparison `json:"comparison,omitempty"`
	NotFound   *NotFound   `json:"notFound,omitempty"`
	Tree       []Node      `json:"tree,omitempty"`
	TreeWhy    *Why        `json:"treeWhy,omitempty"`
	// Turn — ход, в котором правка сделана; ставит тот, кто записывает ход.
	Turn string `json:"turn,omitempty"`
}

// State — карточки пути ветки: всё, что открывалось, в порядке появления.
type State struct {
	Cards []Card `json:"cards"`
	// Current — карточка, с которой работали последней: к ней относятся
	// уточнения «а чем она питается?» (С-2).
	Current     string       `json:"current,omitempty"`
	Comparisons []Comparison `json:"comparisons"`
	NotFound    []NotFound   `json:"notFound"`
}

// Fold сворачивает правки в состояние. Повторная карточка того же таксона
// не затирает прочитанные разделы: открыть животное снова — не значит
// забыть то, что о нём уже прочитано (ФТ-13).
func Fold(deltas []Delta) State {
	s := State{Cards: []Card{}, Comparisons: []Comparison{}, NotFound: []NotFound{}}
	for _, d := range deltas {
		switch d.Kind {
		case DeltaCard:
			if d.Card == nil {
				continue
			}
			c := d.Card.Clone()
			stamp(&c, d.Turn)
			if have := s.Card(c.ID); have != nil {
				for _, sec := range have.Sections {
					if sec.Status == SectionRead || sec.Status == SectionNone {
						c.setSection(sec)
					}
				}
				if len(c.Tree) == 0 {
					c.Tree, c.TreeWhy = have.Tree, have.TreeWhy
				}
				c.Neighbors = append(have.Neighbors, c.Neighbors...)
				*have = c
			} else {
				s.Cards = append(s.Cards, c)
			}
			s.Current = c.ID
		case DeltaTree:
			if c := s.Card(d.CardID); c != nil {
				c.Tree = append([]Node(nil), d.Tree...)
				c.TreeWhy = withTurn(d.TreeWhy, d.Turn)
			}
		case DeltaSection:
			if c := s.Card(d.CardID); c != nil && d.Section != nil {
				sec := *d.Section
				sec.Why = withTurn(sec.Why, d.Turn)
				c.setSection(sec)
				s.Current = c.ID
			}
		case DeltaNeighbors:
			if c := s.Card(d.CardID); c != nil && d.Neighbors != nil {
				n := *d.Neighbors
				n.Why = withTurn(n.Why, d.Turn)
				c.setNeighbors(n)
			}
		case DeltaComparison:
			if d.Comparison != nil {
				s.Comparisons = append(s.Comparisons, *d.Comparison)
			}
		case DeltaNotFound:
			if d.NotFound != nil {
				s.NotFound = append(s.NotFound, *d.NotFound)
			}
		}
	}
	return s
}

func withTurn(w *Why, turn string) *Why {
	if w == nil {
		return nil
	}
	out := *w
	if out.Turn == "" {
		out.Turn = turn
	}
	return &out
}

func stamp(c *Card, turn string) {
	c.LatinWhy = withTurn(c.LatinWhy, turn)
	c.SummaryWhy = withTurn(c.SummaryWhy, turn)
	c.TreeWhy = withTurn(c.TreeWhy, turn)
}

func (c *Card) setSection(sec Section) {
	if have := c.Section(sec.Key); have != nil {
		*have = sec
		return
	}
	c.Sections = append(c.Sections, sec)
}

func (c *Card) setNeighbors(n Neighbors) {
	for i := range c.Neighbors {
		if c.Neighbors[i].NodeKey == n.NodeKey {
			c.Neighbors[i] = n
			return
		}
	}
	c.Neighbors = append(c.Neighbors, n)
}

// Card — карточка по ключу.
func (s *State) Card(id string) *Card {
	for i := range s.Cards {
		if s.Cards[i].ID == id {
			return &s.Cards[i]
		}
	}
	return nil
}

// CurrentCard — карточка, с которой работали последней.
func (s *State) CurrentCard() *Card {
	if s.Current == "" {
		return nil
	}
	return s.Card(s.Current)
}

// Find — карточка по названию: русскому, латинскому, заголовку статьи или
// тому, как животное назвал пользователь. Сравнение без регистра и «ё».
func (s *State) Find(name string) *Card {
	want := norm(name)
	if want == "" {
		return nil
	}
	for i := len(s.Cards) - 1; i >= 0; i-- {
		c := &s.Cards[i]
		for _, v := range []string{c.Name, c.Latin, c.Article, c.Query} {
			if norm(v) == want {
				return c
			}
		}
	}
	// Слово в названии: «рысь» находит «Обыкновенная рысь».
	for i := len(s.Cards) - 1; i >= 0; i-- {
		c := &s.Cards[i]
		for _, word := range strings.Fields(norm(c.Name)) {
			if word == want {
				return c
			}
		}
	}
	return nil
}

// Refused — последний отказ пути по тому же названию запроса: сравнение
// без регистра и «ё», как у Find.
func (s *State) Refused(name string) *NotFound {
	want := norm(name)
	if want == "" {
		return nil
	}
	for i := len(s.NotFound) - 1; i >= 0; i-- {
		if norm(s.NotFound[i].Query) == want {
			return &s.NotFound[i]
		}
	}
	return nil
}

func norm(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.ReplaceAll(s, "ё", "е")
}

// Brief — карточки одной строкой на каждую: блок для ведущего диалога,
// чтобы короткий вопрос относился к животному из прошлых ходов (С-2).
func (s *State) Brief() string {
	if len(s.Cards) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range s.Cards {
		mark := ""
		if c.ID == s.Current {
			mark = " ← текущая"
		}
		b.WriteString("- " + c.Title() + ", статья «" + c.Article + "»" + mark)
		var read []string
		for _, sec := range c.Sections {
			if sec.Status == SectionRead {
				read = append(read, sec.Title)
			}
		}
		if len(read) > 0 {
			b.WriteString("; прочитаны разделы: " + strings.Join(read, ", "))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
