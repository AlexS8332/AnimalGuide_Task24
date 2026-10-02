package trivia

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
)

var pickNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// pickFacts — что «знает сеть» о виде в фейковом Checker.
type pickFacts struct {
	occ       int
	noArticle bool
	err       error
}

// pickFakeChecker — Checker на карте фактов; вид без записи — со статьёй и
// 1000 наблюдений. Считает вызовы.
type pickFakeChecker struct {
	mu    sync.Mutex
	facts map[int]pickFacts
	calls map[int]int
	total int
	hook  func(ctx context.Context) // перед ответом, если задан
}

func newPickFakeChecker() *pickFakeChecker {
	return &pickFakeChecker{facts: map[int]pickFacts{}, calls: map[int]int{}}
}

func (c *pickFakeChecker) Check(ctx context.Context, sp mdd.Species, minOcc int) (Eligibility, error) {
	c.mu.Lock()
	c.calls[sp.ID]++
	c.total++
	f, ok := c.facts[sp.ID]
	hook := c.hook
	c.mu.Unlock()
	if hook != nil {
		hook(ctx)
		if err := ctx.Err(); err != nil {
			return Eligibility{}, err
		}
	}
	if !ok {
		f = pickFacts{occ: 1000}
	}
	if f.err != nil {
		return Eligibility{}, f.err
	}
	e := Eligibility{SpeciesID: sp.ID, SciName: sp.SciName, Occurrences: f.occ, GBIFKey: sp.ID}
	switch {
	case f.noArticle:
		e.Reason = ReasonNoArticle
		return e, nil
	}
	e.WikiLang, e.WikiTitle, e.EnTitle = "en", sp.SciName, sp.SciName
	if f.occ < minOcc {
		e.Reason = ReasonFewRecords
		return e, nil
	}
	e.OK = true
	return e, nil
}

func (c *pickFakeChecker) callsOf(id int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[id]
}

func (c *pickFakeChecker) totalCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// pickFakeStore — PickStore в памяти для тестов Picker.
type pickFakeStore struct {
	mu     sync.Mutex
	picks  []Pick
	checks map[int]Eligibility
}

func newPickFakeStore() *pickFakeStore { return &pickFakeStore{checks: map[int]Eligibility{}} }

func (s *pickFakeStore) SavePick(ctx context.Context, p Pick) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.ID = int64(len(s.picks) + 1)
	s.picks = append(s.picks, p)
	return p.ID, nil
}

func (s *pickFakeStore) RecentSpecies(ctx context.Context, since time.Time) ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int
	for _, p := range s.picks {
		if !p.PickedAt.Before(since) && !slices.Contains(ids, p.SpeciesID) {
			ids = append(ids, p.SpeciesID)
		}
	}
	return ids, nil
}

func (s *pickFakeStore) Picks(ctx context.Context, limit int) ([]Pick, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.picks)
	slices.Reverse(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *pickFakeStore) CachedCheck(ctx context.Context, id int, notBefore time.Time) (Eligibility, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.checks[id]
	if !ok || e.CheckedAt.Before(notBefore) {
		return Eligibility{}, false, nil
	}
	return e, true, nil
}

func (s *pickFakeStore) SaveCheck(ctx context.Context, e Eligibility) error {
	if e.Reason == ReasonCheckFailed {
		return errors.New("проверку с check_failed сохранять нельзя")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks[e.SpeciesID] = e
	return nil
}

func (s *pickFakeStore) cached(id int) (Eligibility, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.checks[id]
	return e, ok
}

// pickDataset — синтетический справочник: counts — число недомашних видов
// по коду МСОП (ключ "" — без кода), domestic — домашних (LC). ID идут
// подряд с 1 в порядке отсортированных кодов.
func pickDataset(counts map[string]int, domestic int) *mdd.Dataset {
	codes := make([]string, 0, len(counts))
	for c := range counts {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	var list []mdd.Species
	add := func(code string, dom bool) {
		id := len(list) + 1
		list = append(list, mdd.Species{ID: id, Phylosort: id, SciName: fmt.Sprintf("Genus%d species%d", id, id),
			Order: "Rodentia", Family: "Muridae", Genus: fmt.Sprintf("Genus%d", id), Epithet: fmt.Sprintf("species%d", id),
			IUCN: code, Domestic: dom})
	}
	for _, c := range codes {
		for range counts[c] {
			add(c, false)
		}
	}
	for range domestic {
		add("LC", true)
	}
	return &mdd.Dataset{Release: mdd.Release{Version: "test"}, Species: list}
}

func pickMemory(t *testing.T, d *mdd.Dataset) *mdd.Memory {
	t.Helper()
	m := mdd.NewMemory()
	if err := m.Replace(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return m
}

func newPickTestPicker(species mdd.Store, c Checker, s PickStore, opt PickOptions, seed uint64) *Picker {
	return &Picker{Species: species, Checker: c, Store: s, Options: opt,
		Rand: rand.New(rand.NewPCG(seed, 2)), Now: func() time.Time { return pickNow }}
}

func TestPickSample(t *testing.T) {
	ctx := context.Background()
	c, s := newPickFakeChecker(), newPickFakeStore()
	p := newPickTestPicker(pickMemory(t, mddtest.Sample()), c, s, PickOptions{}, 1)
	got, err := p.Pick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 1 || got.SpeciesID == 0 || got.SciName == "" || !got.PickedAt.Equal(pickNow) {
		t.Errorf("Pick заполнен не полностью: %+v", got)
	}
	if !got.Eligibility.OK || got.Eligibility.SpeciesID != got.SpeciesID {
		t.Errorf("Eligibility = %+v", got.Eligibility)
	}
	if got.Attempts != len(got.Rejected)+1 {
		t.Errorf("Attempts = %d, отказов %d", got.Attempts, len(got.Rejected))
	}
	sum := 0.0
	for _, v := range got.Weights {
		sum += v
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("сумма Weights = %v: %v", sum, got.Weights)
	}
	// В Sample (без кошки): NT, EX, NE, VU×3, LC×3, CR. Ожидаемые доли с
	// весами по умолчанию: Σ = 1.5+2+1+6+3+4 = 17.5.
	want := WeightsReport{"NT": 1.5 / 17.5, "EX": 2 / 17.5, "NE": 1 / 17.5, "VU": 6 / 17.5, "LC": 3 / 17.5, "CR": 4 / 17.5}
	if len(got.Weights) != len(want) {
		t.Errorf("Weights = %v, want %v", got.Weights, want)
	}
	for k, v := range want {
		if math.Abs(got.Weights[k]-v) > 1e-9 {
			t.Errorf("Weights[%s] = %v, want %v", k, got.Weights[k], v)
		}
	}
	if picks, _ := s.Picks(ctx, 0); len(picks) != 1 || picks[0].SpeciesID != got.SpeciesID {
		t.Errorf("в хранилище %+v", picks)
	}
	if _, ok := s.cached(got.SpeciesID); !ok {
		t.Error("проверка выбранного вида не закэширована")
	}
}

func TestPickDeterministic(t *testing.T) {
	run := func() []int {
		c, s := newPickFakeChecker(), newPickFakeStore()
		c.facts[mddtest.Lion] = pickFacts{occ: 10}
		c.facts[mddtest.Manul] = pickFacts{noArticle: true}
		now := pickNow
		p := &Picker{Species: pickMemory(t, mddtest.Sample()), Checker: c, Store: s,
			Rand: rand.New(rand.NewPCG(1, 2)), Now: func() time.Time { return now }}
		var ids []int
		for range 8 {
			got, err := p.Pick(context.Background())
			if err != nil {
				ids = append(ids, -1)
			} else {
				ids = append(ids, got.SpeciesID)
			}
			now = now.Add(time.Hour)
		}
		return ids
	}
	a, b := run(), run()
	if !slices.Equal(a, b) {
		t.Fatalf("при одном seed разные выборы: %v и %v", a, b)
	}
	// 10 недомашних видов, двое непригодны, NoRepeat 30 дней: восемь
	// выборов подряд — восемь разных пригодных видов.
	seen := map[int]bool{}
	for _, id := range a {
		if id <= 0 || seen[id] || id == mddtest.Lion || id == mddtest.Manul || id == mddtest.DomesticCat {
			t.Fatalf("выборы %v: повтор, непригодный или домашний вид", a)
		}
		seen[id] = true
	}
}

// Статистика розыгрыша без проверок: доли статусов сходятся с
// вес×число/Σ, внутри корзины вид равновероятен, домашние не выпадают.
func TestPickDistribution(t *testing.T) {
	ctx := context.Background()
	counts := map[string]int{"LC": 300, "NT": 50, "VU": 40, "EN": 30, "CR": 20, "EW": 2, "EX": 10,
		"DD": 60, "NE": 20, "": 15, "LR": 5}
	species := pickMemory(t, pickDataset(counts, 30))
	opt, err := pickOptions(PickOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := pickMakePlan(ctx, species, opt.Weights)
	if err != nil {
		t.Fatal(err)
	}
	if plan.all != 552 {
		t.Fatalf("недомашних %d, want 552", plan.all)
	}

	// Ожидаемые доли: пустой код и неизвестный LR — корзина OtherStatus
	// (20 видов, вес 1).
	bucketCount := map[string]int{OtherStatus: 20}
	for c, n := range counts {
		if c != "" && c != "LR" {
			bucketCount[c] = n
		}
	}
	mass := 0.0
	for c, n := range bucketCount {
		w, ok := DefaultWeights[c]
		if !ok {
			w = 1
		}
		mass += w * float64(n)
	}
	report := plan.report()
	for c, n := range bucketCount {
		w, ok := DefaultWeights[c]
		if !ok {
			w = 1
		}
		if want := w * float64(n) / mass; math.Abs(report[c]-want) > 1e-9 {
			t.Errorf("report[%s] = %v, want %v", c, report[c], want)
		}
	}

	const N = 20000
	rng := rand.New(rand.NewPCG(1, 2))
	byBucket := map[string]int{}
	bySpecies := map[int]int{}
	for range N {
		sp, ok, err := pickDraw(ctx, species, rng, plan)
		if err != nil || !ok {
			t.Fatalf("pickDraw: %v %v", ok, err)
		}
		if sp.Domestic {
			t.Fatalf("выпал домашний вид %d", sp.ID)
		}
		k := sp.IUCN
		if k == "" || k == "LR" {
			k = OtherStatus
		}
		byBucket[k]++
		bySpecies[sp.ID]++
	}
	maxZ := 0.0
	for c := range bucketCount {
		p := report[c]
		obs := float64(byBucket[c]) / N
		sd := math.Sqrt(p * (1 - p) / N)
		z := math.Abs(obs-p) / sd
		maxZ = max(maxZ, z)
		t.Logf("%-2s ожидалось %.4f, вышло %.4f (z=%.2f)", c, p, obs, z)
		if z > 4.5 {
			t.Errorf("доля %s: %.4f, ожидалась %.4f (z=%.1f)", c, obs, p, z)
		}
	}
	t.Logf("наибольшее отклонение — %.2f σ", maxZ)

	// Внутри корзины OtherStatus (ID 1..15 — пустой код, LR — свои ID) все
	// 20 видов выпадают примерно поровну: χ² с 19 степенями свободы.
	var other []int
	for _, s := range pickDataset(counts, 0).Species {
		if s.IUCN == "" || s.IUCN == "LR" {
			other = append(other, s.ID)
		}
	}
	total := 0
	for _, id := range other {
		total += bySpecies[id]
	}
	exp := float64(total) / float64(len(other))
	chi2 := 0.0
	for _, id := range other {
		d := float64(bySpecies[id]) - exp
		chi2 += d * d / exp
	}
	t.Logf("корзина %s: %d выпадений на %d видов, χ² = %.1f (df=19)", OtherStatus, total, len(other), chi2)
	if chi2 > 50 { // p ≈ 1e-4 при df=19
		t.Errorf("корзина %s неравномерна: χ² = %.1f", OtherStatus, chi2)
	}
}

// Маленькая корзина OtherStatus разыгрывается перебором страниц — и тоже
// равновероятно.
func TestPickDrawOtherByWalk(t *testing.T) {
	ctx := context.Background()
	species := pickMemory(t, pickDataset(map[string]int{"LC": 500, "": 3}, 10))
	opt, _ := pickOptions(PickOptions{Weights: map[string]float64{"LC": 0, "": 1}})
	plan, err := pickMakePlan(ctx, species, opt.Weights)
	if err != nil {
		t.Fatal(err)
	}
	if r := plan.report(); r[OtherStatus] != 1 || r["LC"] != 0 {
		t.Fatalf("report = %v", r)
	}
	rng := rand.New(rand.NewPCG(3, 4))
	got := map[int]int{}
	for range 3000 {
		sp, ok, err := pickDraw(ctx, species, rng, plan)
		if err != nil || !ok {
			t.Fatalf("pickDraw: %v %v", ok, err)
		}
		if sp.IUCN != "" {
			t.Fatalf("из корзины без кода выпал %+v", sp)
		}
		got[sp.ID]++
	}
	if len(got) != 3 {
		t.Fatalf("выпали %v", got)
	}
	for id, n := range got {
		if n < 850 || n > 1150 {
			t.Errorf("вид %d выпал %d раз из 3000", id, n)
		}
	}
}

func TestPickWeightsOptions(t *testing.T) {
	ctx := context.Background()
	species := pickMemory(t, pickDataset(map[string]int{"LC": 10, "LR": 5, "": 5}, 0))
	// Вес для нестандартного кода даёт ему свою корзину; ключ без учёта регистра.
	opt, err := pickOptions(PickOptions{Weights: map[string]float64{"lr": 2}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := pickMakePlan(ctx, species, opt.Weights)
	if err != nil {
		t.Fatal(err)
	}
	want := WeightsReport{"LC": 10.0 / 25, "LR": 10.0 / 25, OtherStatus: 5.0 / 25}
	if r := plan.report(); len(r) != 3 || math.Abs(r["LC"]-want["LC"]) > 1e-9 ||
		math.Abs(r["LR"]-want["LR"]) > 1e-9 || math.Abs(r[OtherStatus]-want[OtherStatus]) > 1e-9 {
		t.Errorf("report = %v, want %v", r, want)
	}
	for _, w := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, err := pickOptions(PickOptions{Weights: map[string]float64{"LC": w}}); err == nil {
			t.Errorf("вес %v принят", w)
		}
	}
	// Все веса нулевые — выбирать не из чего.
	p := newPickTestPicker(species, newPickFakeChecker(), newPickFakeStore(),
		PickOptions{Weights: map[string]float64{"LC": 0, "LR": 0, "": 0}}, 1)
	if _, err := p.Pick(ctx); err == nil || errors.Is(err, ErrNoCandidate) {
		t.Errorf("все веса нулевые: err = %v", err)
	}
}

func TestPickNoRepeat(t *testing.T) {
	ctx := context.Background()
	species := pickMemory(t, pickDataset(map[string]int{"LC": 3}, 0))
	for seed := range uint64(10) {
		c, s := newPickFakeChecker(), newPickFakeStore()
		s.picks = []Pick{
			{SpeciesID: 1, PickedAt: pickNow.Add(-time.Hour)},
			{SpeciesID: 2, PickedAt: pickNow.Add(-29 * 24 * time.Hour)},
			{SpeciesID: 3, PickedAt: pickNow.Add(-31 * 24 * time.Hour)}, // давно — можно
		}
		got, err := newPickTestPicker(species, c, s, PickOptions{}, seed).Pick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.SpeciesID != 3 {
			t.Fatalf("seed %d: выбран %d", seed, got.SpeciesID)
		}
		for _, r := range got.Rejected {
			if r.Reason != ReasonRecent || r.SpeciesID == 3 {
				t.Errorf("seed %d: отказ %+v", seed, r)
			}
		}
		if c.totalCalls() != 1 {
			t.Errorf("seed %d: недавние виды проверялись: %d вызовов", seed, c.totalCalls())
		}
		if got.Attempts != len(got.Rejected)+1 {
			t.Errorf("seed %d: Attempts %d, отказов %d", seed, got.Attempts, len(got.Rejected))
		}
	}

	// Все виды недавние — кандидатов нет, Checker не зовётся, Pick завершается.
	c, s := newPickFakeChecker(), newPickFakeStore()
	for id := 1; id <= 3; id++ {
		s.picks = append(s.picks, Pick{SpeciesID: id, PickedAt: pickNow.Add(-time.Minute)})
	}
	_, err := newPickTestPicker(species, c, s, PickOptions{}, 1).Pick(ctx)
	if !errors.Is(err, ErrNoCandidate) || c.totalCalls() != 0 {
		t.Errorf("все недавние: err = %v, вызовов %d", err, c.totalCalls())
	}
	// Отрицательный NoRepeat запрет отключает.
	got, err := newPickTestPicker(species, c, s, PickOptions{NoRepeat: -1}, 1).Pick(ctx)
	if err != nil || len(got.Rejected) != 0 {
		t.Errorf("NoRepeat<0: %+v, %v", got, err)
	}
}

func TestPickCache(t *testing.T) {
	ctx := context.Background()
	species := pickMemory(t, pickDataset(map[string]int{"VU": 1}, 0))
	c, s := newPickFakeChecker(), newPickFakeStore()
	now := pickNow
	p := &Picker{Species: species, Checker: c, Store: s, Options: PickOptions{NoRepeat: -1},
		Rand: rand.New(rand.NewPCG(1, 2)), Now: func() time.Time { return now }}

	first, err := p.Pick(ctx)
	if err != nil || c.callsOf(1) != 1 {
		t.Fatalf("первый выбор: %v, вызовов %d", err, c.callsOf(1))
	}
	now = now.Add(29 * 24 * time.Hour)
	second, err := p.Pick(ctx)
	if err != nil || c.callsOf(1) != 1 {
		t.Fatalf("второй выбор в пределах TTL: %v, вызовов %d", err, c.callsOf(1))
	}
	if !second.Eligibility.CheckedAt.Equal(first.Eligibility.CheckedAt) {
		t.Errorf("второй выбор не из кэша: %v", second.Eligibility.CheckedAt)
	}
	now = now.Add(2 * 24 * time.Hour) // 31 день от проверки
	third, err := p.Pick(ctx)
	if err != nil || c.callsOf(1) != 2 {
		t.Fatalf("после TTL: %v, вызовов %d", err, c.callsOf(1))
	}
	if !third.Eligibility.CheckedAt.Equal(now) {
		t.Errorf("CheckedAt = %v, want %v", third.Eligibility.CheckedAt, now)
	}
	// Отрицательный CheckTTL — кэш не читается.
	p.Options.CheckTTL = -1
	if _, err := p.Pick(ctx); err != nil || c.callsOf(1) != 3 {
		t.Errorf("CheckTTL<0: %v, вызовов %d", err, c.callsOf(1))
	}

	// Кэшированный отказ тоже не перепроверяется.
	c2, s2 := newPickFakeChecker(), newPickFakeStore()
	s2.checks[1] = Eligibility{SpeciesID: 1, CheckedAt: pickNow.Add(-time.Hour), Reason: ReasonNoArticle}
	_, err = newPickTestPicker(species, c2, s2, PickOptions{}, 1).Pick(ctx)
	if !errors.Is(err, ErrNoCandidate) || c2.totalCalls() != 0 {
		t.Errorf("кэшированный отказ: %v, вызовов %d", err, c2.totalCalls())
	}
}

func TestPickApplyThreshold(t *testing.T) {
	ok60 := Eligibility{OK: true, Occurrences: 60, WikiTitle: "X"}
	few30 := Eligibility{Reason: ReasonFewRecords, Occurrences: 30, WikiTitle: "X"}
	few30NoWiki := Eligibility{Reason: ReasonFewRecords, Occurrences: 30}
	noArt := Eligibility{Reason: ReasonNoArticle, Occurrences: 5000}
	tests := []struct {
		name       string
		e          Eligibility
		min        int
		wantOK     bool
		wantReason string
		usable     bool
	}{
		{"пригоден, порог тот же", ok60, 50, true, ReasonOK, true},
		{"пригоден, порог подняли выше наблюдений", ok60, 100, false, ReasonFewRecords, true},
		{"мало, порог всё ещё выше", few30, 50, false, ReasonFewRecords, true},
		{"мало, порог опустили, статья есть", few30, 20, true, ReasonOK, true},
		{"мало, порог опустили, статья не проверена", few30NoWiki, 20, false, ReasonFewRecords, false},
		{"нет статьи — от порога не зависит", noArt, 1, false, ReasonNoArticle, true},
		{"check_failed в кэше не доверяем", Eligibility{Reason: ReasonCheckFailed}, 1, false, ReasonCheckFailed, false},
	}
	for _, tt := range tests {
		got, usable := pickApplyThreshold(tt.e, tt.min)
		if got.OK != tt.wantOK || got.Reason != tt.wantReason || usable != tt.usable {
			t.Errorf("%s: OK=%v Reason=%q usable=%v", tt.name, got.OK, got.Reason, usable)
		}
	}

	// Через Pick: порог опустили — вид пригоден без обращения к Checker, а
	// кэш не переписан (в нём факты, а не решение).
	ctx := context.Background()
	species := pickMemory(t, pickDataset(map[string]int{"EN": 1}, 0))
	c, s := newPickFakeChecker(), newPickFakeStore()
	few30.SpeciesID, few30.CheckedAt = 1, pickNow.Add(-time.Hour)
	s.checks[1] = few30
	got, err := newPickTestPicker(species, c, s, PickOptions{MinOccurrences: 20}, 1).Pick(ctx)
	if err != nil || !got.Eligibility.OK || c.totalCalls() != 0 {
		t.Fatalf("порог опущен: %+v, %v, вызовов %d", got, err, c.totalCalls())
	}
	if e, _ := s.cached(1); e.OK {
		t.Error("кэш переписан решением под текущий порог")
	}
	// Порог подняли — отказ по кэшу с понятным пояснением.
	s.checks[1] = Eligibility{SpeciesID: 1, OK: true, Occurrences: 60, WikiTitle: "X", CheckedAt: pickNow.Add(-time.Hour)}
	_, err = newPickTestPicker(species, c, s, PickOptions{MinOccurrences: 100, NoRepeat: -1}, 1).Pick(ctx)
	if !errors.Is(err, ErrNoCandidate) || c.totalCalls() != 0 {
		t.Errorf("порог поднят: %v, вызовов %d", err, c.totalCalls())
	}
}

var errPickNet = errors.New("dial tcp: connection refused")

func TestPickNetworkError(t *testing.T) {
	ctx := context.Background()
	species := pickMemory(t, pickDataset(map[string]int{"LC": 2}, 0))
	sawRejection := false
	for seed := range uint64(10) {
		c, s := newPickFakeChecker(), newPickFakeStore()
		c.facts[1] = pickFacts{err: errPickNet}
		got, err := newPickTestPicker(species, c, s, PickOptions{}, seed).Pick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.SpeciesID != 2 {
			t.Fatalf("seed %d: выбран %d", seed, got.SpeciesID)
		}
		for _, r := range got.Rejected {
			if r.SpeciesID == 1 {
				sawRejection = true
				if r.Reason != ReasonCheckFailed || !strings.Contains(r.Detail, "connection refused") {
					t.Errorf("отказ %+v", r)
				}
			}
		}
		if _, ok := s.cached(1); ok {
			t.Error("сетевая ошибка закэширована")
		}
	}
	if !sawRejection {
		t.Error("ни в одном seed вид с сетевой ошибкой не попался первым")
	}
}

func TestPickAllFail(t *testing.T) {
	ctx := context.Background()
	species := pickMemory(t, pickDataset(map[string]int{"LC": 20}, 0))

	// Все проверки — сетевые ошибки: это «сеть», а не «нет кандидатов».
	c := newPickFakeChecker()
	for id := 1; id <= 20; id++ {
		c.facts[id] = pickFacts{err: errPickNet}
	}
	_, err := newPickTestPicker(species, c, newPickFakeStore(), PickOptions{}, 1).Pick(ctx)
	if !errors.Is(err, ErrCheckFailed) || !errors.Is(err, errPickNet) || errors.Is(err, ErrNoCandidate) {
		t.Errorf("все сетевые: %v", err)
	}
	if c.totalCalls() != 8 {
		t.Errorf("вызовов Checker %d, want MaxAttempts=8", c.totalCalls())
	}

	// Все непригодны — ErrNoCandidate.
	c = newPickFakeChecker()
	for id := 1; id <= 20; id++ {
		c.facts[id] = pickFacts{occ: 3}
	}
	_, err = newPickTestPicker(species, c, newPickFakeStore(), PickOptions{}, 1).Pick(ctx)
	if !errors.Is(err, ErrNoCandidate) || errors.Is(err, ErrCheckFailed) {
		t.Errorf("все непригодны: %v", err)
	}

	// Вперемешку — тоже ErrNoCandidate: часть кандидатов проверена и отвергнута.
	c = newPickFakeChecker()
	for id := 1; id <= 20; id++ {
		if id%2 == 0 {
			c.facts[id] = pickFacts{err: errPickNet}
		} else {
			c.facts[id] = pickFacts{noArticle: true}
		}
	}
	_, err = newPickTestPicker(species, c, newPickFakeStore(), PickOptions{}, 1).Pick(ctx)
	if !errors.Is(err, ErrNoCandidate) {
		t.Errorf("вперемешку: %v", err)
	}
}

func TestPickMaxAttempts(t *testing.T) {
	ctx := context.Background()
	species := pickMemory(t, pickDataset(map[string]int{"LC": 100}, 0))
	c := newPickFakeChecker()
	for id := 1; id <= 100; id++ {
		c.facts[id] = pickFacts{noArticle: true}
	}
	c.facts[100] = pickFacts{occ: 1000} // единственный пригодный — не успеем
	_, err := newPickTestPicker(species, c, newPickFakeStore(), PickOptions{MaxAttempts: 3}, 1).Pick(ctx)
	if !errors.Is(err, ErrNoCandidate) || c.totalCalls() != 3 {
		t.Errorf("err = %v, вызовов %d, want 3", err, c.totalCalls())
	}
}

func TestPickContextCanceled(t *testing.T) {
	species := pickMemory(t, mddtest.Sample())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, s := newPickFakeChecker(), newPickFakeStore()
	if _, err := newPickTestPicker(species, c, s, PickOptions{}, 1).Pick(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("отменённый ctx: %v", err)
	}

	// Отмена во время проверки: ошибка контекста, а не «сеть», и ничего не сохранено.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	c.hook = func(context.Context) { cancel() }
	_, err := newPickTestPicker(species, c, s, PickOptions{}, 1).Pick(ctx)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrCheckFailed) {
		t.Errorf("отмена во время проверки: %v", err)
	}
	if c.totalCalls() != 1 || len(s.picks) != 0 || len(s.checks) != 0 {
		t.Errorf("после отмены: вызовов %d, выборов %d, проверок %d", c.totalCalls(), len(s.picks), len(s.checks))
	}
}

func TestPickEmptyDirectory(t *testing.T) {
	ctx := context.Background()
	p := newPickTestPicker(mdd.NewMemory(), newPickFakeChecker(), newPickFakeStore(), PickOptions{}, 1)
	if _, err := p.Pick(ctx); !errors.Is(err, mdd.ErrNotFound) {
		t.Errorf("пустой справочник: %v", err)
	}
	p.Species = pickMemory(t, pickDataset(nil, 5))
	_, err := p.Pick(ctx)
	if err == nil || errors.Is(err, ErrNoCandidate) || !strings.Contains(err.Error(), "недомашних") {
		t.Errorf("только домашние: %v", err)
	}
}

func TestPickNeverDomestic(t *testing.T) {
	ctx := context.Background()
	species := pickMemory(t, mddtest.Sample())
	c, s := newPickFakeChecker(), newPickFakeStore()
	p := newPickTestPicker(species, c, s, PickOptions{NoRepeat: -1}, 7)
	seen := map[int]bool{}
	for range 300 {
		got, err := p.Pick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.SpeciesID == mddtest.DomesticCat {
			t.Fatal("выбрана домашняя кошка")
		}
		seen[got.SpeciesID] = true
	}
	if len(seen) != 10 {
		t.Errorf("за 300 выборов выпали %d видов из 10 недомашних", len(seen))
	}
}
