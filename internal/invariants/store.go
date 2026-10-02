package invariants

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

// FileKind — вид файла «свод»: invariants/<свод>.json. Имя Kind занято
// видом инварианта.
var FileKind = &store.Kind{Name: "свод", Dir: "invariants", Current: 1}

// GuideID — свод справочника: invariants/guide.json (ФТ-29).
const GuideID = "guide"

// Store — свод на диске. Свод один на все диалоги и переживает их все:
// положи его в диалог — в новом разговоре правило забыто; поэтому файл
// свой, и правится он чтением-правкой-записью под общим замком.
type Store struct {
	mu   sync.Mutex
	dir  *store.Dir
	seed func() Charter
}

// NewStore — хранилище поверх каталога данных. Пока файла нет, свод — это
// заготовка И-1…И-6: справочник без правил не бывает даже до первой записи.
func NewStore(d *store.Dir) *Store { return &Store{dir: d, seed: Preset} }

// DisplayPath — файл свода для показа.
func (s *Store) DisplayPath(id string) string { return s.dir.DisplayPath(FileKind, id) }

// Has — есть ли файл свода.
func (s *Store) Has(id string) bool { return s.dir.Has(FileKind, id) }

// Get — свод; нет файла — заготовка.
func (s *Store) Get(id string) (Charter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked(id)
}

func (s *Store) fresh(id string) Charter {
	c := s.seed()
	c.ID = id
	return c
}

func (s *Store) readLocked(id string) (Charter, error) {
	var c Charter
	if _, err := s.dir.Read(FileKind, id, &c); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.fresh(id), nil
		}
		return Charter{}, err
	}
	if err := normalize(&c, id); err != nil {
		return Charter{}, fmt.Errorf("свод %s: %w", s.DisplayPath(id), err)
	}
	return c, nil
}

// normalize — проверка файла, который правят и руками. Чинить молча нельзя:
// инвариант без формулировки или с неизвестным видом — сломанное
// ограничение, и работать по нему опаснее, чем не работать вовсе.
func normalize(c *Charter, id string) error {
	c.ID = id
	if c.Version < 1 {
		c.Version = 1
	}
	if c.Items == nil {
		c.Items = []Invariant{}
	}
	seen := make(map[string]bool, len(c.Items))
	for i := range c.Items {
		inv := &c.Items[i]
		if strings.TrimSpace(inv.ID) == "" {
			return errors.New("инвариант без идентификатора")
		}
		key := strings.ToLower(inv.ID)
		if seen[key] {
			return fmt.Errorf("инвариант %s встречается дважды", inv.ID)
		}
		seen[key] = true
		if !inv.Kind.Valid() {
			return fmt.Errorf("инвариант %s — неизвестный вид %q", inv.ID, inv.Kind)
		}
		if strings.TrimSpace(inv.Rule) == "" {
			return fmt.Errorf("инвариант %s без формулировки", inv.ID)
		}
		if inv.Status == "" {
			inv.Status = StatusActive
		}
		if inv.Title == "" {
			inv.Title = inv.ID
		}
	}
	return nil
}

// Update правит свод под замком; fn говорит, писать ли. Редакцию хранилище
// не трогает: её поднимает только принятая поправка — иначе по номеру
// нельзя было бы судить, по каким правилам собран ответ.
func (s *Store) Update(id string, fn func(c *Charter) bool) (Charter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.readLocked(id)
	if err != nil {
		return Charter{}, err
	}
	if !fn(&c) {
		return c, nil
	}
	if c.Created.IsZero() {
		c.Created = time.Now()
	}
	c.Updated = time.Now()
	if err := s.dir.Write(FileKind, id, c); err != nil {
		return Charter{}, err
	}
	return c, nil
}

// Ensure — записать заготовку, если файла ещё нет: свод должен лежать на
// диске, чтобы его можно было прочитать и поправить без приложения. Есть
// файл — не трогаем: заготовка не затирает то, о чём договорились.
func (s *Store) Ensure(id string) (Charter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir.Has(FileKind, id) {
		return s.readLocked(id)
	}
	c := s.fresh(id)
	if err := s.dir.Write(FileKind, id, c); err != nil {
		return Charter{}, err
	}
	return c, nil
}

// Begin — начало хода свода: счётчик общий для всех диалогов.
func (s *Store) Begin(id string) (Charter, error) {
	return s.Update(id, func(c *Charter) bool {
		c.Turn++
		return true
	})
}

// Raw — файл свода как есть: свод — документ, его показывают целиком.
func (s *Store) Raw(id string) (string, []byte, error) {
	data, err := s.dir.Raw(FileKind, id)
	return s.DisplayPath(id), data, err
}
