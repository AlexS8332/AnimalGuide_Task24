package trivia

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Memory — PickStore в памяти: для тестов выбора и планировщика. Выборы и
// проверки копируются на входе и на выходе: вызывающий, дописав что-то в
// Rejected или Weights после SavePick, не изменит сохранённое, а получатель
// Picks не испортит хранилище.
type Memory struct {
	mu     sync.RWMutex
	nextID int64
	picks  []Pick // в порядке сохранения, то есть по возрастанию ID
	checks map[int]Eligibility

	// Выпуски (memory_issue.go): в порядке сохранения, то есть по
	// возрастанию ID; счётчик свой — ID выпусков и выборов независимы, как
	// у двух таблиц SQLite.
	nextIssueID int64
	issues      []Issue

	// Сводки (memory_summary.go): так же, по возрастанию ID, счётчик свой.
	nextSummaryID int64
	summaries     []Summary
}

var _ PickStore = (*Memory)(nil)

// NewMemory — пустое хранилище.
func NewMemory() *Memory { return &Memory{checks: map[int]Eligibility{}} }

func (m *Memory) SavePick(ctx context.Context, p Pick) (int64, error) {
	if err := storeValidatePick(p); err != nil {
		return 0, err
	}
	p = storeClonePick(p)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	p.ID = m.nextID
	m.picks = append(m.picks, p)
	return p.ID, nil
}

func (m *Memory) RecentSpecies(ctx context.Context, since time.Time) ([]int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := map[int]bool{}
	ids := []int{}
	for _, p := range m.picks {
		if !p.PickedAt.Before(since) && !seen[p.SpeciesID] {
			seen[p.SpeciesID] = true
			ids = append(ids, p.SpeciesID)
		}
	}
	// Порядок контрактом не задан; по возрастанию — как у SQLite.
	sort.Ints(ids)
	return ids, nil
}

func (m *Memory) Picks(ctx context.Context, limit int) ([]Pick, error) {
	m.mu.RLock()
	list := make([]Pick, len(m.picks))
	for i, p := range m.picks {
		list[i] = storeClonePick(p)
	}
	m.mu.RUnlock()

	// Новые первыми по времени выбора; при равном времени — позже
	// сохранённый (больший ID). PickedAt не обязан расти вместе с ID:
	// выбор может сохраниться с запозданием или с чужими часами.
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if !a.PickedAt.Equal(b.PickedAt) {
			return a.PickedAt.After(b.PickedAt)
		}
		return a.ID > b.ID
	})
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	return list, nil
}

func (m *Memory) CachedCheck(ctx context.Context, speciesID int, notBefore time.Time) (Eligibility, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.checks[speciesID]
	if !ok || e.CheckedAt.Before(notBefore) {
		return Eligibility{}, false, nil
	}
	return e, true, nil
}

func (m *Memory) SaveCheck(ctx context.Context, e Eligibility) error {
	if err := storeValidateCheck(e); err != nil {
		return err
	}
	e.CheckedAt = e.CheckedAt.Round(0)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checks[e.SpeciesID] = e
	return nil
}
