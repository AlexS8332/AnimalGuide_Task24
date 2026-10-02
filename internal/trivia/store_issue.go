package trivia

import (
	"fmt"
	"strings"
)

// Общие для Memory и SQLite правила хранения выпусков: проверка входа,
// копирование, строка поиска и пределы выборки. Как и store.go — чтобы обе
// реализации вели себя одинаково на одних и тех же данных.

// Пределы IssueQuery.Limit.
const (
	issueDefaultLimit = 20
	issueMaxLimit     = 100
)

// issueLimit — Limit и Offset запроса после умолчаний и пределов.
// Отрицательные значения — как нулевые: «не задано», а не ошибка.
func issueLimit(q IssueQuery) (limit, offset int) {
	limit = q.Limit
	if limit <= 0 {
		limit = issueDefaultLimit
	}
	if limit > issueMaxLimit {
		limit = issueMaxLimit
	}
	offset = q.Offset
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// issueValidate — выпуск можно сохранить. Неизвестный статус — ошибка:
// сводка считает выпуски по Status, и опечатка («OK», «fail») выпала бы из
// всех её строк молча.
func issueValidate(is Issue) error {
	if is.PickID <= 0 {
		return fmt.Errorf("trivia: выпуск не сохранён: неверный id выбора %d", is.PickID)
	}
	if is.SpeciesID <= 0 {
		return fmt.Errorf("trivia: выпуск не сохранён: неверный id вида %d", is.SpeciesID)
	}
	switch is.Status {
	case IssueOK, IssueThin, IssueFailed:
	default:
		return fmt.Errorf("trivia: выпуск о виде %d не сохранён: неизвестное состояние %q", is.SpeciesID, is.Status)
	}
	if err := storeCheckTime("время выпуска", is.CreatedAt); err != nil {
		return fmt.Errorf("trivia: выпуск о виде %d не сохранён: %w", is.SpeciesID, err)
	}
	return nil
}

// issueSearchText — строка, по которой ищет IssueQuery.Text: поля через
// перевод строки (подстрока запроса не склеит конец одного поля с началом
// другого, если сама не содержит перевода строки), в нижнем регистре Go.
//
// Почему не lower()/LIKE в SQLite: без ICU они сворачивают регистр только у
// ASCII, и «МАНУЛ» не нашёл бы «Манул». strings.ToLower знает весь Unicode,
// а одна и та же функция здесь и у Memory гарантирует одинаковый поиск.
//
// Отброшенные факты не ищутся: запрос — о том, что есть в выпуске.
func issueSearchText(is Issue) string {
	parts := []string{is.SciName, is.NameRu, is.Title, is.Lead}
	for _, f := range is.Facts {
		parts = append(parts, f.Text)
	}
	return strings.ToLower(strings.Join(parts, "\n"))
}

// issueNeedle — подстрока запроса в том же виде, что issueSearchText;
// пусто — фильтра нет.
func issueNeedle(q IssueQuery) string {
	return strings.ToLower(strings.TrimSpace(q.Text))
}

// issueClone — копия выпуска, не делящая срезы с исходным. Пустые срезы
// становятся nil: в SQLite они всё равно проходят через JSON, где пустой
// срез с omitempty не отличим от отсутствующего, и Memory не должна
// отдавать иначе. Монотонные показания часов снимаются — как у выборов.
func issueClone(is Issue) Issue {
	is.Realms = issueCloneSlice(is.Realms)
	is.Facts = issueCloneFacts(is.Facts)
	is.Dropped = issueCloneFacts(is.Dropped)
	is.Sources = issueCloneSlice(is.Sources)
	is.Spend = issueCloneSlice(is.Spend)
	is.Observations.ByCountry = issueCloneSlice(is.Observations.ByCountry)
	is.Observations.RecentByCountry = issueCloneSlice(is.Observations.RecentByCountry)
	is.Observations.OutOfRange = issueCloneSlice(is.Observations.OutOfRange)
	is.CreatedAt = is.CreatedAt.Round(0)
	return is
}

func issueCloneSlice[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return append([]T(nil), s...)
}

func issueCloneFacts(s []Fact) []Fact {
	s = issueCloneSlice(s)
	for i := range s {
		s[i].Sources = issueCloneSlice(s[i].Sources)
	}
	return s
}

// issueMatches — выпуск проходит фильтры запроса (кроме Limit/Offset).
// needle — issueNeedle(q), посчитанный один раз на запрос.
func issueMatches(is Issue, q IssueQuery, needle string) bool {
	if q.SpeciesID != 0 && is.SpeciesID != q.SpeciesID {
		return false
	}
	if len(q.Status) > 0 {
		found := false
		for _, s := range q.Status {
			if is.Status == s {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if !q.Since.IsZero() && is.CreatedAt.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && !is.CreatedAt.Before(q.Until) {
		return false
	}
	if needle != "" && !strings.Contains(issueSearchText(is), needle) {
		return false
	}
	return true
}
