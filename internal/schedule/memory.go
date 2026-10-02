package schedule

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"
)

// Memory — RunStore в памяти: для тестов планировщика и запуска без базы.
// Время приводится к виду, в каком его вернула бы SQLite (UTC, без
// монотонных показаний), — поведение обеих реализаций одинаково.
type Memory struct {
	mu     sync.Mutex
	nextID int64
	runs   []Run // в порядке записи, то есть по возрастанию ID
}

var _ RunStore = (*Memory)(nil)

// NewMemory — пустой журнал.
func NewMemory() *Memory { return &Memory{} }

func (m *Memory) Begin(ctx context.Context, r Run) (int64, error) {
	if err := storeValidateBegin(r); err != nil {
		return 0, err
	}
	r = storeNormalize(r)
	r.Status = RunRunning
	r.Finished = time.Time{}
	r.Error = ""
	r.Outcome = Outcome{}
	return m.add(r), nil
}

func (m *Memory) Record(ctx context.Context, r Run) (int64, error) {
	if err := storeValidateRecord(r); err != nil {
		return 0, err
	}
	return m.add(storeNormalize(r)), nil
}

func (m *Memory) add(r Run) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	r.ID = m.nextID
	m.runs = append(m.runs, r)
	return r.ID
}

func (m *Memory) Finish(ctx context.Context, r Run) error {
	if err := storeValidateFinish(r); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.find(r.ID)
	if i < 0 {
		return fmt.Errorf("schedule: итог запуска %d: нет такого запуска", r.ID)
	}
	cur := &m.runs[i]
	if cur.Status != RunRunning {
		return fmt.Errorf("schedule: итог запуска %d: запуск уже завершён (%s)", r.ID, cur.Status)
	}
	cur.Finished = storeTime(r.Finished)
	cur.Status = r.Status
	cur.Error = r.Error
	cur.Outcome = r.Outcome
	return nil
}

// find — индекс запуска по ID; runs упорядочены по ID.
func (m *Memory) find(id int64) int {
	i := sort.Search(len(m.runs), func(i int) bool { return m.runs[i].ID >= id })
	if i < len(m.runs) && m.runs[i].ID == id {
		return i
	}
	return -1
}

// newestFirst — порядок журнала: Started, затем ID по убыванию.
func newestFirst(a, b Run) int {
	if c := b.Started.Compare(a.Started); c != 0 {
		return c
	}
	switch {
	case a.ID > b.ID:
		return -1
	case a.ID < b.ID:
		return 1
	}
	return 0
}

func (m *Memory) Runs(ctx context.Context, q RunQuery) ([]Run, error) {
	m.mu.Lock()
	list := []Run{}
	for _, r := range m.runs {
		if q.Job != "" && r.Job != q.Job {
			continue
		}
		if len(q.Status) > 0 && !slices.Contains(q.Status, r.Status) {
			continue
		}
		if !q.Since.IsZero() && r.Started.Before(q.Since) {
			continue
		}
		if !q.Until.IsZero() && !r.Started.Before(q.Until) {
			continue
		}
		list = append(list, r)
	}
	m.mu.Unlock()
	slices.SortFunc(list, newestFirst)
	if n := storeLimit(q.Limit); len(list) > n {
		list = list[:n]
	}
	return list, nil
}

func (m *Memory) Last(ctx context.Context, job string) (Run, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var (
		best  Run
		found bool
	)
	for _, r := range m.runs {
		if r.Job != job || r.Status == RunBudget || r.Status == RunSkipped {
			continue
		}
		if !found || newestFirst(r, best) < 0 {
			best, found = r, true
		}
	}
	return best, found, nil
}

func (m *Memory) Spent(ctx context.Context, since, until time.Time) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sum := 0.0
	for _, r := range m.runs {
		if r.Started.Before(since) || (!until.IsZero() && !r.Started.Before(until)) {
			continue
		}
		sum += r.CostUSD
	}
	return sum, nil
}

func (m *Memory) Abandon(ctx context.Context, at time.Time) (int, error) {
	if err := storeCheckTime("время остановки", at); err != nil {
		return 0, fmt.Errorf("schedule: незавершённые запуски: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for i := range m.runs {
		if m.runs[i].Status != RunRunning {
			continue
		}
		m.runs[i].Status = RunFailed
		m.runs[i].Error = abandonError
		m.runs[i].Finished = storeTime(at)
		n++
	}
	return n, nil
}
