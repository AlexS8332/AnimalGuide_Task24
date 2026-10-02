package daemon_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/daemon"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// ------------------------------------------------------- фейки выпуска

// jobsFakeSources — фабрики Fetcher, Checker и Collector: запоминают, какой
// Fetcher достался каждому запуску, и не ходят в сеть.
type jobsFakeSources struct {
	mu         sync.Mutex
	fetchers   []*tools.Fetcher // по одному на вызов NewFetcher
	checkerF   []*tools.Fetcher // что получила фабрика Checker
	collectorF []*tools.Fetcher // что получила фабрика Collector
	eligible   bool             // false — Checker отвергает всех
	checks     int
}

func (s *jobsFakeSources) deps(d daemon.IssueDeps) daemon.IssueDeps {
	d.NewFetcher = func() *tools.Fetcher {
		f := tools.NewFetcher()
		s.mu.Lock()
		s.fetchers = append(s.fetchers, f)
		s.mu.Unlock()
		return f
	}
	d.Checker = func(f *tools.Fetcher) trivia.Checker {
		s.mu.Lock()
		s.checkerF = append(s.checkerF, f)
		s.mu.Unlock()
		return jobsFakeChecker{s}
	}
	d.Collector = func(f *tools.Fetcher) trivia.Collector {
		s.mu.Lock()
		s.collectorF = append(s.collectorF, f)
		s.mu.Unlock()
		return jobsFakeCollector{}
	}
	return d
}

type jobsFakeChecker struct{ s *jobsFakeSources }

func (c jobsFakeChecker) Check(ctx context.Context, sp mdd.Species, minOcc int) (trivia.Eligibility, error) {
	c.s.mu.Lock()
	c.s.checks++
	ok := c.s.eligible
	c.s.mu.Unlock()
	e := trivia.Eligibility{SpeciesID: sp.ID, SciName: sp.SciName, CheckedAt: time.Now()}
	if !ok {
		e.Reason = trivia.ReasonNoArticle
		return e, nil
	}
	e.OK, e.WikiLang, e.WikiTitle, e.Occurrences = true, "ru", "Манул", 1000
	return e, nil
}

type jobsFakeCollector struct{}

func (jobsFakeCollector) Collect(ctx context.Context, p trivia.Pick, sp mdd.Species) (trivia.Dossier, error) {
	return trivia.Dossier{
		Pick: p, Species: sp, NameRu: "Манул",
		Materials: []trivia.Material{
			{ID: "S1", Kind: trivia.KindMDD, Title: "MDD", Text: "Otocolobus manul, Felidae."},
			{ID: "S2", Kind: trivia.KindWikipedia, Title: "Манул", Text: "Манул живёт в степях и горах."},
		},
		CollectedAt: time.Now(),
	}, nil
}

// jobsFakeLLM — редактор пишет пять фактов; проверяющий подтверждает всё
// (reject = false) или отвергает всё; editorErr — сбой модели у редактора.
func jobsFakeLLM(reject bool, editorErr error) *llmtest.Fake {
	return &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		var resp llm.Response
		switch req.Messages[0].Content {
		case trivia.EditorSystem:
			if editorErr != nil {
				return llm.Response{}, editorErr
			}
			var facts []string
			for i := 1; i <= 5; i++ {
				facts = append(facts, fmt.Sprintf(`{"text":"Факт номер %d о мануле.","sources":["S2"]}`, i))
			}
			resp = llmtest.Text(`{"title":"Манул","lead":"Манул — дикая кошка.","facts":[` + strings.Join(facts, ",") + `]}`)
		case trivia.VerifySystem:
			var v []string
			for i := 1; i <= 6; i++ {
				if reject {
					v = append(v, fmt.Sprintf(`{"n":%d,"ok":false,"reason":"в S2 этого нет"}`, i))
				} else {
					v = append(v, fmt.Sprintf(`{"n":%d,"ok":true}`, i))
				}
			}
			resp = llmtest.Text("[" + strings.Join(v, ",") + "]")
		default:
			return llm.Response{}, errors.New("неожиданный запрос")
		}
		resp.Usage = llm.Usage{Prompt: 2000, Completion: 400, Total: 2400}
		return resp, nil
	}}
}

// jobsFakeSpecies — справочник из видов Sample с указанными ID.
func jobsFakeSpecies(t *testing.T, ids ...int) *mdd.Memory {
	t.Helper()
	ds := mddtest.Sample()
	keep := map[int]bool{}
	for _, id := range ids {
		keep[id] = true
	}
	var list []mdd.Species
	for _, sp := range ds.Species {
		if keep[sp.ID] {
			list = append(list, sp)
		}
	}
	ds.Species = list
	st := mdd.NewMemory()
	if err := st.Replace(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	return st
}

// jobsIssueDeps — зависимости на один вид (манул): выбор предсказуем.
// Повторы и кэш проверок выключены, чтобы каждый запуск звал Checker.
func jobsIssueDeps(t *testing.T, src *jobsFakeSources, chat llm.Chatter) (daemon.IssueDeps, *trivia.Memory) {
	store := trivia.NewMemory()
	return src.deps(daemon.IssueDeps{
		Species: jobsFakeSpecies(t, mddtest.Manul),
		Picks:   store,
		Issues:  store,
		LLM:     chat,
		Options: trivia.PickOptions{NoRepeat: -1, CheckTTL: -1},
	}), store
}

// ------------------------------------------------------------- выпуск

func TestIssueJobSuccess(t *testing.T) {
	ctx := context.Background()
	src := &jobsFakeSources{eligible: true}
	d, store := jobsIssueDeps(t, src, jobsFakeLLM(false, nil))
	job := daemon.IssueJob(d, time.Hour)
	if job.Name != daemon.JobIssue || job.Every != time.Hour || !job.Paid || job.Daily != "" {
		t.Fatalf("задание: %+v", job)
	}

	out, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("запуск: %v", err)
	}
	list, total, err := store.Issues(ctx, trivia.IssueQuery{})
	if err != nil || total != 1 {
		t.Fatalf("выпуски: %d, %v", total, err)
	}
	is := list[0]
	if is.Status != trivia.IssueOK || len(is.Facts) != 5 {
		t.Fatalf("выпуск: %s, фактов %d, %s", is.Status, len(is.Facts), is.Error)
	}
	if want := fmt.Sprintf("issue:%d", is.ID); out.Ref != want {
		t.Errorf("Ref %q, ожидалось %q", out.Ref, want)
	}
	if want := "Манул (Otocolobus manul): 5 фактов"; out.Detail != want {
		t.Errorf("Detail %q, ожидалось %q", out.Detail, want)
	}
	if out.CostUSD <= 0 || out.CostUSD != is.Cost.USD {
		t.Errorf("расход %v, в выпуске %v", out.CostUSD, is.Cost.USD)
	}
}

// Каждый запуск — свой Fetcher, и он же уходит и в проверку, и в досье.
func TestIssueJobFreshFetcherPerRun(t *testing.T) {
	ctx := context.Background()
	src := &jobsFakeSources{eligible: true}
	d, _ := jobsIssueDeps(t, src, jobsFakeLLM(false, nil))
	job := daemon.IssueJob(d, time.Hour)
	const runs = 3
	for i := 0; i < runs; i++ {
		if _, err := job.Run(ctx); err != nil {
			t.Fatalf("запуск %d: %v", i+1, err)
		}
	}
	if len(src.fetchers) != runs || len(src.checkerF) != runs || len(src.collectorF) != runs {
		t.Fatalf("фабрики: fetcher %d, checker %d, collector %d; ожидалось по %d",
			len(src.fetchers), len(src.checkerF), len(src.collectorF), runs)
	}
	seen := map[*tools.Fetcher]bool{}
	for i, f := range src.fetchers {
		if seen[f] {
			t.Errorf("запуск %d получил Fetcher прошлого запуска", i+1)
		}
		seen[f] = true
		if src.checkerF[i] != f || src.collectorF[i] != f {
			t.Errorf("запуск %d: проверка и досье получили не тот Fetcher", i+1)
		}
	}
	if src.checks != runs {
		t.Errorf("проверок %d, ожидалось %d", src.checks, runs)
	}
}

// Проверяющий отверг всё: выпуск сохранён с failed, запуск — ошибка, но
// Outcome несёт ссылку и потраченное.
func TestIssueJobFailedIssue(t *testing.T) {
	ctx := context.Background()
	src := &jobsFakeSources{eligible: true}
	d, store := jobsIssueDeps(t, src, jobsFakeLLM(true, nil))
	out, err := daemon.IssueJob(d, time.Hour).Run(ctx)
	if err == nil {
		t.Fatal("выпуск без фактов — не ошибка")
	}
	if errors.Is(err, schedule.ErrSkip) {
		t.Fatalf("сбой выпуска выдан за пропуск: %v", err)
	}
	list, _, _ := store.Issues(ctx, trivia.IssueQuery{})
	if len(list) != 1 || list[0].Status != trivia.IssueFailed {
		t.Fatalf("выпуски: %+v", list)
	}
	is := list[0]
	if out.Ref != fmt.Sprintf("issue:%d", is.ID) || out.CostUSD <= 0 || out.CostUSD != is.Cost.USD {
		t.Errorf("Outcome %+v, расход выпуска %v", out, is.Cost.USD)
	}
	if !strings.HasPrefix(out.Detail, "Манул (Otocolobus manul): выпуск не собран — ") {
		t.Errorf("Detail %q", out.Detail)
	}
}

// Модель редактора упала: Builder вернул ошибку вместе с сохранённым
// выпуском — Outcome тоже с Ref.
func TestIssueJobEditorError(t *testing.T) {
	ctx := context.Background()
	src := &jobsFakeSources{eligible: true}
	d, store := jobsIssueDeps(t, src, jobsFakeLLM(false, errors.New("API недоступно")))
	out, err := daemon.IssueJob(d, time.Hour).Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "API недоступно") {
		t.Fatalf("ошибка: %v", err)
	}
	list, _, _ := store.Issues(ctx, trivia.IssueQuery{})
	if len(list) != 1 || out.Ref != fmt.Sprintf("issue:%d", list[0].ID) {
		t.Fatalf("Outcome %+v, выпуски %d", out, len(list))
	}
	if !strings.Contains(out.Detail, "выпуск не собран — редактор: ") {
		t.Errorf("Detail %q", out.Detail)
	}
}

func TestIssueJobEmptyMDD(t *testing.T) {
	src := &jobsFakeSources{eligible: true}
	chat := jobsFakeLLM(false, nil)
	d, _ := jobsIssueDeps(t, src, chat)
	d.Species = mdd.NewMemory()
	out, err := daemon.IssueJob(d, time.Hour).Run(context.Background())
	if !errors.Is(err, schedule.ErrSkip) {
		t.Fatalf("пустой справочник: %v", err)
	}
	if out.Detail != daemon.DetailMDDEmpty || out.CostUSD != 0 || out.Ref != "" {
		t.Errorf("Outcome %+v", out)
	}
	if chat.Calls() != 0 || src.checks != 0 {
		t.Errorf("модель %d, проверок %d", chat.Calls(), src.checks)
	}
}

func TestIssueJobNoCandidate(t *testing.T) {
	src := &jobsFakeSources{eligible: false}
	chat := jobsFakeLLM(false, nil)
	d, store := jobsIssueDeps(t, src, chat)
	out, err := daemon.IssueJob(d, time.Hour).Run(context.Background())
	if !errors.Is(err, trivia.ErrNoCandidate) || errors.Is(err, schedule.ErrSkip) {
		t.Fatalf("ошибка: %v", err)
	}
	if out != (schedule.Outcome{}) || chat.Calls() != 0 {
		t.Errorf("Outcome %+v, модель %d", out, chat.Calls())
	}
	if _, total, _ := store.Issues(context.Background(), trivia.IssueQuery{}); total != 0 {
		t.Errorf("выпусков %d", total)
	}
}

func TestIssueJobMissingDeps(t *testing.T) {
	if _, err := daemon.IssueJob(daemon.IssueDeps{}, time.Hour).Run(context.Background()); err == nil {
		t.Fatal("задание без зависимостей отработало")
	}
}

// ---------------------------------------------------------- релиз MDD

// jobsFakeArchive — сервер архива MDD с ETag; fail — ответ 500.
type jobsFakeArchive struct {
	mu   sync.Mutex
	data []byte
	etag string
	fail bool
}

func (a *jobsFakeArchive) set(data []byte, etag string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.data, a.etag = data, etag
}

func (a *jobsFakeArchive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	data, etag, fail := a.data, a.etag, a.fail
	a.mu.Unlock()
	if fail {
		http.Error(w, "упал", http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Write(data)
}

// jobsMiniFiles — урезанный настоящий архив MDD v2.5 из testdata пакета mdd
// (12 видов, 5 строк Diff).
func jobsMiniFiles(t *testing.T) map[string][]byte {
	t.Helper()
	root := filepath.Join("..", "mdd", "testdata", "mini")
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatalf("testdata/mini: %v", err)
	}
	return files
}

func jobsZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// jobsReleaseV25 — v2.5 без последнего вида (бизона): 11 видов, чтобы у
// следующего релиза было «+1».
func jobsReleaseV25(t *testing.T) []byte {
	files := jobsMiniFiles(t)
	const csv = "MDD/MDD_v2.5_12species.csv"
	lines := strings.SplitAfter(strings.TrimRight(string(files[csv]), "\n"), "\n")
	if !strings.HasPrefix(lines[len(lines)-1], "Bos_bison,") {
		t.Fatalf("последняя строка мини-архива: %.30q", lines[len(lines)-1])
	}
	files[csv] = []byte(strings.Join(lines[:len(lines)-1], ""))
	return jobsZip(t, files)
}

// jobsReleaseV26 — тот же мини-архив, выданный за v2.6 (12 видов).
func jobsReleaseV26(t *testing.T) []byte {
	files := jobsMiniFiles(t)
	files["MDD/release.toml"] = bytes.ReplaceAll(files["MDD/release.toml"], []byte(`version = "v2.5"`), []byte(`version = "v2.6"`))
	files["MDD/Diff_v2.5-v2.6.csv"] = bytes.ReplaceAll(files["MDD/Diff_v2.4-v2.5.csv"], []byte("MDDv2.4_Name,MDDv2.5_Name"), []byte("MDDv2.5_Name,MDDv2.6_Name"))
	delete(files, "MDD/Diff_v2.4-v2.5.csv")
	files["MDD/MDD_v2.6_12species.csv"] = files["MDD/MDD_v2.5_12species.csv"]
	delete(files, "MDD/MDD_v2.5_12species.csv")
	return jobsZip(t, files)
}

func TestMDDJob(t *testing.T) {
	arch := &jobsFakeArchive{}
	arch.set(jobsReleaseV25(t), `"a"`)
	srv := httptest.NewServer(arch)
	defer srv.Close()
	ctx := context.Background()
	st := mdd.NewMemory()
	now := time.Date(2026, 9, 24, 4, 0, 0, 0, time.UTC)
	job := daemon.MDDJob(st, mdd.SyncOptions{URL: srv.URL, HTTP: srv.Client(), Now: func() time.Time { return now }}, "04:00")
	if job.Name != daemon.JobMDD || job.Daily != "04:00" || job.Paid || job.Every != 0 {
		t.Fatalf("задание: %+v", job)
	}

	steps := []struct {
		name       string
		data       []byte
		etag       string
		ref        string
		detail     string
		newRelease bool
	}{
		{"первая загрузка", nil, "", "mdd:v2.5", "загружен релиз v2.5 (11 видов)", false},
		{"без изменений", nil, "", "mdd:v2.5", "релиз v2.5 не менялся", false},
		{"новый релиз", jobsReleaseV26(t), `"b"`, "mdd:v2.6",
			"новый релиз v2.6 (было v2.5): 12 видов (+1), изменений в систематике 5", true},
		{"поправлен архив", jobsReleaseV26(t), `"c"`, "mdd:v2.6",
			"релиз v2.6 перезагружен: архив поправлен без смены версии (12 видов)", false},
	}
	for _, s := range steps {
		if s.data != nil {
			arch.set(s.data, s.etag)
		}
		out, err := job.Run(ctx)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if out.Ref != s.ref || out.Detail != s.detail || out.CostUSD != 0 {
			t.Errorf("%s: %+v", s.name, out)
		}
		if got := strings.HasPrefix(out.Detail, daemon.MDDNewRelease); got != s.newRelease {
			t.Errorf("%s: признак нового релиза %v", s.name, got)
		}
	}
	if rel, err := st.Release(ctx); err != nil || rel.Version != "v2.6" || !rel.LoadedAt.Equal(now) {
		t.Errorf("в хранилище: %+v, %v", rel, err)
	}

	arch.mu.Lock()
	arch.fail = true
	arch.mu.Unlock()
	if out, err := job.Run(ctx); err == nil || out != (schedule.Outcome{}) {
		t.Errorf("сбой сервера: %+v, %v", out, err)
	}
}

// ------------------------------------------------------------- сводка

type jobsFakeSummarizer struct {
	text  string
	cost  float64
	err   error
	calls int
	got   trivia.Aggregate
}

func (s *jobsFakeSummarizer) Summarize(ctx context.Context, a trivia.Aggregate) (string, trivia.Spend, error) {
	s.calls++
	s.got = a
	var sp trivia.Spend
	if s.cost > 0 {
		sp = trivia.Spend{Model: llm.DefaultModel, Requests: 1, Cost: llm.Cost{USD: s.cost, Known: true}}
	}
	return s.text, sp, s.err
}

// jobsFakeSummaryStore — SummaryStore в памяти (настоящие реализации
// пишутся отдельно; здесь нужен только SaveSummary).
type jobsFakeSummaryStore struct {
	mu    sync.Mutex
	saved []trivia.Summary
	err   error
}

func (m *jobsFakeSummaryStore) SaveSummary(ctx context.Context, s trivia.Summary) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, m.err
	}
	s.ID = int64(len(m.saved) + 1)
	m.saved = append(m.saved, s)
	return s.ID, nil
}

func (m *jobsFakeSummaryStore) Summary(ctx context.Context, id int64) (trivia.Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id < 1 || int(id) > len(m.saved) {
		return trivia.Summary{}, trivia.ErrIssueNotFound
	}
	return m.saved[id-1], nil
}

func (m *jobsFakeSummaryStore) Summaries(ctx context.Context, limit int) ([]trivia.Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]trivia.Summary(nil), m.saved...), nil
}

func (m *jobsFakeSummaryStore) LatestSummary(ctx context.Context) (trivia.Summary, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.saved) == 0 {
		return trivia.Summary{}, false, nil
	}
	return m.saved[len(m.saved)-1], true, nil
}

var _ trivia.SummaryStore = (*jobsFakeSummaryStore)(nil)

// jobsFakeAggregate — агрегат с тремя выпусками; запоминает период.
type jobsFakeAggregate struct {
	from, to time.Time
	calls    int
	err      error
	empty    bool
}

func (a *jobsFakeAggregate) fn(ctx context.Context, from, to time.Time) (trivia.Aggregate, error) {
	a.calls++
	a.from, a.to = from, to
	if a.err != nil {
		return trivia.Aggregate{}, a.err
	}
	agg := trivia.Aggregate{From: from, To: to}
	if !a.empty {
		agg.Issues, agg.Facts = 3, 12
	}
	return agg, nil
}

func jobsSummaryDeps(agg *jobsFakeAggregate, sum *jobsFakeSummarizer, st *jobsFakeSummaryStore, now time.Time) daemon.SummaryDeps {
	return daemon.SummaryDeps{Aggregate: agg.fn, Summarizer: sum, Store: st, Now: func() time.Time { return now }}
}

var jobsNow = time.Date(2026, 9, 24, 9, 0, 7, 500, time.UTC)

func TestSummaryJob(t *testing.T) {
	ctx := context.Background()
	agg := &jobsFakeAggregate{}
	long := strings.Repeat("За сутки вышло три выпуска, самый интересный — о мануле. ", 5)
	sum := &jobsFakeSummarizer{text: "  " + long, cost: 0.002}
	st := &jobsFakeSummaryStore{}
	job := daemon.SummaryJob(jobsSummaryDeps(agg, sum, st, jobsNow), "09:00")
	if job.Name != daemon.JobSummary || job.Daily != "09:00" || !job.Paid {
		t.Fatalf("задание: %+v", job)
	}

	out, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("запуск: %v", err)
	}
	// Конец периода — начало минуты запуска, начало — ровно сутками раньше.
	wantTo := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	if !agg.to.Equal(wantTo) || !agg.from.Equal(wantTo.Add(-24*time.Hour)) {
		t.Errorf("период %s — %s", agg.from, agg.to)
	}
	if len(st.saved) != 1 {
		t.Fatalf("сохранено %d", len(st.saved))
	}
	s := st.saved[0]
	if s.Trigger != "schedule" || s.Aggregate.Issues != 3 || s.Text != strings.TrimSpace(long) ||
		s.Error != "" || s.Cost.USD != 0.002 || !s.From.Equal(agg.from) || !s.To.Equal(agg.to) {
		t.Errorf("сводка: %+v", s)
	}
	if out.Ref != "summary:1" || out.CostUSD != 0.002 {
		t.Errorf("Outcome %+v", out)
	}
	if n := utf8.RuneCountInString(out.Detail); n > 121 || !strings.HasSuffix(out.Detail, "…") ||
		!strings.HasPrefix(out.Detail, "За сутки вышло три выпуска") {
		t.Errorf("Detail (%d знаков) %q", n, out.Detail)
	}
}

func TestSummaryJobPeriod(t *testing.T) {
	agg := &jobsFakeAggregate{}
	d := jobsSummaryDeps(agg, &jobsFakeSummarizer{text: "Коротко."}, &jobsFakeSummaryStore{}, jobsNow)
	d.Period = 7 * 24 * time.Hour
	out, err := daemon.SummaryJob(d, "09:00").Run(context.Background())
	if err != nil || out.Detail != "Коротко." {
		t.Fatalf("%+v, %v", out, err)
	}
	if got := agg.to.Sub(agg.from); got != 7*24*time.Hour {
		t.Errorf("период %s", got)
	}
}

// Модель упала: сводка всё равно сохранена с агрегатом и ошибкой, запуск
// failed, но с Ref и расходом.
func TestSummaryJobModelFailure(t *testing.T) {
	agg := &jobsFakeAggregate{}
	sum := &jobsFakeSummarizer{cost: 0.001, err: errors.New("таймаут модели")}
	st := &jobsFakeSummaryStore{}
	out, err := daemon.SummaryJob(jobsSummaryDeps(agg, sum, st, jobsNow), "09:00").Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "таймаут модели") {
		t.Fatalf("ошибка: %v", err)
	}
	if len(st.saved) != 1 {
		t.Fatalf("сохранено %d", len(st.saved))
	}
	s := st.saved[0]
	if s.Text != "" || !strings.Contains(s.Error, "таймаут модели") || s.Aggregate.Issues != 3 || s.Cost.USD != 0.001 {
		t.Errorf("сводка: %+v", s)
	}
	if out.Ref != "summary:1" || out.CostUSD != 0.001 || !strings.HasPrefix(out.Detail, "сводка без текста: ") {
		t.Errorf("Outcome %+v", out)
	}
}

// Пустой агрегат: текст пишет код (без расхода) — сводка обычная.
func TestSummaryJobEmptyAggregate(t *testing.T) {
	agg := &jobsFakeAggregate{empty: true}
	sum := &jobsFakeSummarizer{text: "За период выпусков не было."}
	st := &jobsFakeSummaryStore{}
	out, err := daemon.SummaryJob(jobsSummaryDeps(agg, sum, st, jobsNow), "09:00").Run(context.Background())
	if err != nil {
		t.Fatalf("запуск: %v", err)
	}
	if sum.got.Issues != 0 || len(st.saved) != 1 || st.saved[0].Aggregate.Issues != 0 {
		t.Errorf("агрегат %+v, сохранено %d", sum.got, len(st.saved))
	}
	if out.CostUSD != 0 || out.Ref != "summary:1" || out.Detail != "За период выпусков не было." {
		t.Errorf("Outcome %+v", out)
	}
}

func TestSummaryJobAggregateError(t *testing.T) {
	agg := &jobsFakeAggregate{err: errors.New("база заблокирована")}
	sum := &jobsFakeSummarizer{text: "x"}
	st := &jobsFakeSummaryStore{}
	out, err := daemon.SummaryJob(jobsSummaryDeps(agg, sum, st, jobsNow), "09:00").Run(context.Background())
	if err == nil || out != (schedule.Outcome{}) || sum.calls != 0 || len(st.saved) != 0 {
		t.Errorf("Outcome %+v, ошибка %v, модель %d, сохранено %d", out, err, sum.calls, len(st.saved))
	}
}

// Сводка не сохранилась, а модель уже заплачена: расход виден в Outcome.
func TestSummaryJobSaveError(t *testing.T) {
	sum := &jobsFakeSummarizer{text: "Текст.", cost: 0.003}
	st := &jobsFakeSummaryStore{err: errors.New("диск полон")}
	out, err := daemon.SummaryJob(jobsSummaryDeps(&jobsFakeAggregate{}, sum, st, jobsNow), "09:00").Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "диск полон") {
		t.Fatalf("ошибка: %v", err)
	}
	if out.CostUSD != 0.003 || out.Ref != "" {
		t.Errorf("Outcome %+v", out)
	}
}

func TestBuildSummaryManual(t *testing.T) {
	agg := &jobsFakeAggregate{}
	st := &jobsFakeSummaryStore{}
	d := jobsSummaryDeps(agg, &jobsFakeSummarizer{text: "Итог."}, st, jobsNow)
	from := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	s, err := daemon.BuildSummary(context.Background(), d, from, to, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != 1 || s.Trigger != "manual" || !s.From.Equal(from) || !s.To.Equal(to) ||
		!agg.from.Equal(from) || !agg.to.Equal(to) || !s.CreatedAt.Equal(jobsNow) {
		t.Errorf("сводка %+v", s)
	}
	if got, _ := st.Summary(context.Background(), 1); got.Text != "Итог." {
		t.Errorf("в хранилище %+v", got)
	}

	if _, err := daemon.BuildSummary(context.Background(), d, to, from, "manual"); err == nil {
		t.Error("перевёрнутый период принят")
	}
	if _, err := daemon.BuildSummary(context.Background(), daemon.SummaryDeps{}, from, to, "manual"); err == nil {
		t.Error("сводка без зависимостей")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := daemon.BuildSummary(ctx, d, from, to, "manual"); !errors.Is(err, context.Canceled) || len(st.saved) != 1 {
		t.Errorf("отменённый ctx: %v, сохранено %d", err, len(st.saved))
	}
}

// ------------------------------------------------------ журнал запусков

// jobsFakeRunStore — RunStore в памяти: Runs фильтрует по [Since, Until),
// сортирует новыми первыми и режет по Limit (0 → 50, предел 500), как
// требует контракт. Остальные методы тесту не нужны.
type jobsFakeRunStore struct {
	runs    []schedule.Run
	queries []schedule.RunQuery
}

func (s *jobsFakeRunStore) Runs(ctx context.Context, q schedule.RunQuery) ([]schedule.Run, error) {
	s.queries = append(s.queries, q)
	var out []schedule.Run
	for _, r := range s.runs {
		if !q.Since.IsZero() && r.Started.Before(q.Since) {
			continue
		}
		if !q.Until.IsZero() && !r.Started.Before(q.Until) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Started.Equal(out[j].Started) {
			return out[i].Started.After(out[j].Started)
		}
		return out[i].ID > out[j].ID
	})
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *jobsFakeRunStore) Begin(context.Context, schedule.Run) (int64, error)  { return 0, nil }
func (s *jobsFakeRunStore) Finish(context.Context, schedule.Run) error          { return nil }
func (s *jobsFakeRunStore) Record(context.Context, schedule.Run) (int64, error) { return 0, nil }
func (s *jobsFakeRunStore) Last(context.Context, string) (schedule.Run, bool, error) {
	return schedule.Run{}, false, nil
}
func (s *jobsFakeRunStore) Spent(context.Context, time.Time, time.Time) (float64, error) {
	return 0, nil
}
func (s *jobsFakeRunStore) Abandon(context.Context, time.Time) (int, error) { return 0, nil }

var _ schedule.RunStore = (*jobsFakeRunStore)(nil)

func TestRunSourceOf(t *testing.T) {
	base := time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)
	st := &jobsFakeRunStore{runs: []schedule.Run{
		{ID: 1, Job: "issue", Status: schedule.RunOK, Started: base.Add(-time.Hour)}, // до периода
		{ID: 2, Job: "mdd", Status: schedule.RunOK, Started: base, Outcome: schedule.Outcome{Ref: "mdd:v2.5", Detail: "релиз v2.5 не менялся"}},
		{ID: 3, Job: "issue", Status: schedule.RunFailed, Started: base.Add(time.Hour), Error: "досье: сеть",
			Outcome: schedule.Outcome{CostUSD: 0.01, Ref: "issue:7", Detail: "Манул: выпуск не собран"}},
		{ID: 4, Job: "issue", Status: schedule.RunBudget, Started: base.Add(2 * time.Hour)},
		{ID: 5, Job: "summary", Status: schedule.RunRunning, Started: base.Add(3 * time.Hour)},
		{ID: 6, Job: "issue", Status: schedule.RunOK, Started: base.Add(24 * time.Hour)}, // конец исключён
	}}
	got, err := daemon.RunSourceOf(st).RunsBetween(context.Background(), base, base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []trivia.RunInfo{
		{Job: "mdd", Status: "ok", Started: base, Ref: "mdd:v2.5", Detail: "релиз v2.5 не менялся"},
		{Job: "issue", Status: "failed", Started: base.Add(time.Hour), CostUSD: 0.01, Ref: "issue:7",
			Detail: "Манул: выпуск не собран", Error: "досье: сеть"},
		{Job: "issue", Status: "budget", Started: base.Add(2 * time.Hour)},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("запуски:\n%+v\nожидалось\n%+v", got, want)
	}
	if q := st.queries[0]; q.Limit != 500 || !q.Since.Equal(base) || !q.Until.Equal(base.Add(24*time.Hour)) {
		t.Errorf("запрос %+v", q)
	}
}

// Больше 500 запусков: журнал читается окнами, а запуски с одинаковым
// Started на границе окна не теряются и не повторяются.
func TestRunSourceOfPaging(t *testing.T) {
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	st := &jobsFakeRunStore{}
	const n = 1300
	for i := 0; i < n; i++ {
		st.runs = append(st.runs, schedule.Run{ID: int64(i + 1), Job: "issue", Status: schedule.RunOK,
			Started: from.Add(time.Duration(i) * time.Minute)})
	}
	// Пять запусков вокруг границы первого окна (500-й с конца) — в одну
	// и ту же секунду.
	same := st.runs[n-503].Started
	for i := n - 503; i < n-498; i++ {
		st.runs[i].Started = same
	}
	got, err := daemon.RunSourceOf(st).RunsBetween(context.Background(), from, from.Add(n*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("запусков %d, ожидалось %d (запросов %d)", len(got), n, len(st.queries))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Started.Before(got[i-1].Started) {
			t.Fatalf("порядок нарушен на %d", i)
		}
	}
	if len(st.queries) < 3 {
		t.Errorf("запросов %d — окна не использовались", len(st.queries))
	}
}
