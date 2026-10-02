// Package clocktest — подставные часы для тестов планировщика: время идёт
// только по Advance, поэтому сутки расписания проходят за миллисекунды, а
// тест не зависит от загрузки машины.
package clocktest

import (
	"sort"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
)

// Fake — часы, которые стоят, пока их не подвинут. Безопасны для
// конкурентного использования.
//
// Синхронизация теста с кодом под проверкой — через BlockUntil: «дождаться,
// пока n горутин ждут на After». Без неё тест не знает, успел ли
// планировщик заснуть до Advance, и пришлось бы спать наугад.
type Fake struct {
	mu     sync.Mutex
	cond   *sync.Cond
	now    time.Time
	seq    int
	timers []*timer // ещё не сработавшие
}

type timer struct {
	at  time.Time
	seq int // порядок заведения: при равном сроке первым срабатывает ранний
	ch  chan time.Time
}

var _ schedule.Clock = (*Fake)(nil)

// NewFake — часы, показывающие start.
func NewFake(start time.Time) *Fake {
	f := &Fake{now: start}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// Now — текущее подставное время.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After — канал, в который придёт срок Now()+d, когда Advance до него
// дойдёт. d ≤ 0 — канал уже со значением, и ожидающим такой вызов не
// считается: настоящий time.After(0) тоже срабатывает сразу.
func (f *Fake) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1) // буфер: Advance не ждёт получателя
	f.mu.Lock()
	defer f.mu.Unlock()
	if d <= 0 {
		ch <- f.now
		return ch
	}
	f.seq++
	f.timers = append(f.timers, &timer{at: f.now.Add(d), seq: f.seq, ch: ch})
	f.cond.Broadcast()
	return ch
}

// Advance сдвигает часы на d и срабатывает все таймеры, чей срок наступил,
// по порядку сроков. В канал приходит срок таймера, а Now() к моменту
// отправки уже показывает новое время: получатель, спросивший часы после
// пробуждения, видит итоговое время, а не промежуточное.
func (f *Fake) Advance(d time.Duration) {
	if d < 0 {
		panic("clocktest: время назад не идёт")
	}
	f.mu.Lock()
	f.now = f.now.Add(d)
	var due, rest []*timer
	for _, t := range f.timers {
		if t.at.After(f.now) {
			rest = append(rest, t)
		} else {
			due = append(due, t)
		}
	}
	f.timers = rest
	f.mu.Unlock()

	sort.Slice(due, func(i, j int) bool {
		if !due[i].at.Equal(due[j].at) {
			return due[i].at.Before(due[j].at)
		}
		return due[i].seq < due[j].seq
	})
	for _, t := range due {
		t.ch <- t.at
	}
}

// AdvanceTo сдвигает часы до t (если t не в прошлом) — см. Advance.
func (f *Fake) AdvanceTo(t time.Time) {
	f.Advance(max(t.Sub(f.Now()), 0))
}

// Waiters — сколько таймеров After ещё не сработало.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// BlockUntil ждёт, пока несработавших таймеров станет не меньше n. Таймер,
// брошенный получателем (select ушёл по другому каналу), тоже считается —
// тест должен это учитывать. Зависание здесь — признак ошибки в коде под
// проверкой; его прервёт таймаут go test.
func (f *Fake) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.timers) < n {
		f.cond.Wait()
	}
}
