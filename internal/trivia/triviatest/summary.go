package triviatest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// SampleSummary — сводка за сутки перед at (запуск по расписанию): агрегат
// с заполненными всеми полями и разбивками, текст, расход. ID нулевой — его
// присваивает SaveSummary. Потеря любого поля хранилищем будет заметна.
func SampleSummary(at time.Time) trivia.Summary {
	from := at.Add(-24 * time.Hour)
	spend := trivia.Spend{
		Model: "deepseek-v4-flash", Requests: 1,
		Usage: llm.Usage{Prompt: 3100, Completion: 420, Total: 3520, CacheHit: 1536, CacheMiss: 1564, Reasoning: 90},
		Cost:  llm.Cost{USD: 0.0009, Tariff: llm.TariffOffPeak, Known: true},
		Took:  7*time.Second + 300*time.Millisecond,
	}
	return trivia.Summary{
		From:      from,
		To:        at,
		CreatedAt: at.Add(90 * time.Second),
		Trigger:   trivia.SummaryTriggerSchedule,
		Aggregate: trivia.Aggregate{
			From: from, To: at,
			Issues:   3,
			ByStatus: []trivia.Count{{Key: "ok", Count: 2}, {Key: "failed", Count: 1}},
			Species: []trivia.SpeciesLine{
				{IssueID: 11, SpeciesID: 1006010, SciName: "Otocolobus manul", NameRu: "Манул", IUCN: "LC",
					Order: "Carnivora", Status: "ok", Title: "Манул: кот с круглыми зрачками", Facts: 3,
					Highlight: "У манула круглые зрачки.", Recent: 97, OutOfRange: []string{"Germany"}},
				{IssueID: 12, SpeciesID: 1006020, SciName: "Ailurus fulgens", NameRu: "Малая панда", IUCN: "EN",
					Order: "Carnivora", Status: "ok", Title: "Малая панда", Facts: 4,
					Highlight: "Малая панда большую часть дня ест бамбук.", Recent: 310},
				{IssueID: 13, SpeciesID: 1001001, SciName: "Castor fiber", IUCN: "LC", Order: "Rodentia",
					Status: "failed"},
			},
			ByOrder:            []trivia.Count{{Key: "Carnivora", Count: 2}, {Key: "Rodentia", Count: 1}},
			ByIUCN:             []trivia.Count{{Key: "LC", Count: 2}, {Key: "EN", Count: 1}},
			ByRealm:            []trivia.Count{{Key: "Palearctic", Count: 2}, {Key: "Indomalayan", Count: 1}},
			Facts:              7,
			Dropped:            1,
			DroppedShare:       0.125,
			Picks:              3,
			Rejected:           5,
			RejectedByReason:   []trivia.Count{{Key: trivia.ReasonFewRecords, Count: 3}, {Key: trivia.ReasonNoArticle, Count: 2}},
			RecentObservations: 407,
			OutOfRangeSpecies:  []string{"Otocolobus manul: Germany"},
			Runs:               []trivia.Count{{Key: "issue/ok", Count: 2}, {Key: "issue/failed", Count: 1}, {Key: "mdd/ok", Count: 1}},
			Failures:           []string{"04:00 issue: досье: Википедия: 503"},
			MDDRelease:         []string{"вышел релиз v2.6 (было v2.5)"},
			CostUSD:            0.0213,
			BudgetSkips:        0,
		},
		Text:  "За сутки вышло 3 выпуска.",
		Spend: spend,
		Cost:  spend.Cost,
	}
}

// SummaryStoreConformance проверяет контракт trivia.SummaryStore на любой
// реализации; newStore каждый раз отдаёт новое пустое хранилище. Кроме
// обещанного интерфейсом — то, что обе реализации пакета делают одинаково:
// наносекундная точность времени, отказ сохранять сводку с нулевым
// временем, перевёрнутым периодом или неизвестным запуском.
func SummaryStoreConformance(t *testing.T, newStore func(t *testing.T) trivia.SummaryStore) {
	ctx := context.Background()
	zone := time.FixedZone("UTC+7", 7*60*60)
	t0 := time.Date(2026, 9, 25, 0, 0, 0, 123456789, zone)

	save := func(t *testing.T, st trivia.SummaryStore, s trivia.Summary) int64 {
		t.Helper()
		id, err := st.SaveSummary(ctx, s)
		if err != nil {
			t.Fatalf("SaveSummary: %v", err)
		}
		return id
	}
	ids := func(t *testing.T, st trivia.SummaryStore, limit int) []int64 {
		t.Helper()
		list, err := st.Summaries(ctx, limit)
		if err != nil {
			t.Fatalf("Summaries(%d): %v", limit, err)
		}
		out := make([]int64, len(list))
		for i, s := range list {
			out[i] = s.ID
		}
		return out
	}

	t.Run("Empty", func(t *testing.T) {
		st := newStore(t)
		list, err := st.Summaries(ctx, 0)
		if err != nil || list == nil || len(list) != 0 {
			t.Errorf("Summaries на пустом: %v, %v; ждали пустой список", list, err)
		}
		if _, ok, err := st.LatestSummary(ctx); ok || err != nil {
			t.Errorf("LatestSummary на пустом: ok=%v, %v", ok, err)
		}
		if _, err := st.Summary(ctx, 1); !errors.Is(err, trivia.ErrIssueNotFound) {
			t.Errorf("Summary(1) на пустом: %v, ждали ErrIssueNotFound", err)
		}
	})

	t.Run("RoundTrip", func(t *testing.T) {
		st := newStore(t)
		want := SampleSummary(t0)
		want.ID = 555 // входной ID игнорируется
		want.ID = save(t, st, want)
		if want.ID <= 0 {
			t.Fatalf("SaveSummary: ID %d", want.ID)
		}
		got, err := st.Summary(ctx, want.ID)
		if err != nil {
			t.Fatal(err)
		}
		if msg := diffSummary(got, want); msg != "" {
			t.Errorf("Summary: %s", msg)
		}
		latest, ok, err := st.LatestSummary(ctx)
		if err != nil || !ok {
			t.Fatalf("LatestSummary: ok=%v, %v", ok, err)
		}
		if msg := diffSummary(latest, want); msg != "" {
			t.Errorf("LatestSummary: %s", msg)
		}
		list, err := st.Summaries(ctx, 0)
		if err != nil || len(list) != 1 {
			t.Fatalf("Summaries: %d, %v", len(list), err)
		}
		if msg := diffSummary(list[0], want); msg != "" {
			t.Errorf("Summaries: %s", msg)
		}
	})

	t.Run("RoundTripFailedAndEmpty", func(t *testing.T) {
		// Модель не ответила: текста нет, есть ошибка и расход; агрегат
		// пустой — разбивки nil.
		st := newStore(t)
		want := trivia.Summary{From: t0.Add(-24 * time.Hour), To: t0, CreatedAt: t0,
			Trigger: trivia.SummaryTriggerManual, Error: "сводка: таймаут",
			Aggregate: trivia.Aggregate{From: t0.Add(-24 * time.Hour), To: t0},
			Spend:     trivia.Spend{Model: "deepseek-v4-flash", Requests: 2}}
		want.ID = save(t, st, want)
		got, err := st.Summary(ctx, want.ID)
		if err != nil {
			t.Fatal(err)
		}
		if msg := diffSummary(got, want); msg != "" {
			t.Error(msg)
		}
	})

	t.Run("Invalid", func(t *testing.T) {
		st := newStore(t)
		cases := map[string]trivia.Summary{}
		s := SampleSummary(t0)
		s.Trigger = ""
		cases["без запуска"] = s
		s = SampleSummary(t0)
		s.Trigger = "cron"
		cases["неизвестный запуск"] = s
		s = SampleSummary(t0)
		s.CreatedAt = time.Time{}
		cases["нулевое время сводки"] = s
		s = SampleSummary(t0)
		s.From = time.Time{}
		cases["нулевое начало"] = s
		s = SampleSummary(t0)
		s.To = time.Time{}
		cases["нулевой конец"] = s
		s = SampleSummary(t0)
		s.From, s.To = s.To, s.From
		cases["конец раньше начала"] = s
		s = SampleSummary(t0)
		s.Cost.USD = math.NaN()
		cases["NaN в стоимости"] = s
		for name, s := range cases {
			if _, err := st.SaveSummary(ctx, s); err == nil {
				t.Errorf("SaveSummary %s: нет ошибки", name)
			}
		}
		if got := ids(t, st, 0); len(got) != 0 {
			t.Errorf("после отказов сохранено %d сводок", len(got))
		}
	})

	t.Run("Order", func(t *testing.T) {
		st := newStore(t)
		at := func(d time.Duration) trivia.Summary {
			s := SampleSummary(t0)
			s.CreatedAt = t0.Add(d)
			return s
		}
		a := save(t, st, at(0))
		b := save(t, st, at(time.Hour))
		c := save(t, st, at(0)) // то же время, что a, позже сохранена
		d := save(t, st, at(-time.Hour))
		e := save(t, st, at(time.Nanosecond))
		want := []int64{b, e, c, a, d}
		if got := ids(t, st, 0); !reflect.DeepEqual(got, want) {
			t.Errorf("Summaries: %v, ждали %v", got, want)
		}
		if got := ids(t, st, 2); !reflect.DeepEqual(got, want[:2]) {
			t.Errorf("Summaries(2): %v, ждали %v", got, want[:2])
		}
		if latest, ok, err := st.LatestSummary(ctx); err != nil || !ok || latest.ID != b {
			t.Errorf("LatestSummary: %d, ok=%v, %v; ждали %d", latest.ID, ok, err, b)
		}
		for _, id := range []int64{0, -1, b + 100} {
			if _, err := st.Summary(ctx, id); !errors.Is(err, trivia.ErrIssueNotFound) {
				t.Errorf("Summary(%d): %v, ждали ErrIssueNotFound", id, err)
			}
		}
	})

	t.Run("Limit", func(t *testing.T) {
		st := newStore(t)
		const n = 105
		for i := 0; i < n; i++ {
			s := SampleSummary(t0.Add(time.Duration(i) * time.Hour))
			s.Aggregate.Species = nil // короче запись — быстрее тест
			save(t, st, s)
		}
		for _, c := range []struct{ limit, want int }{{0, 20}, {-5, 20}, {7, 7}, {100, 100}, {101, 100}} {
			if got := ids(t, st, c.limit); len(got) != c.want {
				t.Errorf("Summaries(%d): %d, ждали %d", c.limit, len(got), c.want)
			}
		}
	})

	t.Run("Copies", func(t *testing.T) {
		st := newStore(t)
		in := SampleSummary(t0)
		want := SampleSummary(t0)
		want.ID = save(t, st, in)
		in.Aggregate.ByOrder[0].Count = -1
		in.Aggregate.Species[0].OutOfRange[0] = "испорчено"
		in.Aggregate.Failures[0] = "испорчено"
		got, err := st.Summary(ctx, want.ID)
		if err != nil {
			t.Fatal(err)
		}
		if msg := diffSummary(got, want); msg != "" {
			t.Fatalf("после правки входа: %s", msg)
		}
		got.Aggregate.ByIUCN[0].Key = "испорчено"
		got.Aggregate.Species[1].Highlight = "испорчено"
		again, _ := st.Summary(ctx, want.ID)
		if msg := diffSummary(again, want); msg != "" {
			t.Errorf("после правки выданного: %s", msg)
		}
	})

	t.Run("ConcurrentSave", func(t *testing.T) {
		st := newStore(t)
		const workers, each = 6, 5
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			seen = map[int64]bool{}
			errs []error
		)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < each; i++ {
					id, err := st.SaveSummary(ctx, SampleSummary(t0.Add(time.Duration(w*each+i)*time.Minute)))
					mu.Lock()
					switch {
					case err != nil:
						errs = append(errs, err)
					case seen[id]:
						errs = append(errs, fmt.Errorf("ID %d выдан дважды", id))
					default:
						seen[id] = true
					}
					mu.Unlock()
					if _, _, err := st.LatestSummary(ctx); err != nil {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
					}
				}
			}(w)
		}
		wg.Wait()
		for _, err := range errs {
			t.Error(err)
		}
		if got := ids(t, st, 100); len(got) != workers*each {
			t.Errorf("Summaries: %d, ждали %d", len(got), workers*each)
		}
	})
}

// diffSummary — расхождение сводок; пусто — совпали. Время — через Equal
// (зона не сохраняется), остальное — reflect.DeepEqual.
func diffSummary(got, want trivia.Summary) string {
	times := []struct {
		name string
		g, w time.Time
	}{
		{name: "From", g: got.From, w: want.From}, {name: "To", g: got.To, w: want.To},
		{name: "CreatedAt", g: got.CreatedAt, w: want.CreatedAt},
		{name: "Aggregate.From", g: got.Aggregate.From, w: want.Aggregate.From},
		{name: "Aggregate.To", g: got.Aggregate.To, w: want.Aggregate.To},
	}
	for _, tm := range times {
		if !tm.g.Equal(tm.w) {
			return fmt.Sprintf("сводка %d: %s %s, ждали %s", want.ID, tm.name,
				tm.g.Format(time.RFC3339Nano), tm.w.Format(time.RFC3339Nano))
		}
	}
	for _, s := range []*trivia.Summary{&got, &want} {
		s.From, s.To, s.CreatedAt = time.Time{}, time.Time{}, time.Time{}
		s.Aggregate.From, s.Aggregate.To = time.Time{}, time.Time{}
	}
	if reflect.DeepEqual(got, want) {
		return ""
	}
	return fmt.Sprintf("сводка %d:\n got %+v\nwant %+v", want.ID, got, want)
}
