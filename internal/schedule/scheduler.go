package schedule

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// defaultTimeout — предел запуска, если Job.Timeout не задан.
const defaultTimeout = 10 * time.Minute

// busyError — ошибка пропущенного слота: прошлый запуск задания не кончился.
const busyError = "предыдущий запуск ещё идёт"

// Scheduler запускает задания по расписанию и вручную.
//
// # Следующий слот
//
// Слот — момент расписания. Следующий слот задания считается от Scheduled
// последнего запуска из журнала (RunStore.Last), а не от времени окончания:
// сетка расписания не ползёт от длительности запусков.
//
//   - Every: last.Scheduled + Every. Нет запусков вовсе — сразу при старте
//     (первый выпуск при первом старте демона).
//   - Daily: ближайшее HH:MM в Location строго после last.Scheduled. Нет
//     запусков — ближайшее HH:MM не раньше текущего момента: демон,
//     впервые запущенный в 03:00, ждёт 09:00, а не выпускает сводку за
//     «вчерашние 09:00». Слот строится через time.Date в Location, поэтому
//     в сутки перехода на летнее время между двумя 09:00 проходит 23 часа,
//     а не 24.
//
// # Догон
//
// Когда слот наступил, берётся ПОСЛЕДНИЙ наступивший слот сетки S (≤ now),
// сколько бы их ни было пропущено, и запускается ровно один раз со
// Scheduled = S. Следующий слот — первый после S, то есть заведомо позже
// now: второго немедленного запуска не бывает, а сетка остаётся прежней.
// Пример: Every = 1h, последний запуск в журнале — Scheduled 10:00, демон
// поднят в 14:25. Пропущены 11:00, 12:00, 13:00, 14:00 → один запуск
// catch-up со Scheduled 14:00, следующий — 15:00 (а не 15:25 и не сразу).
// Для Daily так же: последний 09:00 три дня назад, старт в 10:00 → один
// catch-up за сегодняшние 09:00, следующий — завтра в 09:00.
//
// Trigger catch-up ставится, если слот пропущен за время простоя (при
// старте Run слот уже в прошлом) или если на ходу пропущено больше одного
// слота (машина спала). Просто сработавший вовремя таймер — schedule.
//
// # Пропуски
//
// Одновременно идёт не больше одного запуска задания; разные задания идут
// параллельно. Слот, наступивший, пока задание ещё идёт, не ставится в
// очередь, а записывается RunSkipped с ошибкой busyError. Платное задание
// при исчерпанном дневном лимите записывается RunBudget без вызова Run.
// Оба пропуска RunStore.Last не видит, сетка от них не зависит.
//
// # Ручной запуск
//
// RunNow — запуск с Trigger manual и Scheduled = момент нажатия. Он СДВИГАЕТ
// расписание: следующий слот Every-задания — через Every после нажатия
// (у Daily следующий слот и так ближайшее HH:MM — он не меняется). Почему:
// журнал (Last) не отличает ручной запуск от планового, и после перезапуска
// демона сетка всё равно считалась бы от ручного — поведение «на ходу» и
// «после перезапуска» должно совпадать. И по смыслу: выпуск только что
// собран руками, платить за следующий через пять минут незачем.
// Пропуск по лимиту расписание не сдвигает — его и Last не видит.
//
// # Остановка
//
// Отмена ctx у Run отменяет контекст идущих запусков (и плановых, и
// ручных); Run возвращается, когда все они завершились и записали итог.
// Прерванный запуск записывается RunFailed с ошибкой «демон остановлен: …»
// — вечного running после перезапуска нет. Если процесс убит, не успев
// записать итог, запись остаётся running до Abandon при следующем старте.
// Задание обязано уважать ctx: запуск, не реагирующий на отмену, держит
// остановку демона до своего конца (или до таймаута).
//
// # OnRun
//
// Зовётся после каждой записи итога (Finish) или пропуска (Record), по
// одному вызову за раз, и уже после снятия отметки «идёт»: получив итог,
// можно сразу звать RunNow — но из другой горутины, не изнутри OnRun
// (RunNow сам зовёт OnRun и ждал бы сам себя).
type Scheduler struct {
	store  RunStore
	clock  Clock
	loc    *time.Location
	budget float64
	onRun  func(Run)

	jobs   []*job
	byName map[string]*job

	// notifyMu — OnRun зовётся из разных горутин (цикл, запуски, RunNow);
	// по одному за раз — получателю не нужна своя синхронизация.
	notifyMu sync.Mutex

	mu      sync.Mutex
	started bool            // Run уже вызывали: второй Run — ошибка
	planned bool            // next у заданий посчитан циклом Run
	active  bool            // Run идёт и ещё не останавливается
	daemon  context.Context // ctx у Run, пока active
	wg      sync.WaitGroup  // запуски, которые Run ждёт при остановке
}

type job struct {
	Job
	hour, minute int // Daily

	// Под Scheduler.mu.
	running bool
	next    time.Time
	history bool // при старте Run в журнале был запуск: пропущенный слот — догон
}

// New проверяет задания и собирает планировщик. Задания не запускаются до
// Run; RunNow работает и без Run (разовый запуск из командной строки).
func New(store RunStore, jobs []Job, o Options) (*Scheduler, error) {
	if store == nil {
		return nil, errors.New("schedule: нет журнала запусков")
	}
	if math.IsNaN(o.Budget) || math.IsInf(o.Budget, 0) || o.Budget < 0 {
		return nil, fmt.Errorf("schedule: неверный дневной лимит %v", o.Budget)
	}
	s := &Scheduler{
		store:  store,
		clock:  o.Clock,
		loc:    o.Location,
		budget: o.Budget,
		onRun:  o.OnRun,
		byName: map[string]*job{},
	}
	if s.clock == nil {
		s.clock = System
	}
	if s.loc == nil {
		s.loc = time.Local
	}
	for _, jb := range jobs {
		j, err := newJob(jb)
		if err != nil {
			return nil, err
		}
		if s.byName[j.Name] != nil {
			return nil, fmt.Errorf("schedule: задание %q объявлено дважды", j.Name)
		}
		s.byName[j.Name] = j
		s.jobs = append(s.jobs, j)
	}
	return s, nil
}

func newJob(jb Job) (*job, error) {
	if strings.TrimSpace(jb.Name) == "" {
		return nil, errors.New("schedule: у задания нет имени")
	}
	if jb.Name != strings.TrimSpace(jb.Name) {
		return nil, fmt.Errorf("schedule: имя задания %q с пробелами по краям", jb.Name)
	}
	j := &job{Job: jb}
	switch {
	case jb.Every != 0 && jb.Daily != "":
		return nil, fmt.Errorf("schedule: задание %q: заданы и Every, и Daily — нужно одно", jb.Name)
	case jb.Every == 0 && jb.Daily == "":
		return nil, fmt.Errorf("schedule: задание %q: не задано ни Every, ни Daily", jb.Name)
	case jb.Every < 0:
		return nil, fmt.Errorf("schedule: задание %q: отрицательный интервал %s", jb.Name, jb.Every)
	case jb.Daily != "":
		h, m, err := parseDaily(jb.Daily)
		if err != nil {
			return nil, fmt.Errorf("schedule: задание %q: %w", jb.Name, err)
		}
		j.hour, j.minute = h, m
	}
	if jb.Run == nil {
		return nil, fmt.Errorf("schedule: задание %q: нет функции Run", jb.Name)
	}
	if jb.Timeout < 0 {
		return nil, fmt.Errorf("schedule: задание %q: отрицательный предел %s", jb.Name, jb.Timeout)
	}
	return j, nil
}

// parseDaily разбирает «HH:MM» строго: две цифры часа 00–23 и две минуты
// 00–59. «9:00» или «24:00» — скорее опечатка в настройке, чем намерение.
func parseDaily(s string) (hour, minute int, err error) {
	bad := fmt.Errorf("время суток %q не в формате HH:MM", s)
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, bad
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, 0, bad
		}
	}
	hour = int(s[0]-'0')*10 + int(s[1]-'0')
	minute = int(s[3]-'0')*10 + int(s[4]-'0')
	if hour > 23 || minute > 59 {
		return 0, 0, bad
	}
	return hour, minute, nil
}

// dailyAfter — первый слот HH:MM в loc строго после t. Через time.Date:
// «09:00 следующего дня» в сутки перехода на летнее время — через 23 или 25
// часов, а не через 24.
func (s *Scheduler) dailyAfter(j *job, t time.Time) time.Time {
	lt := t.In(s.loc)
	for d := 0; ; d++ {
		c := time.Date(lt.Year(), lt.Month(), lt.Day()+d, j.hour, j.minute, 0, 0, s.loc)
		if c.After(t) {
			return c
		}
	}
}

// dailyAtOrBefore — последний слот HH:MM в loc не позже t.
func (s *Scheduler) dailyAtOrBefore(j *job, t time.Time) time.Time {
	lt := t.In(s.loc)
	for d := 0; ; d-- {
		c := time.Date(lt.Year(), lt.Month(), lt.Day()+d, j.hour, j.minute, 0, 0, s.loc)
		if !c.After(t) {
			return c
		}
	}
}

// slotAfter — следующий слот после слота t.
func (s *Scheduler) slotAfter(j *job, t time.Time) time.Time {
	if j.Every > 0 {
		return t.Add(j.Every)
	}
	return s.dailyAfter(j, t)
}

// lastDue — последний наступивший слот сетки, начинающейся с next ≤ now.
func (s *Scheduler) lastDue(j *job, next, now time.Time) time.Time {
	if j.Every > 0 {
		return next.Add(now.Sub(next) / j.Every * j.Every)
	}
	// Слот next — тоже слот сетки, так что последний слот не раньше него;
	// max — страховка от странностей зоны.
	if d := s.dailyAtOrBefore(j, now); d.After(next) {
		return d
	}
	return next
}

// plan — первый слот задания по журналу (см. комментарий к Scheduler).
// Может быть в прошлом — значит, слот пропущен и ждёт догона.
func (s *Scheduler) plan(j *job, last Run, ok bool, now time.Time) time.Time {
	switch {
	case ok:
		return s.slotAfter(j, last.Scheduled)
	case j.Every > 0:
		return now
	}
	return s.dailyAfter(j, now.Add(-time.Nanosecond)) // не раньше now
}

// Run крутит расписание до отмены ctx. При старте помечает незавершённые
// запуски прошлого процесса (Abandon) и считает слоты по журналу. Ошибку
// возвращает только при сбое старта; остановка по ctx — nil. Второй вызов
// Run на том же планировщике — ошибка.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("schedule: планировщик уже запущен")
	}
	s.started = true
	s.mu.Unlock()

	now := s.clock.Now()
	if _, err := s.store.Abandon(ctx, now); err != nil {
		return err
	}
	type plan struct {
		next    time.Time
		history bool
	}
	plans := make([]plan, len(s.jobs))
	for i, j := range s.jobs {
		last, ok, err := s.store.Last(ctx, j.Name)
		if err != nil {
			return err
		}
		plans[i] = plan{s.plan(j, last, ok, now), ok}
	}

	s.mu.Lock()
	for i, j := range s.jobs {
		// Ручной запуск, начатый до Run, мог уже сдвинуть слот.
		if !j.next.After(plans[i].next) {
			j.next = plans[i].next
		}
		j.history = plans[i].history
	}
	s.planned = true
	s.active = true
	s.daemon = ctx
	s.mu.Unlock()

	for startup := true; ; startup = false {
		now := s.clock.Now()
		s.tick(ctx, now, startup)

		wake := s.earliest()
		if wake.IsZero() { // заданий нет — ждать нечего
			<-ctx.Done()
			break
		}
		select {
		case <-s.clock.After(wake.Sub(now)):
			continue
		case <-ctx.Done():
		}
		break
	}

	s.mu.Lock()
	s.active = false
	s.daemon = nil
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

// earliest — ближайший слот среди заданий; нулевое время — заданий нет.
func (s *Scheduler) earliest() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var t time.Time
	for _, j := range s.jobs {
		if t.IsZero() || j.next.Before(t) {
			t = j.next
		}
	}
	return t
}

// tick запускает (или записывает пропущенными) задания, чей слот наступил.
// Пропуски и Begin пишутся здесь же, синхронно: к моменту, когда цикл снова
// ждёт на часах, всё решённое в этом такте уже в журнале.
func (s *Scheduler) tick(ctx context.Context, now time.Time, startup bool) {
	for _, j := range s.jobs {
		s.mu.Lock()
		if j.next.After(now) {
			s.mu.Unlock()
			continue
		}
		slot := s.lastDue(j, j.next, now)
		trigger := TriggerSchedule
		if (startup && j.history && slot.Before(now)) || slot.After(j.next) {
			trigger = TriggerCatchUp
		}
		j.next = s.slotAfter(j, slot)
		busy := j.running
		if !busy {
			j.running = true
			s.wg.Add(1)
		}
		s.mu.Unlock()

		if busy {
			s.notify(s.save(ctx, Run{Job: j.Name, Trigger: trigger, Scheduled: slot,
				Started: now, Finished: now, Status: RunSkipped, Error: busyError}))
			continue
		}
		r, ok, _ := s.start(ctx, j, trigger, slot) // сбой журнала — в r.Error
		if !ok {
			s.unmark(j)
			s.notify(r)
			s.wg.Done()
			continue
		}
		go func() {
			defer s.wg.Done()
			s.execute(ctx, ctx, j, r)
		}()
	}
}

// unmark снимает отметку «идёт». Зовётся ДО OnRun: получатель, узнав об
// итоге, может сразу нажать «Собрать сейчас» — и не должен получить ErrBusy
// от уже закончившегося запуска.
func (s *Scheduler) unmark(j *job) {
	s.mu.Lock()
	j.running = false
	s.mu.Unlock()
}

// start проверяет лимит и записывает начало запуска. run=false — запускать
// не нужно: пропуск по лимиту (записан Record) или сбой журнала (err); r —
// запись для OnRun, сообщает о ней вызывающий, сняв отметку «идёт».
func (s *Scheduler) start(ctx context.Context, j *job, trigger string, slot time.Time) (r Run, run bool, err error) {
	now := s.clock.Now()
	r = Run{Job: j.Name, Trigger: trigger, Scheduled: slot, Started: now}
	if j.Paid && s.budget > 0 {
		spent, err := s.spentToday(ctx, now)
		if err != nil {
			// Не зная расхода, платить нельзя: лимит для того и есть.
			r.Status, r.Finished = RunFailed, now
			r.Error = "лимит расходов не проверен: " + err.Error()
			return s.save(ctx, r), false, err
		}
		if spent >= s.budget {
			r.Status, r.Finished = RunBudget, now
			r.Detail = fmt.Sprintf("потрачено $%.4f из $%.4g", spent, s.budget)
			return s.save(ctx, r), false, nil
		}
	}
	r.Status = RunRunning
	id, err := s.store.Begin(ctx, r)
	if err != nil {
		// Без записи о начале расход не попал бы в Spent — не запускаем.
		r.Status, r.Finished = RunFailed, now
		r.Error = "журнал: " + err.Error()
		return r, false, err
	}
	r.ID = id
	if trigger == TriggerManual {
		s.shift(j, slot)
	}
	return r, true, nil
}

// shift — ручной запуск сдвигает следующий слот (см. комментарий к
// Scheduler). Только вперёд: цикл Run ждёт на таймере до прежнего слота,
// проснётся, увидит, что рано, и заснёт до нового — будить его не нужно.
func (s *Scheduler) shift(j *job, manual time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.slotAfter(j, manual); n.After(j.next) {
		j.next = n
	}
}

// execute вызывает Run задания и записывает итог. parent — контекст
// запуска; daemon — ctx у Run (nil — Run не идёт): по нему отмена
// называется «демон остановлен».
func (s *Scheduler) execute(parent, daemon context.Context, j *job, r Run) Run {
	timeout := j.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	// Таймаут — по настоящим часам, через context.WithTimeout: срок должен
	// быть виден HTTP-клиентам модели и справочников (ctx.Deadline), а
	// Clock.After этого не даёт. Предел — защита от зависшей сети, а не
	// часть расписания.
	ctx, cancel := context.WithTimeout(WithRun(parent, r), timeout)
	defer cancel()
	out, err := call(ctx, j.Run)

	r.Finished = s.clock.Now()
	r.Outcome = out
	switch {
	case err == nil:
		r.Status = RunOK
	case errors.Is(err, ErrSkip):
		r.Status = RunSkipped
		if err != ErrSkip { // обёрнутая причина пропуска — людям пригодится
			r.Error = err.Error()
		}
	default:
		r.Status = RunFailed
		switch {
		case daemon != nil && daemon.Err() != nil:
			r.Error = "демон остановлен: " + err.Error()
		case parent.Err() != nil:
			r.Error = "запуск отменён: " + err.Error()
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			r.Error = fmt.Sprintf("превышен предел %s: %v", timeout, err)
		default:
			r.Error = err.Error()
		}
	}
	// Итог пишется и при отменённом ctx: иначе запуск, прерванный
	// остановкой, висел бы running до следующего старта. И Outcome
	// пишется при ошибке: задание, упавшее после вызова модели, деньги уже
	// потратило — они обязаны попасть в Spent.
	if err := s.store.Finish(context.WithoutCancel(parent), r); err != nil {
		r.Error = joinError(r.Error, "журнал: "+err.Error())
	}
	s.unmark(j)
	s.notify(r)
	return r
}

// call вызывает задание; паника становится ошибкой — одно упавшее задание
// не роняет демон.
func call(ctx context.Context, run func(context.Context) (Outcome, error)) (out Outcome, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("паника: %v", p)
		}
	}()
	return run(ctx)
}

// save пишет пропуск одной записью; сбой журнала — в r.Error, чтобы OnRun
// (журнал процесса) его показал.
func (s *Scheduler) save(ctx context.Context, r Run) Run {
	id, err := s.store.Record(context.WithoutCancel(ctx), r)
	if err != nil {
		r.Error = joinError(r.Error, "журнал: "+err.Error())
	} else {
		r.ID = id
	}
	return r
}

func (s *Scheduler) notify(r Run) {
	if s.onRun == nil {
		return
	}
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	s.onRun(r)
}

func joinError(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// day — границы суток в Location, в которые попадает t.
func (s *Scheduler) day(t time.Time) (start, end time.Time) {
	lt := t.In(s.loc)
	start = time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, s.loc)
	end = time.Date(lt.Year(), lt.Month(), lt.Day()+1, 0, 0, 0, 0, s.loc)
	return start, end
}

// spentToday — расход за текущие сутки. Верхняя граница — конец суток, а не
// now: запуск, начатый в тот же момент (подставные часы, два RunNow подряд),
// тоже должен учитываться.
func (s *Scheduler) spentToday(ctx context.Context, now time.Time) (float64, error) {
	start, end := s.day(now)
	return s.store.Spent(ctx, start, end)
}

// RunNow запускает задание вручную (Trigger manual) и ждёт итога.
//
// Ошибка — только когда запуск не состоялся по причине планировщика:
// ErrUnknownJob, ErrBusy (задание уже идёт — вторую копию не запускаем, в
// очередь не ставим) или сбой журнала. Итог самого задания — в Run:
// Status failed с Error, skipped, и RunBudget, если дневной лимит
// исчерпан (ручной запуск тоже платный), — всё это с err == nil: запись в
// журнале есть, вызывающему (инструмент run_now, кнопка) достаточно
// показать её.
//
// Пока идёт Run, ручной запуск отменяется и остановкой демона.
func (s *Scheduler) RunNow(ctx context.Context, name string) (Run, error) {
	j := s.byName[name]
	if j == nil {
		return Run{}, fmt.Errorf("%w: %q", ErrUnknownJob, name)
	}
	s.mu.Lock()
	if j.running {
		s.mu.Unlock()
		return Run{}, fmt.Errorf("%w: %q", ErrBusy, name)
	}
	j.running = true
	daemon := s.daemon
	counted := s.active
	if counted {
		s.wg.Add(1) // Run при остановке дождётся и ручного запуска
	}
	s.mu.Unlock()
	if counted {
		defer s.wg.Done()
	}

	parent := ctx
	if daemon != nil {
		c, cancel := context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(daemon, cancel)()
		parent = c
	}
	r, run, err := s.start(parent, j, TriggerManual, s.clock.Now())
	if !run {
		s.unmark(j)
		s.notify(r)
		return r, err
	}
	return s.execute(parent, daemon, j, r), nil
}

// Status — снимок: слоты, идущие запуски, последние записи журнала, расход
// за сутки. JobStatus.Last — последняя запись журнала по заданию с любым
// статусом (в том числе budget и skipped): интерфейсу важно показать и
// пропуск. До Run слот считается по журналу так же, как посчитал бы Run.
func (s *Scheduler) Status(ctx context.Context) (Status, error) {
	now := s.clock.Now()
	spent, err := s.spentToday(ctx, now)
	if err != nil {
		return Status{}, err
	}
	st := Status{
		Now:      now.In(s.loc),
		Location: s.loc.String(),
		Budget:   s.budget,
		Spent:    spent,
		Jobs:     make([]JobStatus, 0, len(s.jobs)),
	}
	for _, j := range s.jobs {
		s.mu.Lock()
		js := JobStatus{Name: j.Name, Daily: j.Daily, Paid: j.Paid, Running: j.running, Next: j.next}
		planned := s.planned
		s.mu.Unlock()
		if j.Every > 0 {
			js.Every = j.Every.String()
		}
		if !planned {
			last, ok, err := s.store.Last(ctx, j.Name)
			if err != nil {
				return Status{}, err
			}
			if n := s.plan(j, last, ok, now); n.After(js.Next) {
				js.Next = n
			}
		}
		js.Next = js.Next.In(s.loc)
		runs, err := s.store.Runs(ctx, RunQuery{Job: j.Name, Limit: 1})
		if err != nil {
			return Status{}, err
		}
		if len(runs) > 0 {
			js.Last = &runs[0]
		}
		st.Jobs = append(st.Jobs, js)
	}
	return st, nil
}
