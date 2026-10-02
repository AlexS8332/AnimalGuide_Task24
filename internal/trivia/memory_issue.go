package trivia

import (
	"context"
	"sort"
)

// IssueStore у Memory — для тестов сборки выпуска и сводок. Копирование на
// входе и выходе — как у выборов (issueClone).

var _ IssueStore = (*Memory)(nil)

func (m *Memory) SaveIssue(ctx context.Context, is Issue) (int64, error) {
	if err := issueValidate(is); err != nil {
		return 0, err
	}
	is = issueClone(is)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextIssueID++
	is.ID = m.nextIssueID
	m.issues = append(m.issues, is)
	return is.ID, nil
}

func (m *Memory) Issue(ctx context.Context, id int64) (Issue, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// ID растут с позицией в срезе: поиск делением пополам.
	i := sort.Search(len(m.issues), func(i int) bool { return m.issues[i].ID >= id })
	if i == len(m.issues) || m.issues[i].ID != id {
		return Issue{}, ErrIssueNotFound
	}
	return issueClone(m.issues[i]), nil
}

func (m *Memory) Issues(ctx context.Context, q IssueQuery) ([]Issue, int, error) {
	limit, offset := issueLimit(q)
	needle := issueNeedle(q)

	m.mu.RLock()
	var found []Issue
	for _, is := range m.issues {
		if issueMatches(is, q, needle) {
			found = append(found, is)
		}
	}
	m.mu.RUnlock()

	// Новые первыми, при равном времени — позже сохранённый: CreatedAt не
	// обязан расти вместе с ID (выпуск собирается минутами, два сборщика
	// сохраняют в разном порядке).
	sort.Slice(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	})
	total := len(found)
	list := []Issue{}
	for i := offset; i < total && len(list) < limit; i++ {
		list = append(list, issueClone(found[i]))
	}
	return list, total, nil
}

func (m *Memory) Pick(ctx context.Context, id int64) (Pick, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	i := sort.Search(len(m.picks), func(i int) bool { return m.picks[i].ID >= id })
	if i == len(m.picks) || m.picks[i].ID != id {
		return Pick{}, ErrIssueNotFound
	}
	return storeClonePick(m.picks[i]), nil
}
