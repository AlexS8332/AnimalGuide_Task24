package clocktest_test

import (
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule/clocktest"
)

var t0 = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func recv(t *testing.T, ch <-chan time.Time) (time.Time, bool) {
	t.Helper()
	select {
	case v := <-ch:
		return v, true
	default:
		return time.Time{}, false
	}
}

func TestFakeAdvance(t *testing.T) {
	f := clocktest.NewFake(t0)
	if !f.Now().Equal(t0) {
		t.Fatalf("Now %v", f.Now())
	}
	late := f.After(2 * time.Hour)
	early := f.After(time.Hour)
	far := f.After(5 * time.Hour)
	if n := f.Waiters(); n != 3 {
		t.Fatalf("Waiters %d", n)
	}

	f.Advance(59 * time.Minute)
	if _, ok := recv(t, early); ok {
		t.Error("сработал раньше срока")
	}
	f.Advance(90 * time.Minute) // оба срока наступили
	if v, ok := recv(t, early); !ok || !v.Equal(t0.Add(time.Hour)) {
		t.Errorf("early: %v %v", v, ok)
	}
	if v, ok := recv(t, late); !ok || !v.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("late: %v %v", v, ok)
	}
	if _, ok := recv(t, far); ok {
		t.Error("far сработал")
	}
	if n := f.Waiters(); n != 1 {
		t.Errorf("Waiters %d, ждали 1", n)
	}
	if !f.Now().Equal(t0.Add(149 * time.Minute)) {
		t.Errorf("Now %v", f.Now())
	}

	now := f.After(0)
	if v, ok := recv(t, now); !ok || !v.Equal(f.Now()) {
		t.Errorf("After(0): %v %v", v, ok)
	}
	f.AdvanceTo(t0.Add(5 * time.Hour))
	if _, ok := recv(t, far); !ok {
		t.Error("far не сработал по AdvanceTo")
	}
}

func TestFakeBlockUntil(t *testing.T) {
	f := clocktest.NewFake(t0)
	got := make(chan time.Time)
	go func() { got <- <-f.After(time.Minute) }()
	f.BlockUntil(1) // горутина точно ждёт — Advance её не обгонит
	f.Advance(time.Minute)
	if v := <-got; !v.Equal(t0.Add(time.Minute)) {
		t.Errorf("пришло %v", v)
	}
}
