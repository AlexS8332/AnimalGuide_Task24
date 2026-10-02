package schedule

import (
	"fmt"
	"math"
	"time"
)

// Общие для Memory и SQLite проверки и нормализация. Вынесены сюда, чтобы обе
// реализации отказывали одинаково: набор проверок контракта
// (schedule/runtest) гоняет их на одних и тех же данных.

// Предел выборки Runs: 0 → runsDefaultLimit, больше runsMaxLimit — обрезается.
const (
	runsDefaultLimit = 50
	runsMaxLimit     = 500
)

// abandonError — ошибка, которую Abandon ставит незавершённым запускам.
const abandonError = "демон остановлен посреди запуска"

// Границы времени, которое хранилище умеет записать без потерь: SQLite
// держит время целым числом наносекунд Unix (см. sqlite.go), а UnixNano
// определён только для 1678–2262 годов.
var (
	storeMinTime = time.Unix(0, math.MinInt64)
	storeMaxTime = time.Unix(0, math.MaxInt64)
)

// storeCheckTime — сохраняемое время представимо. Нулевое время — почти
// наверняка забытое присваивание: запуск без Started испортил бы и порядок
// журнала, и дневной расход.
func storeCheckTime(what string, t time.Time) error {
	if t.IsZero() {
		return fmt.Errorf("%s не задано", what)
	}
	if t.Before(storeMinTime) || t.After(storeMaxTime) {
		return fmt.Errorf("%s %s вне допустимого диапазона 1678–2262 годов", what, t.Format(time.RFC3339))
	}
	return nil
}

// storeNanos — граница запроса в наносекундах Unix. В отличие от
// сохраняемого времени, границу можно прижать к краю диапазона: «с нулевого
// времени» значит «всё».
func storeNanos(t time.Time) int64 {
	switch {
	case t.Before(storeMinTime):
		return math.MinInt64
	case t.After(storeMaxTime):
		return math.MaxInt64
	}
	return t.UnixNano()
}

// storeLimit — Limit выборки по правилам RunQuery.
func storeLimit(n int) int {
	switch {
	case n <= 0:
		return runsDefaultLimit
	case n > runsMaxLimit:
		return runsMaxLimit
	}
	return n
}

func storeKnownTrigger(t string) bool {
	return t == TriggerSchedule || t == TriggerCatchUp || t == TriggerManual
}

// storeFinalStatus — статус завершённого (или пропущенного) запуска.
func storeFinalStatus(s string) bool {
	return s == RunOK || s == RunFailed || s == RunBudget || s == RunSkipped
}

// storeCheckOutcome — расход можно сложить в дневной лимит. NaN отравил бы
// всю сумму Spent, а отрицательный расход «вернул» бы потраченное.
func storeCheckOutcome(o Outcome) error {
	if math.IsNaN(o.CostUSD) || math.IsInf(o.CostUSD, 0) || o.CostUSD < 0 {
		return fmt.Errorf("неверный расход %v", o.CostUSD)
	}
	return nil
}

// storeValidateStart — общее у Begin и Record: запись о запуске полна.
func storeValidateStart(what string, r Run) error {
	if r.Job == "" {
		return fmt.Errorf("schedule: %s: не указано задание", what)
	}
	if !storeKnownTrigger(r.Trigger) {
		return fmt.Errorf("schedule: %s %q: неизвестная причина запуска %q", what, r.Job, r.Trigger)
	}
	if err := storeCheckTime("слот расписания", r.Scheduled); err != nil {
		return fmt.Errorf("schedule: %s %q: %w", what, r.Job, err)
	}
	if err := storeCheckTime("время начала", r.Started); err != nil {
		return fmt.Errorf("schedule: %s %q: %w", what, r.Job, err)
	}
	return nil
}

// storeValidateBegin — начатый запуск. Статус, ID и итог входа не важны:
// Begin пишет RunRunning и выдаёт ID сам.
func storeValidateBegin(r Run) error {
	return storeValidateStart("начало запуска", r)
}

// storeValidateRecord — запуск одной записью: только итоговый статус —
// «идущий» запуск через Record никто бы не завершил.
func storeValidateRecord(r Run) error {
	if err := storeValidateStart("запись запуска", r); err != nil {
		return err
	}
	if !storeFinalStatus(r.Status) {
		return fmt.Errorf("schedule: запись запуска %q: статус %q не итоговый", r.Job, r.Status)
	}
	if !r.Finished.IsZero() {
		if err := storeCheckTime("время окончания", r.Finished); err != nil {
			return fmt.Errorf("schedule: запись запуска %q: %w", r.Job, err)
		}
	}
	if err := storeCheckOutcome(r.Outcome); err != nil {
		return fmt.Errorf("schedule: запись запуска %q: %w", r.Job, err)
	}
	return nil
}

// storeValidateFinish — итог запуска по ID.
func storeValidateFinish(r Run) error {
	if r.ID <= 0 {
		return fmt.Errorf("schedule: итог запуска: неверный ID %d", r.ID)
	}
	if !storeFinalStatus(r.Status) {
		return fmt.Errorf("schedule: итог запуска %d: статус %q не итоговый", r.ID, r.Status)
	}
	if err := storeCheckTime("время окончания", r.Finished); err != nil {
		return fmt.Errorf("schedule: итог запуска %d: %w", r.ID, err)
	}
	if err := storeCheckOutcome(r.Outcome); err != nil {
		return fmt.Errorf("schedule: итог запуска %d: %w", r.ID, err)
	}
	return nil
}

// storeTime — время в том виде, в каком его вернёт SQLite: UTC, без
// монотонных показаний. Memory приводит к нему же, чтобы реализации не
// различались ничем, кроме места хранения.
func storeTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return time.Unix(0, t.UnixNano()).UTC()
}

// storeNormalize — запуск с временем в виде storeTime.
func storeNormalize(r Run) Run {
	r.Scheduled = storeTime(r.Scheduled)
	r.Started = storeTime(r.Started)
	r.Finished = storeTime(r.Finished)
	return r
}
