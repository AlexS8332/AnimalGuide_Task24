// Package schedule — планировщик демона: задания по интервалу («выпуск раз в
// час») и по времени суток («сводка в 09:00», «проверка релиза MDD в
// 04:00»), журнал запусков в SQLite и дневной лимит расходов на модель.
//
// Правила, ради которых пакет написан, а не взят cron-строкой:
//   - одновременно идёт не больше одного запуска каждого задания;
//   - после простоя (демон был выключен) задание догоняет не больше ОДНОГО
//     пропущенного запуска, а не все 40 за ночь;
//   - задание с Paid = true не запускается, если расход за текущие сутки
//     уже достиг лимита, — запуск записывается в журнал со статусом
//     RunBudget, чтобы пропуск был виден, а не молча исчезал;
//   - время — через Clock: в тестах часы подставные, сутки проходят за
//     миллисекунды.
package schedule

import (
	"context"
	"errors"
	"time"
)

// Состояния запуска.
const (
	RunOK      = "ok"
	RunFailed  = "failed"
	RunBudget  = "budget"  // пропущен: дневной лимит исчерпан
	RunSkipped = "skipped" // пропущен по другой причине (Run вернул ErrSkip)
	RunRunning = "running" // идёт; в журнале только у незавершённого
)

// Причины запуска.
const (
	TriggerSchedule = "schedule" // по расписанию
	TriggerCatchUp  = "catch-up" // догоняет пропущенный за время простоя
	TriggerManual   = "manual"   // RunNow (инструмент run_now, кнопка «Собрать сейчас»)
)

// ErrSkip — задание решило не работать (например, проверять нечего).
var ErrSkip = errors.New("schedule: запуск пропущен")

// ErrBusy — RunNow, пока то же задание уже идёт.
var ErrBusy = errors.New("schedule: задание уже выполняется")

// ErrUnknownJob — RunNow с неизвестным именем.
var ErrUnknownJob = errors.New("schedule: нет такого задания")

// Clock — часы. Реальные — System; в тестах — подставные (пакет
// schedule/clocktest).
type Clock interface {
	Now() time.Time
	// After — канал, в который придёт время не раньше Now()+d.
	After(d time.Duration) <-chan time.Time
}

// System — настоящие часы.
var System Clock = systemClock{}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type runKey struct{}

// WithRun кладёт в контекст запись о запуске: задание узнаёт, почему его
// запустили (Trigger) и какой слот (Scheduled). Планировщик делает это сам.
func WithRun(ctx context.Context, r Run) context.Context { return context.WithValue(ctx, runKey{}, r) }

// CurrentRun — запуск из контекста; ok=false — задание вызвано не
// планировщиком (тест, разовый вызов кодом).
func CurrentRun(ctx context.Context) (Run, bool) {
	r, ok := ctx.Value(runKey{}).(Run)
	return r, ok
}

// Outcome — что задание сообщает о запуске.
type Outcome struct {
	// CostUSD — расход на модель; из него считается дневной лимит.
	CostUSD float64 `json:"cost_usd"`
	// Ref — на что ссылается запуск: «issue:42», «summary:7», «mdd:v2.5».
	Ref string `json:"ref,omitempty"`
	// Detail — строка для людей: «Манул, 5 фактов», «релиз не менялся».
	Detail string `json:"detail,omitempty"`
}

// Job — задание. Ровно одно из Every и Daily задано.
type Job struct {
	Name string // «issue», «summary», «mdd»
	// Every — интервал: следующий запуск через Every после начала
	// предыдущего запланированного.
	Every time.Duration
	// Daily — время суток «HH:MM» в Location планировщика.
	Daily string
	// Paid — задание тратит деньги на модель и подчиняется лимиту.
	Paid bool
	// Timeout — предел одного запуска; 0 — 10 минут.
	Timeout time.Duration
	Run     func(ctx context.Context) (Outcome, error)
}

// Run — запись журнала о запуске.
type Run struct {
	ID        int64     `json:"id"`
	Job       string    `json:"job"`
	Trigger   string    `json:"trigger"`   // Trigger*
	Scheduled time.Time `json:"scheduled"` // слот расписания (для manual — время нажатия)
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished,omitzero"`
	Status    string    `json:"status"` // Run*
	Error     string    `json:"error,omitempty"`
	Outcome
}

// RunQuery — выборка журнала. Пустые поля не ограничивают.
type RunQuery struct {
	Job    string
	Status []string
	Since  time.Time // Started ≥ Since
	Until  time.Time // Started < Until (нулевое — без границы)
	Limit  int       // 0 → 50, предел 500
}

// RunStore — журнал запусков (компонент миграций "schedule", таблица
// schedule_run). Реализации: SQLite и Memory.
type RunStore interface {
	// Begin записывает начатый запуск (Status = RunRunning) и возвращает ID.
	Begin(ctx context.Context, r Run) (int64, error)
	// Finish дописывает итог запуска по ID (Finished, Status, Error, Outcome).
	Finish(ctx context.Context, r Run) error
	// Record — запуск целиком одной записью (пропуски RunBudget/RunSkipped).
	Record(ctx context.Context, r Run) (int64, error)
	// Runs — журнал, новые первыми (Started, затем ID убыв.).
	Runs(ctx context.Context, q RunQuery) ([]Run, error)
	// Last — последний запуск задания с любым статусом, кроме RunBudget и
	// RunSkipped (от него считается следующий слот); ok=false — не было.
	Last(ctx context.Context, job string) (r Run, ok bool, err error)
	// Spent — сумма CostUSD запусков с Started в [since, until).
	Spent(ctx context.Context, since, until time.Time) (float64, error)
	// Abandon помечает все RunRunning как RunFailed с ошибкой «демон
	// остановлен посреди запуска»: зовётся при старте.
	Abandon(ctx context.Context, at time.Time) (int, error)
}

// JobStatus — состояние задания для schedule_status и интерфейса.
type JobStatus struct {
	Name    string    `json:"name"`
	Every   string    `json:"every,omitempty"` // «1h0m0s»
	Daily   string    `json:"daily,omitempty"`
	Paid    bool      `json:"paid"`
	Running bool      `json:"running"`
	Next    time.Time `json:"next,omitzero"`
	Last    *Run      `json:"last,omitempty"`
}

// Status — снимок планировщика.
type Status struct {
	Now      time.Time   `json:"now"`
	Location string      `json:"location"`
	Budget   float64     `json:"budget_usd"`      // лимит на сутки; 0 — без лимита
	Spent    float64     `json:"spent_today_usd"` // за текущие сутки в Location
	Jobs     []JobStatus `json:"jobs"`
}

// Options — настройки планировщика.
type Options struct {
	Clock    Clock          // nil — System
	Location *time.Location // сутки лимита и Daily; nil — time.Local
	Budget   float64        // долларов в сутки; 0 — без лимита
	// OnRun — уведомление о завершённом (или пропущенном) запуске: демон
	// пишет журнал процесса, интерфейс обновляет ленту. Может быть nil.
	OnRun func(Run)
}

// Scheduler — реализация в scheduler.go:
//
//	func New(store RunStore, jobs []Job, o Options) (*Scheduler, error)
//	func (s *Scheduler) Run(ctx context.Context) error    // до отмены ctx
//	func (s *Scheduler) RunNow(ctx context.Context, job string) (Run, error) // ждёт завершения
//	func (s *Scheduler) Status(ctx context.Context) (Status, error)
