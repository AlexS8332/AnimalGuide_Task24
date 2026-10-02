package history

import (
	"fmt"
	"sort"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/paths"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

// Kind — вид файла «диалог». Версия 1 — форматы прошлых упражнений без поля
// schema (плоский диалог и дерево веток), версия 2 — этот.
var Kind = &store.Kind{
	Name: "диалог", Dir: "history", Current: 2,
	Migrate:  map[int]store.Migration{1: migrate1to2},
	ValidKey: paths.ValidHex,
}

// Store — диалоги в каталоге хранилища, по файлу на диалог. Запись
// атомарная, битый файл при загрузке пропускается с предупреждением.
type Store struct {
	dir *store.Dir
}

// NewStore — хранилище поверх каталога данных.
func NewStore(d *store.Dir) *Store { return &Store{dir: d} }

// DisplayDir — каталог диалогов для показа человеку.
func (s *Store) DisplayDir() string { return s.dir.DisplayDir(Kind) }

// DisplayPath — файл диалога для показа человеку.
func (s *Store) DisplayPath(id string) string { return s.dir.DisplayPath(Kind, id) }

// Save записывает диалог.
func (s *Store) Save(c *Conversation) error { return s.dir.Write(Kind, c.ID, c) }

// Get читает один диалог.
func (s *Store) Get(id string) (*Conversation, error) {
	var c Conversation
	if _, err := s.dir.Read(Kind, id, &c); err != nil {
		return nil, err
	}
	if c.ID != id {
		return nil, fmt.Errorf("в файле %s диалог с id %q", id, c.ID)
	}
	normalize(&c)
	return &c, nil
}

// Load читает все диалоги каталога, свежие первыми. Битый или непонятный
// файл не роняет загрузку: он возвращается проблемой (ФТ-15, ФТ-53).
func (s *Store) Load() ([]*Conversation, []error) {
	keys, problems := s.dir.List(Kind)
	var out []*Conversation
	for _, k := range keys {
		c, err := s.Get(k)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, problems
}

// Raw — файл диалога как есть.
func (s *Store) Raw(id string) ([]byte, error) { return s.dir.Raw(Kind, id) }

// Delete удаляет файл диалога.
func (s *Store) Delete(id string) error { return s.dir.Delete(Kind, id) }

// normalize чинит то, что после чтения могло оказаться пустым: nil-срезы
// превратились бы в null в JSON для интерфейса, а активная ветка могла
// указывать в никуда, если файл правили руками.
func normalize(c *Conversation) {
	if c.Owners == nil {
		c.Owners = []string{}
	}
	if len(c.Branches) == 0 {
		root := &Branch{ID: NewID(), Name: RootBranchName, Created: c.Created}
		c.Branches = []*Branch{root}
	}
	for _, b := range c.Branches {
		if b.Messages == nil {
			b.Messages = []llm.Message{}
		}
		if b.Turns == nil {
			b.Turns = []Turn{}
		}
	}
	if c.Find(c.Active) == nil {
		c.Active = c.Branches[0].ID
	}
}
