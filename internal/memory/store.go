package memory

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

// Виды файлов памяти. Версия 1 совпадает с форматом слоёв прежних
// упражнений: поля те же, файлы читаются как есть.
var (
	LongKind = &store.Kind{Name: "долговременная память", Dir: "memory/long", Current: 1}
	WorkKind = &store.Kind{Name: "рабочая память", Dir: "memory/work", Current: 1}
)

// KindOf — вид файла слоя.
func KindOf(layer string) (*store.Kind, error) {
	switch layer {
	case LayerLong:
		return LongKind, nil
	case LayerWork:
		return WorkKind, nil
	}
	return nil, fmt.Errorf("в слое %q записей не бывает", layer)
}

// Store — слои памяти на диске, по файлу на владельца. Правка — под замком:
// запись может переехать из слоя в слой, и половина переезда на диске была
// бы хуже, чем ничего.
type Store struct {
	mu  sync.Mutex
	dir *store.Dir
}

// NewStore — хранилище поверх каталога данных.
func NewStore(d *store.Dir) *Store { return &Store{dir: d} }

// Card — слой владельца; нет файла — пустой слой.
func (s *Store) Card(layer, id, title string) (Card, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked(layer, id, title)
}

func (s *Store) readLocked(layer, id, title string) (Card, error) {
	k, err := KindOf(layer)
	if err != nil {
		return Card{}, err
	}
	c := NewCard(layer, id, title)
	if id == "" {
		return c, nil
	}
	if _, err := s.dir.Read(k, id, &c); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewCard(layer, id, title), nil
		}
		return NewCard(layer, id, title), err
	}
	c.Layer, c.ID = layer, id
	if c.Title == "" {
		c.Title = title
	}
	if c.Entries == nil {
		c.Entries = []Entry{}
	}
	return c, nil
}

// Update правит слои человека и подборки под одним замком и пишет только
// изменившиеся. id == "" — слоя нет (нет собеседника или подборки).
func (s *Store) Update(longID, longTitle, workID, workTitle string, fn func(long, work *Card) error) (Card, Card, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	long, err := s.readLocked(LayerLong, longID, longTitle)
	if err != nil {
		return long, Card{}, err
	}
	work, err := s.readLocked(LayerWork, workID, workTitle)
	if err != nil {
		return long, work, err
	}
	lv, wv := long.Version, work.Version
	var lp, wp *Card
	if longID != "" {
		lp = &long
	}
	if workID != "" {
		wp = &work
	}
	if err := fn(lp, wp); err != nil {
		return long, work, err
	}
	if lp != nil && long.Version != lv {
		if err := s.dir.Write(LongKind, longID, long); err != nil {
			return long, work, err
		}
	}
	if wp != nil && work.Version != wv {
		if err := s.dir.Write(WorkKind, workID, work); err != nil {
			return long, work, err
		}
	}
	return long, work, nil
}

// Put — правка записи руками (окно «Память целиком»).
func (s *Store) Put(layer, id, title, key, value string, turn int) (Card, Change, error) {
	var ch Change
	var out Card
	err := s.edit(layer, id, title, func(c *Card) error {
		old, changed := c.Set(key, value, turn, SourceHuman)
		if !changed {
			return fmt.Errorf("запись «%s» и так такая", key)
		}
		ch = Change{Op: OpSet, Layer: layer, Key: cleanKey(key), Value: value, Old: old, Reason: "правка руками"}
		out = *c
		return nil
	})
	return out, ch, err
}

// Forget — удалить запись руками.
func (s *Store) Forget(layer, id, key string) (Card, Change, error) {
	var ch Change
	var out Card
	err := s.edit(layer, id, "", func(c *Card) error {
		old, had := c.Delete(key)
		if !had {
			return fmt.Errorf("записи «%s» нет", key)
		}
		ch = Change{Op: OpDelete, Layer: layer, Key: key, Old: old, Reason: "удалено руками"}
		out = *c
		return nil
	})
	return out, ch, err
}

// AddToList — дописать название в список, который ведёт код.
func (s *Store) AddToList(id, title, key, item string, turn int) (Card, bool, error) {
	var out Card
	var added bool
	err := s.edit(LayerLong, id, title, func(c *Card) error {
		added = c.AddToList(key, item, turn)
		out = *c
		return nil
	})
	return out, added, err
}

func (s *Store) edit(layer, id, title string, fn func(c *Card) error) error {
	k, err := KindOf(layer)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.readLocked(layer, id, title)
	if err != nil {
		return err
	}
	v := c.Version
	if err := fn(&c); err != nil {
		return err
	}
	if c.Version == v {
		return nil
	}
	return s.dir.Write(k, id, c)
}

// List — все слои одного вида.
func (s *Store) List(layer string) ([]Card, []error) {
	k, err := KindOf(layer)
	if err != nil {
		return nil, []error{err}
	}
	keys, problems := s.dir.List(k)
	var out []Card
	for _, id := range keys {
		c, err := s.Card(layer, id, "")
		if err != nil {
			problems = append(problems, err)
			continue
		}
		out = append(out, c)
	}
	return out, problems
}

// Raw — файл слоя как есть и путь для показа.
func (s *Store) Raw(layer, id string) (string, []byte, error) {
	k, err := KindOf(layer)
	if err != nil {
		return "", nil, err
	}
	data, err := s.dir.Raw(k, id)
	return s.dir.DisplayPath(k, id), data, err
}

// DisplayPath — файл слоя для показа.
func (s *Store) DisplayPath(layer, id string) string {
	k, err := KindOf(layer)
	if err != nil {
		return ""
	}
	return s.dir.DisplayPath(k, id)
}
