package trivia

import (
	"fmt"
	"math"
	"time"
)

// Общие для Memory и SQLite проверки входа. Вынесены сюда, чтобы обе
// реализации отказывали одинаково: набор проверок контракта гоняет их на
// одних и тех же данных.

// Границы времени, которое хранилище умеет записать без потерь. SQLite
// держит время целым числом наносекунд Unix (см. sqlite.go), а
// time.Time.UnixNano определён только для 1678–2262 годов. Нулевой
// time.Time (год 1) — почти наверняка забытое присваивание, а не момент
// выбора, поэтому ошибка лучше молча испорченного порядка.
var (
	storeMinTime = time.Unix(0, math.MinInt64)
	storeMaxTime = time.Unix(0, math.MaxInt64)
)

// storeCheckTime — время, которое собираются сохранить, представимо.
func storeCheckTime(what string, t time.Time) error {
	if t.IsZero() {
		return fmt.Errorf("%s не задано", what)
	}
	if t.Before(storeMinTime) || t.After(storeMaxTime) {
		return fmt.Errorf("%s %s вне допустимого диапазона 1678–2262 годов", what, t.Format(time.RFC3339))
	}
	return nil
}

// storeNanos — время запроса (since, notBefore) в наносекундах Unix. В
// отличие от сохраняемого, границу запроса можно прижать к краю диапазона:
// «с нулевого времени» значит «всё», и это верно без ошибки.
func storeNanos(t time.Time) int64 {
	switch {
	case t.Before(storeMinTime):
		return math.MinInt64
	case t.After(storeMaxTime):
		return math.MaxInt64
	}
	return t.UnixNano()
}

// storeValidatePick — выбор можно сохранить.
func storeValidatePick(p Pick) error {
	if p.SpeciesID <= 0 {
		return fmt.Errorf("trivia: выбор не сохранён: неверный id вида %d", p.SpeciesID)
	}
	if err := storeCheckTime("время выбора", p.PickedAt); err != nil {
		return fmt.Errorf("trivia: выбор вида %d не сохранён: %w", p.SpeciesID, err)
	}
	return nil
}

// storeValidateCheck — проверку можно сохранить. Сетевой сбой — не свойство
// вида: закэшируй его, и вид на CheckTTL выпал бы из выбора из-за одной
// недоступности Википедии.
func storeValidateCheck(e Eligibility) error {
	if e.SpeciesID <= 0 {
		return fmt.Errorf("trivia: проверка не сохранена: неверный id вида %d", e.SpeciesID)
	}
	if e.Reason == ReasonCheckFailed {
		return fmt.Errorf("trivia: проверка вида %d не сохранена: сбой проверки (%s) не кэшируется",
			e.SpeciesID, ReasonCheckFailed)
	}
	if err := storeCheckTime("время проверки", e.CheckedAt); err != nil {
		return fmt.Errorf("trivia: проверка вида %d не сохранена: %w", e.SpeciesID, err)
	}
	return nil
}

// storeClonePick — копия выбора, не делящая срез и карту с исходным.
// Монотонные показания часов снимаются: после SQLite их всё равно нет, и
// Memory не должна вести себя иначе.
func storeClonePick(p Pick) Pick {
	if p.Rejected != nil {
		p.Rejected = append([]Rejection(nil), p.Rejected...)
	}
	if p.Weights != nil {
		w := make(WeightsReport, len(p.Weights))
		for k, v := range p.Weights {
			w[k] = v
		}
		p.Weights = w
	}
	p.PickedAt = p.PickedAt.Round(0)
	p.Eligibility.CheckedAt = p.Eligibility.CheckedAt.Round(0)
	return p
}
