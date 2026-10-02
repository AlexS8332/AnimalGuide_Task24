package mdd

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
)

// Поиск: сколько видов отдавать по умолчанию и предельно (Query.Limit).
const (
	memDefaultLimit = 20
	memMaxLimit     = 100
)

// Memory — Store в памяти: для тестов инструментов и планировщика. Данные
// копируются на входе и на выходе, так что ни вызывающий, ни получатель не
// испортят хранилище.
type Memory struct {
	mu      sync.RWMutex
	loaded  bool
	release Release
	species []Species // по phylosort, затем по id
	byID    map[int]int
	changes []Change
}

// NewMemory — пустое хранилище: Release отвечает ErrNotFound до первого
// Replace.
func NewMemory() *Memory { return &Memory{byID: map[int]int{}} }

var _ Store = (*Memory)(nil)

func (m *Memory) Release(ctx context.Context) (Release, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.loaded {
		return Release{}, ErrNotFound
	}
	return m.release, nil
}

func (m *Memory) Replace(ctx context.Context, d *Dataset) error {
	// Пустой набор — почти наверняка сломанный разбор архива: он не должен
	// стереть рабочий справочник. Так же ведёт себя SQLite.
	if d == nil || len(d.Species) == 0 {
		return errors.New("mdd: пустой набор данных")
	}
	species := make([]Species, len(d.Species))
	for i, s := range d.Species {
		species[i] = memCloneSpecies(s)
	}
	sort.SliceStable(species, func(i, j int) bool { return memLess(species[i], species[j]) })
	byID := make(map[int]int, len(species))
	for i, s := range species {
		byID[s.ID] = i
	}
	rel := d.Release
	rel.Species = len(species)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.loaded = true
	m.release = rel
	m.species = species
	m.byID = byID
	m.changes = append([]Change(nil), d.Changes...)
	return nil
}

func (m *Memory) Get(ctx context.Context, id int) (Species, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	i, ok := m.byID[id]
	if !ok {
		return Species{}, ErrNotFound
	}
	return memCloneSpecies(m.species[i]), nil
}

// Find ищет сначала по латинскому названию, потом по основному английскому,
// потом по прочим: латинское однозначно, английские могут совпасть у
// разных видов — тогда берётся первый в систематическом порядке.
func (m *Memory) Find(ctx context.Context, name string) (Species, error) {
	key := memNorm(name)
	if key == "" {
		return Species{}, ErrNotFound
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for pass := 0; pass < 3; pass++ {
		for _, s := range m.species {
			var hit bool
			switch pass {
			case 0:
				hit = memNorm(s.SciName) == key
			case 1:
				hit = memNorm(s.CommonName) == key
			default:
				for _, n := range s.OtherCommonNames {
					if memNorm(n) == key {
						hit = true
						break
					}
				}
			}
			if hit {
				return memCloneSpecies(s), nil
			}
		}
	}
	return Species{}, ErrNotFound
}

func (m *Memory) Search(ctx context.Context, q Query) ([]Species, int, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = memDefaultLimit
	}
	if limit > memMaxLimit {
		limit = memMaxLimit
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	match := memMatcher(q)

	m.mu.RLock()
	defer m.mu.RUnlock()
	list := []Species{}
	total := 0
	for _, s := range m.species {
		if !match(s) {
			continue
		}
		if total >= offset && len(list) < limit {
			list = append(list, memCloneSpecies(s))
		}
		total++
	}
	return list, total, nil
}

// Changes: limit ≤ 0 — все изменения.
func (m *Memory) Changes(ctx context.Context, category string, limit int) ([]Change, error) {
	category = strings.TrimSpace(category)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []Change{}
	for _, c := range m.changes {
		if category != "" && !strings.EqualFold(c.Category, category) {
			continue
		}
		out = append(out, c)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (m *Memory) IDs(ctx context.Context) ([]int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]int, len(m.species))
	for i, s := range m.species {
		ids[i] = s.ID
	}
	return ids, nil
}

// memMatcher собирает проверку вида по запросу; всё приводится к нижнему
// регистру один раз.
func memMatcher(q Query) func(Species) bool {
	text := memNorm(q.Text)
	order, family, genus := memNorm(q.Order), memNorm(q.Family), memNorm(q.Genus)
	country, realm := memNorm(q.Country), memNorm(q.Realm)
	var iucn []string
	for _, s := range q.IUCN {
		if s = memNorm(s); s != "" {
			iucn = append(iucn, s)
		}
	}
	return func(s Species) bool {
		if text != "" && !memNameContains(s, text) {
			return false
		}
		if order != "" && memNorm(s.Order) != order {
			return false
		}
		if family != "" && memNorm(s.Family) != family {
			return false
		}
		if genus != "" && memNorm(s.Genus) != genus {
			return false
		}
		if country != "" && !memAnyEqual(s.Countries, country) && !memAnyEqual(s.CountriesUncertain, country) {
			return false
		}
		if realm != "" && !memAnyEqual(s.Realms, realm) {
			return false
		}
		if len(iucn) > 0 && !memAnyEqual(iucn, memNorm(s.IUCN)) {
			return false
		}
		if q.Extinct != nil && s.Extinct != *q.Extinct {
			return false
		}
		if q.Domestic != nil && s.Domestic != *q.Domestic {
			return false
		}
		return true
	}
}

func memNameContains(s Species, text string) bool {
	if strings.Contains(memNorm(s.SciName), text) || strings.Contains(memNorm(s.CommonName), text) {
		return true
	}
	for _, n := range s.OtherCommonNames {
		if strings.Contains(memNorm(n), text) {
			return true
		}
	}
	return false
}

func memAnyEqual(list []string, want string) bool {
	for _, v := range list {
		if memNorm(v) == want {
			return true
		}
	}
	return false
}

// memNorm — ключ сравнения: без учёта регистра, «_» равно пробелу.
func memNorm(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(s, "_", " ")))
}

func memLess(a, b Species) bool {
	if a.Phylosort != b.Phylosort {
		return a.Phylosort < b.Phylosort
	}
	return a.ID < b.ID
}

func memCloneSpecies(s Species) Species {
	s.OtherCommonNames = memCloneStrings(s.OtherCommonNames)
	s.Countries = memCloneStrings(s.Countries)
	s.CountriesUncertain = memCloneStrings(s.CountriesUncertain)
	s.Continents = memCloneStrings(s.Continents)
	s.Realms = memCloneStrings(s.Realms)
	return s
}

func memCloneStrings(v []string) []string {
	if v == nil {
		return nil
	}
	return append([]string(nil), v...)
}
