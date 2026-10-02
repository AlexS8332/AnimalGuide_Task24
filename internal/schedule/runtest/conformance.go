// Package runtest — набор проверок контракта schedule.RunStore, общий для
// всех реализаций журнала запусков.
package runtest

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
)

// RunStoreConformance проверяет контракт schedule.RunStore на любой
// реализации. newStore должен каждый раз отдавать новый пустой журнал
// (закрытие — через t.Cleanup).
//
// Кроме обещанного комментариями интерфейса, проверяется то, что обе
// реализации пакета делают одинаково и на что опирается планировщик:
// точность времени до наносекунды, Begin всегда пишет RunRunning, Finish
// отказывает по неизвестному и уже завершённому ID, Record — с неитоговым
// статусом, нулевое Started не сохраняется.
func RunStoreConformance(t *testing.T, newStore func(t *testing.T) schedule.RunStore) {
	ctx := context.Background()
	// База времени с наносекундами и не в UTC: журнал не вправе ни усекать
	// время, ни спотыкаться о зону.
	zone := time.FixedZone("UTC+7", 7*60*60)
	t0 := time.Date(2026, 9, 24, 15, 4, 5, 123456789, zone)
	at := func(d time.Duration) time.Time { return t0.Add(d) }

	sample := func(job string, started time.Time, status string, cost float64) schedule.Run {
		return schedule.Run{
			Job: job, Trigger: schedule.TriggerSchedule,
			Scheduled: started.Add(-time.Second), Started: started, Finished: started.Add(time.Minute),
			Status: status, Outcome: schedule.Outcome{CostUSD: cost, Ref: "issue:1", Detail: "Манул"},
		}
	}
	record := func(t *testing.T, st schedule.RunStore, r schedule.Run) int64 {
		t.Helper()
		id, err := st.Record(ctx, r)
		if err != nil {
			t.Fatalf("Record(%s %s): %v", r.Job, r.Status, err)
		}
		return id
	}
	begin := func(t *testing.T, st schedule.RunStore, r schedule.Run) int64 {
		t.Helper()
		id, err := st.Begin(ctx, r)
		if err != nil {
			t.Fatalf("Begin(%s): %v", r.Job, err)
		}
		return id
	}
	runs := func(t *testing.T, st schedule.RunStore, q schedule.RunQuery) []schedule.Run {
		t.Helper()
		list, err := st.Runs(ctx, q)
		if err != nil {
			t.Fatalf("Runs(%+v): %v", q, err)
		}
		return list
	}
	ids := func(list []schedule.Run) []int64 {
		out := make([]int64, len(list))
		for i, r := range list {
			out[i] = r.ID
		}
		return out
	}
	spent := func(t *testing.T, st schedule.RunStore, since, until time.Time) float64 {
		t.Helper()
		v, err := st.Spent(ctx, since, until)
		if err != nil {
			t.Fatalf("Spent: %v", err)
		}
		return v
	}

	t.Run("Empty", func(t *testing.T) {
		st := newStore(t)
		if list := runs(t, st, schedule.RunQuery{}); len(list) != 0 {
			t.Errorf("Runs на пустом: %d", len(list))
		}
		if _, ok, err := st.Last(ctx, "issue"); err != nil || ok {
			t.Errorf("Last на пустом: ok=%v err=%v", ok, err)
		}
		if v := spent(t, st, time.Time{}, time.Time{}); v != 0 {
			t.Errorf("Spent на пустом: %v", v)
		}
		if n, err := st.Abandon(ctx, t0); err != nil || n != 0 {
			t.Errorf("Abandon на пустом: %d, %v", n, err)
		}
	})

	t.Run("BeginFinishRoundTrip", func(t *testing.T) {
		st := newStore(t)
		in := sample("issue", t0, schedule.RunOK, 0.25) // статус и итог Begin игнорирует
		in.ID = 777
		in.Error = "лишнее"
		id := begin(t, st, in)
		if id <= 0 {
			t.Fatalf("Begin: ID %d", id)
		}
		list := runs(t, st, schedule.RunQuery{})
		if len(list) != 1 {
			t.Fatalf("Runs: %d записей", len(list))
		}
		got := list[0]
		if got.ID != id || got.Job != "issue" || got.Trigger != schedule.TriggerSchedule ||
			got.Status != schedule.RunRunning || !got.Finished.IsZero() || got.Error != "" ||
			got.Outcome != (schedule.Outcome{}) {
			t.Errorf("после Begin: %+v", got)
		}
		if !got.Started.Equal(in.Started) || !got.Scheduled.Equal(in.Scheduled) {
			t.Errorf("время после Begin: started %v scheduled %v, ждали %v %v",
				got.Started, got.Scheduled, in.Started, in.Scheduled)
		}

		fin := got
		fin.Finished = at(90*time.Second + 7)
		fin.Status = schedule.RunFailed
		fin.Error = "проверка не прошла"
		fin.Outcome = schedule.Outcome{CostUSD: 0.125, Ref: "issue:42", Detail: "Манул, 5 фактов"}
		if err := st.Finish(ctx, fin); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		got = runs(t, st, schedule.RunQuery{})[0]
		if got.Status != fin.Status || got.Error != fin.Error || got.Outcome != fin.Outcome ||
			!got.Finished.Equal(fin.Finished) || !got.Started.Equal(in.Started) || got.Job != "issue" {
			t.Errorf("после Finish: %+v, ждали %+v", got, fin)
		}
		if err := st.Finish(ctx, fin); err == nil {
			t.Error("повторный Finish: нет ошибки")
		}
	})

	t.Run("FinishInvalid", func(t *testing.T) {
		st := newStore(t)
		id := begin(t, st, sample("issue", t0, "", 0))
		ok := schedule.Run{ID: id, Status: schedule.RunOK, Finished: at(time.Minute)}
		for name, r := range map[string]schedule.Run{
			"неизвестный ID": {ID: id + 100, Status: schedule.RunOK, Finished: at(time.Minute)},
			"нулевой ID":     {Status: schedule.RunOK, Finished: at(time.Minute)},
			"статус running": {ID: id, Status: schedule.RunRunning, Finished: at(time.Minute)},
			"пустой статус":  {ID: id, Finished: at(time.Minute)},
			"без Finished":   {ID: id, Status: schedule.RunOK},
			"расход NaN":     {ID: id, Status: schedule.RunOK, Finished: at(time.Minute), Outcome: schedule.Outcome{CostUSD: math.NaN()}},
			"расход < 0":     {ID: id, Status: schedule.RunOK, Finished: at(time.Minute), Outcome: schedule.Outcome{CostUSD: -1}},
		} {
			if err := st.Finish(ctx, r); err == nil {
				t.Errorf("Finish (%s): нет ошибки", name)
			}
		}
		if got := runs(t, st, schedule.RunQuery{})[0]; got.Status != schedule.RunRunning {
			t.Errorf("после отказов статус %q", got.Status)
		}
		if err := st.Finish(ctx, ok); err != nil {
			t.Errorf("Finish после отказов: %v", err)
		}
	})

	t.Run("RecordRoundTrip", func(t *testing.T) {
		st := newStore(t)
		in := sample("issue", t0, schedule.RunBudget, 0)
		in.Trigger = schedule.TriggerCatchUp
		in.Detail = "потрачено $0.60 из $0.50"
		in.Error = "причина"
		in.ID = 555
		id := record(t, st, in)
		got := runs(t, st, schedule.RunQuery{})[0]
		in.ID = id
		if got.ID != id || got.Job != in.Job || got.Trigger != in.Trigger || got.Status != in.Status ||
			got.Error != in.Error || got.Outcome != in.Outcome || !got.Scheduled.Equal(in.Scheduled) ||
			!got.Started.Equal(in.Started) || !got.Finished.Equal(in.Finished) {
			t.Errorf("Record: %+v, ждали %+v", got, in)
		}
		// Finished у Record необязателен.
		noFin := sample("mdd", t0, schedule.RunSkipped, 0)
		noFin.Finished = time.Time{}
		record(t, st, noFin)
		if got := runs(t, st, schedule.RunQuery{Job: "mdd"})[0]; !got.Finished.IsZero() {
			t.Errorf("Record без Finished: %v", got.Finished)
		}
	})

	t.Run("Invalid", func(t *testing.T) {
		st := newStore(t)
		good := sample("issue", t0, schedule.RunOK, 0)
		mod := func(f func(*schedule.Run)) schedule.Run { r := good; f(&r); return r }
		bad := map[string]schedule.Run{
			"без задания":       mod(func(r *schedule.Run) { r.Job = "" }),
			"без Started":       mod(func(r *schedule.Run) { r.Started = time.Time{} }),
			"без Scheduled":     mod(func(r *schedule.Run) { r.Scheduled = time.Time{} }),
			"неизвестный повод": mod(func(r *schedule.Run) { r.Trigger = "cron" }),
			"Started за 2262":   mod(func(r *schedule.Run) { r.Started = time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC) }),
		}
		for name, r := range bad {
			if _, err := st.Begin(ctx, r); err == nil {
				t.Errorf("Begin (%s): нет ошибки", name)
			}
			if _, err := st.Record(ctx, r); err == nil {
				t.Errorf("Record (%s): нет ошибки", name)
			}
		}
		for name, r := range map[string]schedule.Run{
			"статус running": mod(func(r *schedule.Run) { r.Status = schedule.RunRunning }),
			"пустой статус":  mod(func(r *schedule.Run) { r.Status = "" }),
			"расход NaN":     mod(func(r *schedule.Run) { r.CostUSD = math.NaN() }),
			"расход < 0":     mod(func(r *schedule.Run) { r.CostUSD = -0.1 }),
		} {
			if _, err := st.Record(ctx, r); err == nil {
				t.Errorf("Record (%s): нет ошибки", name)
			}
		}
		if list := runs(t, st, schedule.RunQuery{}); len(list) != 0 {
			t.Errorf("после отказов записано %d", len(list))
		}
	})

	t.Run("UniqueIDsConcurrent", func(t *testing.T) {
		st := newStore(t)
		const n = 20
		var (
			mu   sync.Mutex
			seen = map[int64]bool{}
			wg   sync.WaitGroup
		)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var (
					id  int64
					err error
				)
				r := sample(fmt.Sprintf("job%d", i%3), at(time.Duration(i)*time.Second), schedule.RunOK, 0)
				if i%2 == 0 {
					id, err = st.Begin(ctx, r)
				} else {
					id, err = st.Record(ctx, r)
				}
				if err != nil {
					t.Errorf("запись %d: %v", i, err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if id <= 0 || seen[id] {
					t.Errorf("запись %d: ID %d неверный или повторился", i, id)
				}
				seen[id] = true
			}()
		}
		wg.Wait()
		if list := runs(t, st, schedule.RunQuery{}); len(list) != n {
			t.Errorf("Runs: %d записей, ждали %d", len(list), n)
		}
	})

	t.Run("RunsOrderAndFilters", func(t *testing.T) {
		st := newStore(t)
		// Записи не по порядку Started; две — с одинаковым Started.
		a := record(t, st, sample("issue", at(2*time.Hour), schedule.RunOK, 0))
		b := record(t, st, sample("summary", at(0), schedule.RunFailed, 0))
		c := record(t, st, sample("issue", at(time.Hour), schedule.RunBudget, 0))
		d := record(t, st, sample("issue", at(time.Hour), schedule.RunSkipped, 0))
		e := begin(t, st, sample("mdd", at(3*time.Hour), "", 0))

		want := []int64{e, a, d, c, b}
		if got := ids(runs(t, st, schedule.RunQuery{})); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("порядок: %v, ждали %v", got, want)
		}
		cases := []struct {
			name string
			q    schedule.RunQuery
			want []int64
		}{
			{"Job", schedule.RunQuery{Job: "issue"}, []int64{a, d, c}},
			{"Job неизвестный", schedule.RunQuery{Job: "нет"}, []int64{}},
			{"Status", schedule.RunQuery{Status: []string{schedule.RunBudget, schedule.RunFailed}}, []int64{c, b}},
			{"Status running", schedule.RunQuery{Status: []string{schedule.RunRunning}}, []int64{e}},
			{"Since включительно", schedule.RunQuery{Since: at(time.Hour)}, []int64{e, a, d, c}},
			{"Until исключительно", schedule.RunQuery{Until: at(time.Hour)}, []int64{b}},
			{"Since+Until", schedule.RunQuery{Since: at(time.Hour), Until: at(2*time.Hour + 1)}, []int64{a, d, c}},
			{"всё вместе", schedule.RunQuery{Job: "issue", Status: []string{schedule.RunOK, schedule.RunSkipped}, Since: at(time.Hour), Until: at(3 * time.Hour)}, []int64{a, d}},
			{"Limit", schedule.RunQuery{Limit: 2}, []int64{e, a}},
		}
		for _, tc := range cases {
			if got := ids(runs(t, st, tc.q)); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("%s: %v, ждали %v", tc.name, got, tc.want)
			}
		}
	})

	t.Run("RunsLimit", func(t *testing.T) {
		st := newStore(t)
		const n = 510
		for i := range n {
			record(t, st, sample("issue", at(time.Duration(i)*time.Minute), schedule.RunOK, 0))
		}
		for _, tc := range []struct{ limit, want int }{
			{0, 50}, {-3, 50}, {7, 7}, {500, 500}, {501, 500}, {10000, 500},
		} {
			list := runs(t, st, schedule.RunQuery{Limit: tc.limit})
			if len(list) != tc.want {
				t.Errorf("Limit %d: %d записей, ждали %d", tc.limit, len(list), tc.want)
			}
			if len(list) > 0 && !list[0].Started.Equal(at((n-1)*time.Minute)) {
				t.Errorf("Limit %d: первая запись не самая новая: %v", tc.limit, list[0].Started)
			}
		}
	})

	t.Run("Last", func(t *testing.T) {
		st := newStore(t)
		if _, ok, _ := st.Last(ctx, "issue"); ok {
			t.Fatal("Last на пустом: ok")
		}
		old := record(t, st, sample("issue", at(0), schedule.RunOK, 0))
		// Пропуски не считаются, даже самые новые.
		record(t, st, sample("issue", at(time.Hour), schedule.RunBudget, 0))
		record(t, st, sample("issue", at(2*time.Hour), schedule.RunSkipped, 0))
		// Чужое задание не мешает.
		record(t, st, sample("summary", at(5*time.Hour), schedule.RunOK, 0))
		r, ok, err := st.Last(ctx, "issue")
		if err != nil || !ok || r.ID != old {
			t.Fatalf("Last: %+v ok=%v err=%v, ждали ID %d", r, ok, err, old)
		}
		if !r.Scheduled.Equal(at(-time.Second)) {
			t.Errorf("Last: Scheduled %v", r.Scheduled)
		}
		// failed и running — считаются.
		failed := record(t, st, sample("issue", at(3*time.Hour), schedule.RunFailed, 0))
		if r, _, _ := st.Last(ctx, "issue"); r.ID != failed {
			t.Errorf("Last после failed: ID %d, ждали %d", r.ID, failed)
		}
		running := begin(t, st, sample("issue", at(4*time.Hour), "", 0))
		if r, _, _ := st.Last(ctx, "issue"); r.ID != running || r.Status != schedule.RunRunning {
			t.Errorf("Last после Begin: %+v, ждали ID %d", r, running)
		}
		// При равном Started — больший ID.
		tie := record(t, st, sample("issue", at(4*time.Hour), schedule.RunOK, 0))
		if r, _, _ := st.Last(ctx, "issue"); r.ID != tie {
			t.Errorf("Last при равном Started: ID %d, ждали %d", r.ID, tie)
		}
		if _, ok, _ := st.Last(ctx, "mdd"); ok {
			t.Error("Last по заданию без запусков: ok")
		}
	})

	t.Run("Spent", func(t *testing.T) {
		st := newStore(t)
		record(t, st, sample("issue", at(0), schedule.RunOK, 0.25))
		record(t, st, sample("issue", at(time.Hour), schedule.RunFailed, 0.5)) // упавший тоже тратил
		record(t, st, sample("summary", at(2*time.Hour), schedule.RunOK, 1))
		record(t, st, sample("issue", at(time.Hour), schedule.RunBudget, 0))
		id := begin(t, st, sample("issue", at(3*time.Hour), "", 0))
		if err := st.Finish(ctx, schedule.Run{ID: id, Status: schedule.RunOK, Finished: at(4 * time.Hour),
			Outcome: schedule.Outcome{CostUSD: 2}}); err != nil {
			t.Fatal(err)
		}
		near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
		for _, tc := range []struct {
			name         string
			since, until time.Time
			want         float64
		}{
			{"всё", time.Time{}, time.Time{}, 3.75},
			{"since включительно", at(time.Hour), time.Time{}, 3.5},
			{"until исключительно", time.Time{}, at(time.Hour), 0.25},
			{"наносекунда до", at(-1), at(1), 0.25},
			{"пустой отрезок", at(0), at(0), 0},
			{"по Started, не Finished", at(3 * time.Hour), at(3*time.Hour + 1), 2},
		} {
			if got := spent(t, st, tc.since, tc.until); !near(got, tc.want) {
				t.Errorf("Spent %s: %v, ждали %v", tc.name, got, tc.want)
			}
		}
	})

	t.Run("Abandon", func(t *testing.T) {
		st := newStore(t)
		r1 := begin(t, st, sample("issue", at(0), "", 0))
		r2 := begin(t, st, sample("summary", at(time.Minute), "", 0))
		done := record(t, st, sample("mdd", at(0), schedule.RunOK, 0))
		stop := at(time.Hour + 5)
		n, err := st.Abandon(ctx, stop)
		if err != nil || n != 2 {
			t.Fatalf("Abandon: %d, %v; ждали 2", n, err)
		}
		for _, r := range runs(t, st, schedule.RunQuery{}) {
			switch r.ID {
			case r1, r2:
				if r.Status != schedule.RunFailed || r.Error == "" || !r.Finished.Equal(stop) {
					t.Errorf("брошенный %d: %+v", r.ID, r)
				}
			case done:
				if r.Status != schedule.RunOK || r.Error != "" {
					t.Errorf("завершённый %d тронут: %+v", r.ID, r)
				}
			}
		}
		if n, err := st.Abandon(ctx, stop); err != nil || n != 0 {
			t.Errorf("второй Abandon: %d, %v", n, err)
		}
		if err := st.Finish(ctx, schedule.Run{ID: r1, Status: schedule.RunOK, Finished: stop}); err == nil {
			t.Error("Finish после Abandon: нет ошибки")
		}
	})
}
