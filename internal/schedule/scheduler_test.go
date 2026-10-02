package schedule_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	_ "time/tzdata" // America/New_York на Windows без системной базы зон

	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule/clocktest"
)

// Все тесты — на подставных часах и журнале в памяти: время идёт только по
// Advance, а синхронизация — через BlockUntil(1) («цикл Run снова ждёт на
// часах») и OnRun. Слоты, пропуски и Begin цикл пишет синхронно, так что
// после BlockUntil журнал уже содержит всё решённое в такте.

var (
	msk = time.FixedZone("MSK", 3*60*60)
	t0  = time.Date(2026, 9, 24, 12, 0, 0, 0, msk)
)

// guard — предел ожидания события: не sleep, а страховка от зависания.
const guard = 5 * time.Second

type harness struct {
	t      *testing.T
	clock  *clocktest.Fake
	store  *schedule.Memory
	s      *schedule.Scheduler
	events chan schedule.Run
	cancel context.CancelFunc
	done   chan error
}

func newHarness(t *testing.T, start time.Time, st *schedule.Memory, o schedule.Options, jobs ...schedule.Job) *harness {
	t.Helper()
	if st == nil {
		st = schedule.NewMemory()
	}
	h := &harness{t: t, clock: clocktest.NewFake(start), store: st, events: make(chan schedule.Run, 1000)}
	o.Clock = h.clock
	if o.Location == nil {
		o.Location = msk
	}
	o.OnRun = func(r schedule.Run) { h.events <- r }
	s, err := schedule.New(st, jobs, o)
	if err != nil {
		t.Fatal(err)
	}
	h.s = s
	return h
}

// start запускает Run и ждёт, пока цикл отработает первый такт.
func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- h.s.Run(ctx) }()
	h.t.Cleanup(h.stop)
	h.clock.BlockUntil(1)
}

// stop отменяет Run и ждёт его возврата.
func (h *harness) stop() {
	h.t.Helper()
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("Run: %v", err)
		}
	case <-time.After(guard):
		h.t.Fatal("Run не вернулся после отмены")
	}
}

// advance двигает часы и ждёт, пока цикл отработает такт и снова заснёт.
func (h *harness) advance(d time.Duration) {
	h.clock.Advance(d)
	h.clock.BlockUntil(1)
}

func (h *harness) event() schedule.Run {
	h.t.Helper()
	select {
	case r := <-h.events:
		return r
	case <-time.After(guard):
		h.t.Fatal("нет события OnRun")
	}
	return schedule.Run{}
}

// eventsByJob — n событий, по имени задания.
func (h *harness) eventsByJob(n int) map[string]schedule.Run {
	h.t.Helper()
	m := map[string]schedule.Run{}
	for range n {
		r := h.event()
		m[r.Job] = r
	}
	return m
}

func (h *harness) noEvent() {
	h.t.Helper()
	select {
	case r := <-h.events:
		h.t.Errorf("лишнее событие: %+v", r)
	default:
	}
}

// runs — журнал задания, новые первыми.
func (h *harness) runs(job string) []schedule.Run {
	h.t.Helper()
	list, err := h.store.Runs(context.Background(), schedule.RunQuery{Job: job, Limit: 500})
	if err != nil {
		h.t.Fatal(err)
	}
	return list
}

func (h *harness) status() schedule.Status {
	h.t.Helper()
	st, err := h.s.Status(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func (h *harness) next(job string) time.Time {
	h.t.Helper()
	for _, j := range h.status().Jobs {
		if j.Name == job {
			return j.Next
		}
	}
	h.t.Fatalf("нет задания %q в Status", job)
	return time.Time{}
}

// seed — запуск в журнале до старта (прошлая жизнь демона).
func seed(t *testing.T, st *schedule.Memory, job string, scheduled time.Time, status string) {
	t.Helper()
	if _, err := st.Record(context.Background(), schedule.Run{Job: job, Trigger: schedule.TriggerSchedule,
		Scheduled: scheduled, Started: scheduled, Finished: scheduled.Add(time.Minute), Status: status}); err != nil {
		t.Fatal(err)
	}
}

func okJob(out schedule.Outcome) func(context.Context) (schedule.Outcome, error) {
	return func(context.Context) (schedule.Outcome, error) { return out, nil }
}

// gate — задание, которое ждёт release (или отмены) и сообщает о старте.
type gate struct {
	started chan struct{}
	release chan struct{}
}

func newGate() *gate {
	return &gate{started: make(chan struct{}, 100), release: make(chan struct{}, 100)}
}

func (g *gate) run(ctx context.Context) (schedule.Outcome, error) {
	g.started <- struct{}{}
	select {
	case <-g.release:
		return schedule.Outcome{Detail: "готово"}, nil
	case <-ctx.Done():
		return schedule.Outcome{}, ctx.Err()
	}
}

func (g *gate) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(guard):
		t.Fatal("задание не стартовало")
	}
}

func checkRun(t *testing.T, r schedule.Run, trigger, status string, scheduled time.Time) {
	t.Helper()
	if r.Trigger != trigger || r.Status != status || !r.Scheduled.Equal(scheduled) {
		t.Errorf("запуск %s: trigger=%s status=%s scheduled=%v; ждали %s %s %v (%+v)",
			r.Job, r.Trigger, r.Status, r.Scheduled.In(msk), trigger, status, scheduled.In(msk), r)
	}
}

func TestNewValidation(t *testing.T) {
	run := okJob(schedule.Outcome{})
	for name, jobs := range map[string][]schedule.Job{
		"без имени":          {{Every: time.Hour, Run: run}},
		"имя с пробелами":    {{Name: " issue", Every: time.Hour, Run: run}},
		"дубль":              {{Name: "a", Every: time.Hour, Run: run}, {Name: "a", Daily: "09:00", Run: run}},
		"и Every, и Daily":   {{Name: "a", Every: time.Hour, Daily: "09:00", Run: run}},
		"ни Every, ни Daily": {{Name: "a", Run: run}},
		"Every < 0":          {{Name: "a", Every: -time.Hour, Run: run}},
		"Daily 9:00":         {{Name: "a", Daily: "9:00", Run: run}},
		"Daily 24:00":        {{Name: "a", Daily: "24:00", Run: run}},
		"Daily 09:60":        {{Name: "a", Daily: "09:60", Run: run}},
		"Daily 09-00":        {{Name: "a", Daily: "09-00", Run: run}},
		"Daily с секундами":  {{Name: "a", Daily: "09:00:00", Run: run}},
		"без Run":            {{Name: "a", Every: time.Hour}},
		"Timeout < 0":        {{Name: "a", Every: time.Hour, Run: run, Timeout: -1}},
	} {
		if _, err := schedule.New(schedule.NewMemory(), jobs, schedule.Options{}); err == nil {
			t.Errorf("%s: нет ошибки", name)
		}
	}
	if _, err := schedule.New(nil, nil, schedule.Options{}); err == nil {
		t.Error("без журнала: нет ошибки")
	}
	if _, err := schedule.New(schedule.NewMemory(), nil, schedule.Options{Budget: -1}); err == nil {
		t.Error("отрицательный лимит: нет ошибки")
	}
	good := []schedule.Job{
		{Name: "issue", Every: time.Hour, Paid: true, Run: run},
		{Name: "summary", Daily: "09:00", Run: run},
		{Name: "mdd", Daily: "23:59", Run: run},
		{Name: "night", Daily: "00:00", Run: run},
	}
	if _, err := schedule.New(schedule.NewMemory(), good, schedule.Options{Budget: 0.5}); err != nil {
		t.Errorf("верные задания: %v", err)
	}
}

// Every: первый старт без журнала — выпуск сразу, дальше по сетке от него.
func TestEveryFirstStartAndGrid(t *testing.T) {
	h := newHarness(t, t0, nil, schedule.Options{},
		schedule.Job{Name: "issue", Every: time.Hour, Run: okJob(schedule.Outcome{Detail: "Манул"})})
	h.start()
	r := h.event()
	checkRun(t, r, schedule.TriggerSchedule, schedule.RunOK, t0)
	if r.ID <= 0 || r.Detail != "Манул" || !r.Started.Equal(t0) {
		t.Errorf("первый запуск: %+v", r)
	}
	if n := h.next("issue"); !n.Equal(t0.Add(time.Hour)) {
		t.Errorf("Next %v, ждали %v", n, t0.Add(time.Hour))
	}

	h.advance(59 * time.Minute)
	if n := len(h.runs("issue")); n != 1 {
		t.Errorf("до слота: %d запусков", n)
	}
	h.advance(time.Minute)
	checkRun(t, h.event(), schedule.TriggerSchedule, schedule.RunOK, t0.Add(time.Hour))
	h.advance(time.Hour)
	checkRun(t, h.event(), schedule.TriggerSchedule, schedule.RunOK, t0.Add(2*time.Hour))
	h.noEvent()
	if n := len(h.runs("issue")); n != 3 {
		t.Errorf("за два часа: %d запусков, ждали 3", n)
	}
}

// Daily: первый старт в 03:00 ждёт 09:00 сегодня, а не догоняет «вчерашние
// 09:00»; с журналом за вчера — тоже без догона.
func TestDailyFirstStart(t *testing.T) {
	start := time.Date(2026, 9, 24, 3, 0, 0, 0, msk)
	nine := time.Date(2026, 9, 24, 9, 0, 0, 0, msk)
	for _, withHistory := range []bool{false, true} {
		t.Run(fmt.Sprint("журнал=", withHistory), func(t *testing.T) {
			st := schedule.NewMemory()
			if withHistory {
				seed(t, st, "summary", nine.AddDate(0, 0, -1), schedule.RunOK)
			}
			h := newHarness(t, start, st, schedule.Options{},
				schedule.Job{Name: "summary", Daily: "09:00", Run: okJob(schedule.Outcome{})})
			if n := h.next("summary"); !n.Equal(nine) {
				t.Errorf("Next до Run: %v, ждали %v", n, nine)
			}
			h.start()
			h.noEvent()
			if n := h.next("summary"); !n.Equal(nine) {
				t.Errorf("Next: %v, ждали %v", n, nine)
			}
			h.advance(6*time.Hour - time.Second)
			h.noEvent()
			h.advance(time.Second)
			checkRun(t, h.event(), schedule.TriggerSchedule, schedule.RunOK, nine)
			if n := h.next("summary"); !n.Equal(nine.AddDate(0, 0, 1)) {
				t.Errorf("Next после запуска: %v", n)
			}
		})
	}
}

// Догон: из четырёх пропущенных слотов — один запуск за последний, и
// никакого второго немедленного после него.
func TestCatchUpEvery(t *testing.T) {
	st := schedule.NewMemory()
	ten := time.Date(2026, 9, 24, 10, 0, 0, 0, msk)
	seed(t, st, "issue", ten, schedule.RunOK)
	seed(t, st, "issue", ten.Add(time.Hour), schedule.RunBudget) // пропуски сетку не двигают
	start := time.Date(2026, 9, 24, 14, 25, 0, 0, msk)
	h := newHarness(t, start, st, schedule.Options{},
		schedule.Job{Name: "issue", Every: time.Hour, Run: okJob(schedule.Outcome{})})
	h.start()

	r := h.event()
	checkRun(t, r, schedule.TriggerCatchUp, schedule.RunOK, ten.Add(4*time.Hour))
	if !r.Started.Equal(start) {
		t.Errorf("Started %v, ждали %v", r.Started, start)
	}
	h.noEvent()
	if n := len(h.runs("issue")); n != 3 {
		t.Errorf("журнал: %d записей, ждали 3 (две старые и один догон)", n)
	}
	fifteen := ten.Add(5 * time.Hour)
	if n := h.next("issue"); !n.Equal(fifteen) {
		t.Errorf("Next после догона: %v, ждали %v", n, fifteen)
	}
	h.advance(34 * time.Minute)
	if n := len(h.runs("issue")); n != 3 {
		t.Errorf("до 15:00: %d записей", n)
	}
	h.advance(time.Minute)
	checkRun(t, h.event(), schedule.TriggerSchedule, schedule.RunOK, fifteen)
}

func TestCatchUpDaily(t *testing.T) {
	st := schedule.NewMemory()
	nine := time.Date(2026, 9, 24, 9, 0, 0, 0, msk)
	seed(t, st, "summary", nine.AddDate(0, 0, -3), schedule.RunOK)
	h := newHarness(t, nine.Add(time.Hour), st, schedule.Options{},
		schedule.Job{Name: "summary", Daily: "09:00", Run: okJob(schedule.Outcome{})})
	h.start()
	checkRun(t, h.event(), schedule.TriggerCatchUp, schedule.RunOK, nine)
	h.noEvent()
	if n := h.next("summary"); !n.Equal(nine.AddDate(0, 0, 1)) {
		t.Errorf("Next: %v", n)
	}
	h.advance(23*time.Hour - time.Second)
	h.noEvent()
	h.advance(time.Second)
	checkRun(t, h.event(), schedule.TriggerSchedule, schedule.RunOK, nine.AddDate(0, 0, 1))
}

// Слот, наступивший ровно в момент старта, — вовремя, не догон.
func TestStartExactlyAtSlot(t *testing.T) {
	st := schedule.NewMemory()
	seed(t, st, "issue", t0.Add(-time.Hour), schedule.RunOK)
	h := newHarness(t, t0, st, schedule.Options{},
		schedule.Job{Name: "issue", Every: time.Hour, Run: okJob(schedule.Outcome{})})
	h.start()
	checkRun(t, h.event(), schedule.TriggerSchedule, schedule.RunOK, t0)
}

// Машина проспала несколько слотов на ходу — тоже один догон.
func TestCatchUpAfterSleep(t *testing.T) {
	h := newHarness(t, t0, nil, schedule.Options{},
		schedule.Job{Name: "issue", Every: time.Hour, Run: okJob(schedule.Outcome{})})
	h.start()
	h.event()
	h.advance(3*time.Hour + 30*time.Minute)
	checkRun(t, h.event(), schedule.TriggerCatchUp, schedule.RunOK, t0.Add(3*time.Hour))
	h.noEvent()
	if n := h.next("issue"); !n.Equal(t0.Add(4 * time.Hour)) {
		t.Errorf("Next: %v", n)
	}
}

// Лимит: два платных по $0.30 при лимите $0.50 → третий budget; наутро
// снова можно; неплатное задание идёт всё время.
func TestBudget(t *testing.T) {
	start := time.Date(2026, 9, 24, 21, 0, 0, 0, msk)
	var paidCalls atomic.Int32
	h := newHarness(t, start, nil, schedule.Options{Budget: 0.5},
		schedule.Job{Name: "issue", Every: time.Hour, Paid: true, Run: func(context.Context) (schedule.Outcome, error) {
			paidCalls.Add(1)
			return schedule.Outcome{CostUSD: 0.3}, nil
		}},
		schedule.Job{Name: "ping", Every: time.Hour, Run: okJob(schedule.Outcome{})})
	h.start()

	ev := h.eventsByJob(2) // 21:00
	checkRun(t, ev["issue"], schedule.TriggerSchedule, schedule.RunOK, start)
	h.advance(time.Hour) // 22:00: потрачено 0.30 < 0.50
	ev = h.eventsByJob(2)
	checkRun(t, ev["issue"], schedule.TriggerSchedule, schedule.RunOK, start.Add(time.Hour))

	h.advance(time.Hour) // 23:00: потрачено 0.60
	ev = h.eventsByJob(2)
	b := ev["issue"]
	checkRun(t, b, schedule.TriggerSchedule, schedule.RunBudget, start.Add(2*time.Hour))
	if b.Detail != "потрачено $0.6000 из $0.5" || b.ID <= 0 {
		t.Errorf("пропуск по лимиту: %+v", b)
	}
	checkRun(t, ev["ping"], schedule.TriggerSchedule, schedule.RunOK, start.Add(2*time.Hour))
	if n := paidCalls.Load(); n != 2 {
		t.Errorf("платное задание вызвано %d раз, ждали 2", n)
	}
	st := h.status()
	if st.Spent < 0.6-1e-9 || st.Spent > 0.6+1e-9 || st.Budget != 0.5 {
		t.Errorf("Status: spent %v budget %v", st.Spent, st.Budget)
	}

	h.advance(time.Hour) // 00:00 следующих суток: расход с нуля
	ev = h.eventsByJob(2)
	checkRun(t, ev["issue"], schedule.TriggerSchedule, schedule.RunOK, start.Add(3*time.Hour))
	if n := paidCalls.Load(); n != 3 {
		t.Errorf("наутро платное вызвано %d раз всего, ждали 3", n)
	}
	// Ручной запуск тоже платный: 0.30 уже есть, ещё 0.30 можно, потом нет.
	r, err := h.s.RunNow(context.Background(), "issue")
	if err != nil || r.Status != schedule.RunOK {
		t.Fatalf("RunNow: %+v, %v", r, err)
	}
	h.event()
	r, err = h.s.RunNow(context.Background(), "issue")
	if err != nil || r.Status != schedule.RunBudget || r.Trigger != schedule.TriggerManual {
		t.Errorf("RunNow сверх лимита: %+v, %v", r, err)
	}
	h.event()
	if n := paidCalls.Load(); n != 4 {
		t.Errorf("всего вызовов %d, ждали 4", n)
	}
}

// Лимит 0 — без лимита.
func TestNoBudget(t *testing.T) {
	st := schedule.NewMemory()
	h := newHarness(t, t0, st, schedule.Options{},
		schedule.Job{Name: "issue", Every: time.Hour, Paid: true, Run: okJob(schedule.Outcome{CostUSD: 100})})
	for range 3 {
		if r, err := h.s.RunNow(context.Background(), "issue"); err != nil || r.Status != schedule.RunOK {
			t.Fatalf("RunNow: %+v, %v", r, err)
		}
	}
}

// Разные задания идут одновременно.
func TestParallelJobs(t *testing.T) {
	a, b := newGate(), newGate()
	h := newHarness(t, t0, nil, schedule.Options{},
		schedule.Job{Name: "a", Every: time.Hour, Run: a.run},
		schedule.Job{Name: "b", Every: time.Hour, Run: b.run})
	h.start()
	a.waitStarted(t)
	b.waitStarted(t) // b стартовал, пока a ещё идёт
	st := h.status()
	for _, j := range st.Jobs {
		if !j.Running {
			t.Errorf("%s: Running=false", j.Name)
		}
	}
	a.release <- struct{}{}
	b.release <- struct{}{}
	ev := h.eventsByJob(2)
	if ev["a"].Status != schedule.RunOK || ev["b"].Status != schedule.RunOK {
		t.Errorf("итоги: %+v", ev)
	}
}

// Слот при идущем долгом запуске пропускается, без очереди.
func TestSkipWhileRunning(t *testing.T) {
	g := newGate()
	h := newHarness(t, t0, nil, schedule.Options{},
		schedule.Job{Name: "issue", Every: time.Hour, Run: g.run})
	h.start()
	g.waitStarted(t)

	h.advance(time.Hour)
	s := h.event()
	checkRun(t, s, schedule.TriggerSchedule, schedule.RunSkipped, t0.Add(time.Hour))
	if s.Error != "предыдущий запуск ещё идёт" || s.ID <= 0 {
		t.Errorf("пропуск: %+v", s)
	}
	g.release <- struct{}{}
	checkRun(t, h.event(), schedule.TriggerSchedule, schedule.RunOK, t0)

	// Очереди нет: освобождение не запускает пропущенный слот.
	h.noEvent()
	if n := len(h.runs("issue")); n != 2 {
		t.Errorf("журнал: %d записей, ждали 2", n)
	}
	g.release <- struct{}{} // следующий запуск пройдёт сразу
	h.advance(time.Hour)
	checkRun(t, h.event(), schedule.TriggerSchedule, schedule.RunOK, t0.Add(2*time.Hour))
}

func TestRunNow(t *testing.T) {
	ctx := context.Background()
	t.Run("успех и сдвиг расписания", func(t *testing.T) {
		h := newHarness(t, t0, nil, schedule.Options{},
			schedule.Job{Name: "issue", Every: time.Hour, Run: okJob(schedule.Outcome{Ref: "issue:1"})})
		h.start()
		h.event() // первый выпуск в t0
		h.advance(30 * time.Minute)
		r, err := h.s.RunNow(ctx, "issue")
		if err != nil {
			t.Fatal(err)
		}
		checkRun(t, r, schedule.TriggerManual, schedule.RunOK, t0.Add(30*time.Minute))
		if r.ID <= 0 || r.Ref != "issue:1" || r.Finished.IsZero() {
			t.Errorf("RunNow: %+v", r)
		}
		if e := h.event(); e.ID != r.ID {
			t.Errorf("OnRun: %+v", e)
		}
		// Прежний слот t0+1h снят: следующий — через час после нажатия.
		if n := h.next("issue"); !n.Equal(t0.Add(90 * time.Minute)) {
			t.Errorf("Next после RunNow: %v", n)
		}
		h.advance(30 * time.Minute)
		h.noEvent()
		if n := len(h.runs("issue")); n != 2 {
			t.Errorf("в t0+1h: %d записей, ждали 2", n)
		}
		h.advance(30 * time.Minute)
		checkRun(t, h.event(), schedule.TriggerSchedule, schedule.RunOK, t0.Add(90*time.Minute))
	})

	t.Run("без Run", func(t *testing.T) {
		h := newHarness(t, t0, nil, schedule.Options{},
			schedule.Job{Name: "issue", Every: time.Hour, Run: okJob(schedule.Outcome{})})
		r, err := h.s.RunNow(ctx, "issue")
		if err != nil || r.Status != schedule.RunOK {
			t.Fatalf("RunNow: %+v, %v", r, err)
		}
		// Run потом считает сетку от ручного запуска, как и после перезапуска.
		h.start()
		h.event()
		h.noEvent()
		if n := h.next("issue"); !n.Equal(t0.Add(time.Hour)) {
			t.Errorf("Next: %v", n)
		}
	})

	t.Run("busy", func(t *testing.T) {
		g := newGate()
		h := newHarness(t, t0, nil, schedule.Options{},
			schedule.Job{Name: "issue", Every: time.Hour, Run: g.run})
		h.start()
		g.waitStarted(t)
		if _, err := h.s.RunNow(ctx, "issue"); !errors.Is(err, schedule.ErrBusy) {
			t.Errorf("RunNow при идущем: %v", err)
		}
		g.release <- struct{}{}
		h.event()
	})

	t.Run("неизвестное", func(t *testing.T) {
		h := newHarness(t, t0, nil, schedule.Options{},
			schedule.Job{Name: "issue", Every: time.Hour, Run: okJob(schedule.Outcome{})})
		if _, err := h.s.RunNow(ctx, "нет"); !errors.Is(err, schedule.ErrUnknownJob) {
			t.Errorf("RunNow неизвестного: %v", err)
		}
	})

	t.Run("лимит", func(t *testing.T) {
		st := schedule.NewMemory()
		if _, err := st.Record(ctx, schedule.Run{Job: "issue", Trigger: schedule.TriggerSchedule,
			Scheduled: t0.Add(-time.Hour), Started: t0.Add(-time.Hour), Status: schedule.RunOK,
			Outcome: schedule.Outcome{CostUSD: 0.6}}); err != nil {
			t.Fatal(err)
		}
		var calls atomic.Int32
		h := newHarness(t, t0, st, schedule.Options{Budget: 0.5},
			schedule.Job{Name: "issue", Every: time.Hour, Paid: true, Run: func(context.Context) (schedule.Outcome, error) {
				calls.Add(1)
				return schedule.Outcome{}, nil
			}})
		r, err := h.s.RunNow(ctx, "issue")
		if err != nil || r.Status != schedule.RunBudget || r.ID <= 0 || calls.Load() != 0 {
			t.Errorf("RunNow сверх лимита: %+v, %v, вызовов %d", r, err, calls.Load())
		}
		// Пропуск по лимиту сетку не сдвигает: Next — по журналу.
		if n := h.next("issue"); !n.Equal(t0) {
			t.Errorf("Next: %v, ждали %v", n, t0)
		}
	})
}

// Задание, упавшее после вызова модели, деньги потратило — они в журнале.
func TestOutcomeOnError(t *testing.T) {
	h := newHarness(t, t0, nil, schedule.Options{Budget: 10},
		schedule.Job{Name: "issue", Every: time.Hour, Paid: true, Run: func(context.Context) (schedule.Outcome, error) {
			return schedule.Outcome{CostUSD: 0.2, Ref: "issue:7", Detail: "черновик"}, errors.New("проверка не прошла")
		}})
	r, err := h.s.RunNow(context.Background(), "issue")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != schedule.RunFailed || r.Error != "проверка не прошла" || r.CostUSD != 0.2 || r.Ref != "issue:7" {
		t.Errorf("RunNow: %+v", r)
	}
	got := h.runs("issue")[0]
	if got.Status != schedule.RunFailed || got.CostUSD != 0.2 || got.Error != "проверка не прошла" {
		t.Errorf("журнал: %+v", got)
	}
	if sp := h.status().Spent; sp != 0.2 {
		t.Errorf("Spent: %v", sp)
	}
}

// Паника в задании — failed, демон работает дальше.
func TestPanic(t *testing.T) {
	var n atomic.Int32
	h := newHarness(t, t0, nil, schedule.Options{},
		schedule.Job{Name: "issue", Every: time.Hour, Run: func(context.Context) (schedule.Outcome, error) {
			if n.Add(1) == 1 {
				panic("бум")
			}
			return schedule.Outcome{}, nil
		}})
	h.start()
	r := h.event()
	if r.Status != schedule.RunFailed || !strings.Contains(r.Error, "паника") || !strings.Contains(r.Error, "бум") {
		t.Errorf("после паники: %+v", r)
	}
	h.advance(time.Hour)
	checkRun(t, h.event(), schedule.TriggerSchedule, schedule.RunOK, t0.Add(time.Hour))
	if r, err := h.s.RunNow(context.Background(), "issue"); err != nil || r.Status != schedule.RunOK {
		t.Errorf("RunNow после паники: %+v, %v", r, err)
	}
}

func TestErrSkip(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		err       error
		wantError string
	}{
		{schedule.ErrSkip, ""},
		{fmt.Errorf("релиз не менялся: %w", schedule.ErrSkip), "релиз не менялся: schedule: запуск пропущен"},
	} {
		h := newHarness(t, t0, nil, schedule.Options{},
			schedule.Job{Name: "mdd", Daily: "04:00", Run: func(context.Context) (schedule.Outcome, error) {
				return schedule.Outcome{Detail: "v2.5"}, tc.err
			}})
		r, err := h.s.RunNow(ctx, "mdd")
		if err != nil || r.Status != schedule.RunSkipped || r.Error != tc.wantError || r.Detail != "v2.5" {
			t.Errorf("ErrSkip (%v): %+v, %v", tc.err, r, err)
		}
		if _, ok, _ := h.store.Last(ctx, "mdd"); ok {
			t.Error("Last видит пропущенный запуск")
		}
	}
}

// Таймаут — по настоящим часам (срок виден в ctx.Deadline), поэтому
// предел маленький; ожидание — на ctx.Done задания, не sleep.
func TestTimeout(t *testing.T) {
	var hadDeadline atomic.Bool
	h := newHarness(t, t0, nil, schedule.Options{},
		schedule.Job{Name: "issue", Every: time.Hour, Timeout: 20 * time.Millisecond,
			Run: func(ctx context.Context) (schedule.Outcome, error) {
				_, ok := ctx.Deadline()
				hadDeadline.Store(ok)
				<-ctx.Done()
				return schedule.Outcome{CostUSD: 0.01}, ctx.Err()
			}})
	r, err := h.s.RunNow(context.Background(), "issue")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != schedule.RunFailed || !strings.Contains(r.Error, "превышен предел 20ms") || r.CostUSD != 0.01 {
		t.Errorf("таймаут: %+v", r)
	}
	if !hadDeadline.Load() {
		t.Error("у ctx задания нет Deadline")
	}
}

// Без Timeout предел — 10 минут, и он тоже виден в ctx.
func TestDefaultTimeout(t *testing.T) {
	var left time.Duration
	h := newHarness(t, t0, nil, schedule.Options{},
		schedule.Job{Name: "issue", Every: time.Hour, Run: func(ctx context.Context) (schedule.Outcome, error) {
			d, _ := ctx.Deadline()
			left = time.Until(d)
			return schedule.Outcome{}, nil
		}})
	if _, err := h.s.RunNow(context.Background(), "issue"); err != nil {
		t.Fatal(err)
	}
	if left <= 9*time.Minute || left > 10*time.Minute {
		t.Errorf("до срока %v, ждали около 10 минут", left)
	}
}

// Запуски, брошенные прошлым процессом, при старте помечаются failed.
func TestAbandonOnStart(t *testing.T) {
	ctx := context.Background()
	st := schedule.NewMemory()
	id, err := st.Begin(ctx, schedule.Run{Job: "summary", Trigger: schedule.TriggerSchedule,
		Scheduled: t0.Add(-time.Hour), Started: t0.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, t0, st, schedule.Options{},
		schedule.Job{Name: "summary", Daily: "09:00", Run: okJob(schedule.Outcome{})})
	h.start()
	got := h.runs("summary")
	if len(got) != 1 || got[0].ID != id || got[0].Status != schedule.RunFailed ||
		got[0].Error == "" || !got[0].Finished.Equal(t0) {
		t.Errorf("после старта: %+v", got)
	}
}

// Отмена ctx: Run возвращается после того, как идущие запуски (плановый и
// ручной) получили отмену и записали итог; вечного running нет.
func TestCancel(t *testing.T) {
	var (
		sched  = newGate()
		manual = newGate()
		ended  atomic.Int32
	)
	wrap := func(g *gate) func(context.Context) (schedule.Outcome, error) {
		return func(ctx context.Context) (schedule.Outcome, error) {
			out, err := g.run(ctx)
			ended.Add(1)
			return out, err
		}
	}
	h := newHarness(t, t0, nil, schedule.Options{},
		schedule.Job{Name: "issue", Every: time.Hour, Run: wrap(sched)},
		schedule.Job{Name: "summary", Daily: "09:00", Run: wrap(manual)})
	h.start()
	sched.waitStarted(t)

	type result struct {
		r   schedule.Run
		err error
	}
	manualDone := make(chan result, 1)
	go func() {
		r, err := h.s.RunNow(context.Background(), "summary")
		manualDone <- result{r, err}
	}()
	manual.waitStarted(t)

	h.stop()
	if n := ended.Load(); n != 2 {
		t.Errorf("к возврату Run завершилось %d запусков, ждали 2", n)
	}
	for _, job := range []string{"issue", "summary"} {
		r := h.runs(job)[0]
		if r.Status != schedule.RunFailed || !strings.HasPrefix(r.Error, "демон остановлен") || r.Finished.IsZero() {
			t.Errorf("%s после остановки: %+v", job, r)
		}
	}
	res := <-manualDone
	if res.err != nil || res.r.Status != schedule.RunFailed || !strings.HasPrefix(res.r.Error, "демон остановлен") {
		t.Errorf("RunNow при остановке: %+v, %v", res.r, res.err)
	}
}

// Отмена ctx самого RunNow (не демона) — «запуск отменён».
func TestRunNowCallerCancel(t *testing.T) {
	g := newGate()
	h := newHarness(t, t0, nil, schedule.Options{},
		schedule.Job{Name: "summary", Daily: "09:00", Run: g.run})
	h.start()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan schedule.Run, 1)
	go func() {
		r, _ := h.s.RunNow(ctx, "summary")
		done <- r
	}()
	g.waitStarted(t)
	cancel()
	r := <-done
	if r.Status != schedule.RunFailed || !strings.HasPrefix(r.Error, "запуск отменён") {
		t.Errorf("RunNow с отменой: %+v", r)
	}
}

func TestRunTwice(t *testing.T) {
	h := newHarness(t, t0, nil, schedule.Options{},
		schedule.Job{Name: "summary", Daily: "09:00", Run: okJob(schedule.Outcome{})})
	h.start()
	if err := h.s.Run(context.Background()); err == nil {
		t.Error("второй Run: нет ошибки")
	}
}

// Daily через переход на летнее время: 09:00 и назавтра 09:00 по местному
// времени, между ними 23 часа.
func TestDailyDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 8 марта 2026 в 02:00 часы в Нью-Йорке переводятся на 03:00.
	start := time.Date(2026, 3, 7, 8, 0, 0, 0, ny)
	h := newHarness(t, start, nil, schedule.Options{Location: ny},
		schedule.Job{Name: "summary", Daily: "09:00", Run: okJob(schedule.Outcome{})})
	h.start()
	h.advance(time.Hour)
	first := time.Date(2026, 3, 7, 9, 0, 0, 0, ny)
	checkRun(t, h.event(), schedule.TriggerSchedule, schedule.RunOK, first)

	second := time.Date(2026, 3, 8, 9, 0, 0, 0, ny)
	if d := second.Sub(first); d != 23*time.Hour {
		t.Fatalf("между слотами %v — зона без перехода?", d)
	}
	if n := h.next("summary"); !n.Equal(second) {
		t.Errorf("Next %v, ждали %v", n, second)
	}
	h.advance(23*time.Hour - time.Second)
	h.noEvent()
	h.advance(time.Second)
	r := h.event()
	checkRun(t, r, schedule.TriggerSchedule, schedule.RunOK, second)
	if hh := r.Scheduled.In(ny).Hour(); hh != 9 {
		t.Errorf("слот в %d часов по местному", hh)
	}
	// Осенью обратно: 1 ноября 2026 — 25 часов между 09:00.
	nov := time.Date(2026, 10, 31, 9, 0, 0, 0, ny)
	h2 := newHarness(t, nov.Add(-time.Minute), nil, schedule.Options{Location: ny},
		schedule.Job{Name: "summary", Daily: "09:00", Run: okJob(schedule.Outcome{})})
	h2.start()
	h2.advance(time.Minute)
	checkRun(t, h2.event(), schedule.TriggerSchedule, schedule.RunOK, nov)
	if n := h2.next("summary"); n.Sub(nov) != 25*time.Hour {
		t.Errorf("осенний переход: следующий через %v", n.Sub(nov))
	}
}

func TestStatus(t *testing.T) {

	st := schedule.NewMemory()
	h := newHarness(t, t0, st, schedule.Options{Budget: 0.5},
		schedule.Job{Name: "issue", Every: time.Hour, Paid: true, Run: okJob(schedule.Outcome{CostUSD: 0.3})},
		schedule.Job{Name: "summary", Daily: "09:00", Run: okJob(schedule.Outcome{})})

	s := h.status()
	if s.Location != "MSK" || s.Budget != 0.5 || s.Spent != 0 || !s.Now.Equal(t0) || len(s.Jobs) != 2 {
		t.Fatalf("Status до Run: %+v", s)
	}
	issue, summary := s.Jobs[0], s.Jobs[1]
	if issue.Name != "issue" || issue.Every != "1h0m0s" || issue.Daily != "" || !issue.Paid ||
		issue.Running || !issue.Next.Equal(t0) || issue.Last != nil {
		t.Errorf("issue до Run: %+v", issue)
	}
	tomorrow9 := time.Date(2026, 9, 25, 9, 0, 0, 0, msk)
	if summary.Name != "summary" || summary.Every != "" || summary.Daily != "09:00" || summary.Paid ||
		!summary.Next.Equal(tomorrow9) {
		t.Errorf("summary до Run: %+v", summary)
	}

	h.start()
	h.event()
	h.advance(time.Hour)
	h.event()
	h.advance(time.Hour) // третий — сверх лимита
	h.event()

	s = h.status()
	if s.Spent < 0.6-1e-9 || s.Spent > 0.6+1e-9 {
		t.Errorf("Spent %v", s.Spent)
	}
	issue = s.Jobs[0]
	if !issue.Next.Equal(t0.Add(3*time.Hour)) || issue.Last == nil || issue.Last.Status != schedule.RunBudget {
		t.Errorf("issue: next %v last %+v", issue.Next, issue.Last)
	}
	if issue.Next.Location() != msk {
		t.Errorf("Next не в Location: %v", issue.Next.Location())
	}

}

// Задание видит свой запуск в контексте: сводка по кнопке ведёт себя иначе,
// чем по расписанию.
func TestCurrentRunInContext(t *testing.T) {
	var got schedule.Run
	var ok bool
	job := schedule.Job{Name: "x", Every: time.Hour, Run: func(ctx context.Context) (schedule.Outcome, error) {
		got, ok = schedule.CurrentRun(ctx)
		return schedule.Outcome{}, nil
	}}
	s, err := schedule.New(schedule.NewMemory(), []schedule.Job{job}, schedule.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunNow(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if !ok || got.Trigger != schedule.TriggerManual || got.Job != "x" || got.ID == 0 {
		t.Errorf("запуск в контексте: %+v, ok=%v", got, ok)
	}
	if _, ok := schedule.CurrentRun(context.Background()); ok {
		t.Error("в пустом контексте нашёлся запуск")
	}
}
