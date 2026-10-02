package trivia_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia/triviatest"
)

// builderFakeCollector отдаёт готовое досье (или ошибку). cancel, если
// задан, вызывается внутри Collect — так имитируется остановка демона
// посреди сборки.
type builderFakeCollector struct {
	d      trivia.Dossier
	err    error
	cancel context.CancelFunc
	calls  int
}

func (c *builderFakeCollector) Collect(ctx context.Context, p trivia.Pick, sp mdd.Species) (trivia.Dossier, error) {
	c.calls++
	if c.cancel != nil {
		c.cancel()
		return trivia.Dossier{}, ctx.Err()
	}
	if c.err != nil {
		return trivia.Dossier{}, c.err
	}
	d := c.d
	d.Pick, d.Species = p, sp
	return d, nil
}

type builderFakeEditor struct {
	draft trivia.Draft
	spend trivia.Spend
	err   error
	calls int
}

func (e *builderFakeEditor) Write(ctx context.Context, d trivia.Dossier) (trivia.Draft, trivia.Spend, error) {
	e.calls++
	return e.draft, e.spend, e.err
}

// builderFakeVerifier подтверждает факты, кроме тех, чей текст есть в
// reject (значение — причина). Вердиктов ровно по числу фактов, если не
// задано short — тогда на один меньше (сломанный проверяющий).
type builderFakeVerifier struct {
	reject map[string]string
	spend  trivia.Spend
	err    error
	short  bool
	got    []trivia.Fact
	calls  int
}

func (v *builderFakeVerifier) Verify(ctx context.Context, d trivia.Dossier, facts []trivia.Fact) ([]trivia.Verdict, trivia.Spend, error) {
	v.calls++
	v.got = append([]trivia.Fact(nil), facts...)
	if v.err != nil {
		return nil, v.spend, v.err
	}
	out := make([]trivia.Verdict, 0, len(facts))
	for _, f := range facts {
		reason, bad := v.reject[f.Text]
		out = append(out, trivia.Verdict{OK: !bad, Reason: reason})
	}
	if v.short && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out, v.spend, nil
}

// builderFakeStore — хранилище, которое отказывает в сохранении.
type builderFakeStore struct {
	trivia.IssueStore
	err error
}

func (s builderFakeStore) SaveIssue(ctx context.Context, is trivia.Issue) (int64, error) {
	return 0, s.err
}

var (
	builderT0          = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	builderEditorSpend = trivia.Spend{Model: "deepseek-v4-pro", Requests: 1,
		Usage: llm.Usage{Prompt: 5000, Completion: 800, Total: 5800},
		Cost:  llm.Cost{USD: 0.006, Tariff: llm.TariffPeak, Known: true}, Took: 12 * time.Second}
	builderVerifierSpend = trivia.Spend{Model: "deepseek-v4-flash", Requests: 1,
		Usage: llm.Usage{Prompt: 4000, Completion: 300, Total: 4300},
		Cost:  llm.Cost{USD: 0.001, Tariff: llm.TariffPeak, Known: true}, Took: 5 * time.Second}
)

// builderSpecies — манул из образца MDD.
func builderSpecies(t *testing.T) mdd.Species {
	t.Helper()
	for _, sp := range mddtest.Sample().Species {
		if sp.ID == mddtest.Manul {
			return sp
		}
	}
	t.Fatal("манула нет в образце MDD")
	return mdd.Species{}
}

// builderDossier — досье с тремя материалами, у каждого есть текст.
func builderDossier() trivia.Dossier {
	obs := triviatest.SampleIssue(1, mddtest.Manul, builderT0).Observations
	return trivia.Dossier{
		NameRu: "Манул",
		Materials: []trivia.Material{
			{ID: "S1", Kind: trivia.KindMDD, Title: "MDD", URL: "https://www.mammaldiversity.org/", Text: "карточка", Tool: "mdd_get"},
			{ID: "S2", Kind: trivia.KindWikipedia, Title: "Манул — Википедия", URL: "https://ru.wikipedia.org/wiki/Манул", Text: "статья", Tool: "read_wikipedia"},
			{ID: "S3", Kind: trivia.KindGBIF, Title: "GBIF", Text: "наблюдения", Tool: "gbif occurrence facet"},
		},
		Observations: obs,
		CollectedAt:  builderT0,
		Took:         3 * time.Second,
	}
}

func builderFacts(n int) []trivia.Fact {
	facts := make([]trivia.Fact, n)
	for i := range facts {
		facts[i] = trivia.Fact{Text: fmt.Sprintf("Факт номер %d о мануле.", i+1), Sources: []string{"S2"}}
	}
	return facts
}

type builderRig struct {
	col   *builderFakeCollector
	ed    *builderFakeEditor
	ver   *builderFakeVerifier
	store *trivia.Memory
	b     *trivia.Builder
	pick  trivia.Pick
	sp    mdd.Species
}

// builderSetup — сборка с исправными шагами: досье, черновик из facts,
// проверяющий всё подтверждает. Часы идут на минуту за каждый вызов Now.
func builderSetup(t *testing.T, facts []trivia.Fact) *builderRig {
	t.Helper()
	r := &builderRig{
		col:   &builderFakeCollector{d: builderDossier()},
		ed:    &builderFakeEditor{draft: trivia.Draft{Title: " Манул ", Lead: "Кот степей.", Facts: facts}, spend: builderEditorSpend},
		ver:   &builderFakeVerifier{spend: builderVerifierSpend},
		store: trivia.NewMemory(),
		sp:    builderSpecies(t),
	}
	r.pick = triviatest.SamplePick(r.sp.ID, builderT0)
	id, err := r.store.SavePick(context.Background(), r.pick)
	if err != nil {
		t.Fatal(err)
	}
	r.pick.ID = id
	clock := builderT0
	r.b = &trivia.Builder{Collector: r.col, Editor: r.ed, Verifier: r.ver, Store: r.store,
		Now: func() time.Time { clock = clock.Add(time.Minute); return clock }}
	return r
}

// build — Build с проверкой, что возвращённый выпуск и есть сохранённый.
func (r *builderRig) build(t *testing.T) (trivia.Issue, error) {
	t.Helper()
	is, err := r.b.Build(context.Background(), r.pick, r.sp)
	if is.ID == 0 {
		t.Fatalf("Build: выпуск без ID, ошибка %v", err)
	}
	saved, gerr := r.store.Issue(context.Background(), is.ID)
	if gerr != nil {
		t.Fatalf("сохранённый выпуск %d: %v", is.ID, gerr)
	}
	if !reflect.DeepEqual(saved, is) {
		t.Errorf("сохранённый выпуск отличается от возвращённого:\n saved %+v\nreturn %+v", saved, is)
	}
	return is, err
}

func builderTexts(facts []trivia.Fact) []string {
	var out []string
	for _, f := range facts {
		out = append(out, f.Text)
	}
	return out
}

func builderVerdicts(facts []trivia.Fact) []string {
	var out []string
	for _, f := range facts {
		out = append(out, f.Verdict)
	}
	return out
}

func TestBuilderOK(t *testing.T) {
	r := builderSetup(t, builderFacts(4))
	is, err := r.build(t)
	if err != nil {
		t.Fatal(err)
	}
	if is.Status != trivia.IssueOK || is.Error != "" {
		t.Errorf("состояние %q, ошибка %q; ждали ok без ошибки", is.Status, is.Error)
	}
	if len(is.Facts) != 4 || len(is.Dropped) != 0 {
		t.Errorf("фактов %d, отброшено %d; ждали 4 и 0", len(is.Facts), len(is.Dropped))
	}
	if is.PickID != r.pick.ID || is.SpeciesID != mddtest.Manul || is.SciName != "Otocolobus manul" ||
		is.NameRu != "Манул" || is.IUCN != "LC" || is.Order != "Carnivora" || is.Family != "Felidae" ||
		!reflect.DeepEqual(is.Realms, []string{"Palearctic"}) {
		t.Errorf("вид в выпуске: %+v", is)
	}
	if is.Title != "Манул" || is.Lead != "Кот степей." {
		t.Errorf("заголовок %q, вступление %q", is.Title, is.Lead)
	}
	// Источники — материалы досье без текста.
	if len(is.Sources) != 3 {
		t.Fatalf("источников %d, ждали 3", len(is.Sources))
	}
	for i, m := range is.Sources {
		want := r.col.d.Materials[i]
		want.Text = ""
		if m != want {
			t.Errorf("источник %d: %+v, ждали %+v", i, m, want)
		}
	}
	if r.col.d.Materials[0].Text == "" {
		t.Error("Build стёр текст в досье сборщика")
	}
	if !reflect.DeepEqual(is.Observations, r.col.d.Observations) {
		t.Errorf("наблюдения: %+v", is.Observations)
	}
	if !reflect.DeepEqual(is.Spend, []trivia.Spend{builderEditorSpend, builderVerifierSpend}) {
		t.Errorf("расход: %+v", is.Spend)
	}
	if want := builderEditorSpend.Cost.Add(builderVerifierSpend.Cost); is.Cost != want {
		t.Errorf("стоимость %+v, ждали %+v", is.Cost, want)
	}
	// Часы: первый вызов — CreatedAt, второй — конец сборки.
	if !is.CreatedAt.Equal(builderT0.Add(time.Minute)) || is.Took != time.Minute {
		t.Errorf("CreatedAt %s, Took %s", is.CreatedAt, is.Took)
	}
}

func TestBuilderStatuses(t *testing.T) {
	cases := []struct {
		name      string
		facts     int
		reject    int // сколько первых фактов не подтверждает проверяющий
		status    string
		confirmed int
	}{
		{"ровно MinFacts", trivia.MinFacts, 0, trivia.IssueOK, trivia.MinFacts},
		{"thin: MinFacts-1", trivia.MinFacts - 1, 0, trivia.IssueThin, trivia.MinFacts - 1},
		{"thin: один", 1, 0, trivia.IssueThin, 1},
		{"thin после проверки", 4, 2, trivia.IssueThin, 2},
		{"failed: всё отвергнуто", 3, 3, trivia.IssueFailed, 0},
		{"failed: пустой черновик", 0, 0, trivia.IssueFailed, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			facts := builderFacts(c.facts)
			r := builderSetup(t, facts)
			r.ver.reject = map[string]string{}
			for _, f := range facts[:c.reject] {
				r.ver.reject[f.Text] = "в статье этого нет"
			}
			is, err := r.build(t)
			if err != nil {
				t.Fatalf("0 фактов — не ошибка сборки: %v", err)
			}
			if is.Status != c.status || len(is.Facts) != c.confirmed {
				t.Errorf("состояние %q, фактов %d; ждали %q, %d", is.Status, len(is.Facts), c.status, c.confirmed)
			}
			if (is.Status == trivia.IssueFailed) != (is.Error != "") {
				t.Errorf("состояние %q, ошибка %q", is.Status, is.Error)
			}
			if len(is.Dropped) != c.reject {
				t.Errorf("отброшено %d, ждали %d", len(is.Dropped), c.reject)
			}
			for _, f := range is.Dropped {
				if f.Verdict != "в статье этого нет" {
					t.Errorf("причина отказа %q", f.Verdict)
				}
			}
			for _, f := range is.Facts {
				if f.Verdict != "" {
					t.Errorf("у подтверждённого факта Verdict %q", f.Verdict)
				}
			}
			// Пустой черновик до проверяющего не доходит: платить не за что.
			if wantCalls := min(c.facts, 1); r.ver.calls != wantCalls {
				t.Errorf("проверяющий вызван %d раз, ждали %d", r.ver.calls, wantCalls)
			}
		})
	}
}

// Отбраковка кодом: до проверяющего доходят только факты с текстом и
// ссылками на существующие материалы, без повторов.
func TestBuilderScreensFacts(t *testing.T) {
	facts := []trivia.Fact{
		{Text: "Хороший факт один.", Sources: []string{"S1"}},
		{Text: "   ", Sources: []string{"S1"}},
		{Text: "Без ссылок.", Sources: nil},
		{Text: "Ссылки из пробелов.", Sources: []string{" ", ""}},
		{Text: "Ссылка на S9.", Sources: []string{"S2", "S9"}},
		{Text: "  хороший   ФАКТ один. ", Sources: []string{"S2"}},
		{Text: "Хороший факт два.", Sources: []string{" S2 ", "S3", "S2"}, Verdict: "мусор от редактора"},
		{Text: "Хороший факт три.", Sources: []string{"S3"}},
	}
	r := builderSetup(t, facts)
	is, err := r.build(t)
	if err != nil {
		t.Fatal(err)
	}
	wantChecked := []string{"Хороший факт один.", "Хороший факт два.", "Хороший факт три."}
	// Последним пунктом проверяющему уходят заголовок и вступление.
	got := builderTexts(r.ver.got)
	if len(got) != len(wantChecked)+1 || !reflect.DeepEqual(got[:len(wantChecked)], wantChecked) ||
		got[len(got)-1] != "Манул. Кот степей." {
		t.Errorf("проверяющему ушло %q, ждали %q и заголовок", got, wantChecked)
	}
	if got := r.ver.got[1]; !reflect.DeepEqual(got.Sources, []string{"S2", "S3"}) || got.Verdict != "" {
		t.Errorf("факт после чистки: %+v", got)
	}
	if is.Status != trivia.IssueOK || !reflect.DeepEqual(builderTexts(is.Facts), wantChecked) {
		t.Errorf("состояние %q, факты %q", is.Status, builderTexts(is.Facts))
	}
	wantVerdicts := []string{"пустой факт", "нет источника", "нет источника",
		"ссылка на несуществующий материал S9", "повтор факта 1"}
	if got := builderVerdicts(is.Dropped); !reflect.DeepEqual(got, wantVerdicts) {
		t.Errorf("причины отбраковки %q, ждали %q", got, wantVerdicts)
	}
	// Черновик редактора Build не меняет.
	if facts[6].Sources[0] != " S2 " || facts[6].Verdict != "мусор от редактора" {
		t.Errorf("черновик изменён: %+v", facts[6])
	}
}

// Повтор отбрасывается, только если первый такой факт остался: если первый
// отбракован (плохая ссылка), второй с верной ссылкой — не повтор.
func TestBuilderDuplicateOfDropped(t *testing.T) {
	r := builderSetup(t, []trivia.Fact{
		{Text: "Факт.", Sources: []string{"S7"}},
		{Text: "Факт.", Sources: []string{"S1"}},
	})
	is, _ := r.build(t)
	if len(is.Facts) != 1 || is.Facts[0].Sources[0] != "S1" || len(is.Dropped) != 1 {
		t.Errorf("факты %+v, отброшены %+v", is.Facts, is.Dropped)
	}
}

func TestBuilderLimit(t *testing.T) {
	facts := builderFacts(8)
	r := builderSetup(t, facts)
	r.ver.reject = map[string]string{facts[1].Text: "не подтверждено"}
	is, err := r.build(t)
	if err != nil {
		t.Fatal(err)
	}
	// 7 подтверждённых → 5 в выпуск (первые по порядку), 2 сверх лимита.
	want := []string{facts[0].Text, facts[2].Text, facts[3].Text, facts[4].Text, facts[5].Text}
	if got := builderTexts(is.Facts); !reflect.DeepEqual(got, want) || is.Status != trivia.IssueOK {
		t.Errorf("факты %q (%s), ждали %q", got, is.Status, want)
	}
	if got, want := builderVerdicts(is.Dropped), []string{"не подтверждено", "сверх лимита", "сверх лимита"}; !reflect.DeepEqual(got, want) {
		t.Errorf("причины %q, ждали %q", got, want)
	}
	if got, want := builderTexts(is.Dropped[1:]), []string{facts[6].Text, facts[7].Text}; !reflect.DeepEqual(got, want) {
		t.Errorf("сверх лимита %q, ждали %q", got, want)
	}
}

func TestBuilderEmptyReason(t *testing.T) {
	facts := builderFacts(1)
	r := builderSetup(t, facts)
	r.ver.reject = map[string]string{facts[0].Text: "  "}
	is, _ := r.build(t)
	if len(is.Dropped) != 1 || is.Dropped[0].Verdict != "не подтверждён проверяющим" {
		t.Errorf("отброшенные %+v", is.Dropped)
	}
}

// Сбой любого шага: выпуск failed с Error, сохранён, в нём всё, что успели;
// Build возвращает и выпуск, и ошибку.
func TestBuilderStepFailures(t *testing.T) {
	boom := errors.New("бум")
	cases := []struct {
		name      string
		setup     func(r *builderRig)
		errPrefix string
		spend     []trivia.Spend
		sources   int
		title     string
		dropped   int
	}{
		{"досье", func(r *builderRig) { r.col.err = boom }, "досье: ", nil, 0, "", 0},
		{"редактор", func(r *builderRig) { r.ed.err = boom; r.ed.spend = trivia.Spend{} },
			"редактор: ", nil, 3, "", 0},
		{"редактор с расходом", func(r *builderRig) { r.ed.err = boom },
			"редактор: ", []trivia.Spend{builderEditorSpend}, 3, "", 0},
		{"проверка", func(r *builderRig) {
			r.ver.err = boom
			r.ed.draft.Facts = append(r.ed.draft.Facts, trivia.Fact{Text: "Без ссылки."})
		}, "проверка: ", []trivia.Spend{builderEditorSpend, builderVerifierSpend}, 3, "Манул", 1},
		{"проверка: не то число вердиктов", func(r *builderRig) { r.ver.short = true },
			"проверка: 3 вердиктов на 4 фактов", []trivia.Spend{builderEditorSpend, builderVerifierSpend}, 3, "Манул", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := builderSetup(t, builderFacts(3))
			c.setup(r)
			is, err := r.build(t)
			if err == nil {
				t.Fatal("нет ошибки")
			}
			if c.name != "проверка: не то число вердиктов" && !errors.Is(err, boom) {
				t.Errorf("ошибка %v не оборачивает исходную", err)
			}
			if is.Status != trivia.IssueFailed || !strings.HasPrefix(is.Error, c.errPrefix) {
				t.Errorf("состояние %q, ошибка %q; ждали failed и %q…", is.Status, is.Error, c.errPrefix)
			}
			if len(is.Facts) != 0 {
				t.Errorf("у сбойного выпуска факты: %+v", is.Facts)
			}
			if !reflect.DeepEqual(is.Spend, c.spend) {
				t.Errorf("расход %+v, ждали %+v", is.Spend, c.spend)
			}
			var cost llm.Cost
			for _, s := range c.spend {
				cost = cost.Add(s.Cost)
			}
			if is.Cost != cost {
				t.Errorf("стоимость %+v, ждали %+v", is.Cost, cost)
			}
			if len(is.Sources) != c.sources || is.Title != c.title || len(is.Dropped) != c.dropped {
				t.Errorf("источников %d, заголовок %q, отброшено %d; ждали %d, %q, %d",
					len(is.Sources), is.Title, len(is.Dropped), c.sources, c.title, c.dropped)
			}
			if c.sources > 0 && is.NameRu != "Манул" {
				t.Errorf("NameRu %q после досье", is.NameRu)
			}
			if is.SciName != "Otocolobus manul" || is.PickID != r.pick.ID || is.Took <= 0 {
				t.Errorf("выпуск: %+v", is)
			}
			list, total, _ := r.store.Issues(context.Background(),
				trivia.IssueQuery{Status: []string{trivia.IssueFailed}})
			if total != 1 || list[0].ID != is.ID {
				t.Errorf("сбойный выпуск не виден в выборке failed: %d", total)
			}
		})
	}
}

// Демон гасится: ctx отменён — ничего не сохраняется, ошибка — ctx.Err().
func TestBuilderCanceled(t *testing.T) {
	t.Run("до сборки", func(t *testing.T) {
		r := builderSetup(t, builderFacts(3))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		builderCheckCanceled(t, r, ctx)
	})
	t.Run("во время досье", func(t *testing.T) {
		r := builderSetup(t, builderFacts(3))
		ctx, cancel := context.WithCancel(context.Background())
		r.col.cancel = cancel
		builderCheckCanceled(t, r, ctx)
		if r.ed.calls != 0 {
			t.Errorf("редактор вызван после отмены: %d", r.ed.calls)
		}
	})
	t.Run("после успешной проверки", func(t *testing.T) {
		// Все шаги прошли, но ctx отменили до сохранения: всё равно не
		// сохранять — демон уходит, запись оборвалась бы на полпути.
		r := builderSetup(t, builderFacts(3))
		ctx, cancel := context.WithCancel(context.Background())
		now := r.b.Now
		calls := 0
		r.b.Now = func() time.Time {
			calls++
			if calls == 2 {
				cancel()
			}
			return now()
		}
		builderCheckCanceled(t, r, ctx)
	})
}

func builderCheckCanceled(t *testing.T, r *builderRig, ctx context.Context) {
	t.Helper()
	is, err := r.b.Build(ctx, r.pick, r.sp)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("ошибка %v, ждали context.Canceled", err)
	}
	if is.ID != 0 {
		t.Errorf("выпуск получил ID %d", is.ID)
	}
	if _, total, _ := r.store.Issues(context.Background(), trivia.IssueQuery{}); total != 0 {
		t.Errorf("сохранено %d выпусков", total)
	}
}

func TestBuilderSaveFails(t *testing.T) {
	r := builderSetup(t, builderFacts(3))
	boom := errors.New("диск полон")
	r.b.Store = builderFakeStore{err: boom}
	is, err := r.b.Build(context.Background(), r.pick, r.sp)
	if !errors.Is(err, boom) || is.ID != 0 || is.Status != trivia.IssueOK || len(is.Facts) != 3 {
		t.Errorf("Build: %+v, %v", is, err)
	}

	// Сбой шага и сбой сохранения — видны оба.
	stepErr := errors.New("редактор упал")
	r.ed.err = stepErr
	_, err = r.b.Build(context.Background(), r.pick, r.sp)
	if !errors.Is(err, boom) || !errors.Is(err, stepErr) {
		t.Errorf("ошибка %v, ждали обе", err)
	}
}

func TestBuilderNotConfigured(t *testing.T) {
	r := builderSetup(t, builderFacts(3))
	r.b.Verifier = nil
	if _, err := r.b.Build(context.Background(), r.pick, r.sp); err == nil {
		t.Error("Build без проверяющего: нет ошибки")
	}
	if r.col.calls != 0 {
		t.Error("Build без проверяющего начал собирать досье")
	}
}

// Пустой вид (справочник не нашёл) — вид берётся из выбора.
func TestBuilderSpeciesFromPick(t *testing.T) {
	r := builderSetup(t, builderFacts(3))
	r.sp = mdd.Species{}
	is, err := r.build(t)
	if err != nil {
		t.Fatal(err)
	}
	if is.SpeciesID != r.pick.SpeciesID || is.SciName != r.pick.SciName || is.IUCN != r.pick.IUCN {
		t.Errorf("вид %d %q %q, ждали из выбора %d %q %q", is.SpeciesID, is.SciName, is.IUCN,
			r.pick.SpeciesID, r.pick.SciName, r.pick.IUCN)
	}
}

// Время по умолчанию — time.Now.
func TestBuilderDefaultNow(t *testing.T) {
	r := builderSetup(t, builderFacts(3))
	r.b.Now = nil
	before := time.Now()
	is, err := r.build(t)
	if err != nil {
		t.Fatal(err)
	}
	if is.CreatedAt.Before(before) || is.CreatedAt.After(time.Now()) || is.Took < 0 {
		t.Errorf("CreatedAt %s, Took %s", is.CreatedAt, is.Took)
	}
}

// Заголовок и вступление, не подтверждённые проверяющим, заменяются
// названием вида и уходят в Dropped; факты при этом не страдают.
func TestBuilderRejectsHead(t *testing.T) {
	r := builderSetup(t, builderFacts(3))
	head := strings.TrimSpace(r.ed.draft.Title) + ". " + r.ed.draft.Lead
	r.ver.reject = map[string]string{head: "в материалах нет «степей»"}
	is, err := r.build(t)
	if err != nil {
		t.Fatal(err)
	}
	if is.Status != trivia.IssueOK || len(is.Facts) != 3 {
		t.Fatalf("состояние %q, фактов %d", is.Status, len(is.Facts))
	}
	if is.Lead != "" || is.Title == r.ed.draft.Title || !strings.Contains(is.Title, is.SciName) {
		t.Errorf("заголовок %q, вступление %q: ждали название вида и пустое вступление", is.Title, is.Lead)
	}
	last := is.Dropped[len(is.Dropped)-1]
	if !strings.HasPrefix(last.Verdict, "заголовок и вступление: ") || !strings.Contains(last.Verdict, "степей") {
		t.Errorf("в отброшенных: %+v", last)
	}
}
