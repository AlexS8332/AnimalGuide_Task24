package collection

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/paths"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

// Kind — вид файла «подборка»: collections/<id>.json.
var Kind = &store.Kind{Name: "подборка", Dir: "collections", Current: 1, ValidKey: paths.ValidHex}

// ErrNotFound — подборки нет.
var ErrNotFound = errors.New("подборки нет")

// Store — подборки на диске. Правка — под замком: инструмент переходов
// читает, проверяет и пишет состояние за один раз.
type Store struct {
	mu  sync.Mutex
	dir *store.Dir
}

// NewStore — хранилище поверх каталога данных.
func NewStore(d *store.Dir) *Store { return &Store{dir: d} }

// Has — есть ли файл подборки.
func (s *Store) Has(id string) bool { return s.dir.Has(Kind, id) }

// Get — подборка; нет файла — новая пустая.
func (s *Store) Get(id, title string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(id, title)
}

func (s *Store) getLocked(id, title string) (State, error) {
	st := New(id, title)
	if _, err := s.dir.Read(Kind, id, &st); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return New(id, title), nil
		}
		return New(id, title), err
	}
	st.ID = id
	if st.Title == "" {
		st.Title = title
	}
	if st.Items == nil {
		st.Items = []Item{}
	}
	if st.Log == nil {
		st.Log = []Change{}
	}
	return st, nil
}

// Update правит подборку под замком; fn говорит, писать ли.
func (s *Store) Update(id, title string, fn func(st *State) bool) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.getLocked(id, title)
	if err != nil {
		return st, err
	}
	if !fn(&st) {
		return st, nil
	}
	return st, s.dir.Write(Kind, id, st)
}

// List — все подборки, свежие первыми.
func (s *Store) List() ([]State, []error) {
	keys, problems := s.dir.List(Kind)
	var out []State
	for _, k := range keys {
		st, err := s.Get(k, "")
		if err != nil {
			problems = append(problems, err)
			continue
		}
		out = append(out, st)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Updated, out[j].Updated
		return a != nil && (b == nil || a.After(*b))
	})
	return out, problems
}

// Raw — файл подборки и путь для показа.
func (s *Store) Raw(id string) (string, []byte, error) {
	data, err := s.dir.Raw(Kind, id)
	return s.dir.DisplayPath(Kind, id), data, err
}

// DisplayPath — файл подборки для показа.
func (s *Store) DisplayPath(id string) string { return s.dir.DisplayPath(Kind, id) }

// Markdown — подборка для выгрузки (ФТ-39): карточки сданных видов со
// ссылками на источники.
func Markdown(st State) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Подборка: %s\n\n", st.Title)
	if st.Goal != "" {
		fmt.Fprintf(&b, "%s\n\n", st.Goal)
	}
	fmt.Fprintf(&b, "Этап: %s. Видов собрано: %d из %d.\n\n", st.Stage.Title(), st.DoneItems(), len(st.Items))
	for _, it := range st.Items {
		if it.Output == nil {
			fmt.Fprintf(&b, "## %d. %s\n\nКарточка не сдана.\n\n", it.N, it.Name)
			continue
		}
		md := card.Markdown(it.Output.Card)
		// Заголовок карточки — второго уровня: подборка — один документ.
		md = strings.Replace(md, "# ", fmt.Sprintf("## %d. ", it.N), 1)
		md = strings.ReplaceAll(md, "\n## ", "\n### ")
		b.WriteString(md)
		b.WriteByte('\n')
	}
	if st.Report != nil {
		b.WriteString("## Сверка\n\n")
		for _, c := range st.Report.Checks {
			mark := "сошлось"
			if !c.OK {
				mark = "не сошлось: " + strings.Join(c.Issues, "; ")
			}
			fmt.Fprintf(&b, "- вид %d — %s\n", c.N, mark)
		}
	}
	return b.String()
}
