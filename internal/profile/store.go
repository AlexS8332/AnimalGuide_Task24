package profile

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

// Kind — вид файла «профиль»: profiles/<человек>.json. Версия 1 — анкета
// прошлых упражнений (обращение «ты»/«вы», длина short/medium/long, поле
// shape и поля, которых у справочника нет); версия 2 — эта.
var Kind = &store.Kind{Name: "профиль", Dir: "profiles", Current: 2,
	Migrate: map[int]store.Migration{1: migrate1to2}}

// legacyValues — как значения прежней анкеты ложатся на нынешнюю. Поле
// поменяло смысл — значит, миграция, а не «просто переименовать» (ФТ-51).
var legacyValues = map[string]struct {
	field  string
	values map[string]string
}{
	"address": {FieldAddress, map[string]string{"ты": "ty", "вы": "vy"}},
	"length":  {FieldLength, map[string]string{"short": "short", "medium": "normal", "long": "long"}},
	"shape":   {FieldForm, map[string]string{"prose": "prose", "list": "list", "table": "table"}},
	"level":   {FieldLevel, map[string]string{"novice": "amateur", "expert": "expert"}},
	"emoji":   {FieldEmoji, map[string]string{"yes": "some", "no": "no"}},
}

// migrate1to2 переводит прежнюю анкету на анкету справочника. Поля, которым
// здесь нет места (имя, род занятий, тон, код, примеры), не выбрасываются, а
// уходят в legacy: данные не теряются (ФТ-52), а человек увидит их в окне
// профиля.
func migrate1to2(doc map[string]json.RawMessage) error {
	var values map[string]json.RawMessage
	if _, err := store.Get(doc, "values", &values); err != nil {
		return err
	}
	next := map[string]json.RawMessage{}
	legacy := map[string]json.RawMessage{}
	for key, raw := range values {
		m, known := legacyValues[key]
		var v struct {
			Value string `json:"value"`
		}
		json.Unmarshal(raw, &v)
		if nv, ok := m.values[v.Value]; known && ok {
			var full map[string]json.RawMessage
			json.Unmarshal(raw, &full)
			store.Set(full, "value", nv)
			data, _ := json.Marshal(full)
			next[m.field] = data
			continue
		}
		legacy[key] = raw
	}
	if err := store.Set(doc, "values", next); err != nil {
		return err
	}
	var asks map[string]int
	if ok, _ := store.Get(doc, "asks", &asks); ok {
		converted := map[string]int{}
		for k, n := range asks {
			field, value, _ := strings.Cut(k, "=")
			if m, known := legacyValues[field]; known {
				if nv, ok := m.values[value]; ok {
					converted[m.field+"="+nv] += n
					continue
				}
			}
			raw, _ := json.Marshal(n)
			legacy["asks:"+k] = raw
		}
		store.Set(doc, "asks", converted)
	}
	if len(legacy) > 0 {
		return store.Set(doc, "legacy", legacy)
	}
	return nil
}

// Store — анкеты на диске, по файлу на человека.
type Store struct {
	mu  sync.Mutex
	dir *store.Dir
}

// NewStore — хранилище поверх каталога данных.
func NewStore(d *store.Dir) *Store { return &Store{dir: d} }

// Get — анкета человека; нет файла — пустая анкета.
func (s *Store) Get(id, title string) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(id, title)
}

func (s *Store) getLocked(id, title string) (Profile, error) {
	p := New(id, title)
	if _, err := s.dir.Read(Kind, id, &p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return New(id, title), nil
		}
		return New(id, title), err
	}
	p.ID = id
	if p.Title == "" {
		p.Title = title
	}
	if p.Values == nil {
		p.Values = map[string]Value{}
	}
	if p.Limits == nil {
		p.Limits = []Limit{}
	}
	return p, nil
}

// Has — есть ли файл анкеты.
func (s *Store) Has(id string) bool { return s.dir.Has(Kind, id) }

// Save записывает анкету.
func (s *Store) Save(p Profile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dir.Write(Kind, p.ID, p)
}

// Update правит анкету под замком и пишет, только если fn что-то изменила.
func (s *Store) Update(id, title string, fn func(p *Profile) (bool, error)) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.getLocked(id, title)
	if err != nil {
		return p, err
	}
	changed, err := fn(&p)
	if err != nil || !changed {
		return p, err
	}
	return p, s.dir.Write(Kind, id, p)
}

// List — все анкеты.
func (s *Store) List() ([]Profile, []error) {
	keys, problems := s.dir.List(Kind)
	var out []Profile
	for _, k := range keys {
		p, err := s.Get(k, "")
		if err != nil {
			problems = append(problems, err)
			continue
		}
		out = append(out, p)
	}
	return out, problems
}

// Raw — файл анкеты как есть и путь для показа.
func (s *Store) Raw(id string) (string, []byte, error) {
	data, err := s.dir.Raw(Kind, id)
	return s.dir.DisplayPath(Kind, id), data, err
}

// DisplayPath — файл анкеты для показа.
func (s *Store) DisplayPath(id string) string { return s.dir.DisplayPath(Kind, id) }

// Delete удаляет анкету.
func (s *Store) Delete(id string) error { return s.dir.Delete(Kind, id) }
