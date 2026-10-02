package triviatest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// SampleIssue — собранный выпуск о мануле (состояние ok): три
// подтверждённых факта со ссылками на материалы, один отброшенный
// проверяющим, источники без текста, наблюдения с отметкой вне ареала и
// расход двух шагов. speciesID подставляется как есть — текст всегда о
// мануле, чтобы поиск в тестах находил предсказуемое. Заполнены все поля
// Issue, кроме ID (его присваивает SaveIssue) и Error: потеря любого
// поля хранилищем будет заметна.
func SampleIssue(pickID int64, speciesID int, at time.Time) trivia.Issue {
	editor := trivia.Spend{
		Model: "deepseek-v4-pro", Requests: 1,
		Usage: llm.Usage{Prompt: 5200, Completion: 900, Total: 6100, CacheHit: 1024, CacheMiss: 4176, Reasoning: 300},
		Cost:  llm.Cost{USD: 0.0061, Tariff: llm.TariffPeak, Known: true},
		Took:  14*time.Second + 250*time.Millisecond,
	}
	verifier := trivia.Spend{
		Model: "deepseek-v4-flash", Requests: 1,
		Usage: llm.Usage{Prompt: 4800, Completion: 350, Total: 5150, CacheMiss: 4800, Reasoning: 120},
		Cost:  llm.Cost{USD: 0.0009, Tariff: llm.TariffPeak, Known: true},
		Took:  6*time.Second + 500*time.Millisecond,
	}
	return trivia.Issue{
		PickID:    pickID,
		SpeciesID: speciesID,
		SciName:   "Otocolobus manul",
		NameRu:    "Манул",
		IUCN:      "LC",
		Order:     "Carnivora",
		Family:    "Felidae",
		Realms:    []string{"Palearctic"},
		CreatedAt: at,
		Status:    trivia.IssueOK,
		Title:     "Манул: кот с круглыми зрачками",
		Lead:      "Степной кот размером с домашнюю кошку, который выглядит вдвое крупнее из-за меха.",
		Facts: []trivia.Fact{
			{Text: "У манула круглые зрачки — в отличие от вертикальных щелей у домашней кошки.", Sources: []string{"S2"}},
			{Text: "Манул живёт в горных степях и полупустынях Центральной Азии, поднимаясь до 5000 м.", Sources: []string{"S1", "S2"}},
			{Text: "Вид описан Петером Палласом в 1776 году по зверю, добытому к югу от Байкала.", Sources: []string{"S1"}},
		},
		Dropped: []trivia.Fact{
			{Text: "Манул плавает лучше тигра.", Sources: []string{"S2"},
				Verdict: "в статье нет ничего о плавании"},
		},
		Sources: []trivia.Material{
			{ID: "S1", Kind: trivia.KindMDD, Title: "Mammal Diversity Database: Otocolobus manul",
				URL: "https://www.mammaldiversity.org/taxon/1006010", Tool: "mdd_get"},
			{ID: "S2", Kind: trivia.KindWikipedia, Title: "Манул — Википедия",
				URL: "https://ru.wikipedia.org/wiki/%D0%9C%D0%B0%D0%BD%D1%83%D0%BB", Tool: "read_wikipedia"},
			{ID: "S3", Kind: trivia.KindGBIF, Title: "GBIF: наблюдения Otocolobus manul",
				URL: "https://www.gbif.org/species/2435022", Tool: "gbif occurrence facet"},
		},
		Observations: trivia.Observations{
			GBIFKey: 2435022, Total: 1843, WindowDays: 365, Recent: 97,
			ByCountry: []trivia.CountryCount{
				{Code: "MN", Name: "Mongolia", Count: 812, Range: trivia.RangeIn},
				{Code: "CN", Name: "China", Count: 403, Range: trivia.RangeIn},
				{Code: "RU", Name: "Russia", Count: 377, Range: trivia.RangeIn},
				{Code: "TJ", Name: "Tajikistan", Count: 41, Range: trivia.RangeUncertain},
				{Code: "DE", Name: "Germany", Count: 18, Range: trivia.RangeOut},
			},
			RecentByCountry: []trivia.CountryCount{
				{Code: "MN", Name: "Mongolia", Count: 51, Range: trivia.RangeIn},
				{Code: "RU", Name: "Russia", Count: 30, Range: trivia.RangeIn},
			},
			OutOfRange: []trivia.CountryCount{
				{Code: "DE", Name: "Germany", Count: 18, Range: trivia.RangeOut},
			},
		},
		Spend: []trivia.Spend{editor, verifier},
		Cost:  editor.Cost.Add(verifier.Cost),
		Took:  38*time.Second + 125*time.Millisecond,
	}
}

// IssueStoreConformance проверяет контракт trivia.IssueStore на любой
// реализации. Хранилище — заодно и PickStore: IssueStore.Pick читает выборы,
// сохранённые SavePick. newStore каждый раз отдаёт новое пустое хранилище.
//
// Кроме обещанного интерфейсом, проверяется то, что обе реализации пакета
// делают одинаково: наносекундная точность CreatedAt, отказ сохранять
// выпуск с нулевым временем или неизвестным состоянием.
func IssueStoreConformance(t *testing.T, newStore func(t *testing.T) interface {
	trivia.IssueStore
	trivia.PickStore
}) {
	ctx := context.Background()
	// Как у выборов: наносекунды и не UTC.
	zone := time.FixedZone("UTC+7", 7*60*60)
	t0 := time.Date(2026, 9, 24, 15, 4, 5, 123456789, zone)

	type store = interface {
		trivia.IssueStore
		trivia.PickStore
	}
	save := func(t *testing.T, st store, is trivia.Issue) int64 {
		t.Helper()
		id, err := st.SaveIssue(ctx, is)
		if err != nil {
			t.Fatalf("SaveIssue(вид %d): %v", is.SpeciesID, err)
		}
		return id
	}
	get := func(t *testing.T, st store, id int64) trivia.Issue {
		t.Helper()
		is, err := st.Issue(ctx, id)
		if err != nil {
			t.Fatalf("Issue(%d): %v", id, err)
		}
		return is
	}
	query := func(t *testing.T, st store, q trivia.IssueQuery) ([]int64, int) {
		t.Helper()
		list, total, err := st.Issues(ctx, q)
		if err != nil {
			t.Fatalf("Issues(%+v): %v", q, err)
		}
		return issueIDs(list), total
	}

	t.Run("Empty", func(t *testing.T) {
		st := newStore(t)
		list, total, err := st.Issues(ctx, trivia.IssueQuery{})
		if err != nil || len(list) != 0 || total != 0 {
			t.Errorf("Issues на пустом: %d выпусков, total %d, %v", len(list), total, err)
		}
		if list == nil {
			t.Error("Issues на пустом: nil вместо пустого списка")
		}
		if _, err := st.Issue(ctx, 1); !errors.Is(err, trivia.ErrIssueNotFound) {
			t.Errorf("Issue(1) на пустом: %v, ждали ErrIssueNotFound", err)
		}
		if _, err := st.Pick(ctx, 1); !errors.Is(err, trivia.ErrIssueNotFound) {
			t.Errorf("Pick(1) на пустом: %v, ждали ErrIssueNotFound", err)
		}
	})

	t.Run("SaveIssueID", func(t *testing.T) {
		st := newStore(t)
		seen := map[int64]bool{}
		for i := 0; i < 3; i++ {
			is := SampleIssue(1, 10, t0.Add(time.Duration(i)*time.Hour))
			is.ID = 777 // входной ID игнорируется
			id := save(t, st, is)
			if id <= 0 || seen[id] {
				t.Errorf("SaveIssue: ID %d (уже выданы %v)", id, seen)
			}
			seen[id] = true
			if got := get(t, st, id); got.ID != id {
				t.Errorf("Issue(%d).ID = %d", id, got.ID)
			}
		}
		// Выбор, сохранённый между выпусками, не приводит к повтору ID выпуска.
		if _, err := st.SavePick(ctx, SamplePick(10, t0)); err != nil {
			t.Fatal(err)
		}
		id := save(t, st, SampleIssue(1, 10, t0))
		if seen[id] {
			t.Errorf("SaveIssue после SavePick: ID %d повторился", id)
		}
	})

	t.Run("SaveIssueInvalid", func(t *testing.T) {
		st := newStore(t)
		cases := map[string]trivia.Issue{}
		is := SampleIssue(0, 5, t0)
		cases["без PickID"] = is
		is = SampleIssue(-1, 5, t0)
		cases["отрицательный PickID"] = is
		is = SampleIssue(1, 0, t0)
		cases["без SpeciesID"] = is
		is = SampleIssue(1, 5, time.Time{})
		cases["нулевое время"] = is
		is = SampleIssue(1, 5, t0)
		is.Status = ""
		cases["без состояния"] = is
		is = SampleIssue(1, 5, t0)
		is.Status = "OK"
		cases["неизвестное состояние"] = is
		for name, is := range cases {
			if _, err := st.SaveIssue(ctx, is); err == nil {
				t.Errorf("SaveIssue %s: нет ошибки", name)
			}
		}
		if _, total := query(t, st, trivia.IssueQuery{}); total != 0 {
			t.Errorf("после отказов сохранено %d выпусков", total)
		}
	})

	t.Run("RoundTrip", func(t *testing.T) {
		st := newStore(t)
		want := SampleIssue(3, 42, t0)
		want.ID = save(t, st, want)
		if msg := diffIssue(get(t, st, want.ID), want); msg != "" {
			t.Errorf("Issue: %s", msg)
		}
		list, total, err := st.Issues(ctx, trivia.IssueQuery{})
		if err != nil || total != 1 || len(list) != 1 {
			t.Fatalf("Issues: %d выпусков, total %d, %v", len(list), total, err)
		}
		if msg := diffIssue(list[0], want); msg != "" {
			t.Errorf("Issues: %s", msg)
		}
	})

	t.Run("RoundTripFailed", func(t *testing.T) {
		// Сбой на досье: ни фактов, ни источников, ни расхода.
		st := newStore(t)
		want := trivia.Issue{PickID: 4, SpeciesID: 43, SciName: "Castor fiber", CreatedAt: t0,
			Status: trivia.IssueFailed, Error: "досье: Википедия: 503 Service Unavailable",
			Took: 3 * time.Second}
		want.ID = save(t, st, want)
		if msg := diffIssue(get(t, st, want.ID), want); msg != "" {
			t.Error(msg)
		}
	})

	t.Run("RoundTripEmptySlices", func(t *testing.T) {
		// Пустой срез и nil — одно и то же и после сохранения.
		st := newStore(t)
		want := SampleIssue(5, 44, t0)
		want.Status = trivia.IssueThin
		want.Facts = want.Facts[:1]
		want.Dropped = []trivia.Fact{}
		want.Realms = []string{}
		want.Observations.OutOfRange = []trivia.CountryCount{}
		want.ID = save(t, st, want)
		if msg := diffIssue(get(t, st, want.ID), want); msg != "" {
			t.Error(msg)
		}
	})

	t.Run("Copies", func(t *testing.T) {
		st := newStore(t)
		in := SampleIssue(1, 7, t0)
		want := SampleIssue(1, 7, t0)
		want.ID = save(t, st, in)

		// Вызывающий меняет свой выпуск после сохранения.
		in.Facts[0].Text = "испорчено"
		in.Facts[0].Sources[0] = "S9"
		in.Dropped[0].Verdict = "испорчено"
		in.Sources[0].URL = "испорчено"
		in.Realms[0] = "испорчено"
		in.Spend[0].Model = "испорчено"
		in.Observations.ByCountry[0].Count = -1
		in.Observations.OutOfRange[0].Name = "испорчено"
		in.Observations.RecentByCountry[0].Code = "XX"

		got := get(t, st, want.ID)
		if msg := diffIssue(got, want); msg != "" {
			t.Fatalf("после правки входа: %s", msg)
		}
		// Получатель меняет выданное.
		got.Facts[0].Sources[0] = "S9"
		got.Facts[1].Text = "испорчено"
		got.Sources[1].Title = "испорчено"
		got.Realms[0] = "испорчено"
		got.Spend[1].Usage.Total = -1
		got.Observations.ByCountry[1].Range = "испорчено"
		if msg := diffIssue(get(t, st, want.ID), want); msg != "" {
			t.Errorf("после правки выданного Issue: %s", msg)
		}
		list, _, err := st.Issues(ctx, trivia.IssueQuery{})
		if err != nil {
			t.Fatal(err)
		}
		list[0].Dropped[0].Text = "испорчено"
		list[0].Observations.OutOfRange[0].Count = 0
		if msg := diffIssue(get(t, st, want.ID), want); msg != "" {
			t.Errorf("после правки выданного Issues: %s", msg)
		}
	})

	t.Run("Order", func(t *testing.T) {
		st := newStore(t)
		// Сохраняются не по времени; два выпуска — в одно и то же время.
		a := save(t, st, SampleIssue(1, 1, t0))
		b := save(t, st, SampleIssue(2, 2, t0.Add(time.Hour)))
		c := save(t, st, SampleIssue(3, 3, t0)) // то же время, что a, но позже сохранён
		d := save(t, st, SampleIssue(4, 4, t0.Add(-time.Hour)))
		e := save(t, st, SampleIssue(5, 5, t0.Add(time.Nanosecond)))
		want := []int64{b, e, c, a, d}
		if got, total := query(t, st, trivia.IssueQuery{}); !reflect.DeepEqual(got, want) || total != 5 {
			t.Errorf("Issues: %v, total %d; ждали %v, 5", got, total, want)
		}
		for _, c := range []struct{ limit, offset int }{{1, 0}, {2, 1}, {3, 3}, {10, 4}} {
			got, total := query(t, st, trivia.IssueQuery{Limit: c.limit, Offset: c.offset})
			end := min(c.offset+c.limit, len(want))
			if !reflect.DeepEqual(got, want[c.offset:end]) || total != 5 {
				t.Errorf("Limit %d Offset %d: %v, total %d; ждали %v, 5", c.limit, c.offset, got, total, want[c.offset:end])
			}
		}
		if got, total := query(t, st, trivia.IssueQuery{Offset: 5}); len(got) != 0 || total != 5 {
			t.Errorf("Offset за концом: %v, total %d; ждали пусто, 5", got, total)
		}
		if got, total := query(t, st, trivia.IssueQuery{Offset: -3}); !reflect.DeepEqual(got, want) || total != 5 {
			t.Errorf("отрицательный Offset: %v, total %d", got, total)
		}
	})

	t.Run("Limit", func(t *testing.T) {
		st := newStore(t)
		const n = 105
		for i := 0; i < n; i++ {
			save(t, st, SampleIssue(int64(i+1), i+1, t0.Add(time.Duration(i)*time.Minute)))
		}
		for _, c := range []struct{ limit, want int }{{0, 20}, {-1, 20}, {7, 7}, {100, 100}, {101, 100}, {1000, 100}} {
			got, total := query(t, st, trivia.IssueQuery{Limit: c.limit})
			if len(got) != c.want || total != n {
				t.Errorf("Limit %d: %d выпусков, total %d; ждали %d, %d", c.limit, len(got), total, c.want, n)
			}
		}
		got, total := query(t, st, trivia.IssueQuery{Limit: 100, Offset: 100})
		if len(got) != 5 || total != n {
			t.Errorf("последняя страница: %d выпусков, total %d; ждали 5, %d", len(got), total, n)
		}
	})

	t.Run("Filters", func(t *testing.T) {
		st := newStore(t)
		manul := save(t, st, SampleIssue(1, 100, t0))

		beaver := SampleIssue(2, 200, t0.Add(time.Hour))
		beaver.SciName, beaver.NameRu, beaver.Order, beaver.Family = "Castor fiber", "Обыкновенный бобр", "Rodentia", "Castoridae"
		beaver.Status = trivia.IssueThin
		beaver.Title = "Бобр строит плотины"
		beaver.Lead = "Крупнейший грызун Евразии; Ёмкость его хатки удивляет."
		beaver.Facts = []trivia.Fact{{Text: "Резцы бобра покрыты железистой ЭМАЛЬЮ оранжевого цвета.", Sources: []string{"S2"}}}
		beaver.Dropped = []trivia.Fact{{Text: "Бобр — родственник выдры.", Sources: []string{"S2"}, Verdict: "неверно"}}
		bobr := save(t, st, beaver)

		failed := trivia.Issue{PickID: 3, SpeciesID: 100, SciName: "Otocolobus manul",
			CreatedAt: t0.Add(2 * time.Hour), Status: trivia.IssueFailed, Error: "редактор: таймаут"}
		fail := save(t, st, failed)

		old := save(t, st, SampleIssue(4, 300, t0.Add(-24*time.Hour)))

		cases := []struct {
			name string
			q    trivia.IssueQuery
			want []int64
		}{
			{"без фильтров", trivia.IssueQuery{}, []int64{fail, bobr, manul, old}},
			{"вид", trivia.IssueQuery{SpeciesID: 100}, []int64{fail, manul}},
			{"вид без выпусков", trivia.IssueQuery{SpeciesID: 999}, nil},
			{"состояние", trivia.IssueQuery{Status: []string{trivia.IssueFailed}}, []int64{fail}},
			{"несколько состояний", trivia.IssueQuery{Status: []string{trivia.IssueOK, trivia.IssueThin}}, []int64{bobr, manul, old}},
			{"неизвестное состояние", trivia.IssueQuery{Status: []string{"nope"}}, nil},
			{"Since ровно на выпуске", trivia.IssueQuery{Since: t0}, []int64{fail, bobr, manul}},
			{"Since в UTC", trivia.IssueQuery{Since: t0.UTC()}, []int64{fail, bobr, manul}},
			{"Since на 1 нс позже", trivia.IssueQuery{Since: t0.Add(time.Nanosecond)}, []int64{fail, bobr}},
			{"Until ровно на выпуске — не включая", trivia.IssueQuery{Until: t0}, []int64{old}},
			{"Until на 1 нс позже", trivia.IssueQuery{Until: t0.Add(time.Nanosecond)}, []int64{manul, old}},
			{"Since и Until", trivia.IssueQuery{Since: t0, Until: t0.Add(2 * time.Hour)}, []int64{bobr, manul}},
			{"пустой интервал", trivia.IssueQuery{Since: t0.Add(time.Hour), Until: t0}, nil},
			{"текст: латынь без регистра", trivia.IssueQuery{Text: "OTOCOLOBUS"}, []int64{fail, manul, old}},
			{"текст: кириллица верхним регистром", trivia.IssueQuery{Text: "МАНУЛ"}, []int64{manul, old}},
			{"текст: кириллица нижним регистром", trivia.IssueQuery{Text: "манул"}, []int64{manul, old}},
			{"текст: заголовок", trivia.IssueQuery{Text: "строит ПЛОТИНЫ"}, []int64{bobr}},
			{"текст: вступление, Ё", trivia.IssueQuery{Text: "ёмкость"}, []int64{bobr}},
			{"текст: факт", trivia.IssueQuery{Text: "эмалью"}, []int64{bobr}},
			{"текст: факт другого выпуска", trivia.IssueQuery{Text: "КРУГЛЫЕ ЗРАЧКИ"}, []int64{manul, old}},
			{"текст с пробелами по краям", trivia.IssueQuery{Text: "  бобр  "}, []int64{bobr}},
			{"текст: символы LIKE — обычные", trivia.IssueQuery{Text: "%"}, nil},
			{"текст: ничего", trivia.IssueQuery{Text: "утконос"}, nil},
			{"всё вместе", trivia.IssueQuery{Text: "манул", SpeciesID: 100, Status: []string{trivia.IssueOK},
				Since: t0.Add(-time.Hour), Until: t0.Add(time.Hour)}, []int64{manul}},
		}
		for _, c := range cases {
			got, total := query(t, st, c.q)
			if len(got) == 0 {
				got = nil
			}
			if !reflect.DeepEqual(got, c.want) || total != len(c.want) {
				t.Errorf("%s: %v, total %d; ждали %v, %d", c.name, got, total, c.want, len(c.want))
			}
		}
	})

	t.Run("PickByID", func(t *testing.T) {
		st := newStore(t)
		want := SamplePick(42, t0)
		want.ID, _ = st.SavePick(ctx, want)
		other := SamplePick(43, t0.Add(time.Hour))
		other.ID, _ = st.SavePick(ctx, other)
		if want.ID == 0 || other.ID == 0 {
			t.Fatal("SavePick не сохранил выбор")
		}
		for _, w := range []trivia.Pick{want, other} {
			got, err := st.Pick(ctx, w.ID)
			if err != nil {
				t.Fatalf("Pick(%d): %v", w.ID, err)
			}
			if msg := diffPick(got, w); msg != "" {
				t.Error(msg)
			}
		}
		// Выданный выбор — копия.
		got, _ := st.Pick(ctx, want.ID)
		got.Rejected[0].Reason = "испорчено"
		got.Weights["LC"] = -1
		again, _ := st.Pick(ctx, want.ID)
		if msg := diffPick(again, want); msg != "" {
			t.Errorf("после правки выданного: %s", msg)
		}
		for _, id := range []int64{0, -1, other.ID + 100} {
			if _, err := st.Pick(ctx, id); !errors.Is(err, trivia.ErrIssueNotFound) {
				t.Errorf("Pick(%d): %v, ждали ErrIssueNotFound", id, err)
			}
		}
		// ID выпуска — не ID выбора.
		isID := save(t, st, SampleIssue(want.ID, 42, t0))
		if _, err := st.Issue(ctx, isID+100); !errors.Is(err, trivia.ErrIssueNotFound) {
			t.Errorf("Issue(%d): %v, ждали ErrIssueNotFound", isID+100, err)
		}
	})

	t.Run("ConcurrentSaveIssue", func(t *testing.T) {
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
					id, err := st.SaveIssue(ctx, SampleIssue(int64(w+1), w*100+i+1, t0.Add(time.Duration(i)*time.Second)))
					mu.Lock()
					if err != nil {
						errs = append(errs, err)
					} else if ids[id] {
						errs = append(errs, fmt.Errorf("ID %d выдан дважды", id))
					} else {
						ids[id] = true
					}
					mu.Unlock()
					// Чтение вперемешку с записью.
					if _, _, err := st.Issues(ctx, trivia.IssueQuery{Text: "манул"}); err != nil {
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
		if _, total := query(t, st, trivia.IssueQuery{}); total != workers*each {
			t.Errorf("Issues: total %d, ждали %d", total, workers*each)
		}
	})
}

func issueIDs(list []trivia.Issue) []int64 {
	ids := make([]int64, len(list))
	for i, is := range list {
		ids[i] = is.ID
	}
	return ids
}

// diffIssue — расхождение выпусков; пусто — совпали. CreatedAt сравнивается
// через Equal (зона и монотонные часы не сохраняются), остальное —
// reflect.DeepEqual после нормализации, где пустой срез равен nil.
func diffIssue(got, want trivia.Issue) string {
	if !got.CreatedAt.Equal(want.CreatedAt) {
		return fmt.Sprintf("выпуск %d: CreatedAt %s, ждали %s", want.ID,
			got.CreatedAt.Format(time.RFC3339Nano), want.CreatedAt.Format(time.RFC3339Nano))
	}
	got, want = normIssue(got), normIssue(want)
	if reflect.DeepEqual(got, want) {
		return ""
	}
	return fmt.Sprintf("выпуск %d:\n got %+v\nwant %+v", want.ID, got, want)
}

func normIssue(is trivia.Issue) trivia.Issue {
	is.CreatedAt = time.Time{}
	is.Realms = normSlice(is.Realms)
	is.Facts = normFacts(is.Facts)
	is.Dropped = normFacts(is.Dropped)
	is.Sources = normSlice(is.Sources)
	is.Spend = normSlice(is.Spend)
	is.Observations.ByCountry = normSlice(is.Observations.ByCountry)
	is.Observations.RecentByCountry = normSlice(is.Observations.RecentByCountry)
	is.Observations.OutOfRange = normSlice(is.Observations.OutOfRange)
	return is
}

func normSlice[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return append([]T(nil), s...)
}

func normFacts(s []trivia.Fact) []trivia.Fact {
	s = normSlice(s)
	for i := range s {
		s[i].Sources = normSlice(s[i].Sources)
	}
	return s
}
