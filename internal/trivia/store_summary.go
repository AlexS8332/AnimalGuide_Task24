package trivia

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Общие для Memory и SQLite правила хранения сводок — как store_issue.go у
// выпусков: обе реализации отказывают и копируют одинаково.

// Пределы SummaryStore.Summaries.
const (
	summaryDefaultLimit = 20
	summaryMaxLimit     = 100
)

// summaryLimit — limit после умолчания и предела.
func summaryLimit(limit int) int {
	switch {
	case limit <= 0:
		return summaryDefaultLimit
	case limit > summaryMaxLimit:
		return summaryMaxLimit
	}
	return limit
}

// summaryValidate — сводку можно сохранить. Неизвестный Trigger — ошибка по
// той же причине, что неизвестное состояние выпуска: опечатка выпала бы из
// отбора «ручные сводки» молча.
func summaryValidate(s Summary) error {
	switch s.Trigger {
	case SummaryTriggerSchedule, SummaryTriggerManual:
	default:
		return fmt.Errorf("trivia: сводка не сохранена: неизвестный запуск %q", s.Trigger)
	}
	if err := storeCheckTime("начало периода", s.From); err != nil {
		return fmt.Errorf("trivia: сводка не сохранена: %w", err)
	}
	if err := storeCheckTime("конец периода", s.To); err != nil {
		return fmt.Errorf("trivia: сводка не сохранена: %w", err)
	}
	if s.To.Before(s.From) {
		return fmt.Errorf("trivia: сводка не сохранена: конец периода раньше начала")
	}
	if err := storeCheckTime("время сводки", s.CreatedAt); err != nil {
		return fmt.Errorf("trivia: сводка не сохранена: %w", err)
	}
	return nil
}

// summaryJSON — JSON-части сводки (агрегат, расход, стоимость). Ошибка —
// например, NaN в стоимости: ошибка расчёта, а не данных.
func summaryJSON(s Summary) (agg, spend, cost string, err error) {
	parts := [3]struct {
		name string
		v    any
		out  *string
	}{{"агрегат", s.Aggregate, &agg}, {"расход", s.Spend, &spend}, {"стоимость", s.Cost, &cost}}
	for _, p := range parts {
		b, err := json.Marshal(p.v)
		if err != nil {
			return "", "", "", fmt.Errorf("trivia: сводка: %s: %w", p.name, err)
		}
		*p.out = string(b)
	}
	return agg, spend, cost, nil
}

// summaryUnJSON — обратно из JSON-частей в сводку.
func summaryUnJSON(s *Summary, agg, spend, cost string) error {
	parts := [3]struct {
		name string
		raw  string
		v    any
	}{{"агрегат", agg, &s.Aggregate}, {"расход", spend, &s.Spend}, {"стоимость", cost, &s.Cost}}
	for _, p := range parts {
		if err := json.Unmarshal([]byte(p.raw), p.v); err != nil {
			return fmt.Errorf("trivia: сводка %d: %s: %w", s.ID, p.name, err)
		}
	}
	return nil
}

// summaryClone — копия сводки через тот же JSON, что у SQLite: Memory
// отдаёт ровно то, что отдала бы база (пустой срез агрегата — null, зона
// времени агрегата — смещение), и не делит срезы с вызывающим. Время
// периода и создания — без монотонных показаний, как у выпусков.
func summaryClone(s Summary) (Summary, error) {
	agg, spend, cost, err := summaryJSON(s)
	if err != nil {
		return Summary{}, err
	}
	out := Summary{ID: s.ID, From: s.From.Round(0), To: s.To.Round(0), CreatedAt: s.CreatedAt.Round(0),
		Trigger: s.Trigger, Text: s.Text, Error: s.Error}
	if err := summaryUnJSON(&out, agg, spend, cost); err != nil {
		return Summary{}, err
	}
	return out, nil
}

// summarySort — новые первыми: CreatedAt, затем ID по убыванию.
func summarySort(list []Summary) {
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	})
}
