package trivia

import (
	"context"
	"sort"
)

// SummaryStore у Memory — для тестов сводки и демона. Копирование на входе
// и выходе — через JSON, как у SQLite (summaryClone).

var _ SummaryStore = (*Memory)(nil)

func (m *Memory) SaveSummary(ctx context.Context, s Summary) (int64, error) {
	if err := summaryValidate(s); err != nil {
		return 0, err
	}
	s, err := summaryClone(s)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextSummaryID++
	s.ID = m.nextSummaryID
	m.summaries = append(m.summaries, s)
	return s.ID, nil
}

func (m *Memory) Summary(ctx context.Context, id int64) (Summary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	i := sort.Search(len(m.summaries), func(i int) bool { return m.summaries[i].ID >= id })
	if i == len(m.summaries) || m.summaries[i].ID != id {
		return Summary{}, ErrIssueNotFound
	}
	return summaryClone(m.summaries[i])
}

func (m *Memory) Summaries(ctx context.Context, limit int) ([]Summary, error) {
	limit = summaryLimit(limit)
	m.mu.RLock()
	all := append([]Summary(nil), m.summaries...)
	m.mu.RUnlock()

	summarySort(all)
	list := []Summary{}
	for i := 0; i < len(all) && len(list) < limit; i++ {
		s, err := summaryClone(all[i])
		if err != nil {
			return nil, err
		}
		list = append(list, s)
	}
	return list, nil
}

func (m *Memory) LatestSummary(ctx context.Context) (Summary, bool, error) {
	list, err := m.Summaries(ctx, 1)
	if err != nil || len(list) == 0 {
		return Summary{}, false, err
	}
	return list[0], true, nil
}
