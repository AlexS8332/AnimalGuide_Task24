package triviatest

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// PickStoreConformance проверяет контракт trivia.PickStore на любой
// реализации. newStore должен каждый раз отдавать новое пустое хранилище
// (закрытие — через t.Cleanup).
//
// Кроме обещанного комментариями интерфейса, проверяется то, что обе
// реализации пакета делают одинаково и на что опираются вызывающие:
// точность времени до наносекунды, отказ сохранять выбор или проверку с
// нулевым временем. Порядок RecentSpecies контрактом не задан и не
// проверяется.
func PickStoreConformance(t *testing.T, newStore func(t *testing.T) trivia.PickStore) {
	ctx := context.Background()
	// База времени с наносекундами и не в UTC: хранилище не вправе ни
	// усекать время, ни спотыкаться о зону.
	zone := time.FixedZone("UTC+7", 7*60*60)
	t0 := time.Date(2026, 9, 24, 15, 4, 5, 123456789, zone)

	save := func(t *testing.T, st trivia.PickStore, p trivia.Pick) int64 {
		t.Helper()
		id, err := st.SavePick(ctx, p)
		if err != nil {
			t.Fatalf("SavePick(вид %d): %v", p.SpeciesID, err)
		}
		return id
	}
	picks := func(t *testing.T, st trivia.PickStore, limit int) []trivia.Pick {
		t.Helper()
		list, err := st.Picks(ctx, limit)
		if err != nil {
			t.Fatalf("Picks(%d): %v", limit, err)
		}
		return list
	}
	recent := func(t *testing.T, st trivia.PickStore, since time.Time) []int {
		t.Helper()
		ids, err := st.RecentSpecies(ctx, since)
		if err != nil {
			t.Fatalf("RecentSpecies: %v", err)
		}
		ids = append([]int(nil), ids...)
		sort.Ints(ids)
		return ids
	}
	cached := func(t *testing.T, st trivia.PickStore, id int, notBefore time.Time) (trivia.Eligibility, bool) {
		t.Helper()
		e, ok, err := st.CachedCheck(ctx, id, notBefore)
		if err != nil {
			t.Fatalf("CachedCheck(%d): %v", id, err)
		}
		return e, ok
	}

	t.Run("Empty", func(t *testing.T) {
		st := newStore(t)
		if list := picks(t, st, 0); len(list) != 0 {
			t.Errorf("Picks на пустом: %d", len(list))
		}
		if ids := recent(t, st, time.Time{}); len(ids) != 0 {
			t.Errorf("RecentSpecies на пустом: %v", ids)
		}
		if _, ok := cached(t, st, 1, time.Time{}); ok {
			t.Error("CachedCheck на пустом: ok=true")
		}
	})

	t.Run("SavePickID", func(t *testing.T) {
		st := newStore(t)
		seen := map[int64]bool{}
		for i := 0; i < 3; i++ {
			p := SamplePick(10, t0.Add(time.Duration(i)*time.Hour))
			p.ID = 777 // входной ID игнорируется
			id := save(t, st, p)
			if id <= 0 {
				t.Errorf("SavePick: ID %d, ждали > 0", id)
			}
			if seen[id] {
				t.Errorf("SavePick: ID %d повторился", id)
			}
			seen[id] = true
		}
		for _, p := range picks(t, st, 0) {
			if !seen[p.ID] {
				t.Errorf("Picks: ID %d не выдавался SavePick", p.ID)
			}
		}
	})

	t.Run("SavePickInvalid", func(t *testing.T) {
		st := newStore(t)
		for _, id := range []int{0, -5} {
			if _, err := st.SavePick(ctx, SamplePick(id, t0)); err == nil {
				t.Errorf("SavePick с id вида %d: нет ошибки", id)
			}
		}
		if _, err := st.SavePick(ctx, SamplePick(3, time.Time{})); err == nil {
			t.Error("SavePick с нулевым временем: нет ошибки")
		}
		if list := picks(t, st, 0); len(list) != 0 {
			t.Errorf("после отказов сохранено %d выборов", len(list))
		}
	})

	t.Run("PickRoundTrip", func(t *testing.T) {
		st := newStore(t)
		want := SamplePick(42, t0)
		want.ID = save(t, st, want)
		list := picks(t, st, 0)
		if len(list) != 1 {
			t.Fatalf("Picks: %d выборов, ждали 1", len(list))
		}
		if msg := diffPick(list[0], want); msg != "" {
			t.Error(msg)
		}
	})

	t.Run("PickEmptyFields", func(t *testing.T) {
		// Выбор с первой попытки: ни отказов, ни весов, ни статуса МСОП.
		st := newStore(t)
		want := trivia.Pick{SpeciesID: 5, SciName: "Castor fiber", PickedAt: t0, Attempts: 1,
			Eligibility: SampleCheck(5, t0, true)}
		want.ID = save(t, st, want)
		list := picks(t, st, 0)
		if len(list) != 1 {
			t.Fatalf("Picks: %d выборов, ждали 1", len(list))
		}
		if msg := diffPick(list[0], want); msg != "" {
			t.Error(msg)
		}
	})

	t.Run("PicksOrder", func(t *testing.T) {
		st := newStore(t)
		// Сохраняются не по времени; два выбора — в одно и то же время.
		a := save(t, st, SamplePick(1, t0))
		b := save(t, st, SamplePick(2, t0.Add(time.Hour)))
		c := save(t, st, SamplePick(3, t0)) // то же время, что a, но позже сохранён
		d := save(t, st, SamplePick(4, t0.Add(-time.Hour)))
		e := save(t, st, SamplePick(5, t0.Add(time.Nanosecond)))
		want := []int64{b, e, c, a, d}

		for _, limit := range []int{0, -1, 10} {
			if got := pickIDs(picks(t, st, limit)); !reflect.DeepEqual(got, want) {
				t.Errorf("Picks(%d): %v, ждали %v", limit, got, want)
			}
		}
		for _, limit := range []int{1, 3} {
			if got := pickIDs(picks(t, st, limit)); !reflect.DeepEqual(got, want[:limit]) {
				t.Errorf("Picks(%d): %v, ждали %v", limit, got, want[:limit])
			}
		}
	})

	t.Run("RecentSpecies", func(t *testing.T) {
		st := newStore(t)
		save(t, st, SamplePick(1, t0.Add(-time.Nanosecond))) // на наносекунду раньше границы
		save(t, st, SamplePick(2, t0))                       // ровно на границе
		save(t, st, SamplePick(3, t0.Add(time.Hour)))
		save(t, st, SamplePick(3, t0.Add(2*time.Hour))) // повтор вида
		save(t, st, SamplePick(1, t0.Add(-48*time.Hour)))

		if got, want := recent(t, st, t0), []int{2, 3}; !reflect.DeepEqual(got, want) {
			t.Errorf("RecentSpecies(t0): %v, ждали %v", got, want)
		}
		// Та же граница в UTC — тот же момент.
		if got, want := recent(t, st, t0.UTC()), []int{2, 3}; !reflect.DeepEqual(got, want) {
			t.Errorf("RecentSpecies(t0 в UTC): %v, ждали %v", got, want)
		}
		if got, want := recent(t, st, t0.Add(-time.Nanosecond)), []int{1, 2, 3}; !reflect.DeepEqual(got, want) {
			t.Errorf("RecentSpecies(t0-1нс): %v, ждали %v", got, want)
		}
		if got, want := recent(t, st, time.Time{}), []int{1, 2, 3}; !reflect.DeepEqual(got, want) {
			t.Errorf("RecentSpecies(нулевое время): %v, ждали %v", got, want)
		}
		if got := recent(t, st, t0.Add(3*time.Hour)); len(got) != 0 {
			t.Errorf("RecentSpecies(после всех): %v", got)
		}
	})

	t.Run("PickCopies", func(t *testing.T) {
		st := newStore(t)
		in := SamplePick(7, t0)
		want := SamplePick(7, t0)
		want.ID = save(t, st, in)

		// Вызывающий меняет свой выбор после сохранения.
		in.Rejected[0].Reason = "испорчено"
		in.Rejected = append(in.Rejected, trivia.Rejection{SpeciesID: 1})
		in.Weights["LC"] = 99
		in.Weights["XX"] = 1

		got := picks(t, st, 0)
		if msg := diffPick(got[0], want); msg != "" {
			t.Fatalf("после правки входа: %s", msg)
		}
		// Получатель меняет выданное.
		got[0].Rejected[0].SciName = "испорчено"
		got[0].Weights["LC"] = -1
		got[0].SciName = "испорчено"
		if msg := diffPick(picks(t, st, 0)[0], want); msg != "" {
			t.Errorf("после правки выданного: %s", msg)
		}
	})

	t.Run("CachedCheck", func(t *testing.T) {
		st := newStore(t)
		want := SampleCheck(42, t0, true)
		if err := st.SaveCheck(ctx, want); err != nil {
			t.Fatal(err)
		}
		// notBefore ровно равен CheckedAt — проверка годна.
		got, ok := cached(t, st, 42, t0)
		if !ok {
			t.Fatal("CachedCheck(notBefore = CheckedAt): ok=false")
		}
		if msg := diffCheck(got, want); msg != "" {
			t.Error(msg)
		}
		if _, ok := cached(t, st, 42, t0.UTC()); !ok {
			t.Error("CachedCheck(notBefore = CheckedAt в UTC): ok=false")
		}
		if _, ok := cached(t, st, 42, t0.Add(-time.Hour)); !ok {
			t.Error("CachedCheck(notBefore раньше): ok=false")
		}
		if _, ok := cached(t, st, 42, time.Time{}); !ok {
			t.Error("CachedCheck(нулевое notBefore): ok=false")
		}
		if e, ok := cached(t, st, 42, t0.Add(time.Nanosecond)); ok {
			t.Errorf("CachedCheck(notBefore на 1 нс позже): ok=true, %+v", e)
		}
		if _, ok := cached(t, st, 43, time.Time{}); ok {
			t.Error("CachedCheck(другой вид): ok=true")
		}
	})

	t.Run("CachedCheckNotOK", func(t *testing.T) {
		// Отрицательный итог тоже кэшируется: «статьи нет» — свойство вида.
		st := newStore(t)
		want := SampleCheck(8, t0, false)
		want.WikiLang, want.WikiTitle, want.WikiURL, want.EnTitle = "", "", "", ""
		want.Reason = trivia.ReasonNoArticle
		if err := st.SaveCheck(ctx, want); err != nil {
			t.Fatal(err)
		}
		got, ok := cached(t, st, 8, t0)
		if !ok {
			t.Fatal("CachedCheck: ok=false")
		}
		if msg := diffCheck(got, want); msg != "" {
			t.Error(msg)
		}
	})

	t.Run("SaveCheckReplaces", func(t *testing.T) {
		st := newStore(t)
		first := SampleCheck(9, t0, false)
		second := SampleCheck(9, t0.Add(time.Hour), true)
		for _, e := range []trivia.Eligibility{first, second} {
			if err := st.SaveCheck(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		got, ok := cached(t, st, 9, time.Time{})
		if !ok {
			t.Fatal("CachedCheck: ok=false")
		}
		if msg := diffCheck(got, second); msg != "" {
			t.Errorf("после замены: %s", msg)
		}
		// Замена, а не «оставить свежую»: более старая проверка тоже заменяет.
		if err := st.SaveCheck(ctx, first); err != nil {
			t.Fatal(err)
		}
		got, _ = cached(t, st, 9, time.Time{})
		if msg := diffCheck(got, first); msg != "" {
			t.Errorf("после замены более старой: %s", msg)
		}
		if _, ok := cached(t, st, 9, t0.Add(time.Minute)); ok {
			t.Error("CachedCheck: заменённая свежая проверка всё ещё видна")
		}
	})

	t.Run("SaveCheckRejects", func(t *testing.T) {
		st := newStore(t)
		good := SampleCheck(11, t0, true)
		if err := st.SaveCheck(ctx, good); err != nil {
			t.Fatal(err)
		}
		failed := trivia.Eligibility{SpeciesID: 11, SciName: good.SciName,
			CheckedAt: t0.Add(time.Hour), Reason: trivia.ReasonCheckFailed}
		if err := st.SaveCheck(ctx, failed); err == nil {
			t.Error("SaveCheck с ReasonCheckFailed: нет ошибки")
		}
		got, ok := cached(t, st, 11, time.Time{})
		if !ok {
			t.Fatal("после отказа пропала прежняя проверка")
		}
		if msg := diffCheck(got, good); msg != "" {
			t.Errorf("после отказа: %s", msg)
		}

		for _, id := range []int{0, -1} {
			if err := st.SaveCheck(ctx, SampleCheck(id, t0, true)); err == nil {
				t.Errorf("SaveCheck с id вида %d: нет ошибки", id)
			}
		}
		if err := st.SaveCheck(ctx, SampleCheck(12, time.Time{}, true)); err == nil {
			t.Error("SaveCheck с нулевым временем: нет ошибки")
		}
		if _, ok := cached(t, st, 12, time.Time{}); ok {
			t.Error("проверка с нулевым временем сохранилась")
		}
	})

	t.Run("ConcurrentSavePick", func(t *testing.T) {
		st := newStore(t)
		const workers, each = 8, 10
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			ids  = map[int64]bool{}
			errs []error
		)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < each; i++ {
					id, err := st.SavePick(ctx, SamplePick(w*100+i+1, t0.Add(time.Duration(i)*time.Second)))
					mu.Lock()
					if err != nil {
						errs = append(errs, err)
					} else if ids[id] {
						errs = append(errs, fmt.Errorf("ID %d выдан дважды", id))
					} else {
						ids[id] = true
					}
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()
		for _, err := range errs {
			t.Error(err)
		}
		if n := len(picks(t, st, 0)); n != workers*each {
			t.Errorf("Picks: %d выборов, ждали %d", n, workers*each)
		}
	})
}

func pickIDs(list []trivia.Pick) []int64 {
	ids := make([]int64, len(list))
	for i, p := range list {
		ids[i] = p.ID
	}
	return ids
}

// diffPick — расхождение выборов словами; пусто — совпали. Время — через
// Equal (зона и монотонные часы не сохраняются), пустой и nil-срез или
// карта равны.
func diffPick(got, want trivia.Pick) string {
	var diffs []string
	add := func(format string, a ...any) { diffs = append(diffs, fmt.Sprintf(format, a...)) }
	if got.ID != want.ID {
		add("ID %d, ждали %d", got.ID, want.ID)
	}
	if got.SpeciesID != want.SpeciesID || got.SciName != want.SciName || got.IUCN != want.IUCN {
		add("вид %d %q %q, ждали %d %q %q", got.SpeciesID, got.SciName, got.IUCN,
			want.SpeciesID, want.SciName, want.IUCN)
	}
	if !got.PickedAt.Equal(want.PickedAt) {
		add("PickedAt %s, ждали %s", got.PickedAt.Format(time.RFC3339Nano), want.PickedAt.Format(time.RFC3339Nano))
	}
	if got.Attempts != want.Attempts {
		add("Attempts %d, ждали %d", got.Attempts, want.Attempts)
	}
	if len(got.Rejected) != 0 || len(want.Rejected) != 0 {
		if !reflect.DeepEqual(got.Rejected, want.Rejected) {
			add("Rejected %+v, ждали %+v", got.Rejected, want.Rejected)
		}
	}
	if len(got.Weights) != 0 || len(want.Weights) != 0 {
		if !reflect.DeepEqual(got.Weights, want.Weights) {
			add("Weights %v, ждали %v", got.Weights, want.Weights)
		}
	}
	if msg := diffCheck(got.Eligibility, want.Eligibility); msg != "" {
		add("Eligibility: %s", msg)
	}
	if len(diffs) == 0 {
		return ""
	}
	return fmt.Sprintf("выбор %s: %v", describePick(want), diffs)
}

// diffCheck — расхождение проверок словами; пусто — совпали.
func diffCheck(got, want trivia.Eligibility) string {
	if !got.CheckedAt.Equal(want.CheckedAt) {
		return fmt.Sprintf("CheckedAt %s, ждали %s",
			got.CheckedAt.Format(time.RFC3339Nano), want.CheckedAt.Format(time.RFC3339Nano))
	}
	got.CheckedAt, want.CheckedAt = time.Time{}, time.Time{}
	if got != want {
		return fmt.Sprintf("\n got %+v\nwant %+v", got, want)
	}
	return ""
}
