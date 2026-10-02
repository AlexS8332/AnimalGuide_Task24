package trivia_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia/triviatest"
)

// aggFakeRuns — журнал запусков: отдаёт заданные запуски как есть (в
// любом порядке) и запоминает период запроса.
type aggFakeRuns struct {
	runs     []trivia.RunInfo
	err      error
	from, to time.Time
	calls    int
}

func (f *aggFakeRuns) RunsBetween(_ context.Context, from, to time.Time) ([]trivia.RunInfo, error) {
	f.calls++
	f.from, f.to = from, to
	return f.runs, f.err
}

// aggPagedIssues — IssueStore, который запоминает запросы: по ним видно,
// что агрегат читает постранично с пределом 100.
type aggPagedIssues struct {
	trivia.IssueStore
	queries []trivia.IssueQuery
}

func (p *aggPagedIssues) Issues(ctx context.Context, q trivia.IssueQuery) ([]trivia.Issue, int, error) {
	p.queries = append(p.queries, q)
	return p.IssueStore.Issues(ctx, q)
}

// aggMSK — зона демона в тестах: сутки сводки с 00:00 по 00:00 МСК, то
// есть с 21:00 UTC предыдущего дня.
var aggMSK = time.FixedZone("MSK", 3*60*60)

func aggSave(t *testing.T, st *trivia.Memory, is trivia.Issue) {
	t.Helper()
	if _, err := st.SaveIssue(context.Background(), is); err != nil {
		t.Fatal(err)
	}
}

func aggSavePick(t *testing.T, st *trivia.Memory, p trivia.Pick) {
	t.Helper()
	if _, err := st.SavePick(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

// Все поля агрегата на небольшом, но полном наборе: три выпуска разных
// состояний, выпуски и выборы ровно на границах периода, журнал с
// успехами, сбоями, лимитом и сменой релиза MDD.
func TestAggregateAllFields(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2026, 9, 24, 0, 0, 0, 0, aggMSK)
	to := from.Add(24 * time.Hour)
	st := trivia.NewMemory()

	manul := triviatest.SampleIssue(1, 1, from) // ровно на from — входит
	panda := triviatest.SampleIssue(2, 2, from.Add(2*time.Hour))
	panda.SciName, panda.NameRu, panda.IUCN = "Ailurus fulgens", "Малая панда", "EN"
	panda.Realms = []string{"Indomalayan", "Palearctic", "Indomalayan"} // повтор области — один раз
	panda.Status = trivia.IssueThin
	panda.Title = "Малая панда"
	panda.Facts = []trivia.Fact{{Text: "  Малая панда ест бамбук.  ", Sources: []string{"S2"}}, {Text: "Второй факт.", Sources: []string{"S2"}}}
	panda.Dropped = append(panda.Dropped, trivia.Fact{Text: "x", Verdict: "нет в S2"})
	panda.Observations.Recent = 10
	panda.Observations.OutOfRange = nil
	beaver := trivia.Issue{PickID: 3, SpeciesID: 3, SciName: "Castor fiber", Order: "Rodentia",
		CreatedAt: from.Add(3 * time.Hour), Status: trivia.IssueFailed, Error: "досье: 503"}
	// Сохраняются не по времени; два — за границами периода.
	aggSave(t, st, beaver)
	aggSave(t, st, triviatest.SampleIssue(9, 9, to)) // ровно на to — не входит
	aggSave(t, st, panda)
	aggSave(t, st, triviatest.SampleIssue(8, 8, from.Add(-time.Nanosecond)))
	aggSave(t, st, manul)

	for i, at := range []time.Time{from.Add(3 * time.Hour), from, from.Add(-time.Nanosecond), to, from.Add(2 * time.Hour)} {
		aggSavePick(t, st, triviatest.SamplePick(i+1, at))
	}

	runs := &aggFakeRuns{runs: []trivia.RunInfo{
		{Job: "issue", Status: "ok", Started: from.Add(2 * time.Hour), CostUSD: 0.007},
		{Job: "mdd", Status: "ok", Started: from.Add(13 * time.Hour), Ref: "mdd:v2.6"},
		{Job: "issue", Status: "failed", Started: from.Add(3 * time.Hour), CostUSD: 0.001, Error: "досье: Википедия: 503"},
		{Job: "issue", Status: "ok", Started: from, CostUSD: 0.007},
		{Job: "mdd", Status: "ok", Started: from.Add(time.Hour), Ref: "mdd:v2.5"},
		{Job: "issue", Status: "budget", Started: from.Add(4 * time.Hour)},
		{Job: "mdd", Status: "failed", Started: from.Add(14 * time.Hour), Detail: "сеть"},
		{Job: "mdd", Status: "ok", Started: from.Add(15 * time.Hour), Ref: "mdd:v2.6"}, // тот же релиз — не событие
	}}
	ag := trivia.Aggregator{Issues: st, Picks: st, Runs: runs, Location: aggMSK}
	got, err := ag.Aggregate(ctx, from.UTC(), to.UTC()) // границы в UTC: зона Failures — из Location
	if err != nil {
		t.Fatal(err)
	}
	if !runs.from.Equal(from) || !runs.to.Equal(to) {
		t.Errorf("RunsBetween(%s, %s)", runs.from, runs.to)
	}

	c := func(kv ...any) []trivia.Count {
		var out []trivia.Count
		for i := 0; i < len(kv); i += 2 {
			out = append(out, trivia.Count{Key: kv[i].(string), Count: kv[i+1].(int)})
		}
		return out
	}
	want := trivia.Aggregate{
		From: from.UTC(), To: to.UTC(),
		Issues:   3,
		ByStatus: c("failed", 1, "ok", 1, "thin", 1),
		Species: []trivia.SpeciesLine{
			{IssueID: 5, SpeciesID: 1, SciName: "Otocolobus manul", NameRu: "Манул", IUCN: "LC", Order: "Carnivora",
				Status: "ok", Title: manul.Title, Facts: 3, Highlight: manul.Facts[0].Text, Recent: 97,
				OutOfRange: []string{"Germany"}},
			{IssueID: 3, SpeciesID: 2, SciName: "Ailurus fulgens", NameRu: "Малая панда", IUCN: "EN", Order: "Carnivora",
				Status: "thin", Title: "Малая панда", Facts: 2, Highlight: "Малая панда ест бамбук.", Recent: 10},
			{IssueID: 1, SpeciesID: 3, SciName: "Castor fiber", Order: "Rodentia", Status: "failed"},
		},
		ByOrder:            c("Carnivora", 2, "Rodentia", 1),
		ByIUCN:             c("EN", 1, "LC", 1, trivia.AggNoKey, 1),
		ByRealm:            c("Palearctic", 2, "Indomalayan", 1, trivia.AggNoKey, 1),
		Facts:              5,
		Dropped:            3,
		DroppedShare:       3.0 / 8,
		Picks:              3,
		Rejected:           6,
		RejectedByReason:   c(trivia.ReasonFewRecords, 3, trivia.ReasonNoArticle, 3),
		RecentObservations: 107,
		OutOfRangeSpecies:  []string{"Otocolobus manul: Germany"},
		// По убыванию числа, затем по ключу.
		Runs:        c("mdd/ok", 3, "issue/ok", 2, "issue/budget", 1, "issue/failed", 1, "mdd/failed", 1),
		Failures:    []string{"03:00 issue: досье: Википедия: 503", "14:00 mdd: сеть"},
		MDDRelease:  []string{"вышел релиз v2.6 (было v2.5)"},
		CostUSD:     0.015,
		BudgetSkips: 1,
	}
	if math.Abs(got.CostUSD-want.CostUSD) > 1e-12 {
		t.Errorf("CostUSD %v, ждали %v", got.CostUSD, want.CostUSD)
	}
	got.CostUSD = want.CostUSD
	aggDiff(t, got, want)
}

// aggDiff сравнивает агрегаты по полям: так в сообщении видно, какое
// поле разошлось.
func aggDiff(t *testing.T, got, want trivia.Aggregate) {
	t.Helper()
	if !got.From.Equal(want.From) || !got.To.Equal(want.To) {
		t.Errorf("период %s–%s, ждали %s–%s", got.From, got.To, want.From, want.To)
	}
	gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
	for i := 0; i < gv.NumField(); i++ {
		name := gv.Type().Field(i).Name
		if name == "From" || name == "To" {
			continue
		}
		if g, w := gv.Field(i).Interface(), wv.Field(i).Interface(); !reflect.DeepEqual(g, w) {
			t.Errorf("%s:\n got %+v\nwant %+v", name, g, w)
		}
	}
}

// Период без выпусков, выборов и запусков: все счётчики нулевые, разбивки
// пустые, журнал без строк. Runs == nil — журнал не читается.
func TestAggregateEmptyPeriod(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2026, 9, 24, 0, 0, 0, 0, aggMSK)
	st := trivia.NewMemory()
	aggSave(t, st, triviatest.SampleIssue(1, 1, from.Add(-time.Hour)))
	aggSavePick(t, st, triviatest.SamplePick(1, from.Add(-time.Hour)))

	for name, ag := range map[string]trivia.Aggregator{
		"без журнала":             {Issues: st, Picks: st},
		"пустой журнал":           {Issues: st, Picks: st, Runs: &aggFakeRuns{}},
		"пустое хранилище":        {Issues: trivia.NewMemory(), Picks: trivia.NewMemory()},
		"нулевая длина (to=from)": {Issues: st, Picks: st, Runs: &aggFakeRuns{}},
	} {
		to := from.Add(24 * time.Hour)
		if strings.HasPrefix(name, "нулевая") {
			to = from
		}
		got, err := ag.Aggregate(ctx, from, to)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		aggDiff(t, got, trivia.Aggregate{From: from, To: to})
		if !trivia.SummaryIsEmpty(got) {
			t.Errorf("%s: SummaryIsEmpty = false", name)
		}
	}
}

func TestAggregateErrors(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	st := trivia.NewMemory()
	boom := errors.New("журнал недоступен")
	cases := map[string]struct {
		ag       trivia.Aggregator
		from, to time.Time
	}{
		"нет хранилищ":        {trivia.Aggregator{}, from, from.Add(time.Hour)},
		"нулевое начало":      {trivia.Aggregator{Issues: st, Picks: st}, time.Time{}, from},
		"нулевой конец":       {trivia.Aggregator{Issues: st, Picks: st}, from, time.Time{}},
		"конец раньше начала": {trivia.Aggregator{Issues: st, Picks: st}, from, from.Add(-time.Nanosecond)},
		"ошибка журнала":      {trivia.Aggregator{Issues: st, Picks: st, Runs: &aggFakeRuns{err: boom}}, from, from.Add(time.Hour)},
	}
	for name, c := range cases {
		if _, err := c.ag.Aggregate(ctx, c.from, c.to); err == nil {
			t.Errorf("%s: нет ошибки", name)
		}
	}
	_, err := (&trivia.Aggregator{Issues: st, Picks: st, Runs: &aggFakeRuns{err: boom}}).Aggregate(ctx, from, from.Add(time.Hour))
	if !errors.Is(err, boom) {
		t.Errorf("ошибка журнала не обёрнута: %v", err)
	}
}

// Больше 100 выпусков — читаются все страницы; выборов больше первого окна
// PickStore, и ещё больше — после периода: окно растёт, пока не дойдёт до
// начала периода.
func TestAggregatePaging(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(30 * 24 * time.Hour)
	st := trivia.NewMemory()
	const n = 250
	for i := 0; i < n; i++ {
		at := from.Add(time.Duration(i) * time.Hour)
		aggSave(t, st, triviatest.SampleIssue(int64(i+1), i+1, at))
		aggSavePick(t, st, triviatest.SamplePick(i+1, at))
	}
	for i := 0; i < 150; i++ { // после периода — новее всех
		at := to.Add(time.Duration(i) * time.Minute)
		aggSave(t, st, triviatest.SampleIssue(int64(1000+i), 1000+i, at))
		aggSavePick(t, st, triviatest.SamplePick(1000+i, at))
	}
	aggSavePick(t, st, triviatest.SamplePick(5000, from.Add(-time.Hour))) // раньше периода

	paged := &aggPagedIssues{IssueStore: st}
	got, err := (&trivia.Aggregator{Issues: paged, Picks: st}).Aggregate(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if got.Issues != n || len(got.Species) != n || got.Picks != n || got.Rejected != 2*n {
		t.Fatalf("выпусков %d (строк %d), выборов %d, отвергнуто %d; ждали %d, %d, %d, %d",
			got.Issues, len(got.Species), got.Picks, got.Rejected, n, n, n, 2*n)
	}
	for i, sp := range got.Species {
		if sp.SpeciesID != i+1 {
			t.Fatalf("Species[%d] — вид %d: порядок не по времени выпуска", i, sp.SpeciesID)
		}
	}
	if len(paged.queries) != 3 {
		t.Errorf("страниц %d, ждали 3", len(paged.queries))
	}
	for i, q := range paged.queries {
		if q.Limit != 100 || q.Offset != 100*i || !q.Since.Equal(from) || !q.Until.Equal(to) {
			t.Errorf("запрос %d: %+v", i, q)
		}
	}
	if got.Facts != 3*n || got.RecentObservations != 97*n {
		t.Errorf("фактов %d, наблюдений %d", got.Facts, got.RecentObservations)
	}
	// Один вид, выпущенный много раз, — одна строка «вне ареала».
	if !reflect.DeepEqual(got.OutOfRangeSpecies, []string{"Otocolobus manul: Germany"}) {
		t.Errorf("OutOfRangeSpecies: %v", got.OutOfRangeSpecies)
	}
}

// Релиз MDD: первый успешный запуск периода сравнить не с чем — событие по
// «новый релиз» в Detail; смена Ref — событие; неуспешные запуски и
// запуски без Ref не сбивают «предыдущий» релиз. Период длиннее суток —
// у времени сбоя есть дата.
func TestAggregateMDDAndLongPeriod(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	runs := &aggFakeRuns{runs: []trivia.RunInfo{
		{Job: "mdd", Status: "ok", Started: from, Ref: "mdd:v2.5", Detail: "Новый релиз v2.5 (было v2.4)"},
		{Job: "mdd", Status: "ok", Started: from.Add(24 * time.Hour), Ref: "mdd:v2.5", Detail: "релиз тот же"},
		{Job: "mdd", Status: "ok", Started: from.Add(30 * time.Hour)}, // без Ref
		{Job: "mdd", Status: "failed", Started: from.Add(40 * time.Hour), Ref: "mdd:v9", Error: "таймаут"},
		{Job: "mdd", Status: "ok", Started: from.Add(48 * time.Hour), Ref: "mdd:v2.6"},
	}}
	st := trivia.NewMemory()
	got, err := (&trivia.Aggregator{Issues: st, Picks: st, Runs: runs, Location: time.UTC}).
		Aggregate(ctx, from, from.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	wantRel := []string{"Новый релиз v2.5 (было v2.4)", "вышел релиз v2.6 (было v2.5)"}
	if !reflect.DeepEqual(got.MDDRelease, wantRel) {
		t.Errorf("MDDRelease: %q, ждали %q", got.MDDRelease, wantRel)
	}
	if want := []string{"21.09 16:00 mdd: таймаут"}; !reflect.DeepEqual(got.Failures, want) {
		t.Errorf("Failures: %q, ждали %q", got.Failures, want)
	}
	if trivia.SummaryIsEmpty(got) {
		t.Error("SummaryIsEmpty = true при запусках без выпусков")
	}
}

// Location не задан — время сбоя в time.Local.
func TestAggregateDefaultLocation(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 9, 24, 12, 34, 0, 0, time.UTC)
	runs := &aggFakeRuns{runs: []trivia.RunInfo{{Job: "issue", Status: "failed", Started: at}}}
	st := trivia.NewMemory()
	got, err := (&trivia.Aggregator{Issues: st, Picks: st, Runs: runs}).Aggregate(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := at.In(time.Local).Format("15:04") + " issue: без текста ошибки"
	if len(got.Failures) != 1 || got.Failures[0] != want {
		t.Errorf("Failures: %q, ждали %q", got.Failures, want)
	}
}
