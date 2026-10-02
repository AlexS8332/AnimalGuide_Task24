package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/daemon"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia/triviatest"
)

// Пояс демона в тестах — не UTC и не пояс машины: так видно, что время в
// ответах переводится в пояс из Status. Имя «MSK» LoadLocation не знает —
// заодно проверяется запасной путь через Status.Now.
var (
	toolsLoc = time.FixedZone("MSK", 3*3600)
	toolsNow = time.Date(2026, 9, 24, 12, 30, 0, 0, toolsLoc)
)

// toolsFakeService — Service на Memory-хранилищах с управляемыми
// Status, RunNow и BuildSummary.
type toolsFakeService struct {
	triv *trivia.Memory
	sp   *mdd.Memory

	mu        sync.Mutex
	status    schedule.Status
	statusErr error
	runs      map[string]schedule.Run
	runErr    error
	runCalls  []string
	build     func(from, to time.Time) (trivia.Summary, error)
	builds    [][2]time.Time
}

func (f *toolsFakeService) Issues() trivia.IssueStore      { return f.triv }
func (f *toolsFakeService) Summaries() trivia.SummaryStore { return f.triv }
func (f *toolsFakeService) Species() mdd.Store             { return f.sp }

func (f *toolsFakeService) Status(ctx context.Context) (schedule.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.statusErr
}

func (f *toolsFakeService) RunNow(ctx context.Context, job string) (schedule.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCalls = append(f.runCalls, job)
	if f.runErr != nil {
		return schedule.Run{}, fmt.Errorf("%w: %q", f.runErr, job)
	}
	r, ok := f.runs[job]
	if !ok {
		return schedule.Run{}, fmt.Errorf("%w: %q", schedule.ErrUnknownJob, job)
	}
	return r, nil
}

func (f *toolsFakeService) BuildSummary(ctx context.Context, from, to time.Time) (trivia.Summary, error) {
	f.mu.Lock()
	f.builds = append(f.builds, [2]time.Time{from, to})
	build := f.build
	f.mu.Unlock()
	if build == nil {
		return trivia.Summary{}, errors.New("сборка не настроена")
	}
	return build(from, to)
}

var _ daemon.Service = (*toolsFakeService)(nil)

// toolsStatus — расписание трёх заданий: выпуск через 30 минут (последний
// час назад), сводка завтра в 09:00, релиз MDD в 04:00.
func toolsStatus() schedule.Status {
	lastIssue := &schedule.Run{ID: 7, Job: daemon.JobIssue, Trigger: schedule.TriggerSchedule,
		Scheduled: toolsNow.Add(-90 * time.Minute).UTC(), Started: toolsNow.Add(-90 * time.Minute).UTC(),
		Finished: toolsNow.Add(-88 * time.Minute).UTC(), Status: schedule.RunOK,
		Outcome: schedule.Outcome{CostUSD: 0.007, Ref: "issue:1", Detail: "Манул (Otocolobus manul): 3 факта"}}
	return schedule.Status{
		Now: toolsNow, Location: "MSK", Budget: 0.5, Spent: 0.1234,
		Jobs: []schedule.JobStatus{
			{Name: daemon.JobIssue, Every: "1h0m0s", Paid: true, Next: toolsNow.Add(30 * time.Minute).UTC(), Last: lastIssue},
			{Name: daemon.JobSummary, Daily: "09:00", Paid: true, Next: time.Date(2026, 9, 25, 9, 0, 0, 0, toolsLoc)},
			{Name: daemon.JobMDD, Daily: "04:00", Next: time.Date(2026, 9, 25, 4, 0, 0, 0, toolsLoc)},
		},
	}
}

// toolsSetup — пустые хранилища, загруженный MDD, расписание.
func toolsSetup(t *testing.T) *toolsFakeService {
	t.Helper()
	sp := mdd.NewMemory()
	if err := sp.Replace(context.Background(), mddtest.Sample()); err != nil {
		t.Fatal(err)
	}
	return &toolsFakeService{triv: trivia.NewMemory(), sp: sp, status: toolsStatus(), runs: map[string]schedule.Run{}}
}

// toolsSeed — четыре выпуска (в порядке сохранения):
//
//	1 — манул позавчера (ok);
//	2 — рысь два часа назад (failed);
//	3 — манул три часа назад (ok) — самый свежий о мануле;
//	4 — лев час назад (thin), в фактах упоминает манула.
func toolsSeed(t *testing.T, f *toolsFakeService) {
	t.Helper()
	ctx := context.Background()
	oldManul := triviatest.SampleIssue(1, mddtest.Manul, toolsNow.Add(-50*time.Hour).UTC())
	oldManul.Title = "Манул позавчера"

	lynx := triviatest.SampleIssue(2, mddtest.Lynx, toolsNow.Add(-2*time.Hour).UTC())
	lynx.SciName, lynx.NameRu, lynx.Status = "Lynx lynx", "Рысь", trivia.IssueFailed
	lynx.Error = "досье: Википедия: 503"
	lynx.Title, lynx.Lead, lynx.Facts, lynx.Dropped = "", "", nil, nil

	manul := triviatest.SampleIssue(3, mddtest.Manul, toolsNow.Add(-3*time.Hour).UTC())
	manul.Lead = strings.Repeat("Длинное вступление. ", 40) // 800 знаков — будет обрезано

	lion := triviatest.SampleIssue(4, mddtest.Lion, toolsNow.Add(-1*time.Hour).UTC())
	lion.SciName, lion.NameRu, lion.Status = "Panthera leo", "Лев", trivia.IssueThin
	lion.Title = "Лев и манул"
	lion.Facts = lion.Facts[:1]
	lion.Facts[0].Sources = []string{"S2", "S9"} // S9 в материалах нет

	for _, is := range []trivia.Issue{oldManul, lynx, manul, lion} {
		if _, err := f.triv.SaveIssue(ctx, is); err != nil {
			t.Fatal(err)
		}
	}
}

func toolsByName(ts []tools.Tool) map[string]tools.Tool {
	m := map[string]tools.Tool{}
	for _, tl := range ts {
		m[tl.Spec().Name] = tl
	}
	return m
}

func toolsCall(t *testing.T, tl tools.Tool, args string, out any) string {
	t.Helper()
	res, err := tl.Call(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s(%s): %v", tl.Spec().Name, args, err)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(res), out); err != nil {
			t.Fatalf("%s: ответ не JSON: %v\n%s", tl.Spec().Name, err, res)
		}
	}
	return res
}

func toolsCallErr(t *testing.T, tl tools.Tool, args string) string {
	t.Helper()
	res, err := tl.Call(context.Background(), json.RawMessage(args))
	if err == nil {
		t.Fatalf("%s(%s): ждали ошибку, получено %s", tl.Spec().Name, args, res)
	}
	return err.Error()
}

// Формы ответов — как их видит клиент.
type (
	toolsSource struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	toolsFact struct {
		Text    string        `json:"text"`
		Sources []toolsSource `json:"sources"`
		Reason  string        `json:"reason"`
	}
	toolsIssue struct {
		ID         int64       `json:"id"`
		CreatedAt  string      `json:"created_at"`
		SpeciesID  int         `json:"species_id"`
		SciName    string      `json:"sci_name"`
		NameRu     string      `json:"name_ru"`
		IUCN       string      `json:"iucn"`
		Status     string      `json:"status"`
		Error      string      `json:"error"`
		Title      string      `json:"title"`
		Lead       string      `json:"lead"`
		Facts      []toolsFact `json:"facts"`
		OutOfRange []string    `json:"out_of_range"`
		CostUSD    float64     `json:"cost_usd"`

		// Только у facts_get.
		Order        string      `json:"order"`
		Dropped      []toolsFact `json:"dropped"`
		Observations *struct {
			Total     int                   `json:"total"`
			Recent    int                   `json:"recent"`
			ByCountry []trivia.CountryCount `json:"by_country"`
		} `json:"observations"`
		Spend []struct {
			Step    string  `json:"step"`
			Model   string  `json:"model"`
			Tokens  int     `json:"tokens"`
			CostUSD float64 `json:"cost_usd"`
			Took    string  `json:"took"`
		} `json:"spend"`
		Took string `json:"took"`
	}
	toolsLatestOut struct {
		Total    int          `json:"total"`
		Returned int          `json:"returned"`
		Issues   []toolsIssue `json:"issues"`
		Hint     string       `json:"hint"`
	}
	toolsSearchOut struct {
		Total      int          `json:"total"`
		Returned   int          `json:"returned"`
		Offset     int          `json:"offset"`
		Issues     []toolsIssue `json:"issues"`
		NextOffset int          `json:"next_offset"`
		Hint       string       `json:"hint"`
	}
	toolsSummaryOut struct {
		ID        int64            `json:"id"`
		From      string           `json:"from"`
		To        string           `json:"to"`
		CreatedAt string           `json:"created_at"`
		Trigger   string           `json:"trigger"`
		Text      string           `json:"text"`
		Error     string           `json:"error"`
		Aggregate trivia.Aggregate `json:"aggregate"`
		Cost      llm.Cost         `json:"cost"`
	}
)

func toolsIDs(list []toolsIssue) []int64 {
	out := []int64{}
	for _, is := range list {
		out = append(out, is.ID)
	}
	return out
}

func TestToolsSpecs(t *testing.T) {
	ts := daemon.Tools(toolsSetup(t))
	if got := tools.Names(ts); !reflect.DeepEqual(got, daemon.ToolNames) {
		t.Fatalf("имена %v, ждали %v", got, daemon.ToolNames)
	}
	if _, err := tools.NewRegistry(ts...); err != nil {
		t.Fatal(err)
	}
	for _, tl := range ts {
		s := tl.Spec()
		wantWrite := s.Name == daemon.ToolSummaryBuild || s.Name == daemon.ToolRunNow
		if s.Write != wantWrite {
			t.Errorf("%s: Write=%v", s.Name, s.Write)
		}
		isContent := strings.HasPrefix(s.Name, "facts_") || strings.HasPrefix(s.Name, "summary_")
		if isContent && !s.Untrusted {
			t.Errorf("%s: тексты выпусков — внешнее содержимое, нужен Untrusted", s.Name)
		}
		if s.Via != tools.ViaLocal {
			t.Errorf("%s: Via=%q", s.Name, s.Via)
		}
		if !strings.Contains(s.Description, "Интересные факты") || !strings.Contains(s.Description, "раз в час") {
			t.Errorf("%s: описание не объясняет, что такое выпуски: %s", s.Name, s.Description)
		}
		var schema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Additional *bool                      `json:"additionalProperties"`
		}
		if err := json.Unmarshal(s.Parameters, &schema); err != nil {
			t.Fatalf("%s: схема не JSON: %v", s.Name, err)
		}
		if schema.Type != "object" || schema.Properties == nil || schema.Additional == nil || *schema.Additional {
			t.Errorf("%s: схема %s", s.Name, s.Parameters)
		}
	}
}

func TestToolsFactsLatest(t *testing.T) {
	f := toolsSetup(t)
	latest := toolsByName(daemon.Tools(f))[daemon.ToolFactsLatest]

	// Пустое хранилище — ответ с подсказкой, а не ошибка.
	var empty toolsLatestOut
	toolsCall(t, latest, `{}`, &empty)
	if empty.Total != 0 || len(empty.Issues) != 0 || !strings.Contains(empty.Hint, daemon.ToolRunNow) {
		t.Errorf("пусто: %+v", empty)
	}

	toolsSeed(t, f)
	var out toolsLatestOut
	toolsCall(t, latest, ``, &out)
	// По умолчанию failed не показывается: 4 (лев), 3, 1.
	if got := toolsIDs(out.Issues); !reflect.DeepEqual(got, []int64{4, 3, 1}) || out.Total != 3 || out.Returned != 3 || out.Hint != "" {
		t.Errorf("по умолчанию: %v, %+v", got, out)
	}
	m := out.Issues[1]
	if m.CreatedAt != "2026-09-24T09:30:00+03:00" {
		t.Errorf("время не в поясе демона: %q", m.CreatedAt)
	}
	if m.SciName != "Otocolobus manul" || m.NameRu != "Манул" || m.IUCN != "LC" || m.Status != "ok" ||
		m.Title == "" || m.SpeciesID != mddtest.Manul || math.Abs(m.CostUSD-0.007) > 1e-9 {
		t.Errorf("выпуск: %+v", m)
	}
	if !strings.HasSuffix(m.Lead, "…[обрезано]") || len([]rune(m.Lead)) > 420 {
		t.Errorf("вступление не обрезано: %d рун", len([]rune(m.Lead)))
	}
	if len(m.Facts) != 3 || !strings.HasPrefix(m.Facts[0].Text, "У манула круглые зрачки") {
		t.Fatalf("факты: %+v", m.Facts)
	}
	wantSrc := []toolsSource{
		{ID: "S1", Title: "Mammal Diversity Database: Otocolobus manul", URL: "https://www.mammaldiversity.org/taxon/1006010"},
		{ID: "S2", Title: "Манул — Википедия", URL: "https://ru.wikipedia.org/wiki/%D0%9C%D0%B0%D0%BD%D1%83%D0%BB"},
	}
	if !reflect.DeepEqual(m.Facts[1].Sources, wantSrc) {
		t.Errorf("источники не развёрнуты: %+v", m.Facts[1].Sources)
	}
	if !reflect.DeepEqual(m.OutOfRange, []string{"Germany"}) {
		t.Errorf("вне ареала: %v", m.OutOfRange)
	}
	if m.Observations != nil || m.Dropped != nil || m.Spend != nil {
		t.Errorf("компактный выпуск несёт лишнее: %+v", m)
	}
	// Ссылка на несуществующий материал остаётся голым id.
	if src := out.Issues[0].Facts[0].Sources; len(src) != 2 || src[1] != (toolsSource{ID: "S9"}) {
		t.Errorf("неизвестный источник: %+v", src)
	}

	var page toolsLatestOut
	toolsCall(t, latest, `{"limit":2,"status":["failed","ok","thin"]}`, &page)
	if got := toolsIDs(page.Issues); !reflect.DeepEqual(got, []int64{4, 2}) || page.Total != 4 ||
		!strings.Contains(page.Hint, daemon.ToolFactsSearch) {
		t.Errorf("limit и status: %v, %+v", got, page)
	}
	if page.Issues[1].Error != "досье: Википедия: 503" || len(page.Issues[1].Facts) != 0 {
		t.Errorf("failed: %+v", page.Issues[1])
	}
	var failed toolsLatestOut
	toolsCall(t, latest, `{"status":["FAILED"]}`, &failed)
	if got := toolsIDs(failed.Issues); !reflect.DeepEqual(got, []int64{2}) {
		t.Errorf("только failed: %v", got)
	}
	var big toolsLatestOut
	toolsCall(t, latest, `{"limit":500}`, &big)
	if big.Returned != 3 {
		t.Errorf("limit сверх предела: %+v", big)
	}

	for args, want := range map[string]string{
		`{"limit":-1}`:          "отрицательным",
		`{"status":["draft"]}`:  "неизвестное состояние",
		`{"status":"ok"}`:       "аргументы не разобрались",
		`{"text":"манул"}`:      "аргументы не разобрались",
		`{"limit":"5"}`:         "аргументы не разобрались",
		`not json`:              "аргументы не разобрались",
		`{"limit":5,"extra":1}`: "unknown field",
	} {
		if msg := toolsCallErr(t, latest, args); !strings.Contains(msg, want) {
			t.Errorf("%s: %q, ждали «%s»", args, msg, want)
		}
	}
}

func TestToolsFactsGet(t *testing.T) {
	f := toolsSetup(t)
	get := toolsByName(daemon.Tools(f))[daemon.ToolFactsGet]

	if msg := toolsCallErr(t, get, `{"species":"Манул"}`); !strings.Contains(msg, "ещё не было") ||
		!strings.Contains(msg, daemon.ToolFactsSearch) {
		t.Errorf("пустое хранилище: %q", msg)
	}
	toolsSeed(t, f)

	cases := map[string]int64{
		`{"id":1}`:                       1,
		`{"id":2}`:                       2,
		`{"species":"1006010"}`:          3, // mdd-id строкой — самый свежий о мануле
		`{"species":1006010}`:            3, // и числом
		`{"species":"Otocolobus manul"}`: 3,
		`{"species":"pallas's cat"}`:     3, // английское
		`{"species":"Манул"}`:            3, // русское: лев свежее и упоминает манула, но имя точнее
		`{"species":"  МАНУЛ "}`:         3,
		`{"species":"Рысь"}`:             2,
		`{"species":"Panthera leo"}`:     4,
		`{"species":"круглые зрачки"}`:   4, // не название: первый найденный по тексту (лев свежее)
	}
	for args, want := range cases {
		var is toolsIssue
		toolsCall(t, get, args, &is)
		if is.ID != want {
			t.Errorf("%s: выпуск %d, ждали %d", args, is.ID, want)
		}
	}

	var full toolsIssue
	toolsCall(t, get, `{"id":3}`, &full)
	if full.Order != "Carnivora" || len(full.Facts) != 3 || full.Took != "38.1s" {
		t.Errorf("выпуск целиком: %+v", full)
	}
	if len(full.Dropped) != 1 || full.Dropped[0].Reason != "в статье нет ничего о плавании" ||
		full.Dropped[0].Sources[0].Title != "Манул — Википедия" {
		t.Errorf("отброшенные: %+v", full.Dropped)
	}
	if o := full.Observations; o == nil || o.Total != 1843 || o.Recent != 97 || len(o.ByCountry) != 5 || o.ByCountry[0].Name != "Mongolia" {
		t.Errorf("наблюдения: %+v", full.Observations)
	}
	if len(full.Spend) != 2 || full.Spend[0].Step != "editor" || full.Spend[1].Step != "verifier" ||
		full.Spend[0].Model != "deepseek-v4-pro" || full.Spend[0].Tokens != 6100 || full.Spend[0].Took != "14.3s" {
		t.Errorf("расход: %+v", full.Spend)
	}
	if full.Lead == "" || strings.Contains(full.CreatedAt, "Z") {
		t.Errorf("поля компактного вида: %+v", full)
	}

	for args, want := range map[string]string{
		`{}`:                           "нужен один аргумент",
		``:                             "нужен один аргумент",
		`{"species":"  "}`:             "нужен один аргумент",
		`{"id":1,"species":"Манул"}`:   "не оба",
		`{"id":0}`:                     "положительным",
		`{"species":"-5"}`:             "положительным",
		`{"species":true}`:             "species",
		`{"name":"Манул"}`:             "аргументы не разобрались",
		`{"id":99}`:                    "выпуска с id 99 нет",
		`{"species":"Panthera uncia"}`: "Panthera uncia (mdd-id 1006024)",
		`{"species":"1000001"}`:        "Ornithorhynchus anatinus",
		`{"species":"42"}`:             "с mdd-id 42",
		`{"species":"Утконос"}`:        "facts_search ищет по тексту",
	} {
		if msg := toolsCallErr(t, get, args); !strings.Contains(msg, want) {
			t.Errorf("%s: %q, ждали «%s»", args, msg, want)
		}
	}
}

func TestToolsFactsSearch(t *testing.T) {
	f := toolsSetup(t)
	search := toolsByName(daemon.Tools(f))[daemon.ToolFactsSearch]

	var empty toolsSearchOut
	toolsCall(t, search, `{}`, &empty)
	if empty.Total != 0 || empty.Issues == nil || !strings.Contains(empty.Hint, "Ничего не найдено") {
		t.Errorf("пусто: %+v", empty)
	}
	toolsSeed(t, f)

	check := func(args string, want []int64, total int) toolsSearchOut {
		t.Helper()
		var out toolsSearchOut
		toolsCall(t, search, args, &out)
		if got := toolsIDs(out.Issues); !reflect.DeepEqual(got, want) || out.Total != total || out.Returned != len(want) {
			t.Errorf("%s: %v (total %d), ждали %v (total %d)", args, got, out.Total, want, total)
		}
		return out
	}
	all := check(`{}`, []int64{4, 2, 3, 1}, 4)
	if r := all.Issues[1]; r.SciName != "Lynx lynx" || r.NameRu != "Рысь" || r.Status != "failed" ||
		r.CreatedAt != "2026-09-24T10:30:00+03:00" || r.Facts != nil || r.Lead != "" {
		t.Errorf("строка: %+v", r)
	}
	check(`{"text":"МАНУЛ"}`, []int64{4, 3, 1}, 3)
	check(`{"text":"манул","status":["thin"]}`, []int64{4}, 1)
	check(`{"since":"2026-09-24"}`, []int64{4, 2, 3}, 3)
	check(`{"until":"2026-09-22"}`, []int64{1}, 1) // дата — включительно: выпуск 22.09 10:30 по Москве
	check(`{"until":"2026-09-22T10:30:00+03:00"}`, []int64{}, 0)
	check(`{"since":"2026-09-24T10:00:00+03:00","until":"2026-09-24T11:00:00+03:00"}`, []int64{2}, 1)
	check(`{"since":"2026-09-24T07:00:00Z"}`, []int64{4, 2}, 2) // 10:00 по Москве

	page := check(`{"limit":2}`, []int64{4, 2}, 4)
	if page.NextOffset != 2 || !strings.Contains(page.Hint, "offset=2") {
		t.Errorf("первая страница: %+v", page)
	}
	last := check(`{"limit":2,"offset":2}`, []int64{3, 1}, 4)
	if last.NextOffset != 0 || last.Hint != "" || last.Offset != 2 {
		t.Errorf("последняя страница: %+v", last)
	}
	beyond := check(`{"offset":10}`, []int64{}, 4)
	if !strings.Contains(beyond.Hint, "за пределами") {
		t.Errorf("за пределами: %+v", beyond)
	}

	for args, want := range map[string]string{
		`{"limit":-1}`:                                "отрицательными",
		`{"offset":-1}`:                               "отрицательными",
		`{"status":["ok","bad"]}`:                     "неизвестное состояние",
		`{"since":"вчера"}`:                           "since: ожидалась дата",
		`{"until":"24.09.2026"}`:                      "until: ожидалась дата",
		`{"since":"2026-09-24","until":"2026-09-23"}`: "пустой период",
		`{"since":"2026-09-24","until":"2026-09-24T00:00:00+03:00"}`: "пустой период",
		`{"species":"манул"}`:                                        "аргументы не разобрались",
	} {
		if msg := toolsCallErr(t, search, args); !strings.Contains(msg, want) {
			t.Errorf("%s: %q, ждали «%s»", args, msg, want)
		}
	}
}

// toolsSaveSummaries сохраняет n сводок, сутки через сутки, последняя — сегодня в 09:01:30.
func toolsSaveSummaries(t *testing.T, f *toolsFakeService, n int) {
	t.Helper()
	for i := n - 1; i >= 0; i-- {
		sm := triviatest.SampleSummary(time.Date(2026, 9, 24, 9, 0, 0, 0, toolsLoc).AddDate(0, 0, -i).UTC())
		sm.Text = fmt.Sprintf("Сводка %d. ", n-i) + strings.Repeat("Длинный текст. ", 30)
		if _, err := f.triv.SaveSummary(context.Background(), sm); err != nil {
			t.Fatal(err)
		}
	}
}

func TestToolsSummaryGet(t *testing.T) {
	f := toolsSetup(t)
	get := toolsByName(daemon.Tools(f))[daemon.ToolSummaryGet]

	// Сводок нет: подсказка со временем первой сводки из расписания.
	msg := toolsCallErr(t, get, `{}`)
	if !strings.Contains(msg, "25.09.2026 09:00") || !strings.Contains(msg, "через 20 ч 30 мин") ||
		!strings.Contains(msg, daemon.ToolSummaryBuild) {
		t.Errorf("сводок нет: %q", msg)
	}
	var emptyList struct {
		Returned  int               `json:"returned"`
		Summaries []toolsSummaryOut `json:"summaries"`
		Hint      string            `json:"hint"`
	}
	toolsCall(t, get, `{"list":true}`, &emptyList)
	if emptyList.Returned != 0 || emptyList.Summaries == nil || !strings.Contains(emptyList.Hint, "25.09.2026 09:00") {
		t.Errorf("пустой список: %+v", emptyList)
	}
	// Расписание недоступно — подсказка без времени, но не падает.
	f.statusErr = errors.New("база закрыта")
	if msg := toolsCallErr(t, get, `{}`); !strings.Contains(msg, "раз в сутки") {
		t.Errorf("без расписания: %q", msg)
	}
	f.statusErr = nil

	toolsSaveSummaries(t, f, 12)
	var latest toolsSummaryOut
	toolsCall(t, get, ``, &latest)
	want := triviatest.SampleSummary(time.Date(2026, 9, 24, 9, 0, 0, 0, toolsLoc).UTC())
	if latest.ID != 12 || latest.From != "2026-09-23T09:00:00+03:00" || latest.To != "2026-09-24T09:00:00+03:00" ||
		latest.CreatedAt != "2026-09-24T09:01:30+03:00" || latest.Trigger != "schedule" ||
		!strings.HasPrefix(latest.Text, "Сводка 12. ") || strings.Contains(latest.Text, "обрезано") {
		t.Errorf("последняя: %+v", latest)
	}
	if !reflect.DeepEqual(latest.Cost, want.Cost) || latest.Aggregate.Issues != 3 ||
		!reflect.DeepEqual(latest.Aggregate.Species, want.Aggregate.Species) ||
		!reflect.DeepEqual(latest.Aggregate.MDDRelease, want.Aggregate.MDDRelease) ||
		!latest.Aggregate.From.Equal(want.Aggregate.From) {
		t.Errorf("агрегат или расход: %+v", latest)
	}

	var byID toolsSummaryOut
	toolsCall(t, get, `{"id":3}`, &byID)
	if byID.ID != 3 || !strings.HasPrefix(byID.Text, "Сводка 3. ") {
		t.Errorf("по id: %+v", byID)
	}

	var list struct {
		Returned  int `json:"returned"`
		Summaries []struct {
			ID        int64  `json:"id"`
			From      string `json:"from"`
			CreatedAt string `json:"created_at"`
			Trigger   string `json:"trigger"`
			Text      string `json:"text"`
		} `json:"summaries"`
		Hint string `json:"hint"`
	}
	raw := toolsCall(t, get, `{"list":true}`, &list)
	if list.Returned != 10 || len(list.Summaries) != 10 || list.Summaries[0].ID != 12 || list.Summaries[9].ID != 3 {
		t.Fatalf("список: %+v", list)
	}
	if r := list.Summaries[0]; !strings.HasSuffix(r.Text, "…[обрезано]") || len([]rune(r.Text)) > 215 ||
		r.From != "2026-09-23T09:00:00+03:00" || r.Trigger != "schedule" {
		t.Errorf("строка списка: %+v", r)
	}
	if strings.Contains(raw, "aggregate") {
		t.Errorf("список несёт агрегаты: %s", raw)
	}

	for args, want := range map[string]string{
		`{"id":1,"list":true}`: "не оба",
		`{"id":0}`:             "положительным",
		`{"id":99}`:            "сводки с id 99 нет",
		`{"list":"yes"}`:       "аргументы не разобрались",
		`{"hours":24}`:         "аргументы не разобрались",
	} {
		if msg := toolsCallErr(t, get, args); !strings.Contains(msg, want) {
			t.Errorf("%s: %q, ждали «%s»", args, msg, want)
		}
	}
}

func TestToolsSummaryBuild(t *testing.T) {
	f := toolsSetup(t)
	build := toolsByName(daemon.Tools(f))[daemon.ToolSummaryBuild]
	f.build = func(from, to time.Time) (trivia.Summary, error) {
		sm := triviatest.SampleSummary(to)
		sm.From, sm.Aggregate.From = from, from
		sm.Trigger = trivia.SummaryTriggerManual
		id, err := f.triv.SaveSummary(context.Background(), sm)
		sm.ID = id
		return sm, err
	}

	var out toolsSummaryOut
	toolsCall(t, build, `{}`, &out)
	if len(f.builds) != 1 || !f.builds[0][1].Equal(toolsNow) || f.builds[0][1].Sub(f.builds[0][0]) != 24*time.Hour {
		t.Fatalf("период по умолчанию: %v", f.builds)
	}
	if out.ID != 1 || out.Trigger != "manual" || out.From != "2026-09-23T12:30:00+03:00" || out.To != "2026-09-24T12:30:00+03:00" ||
		out.Aggregate.Issues != 3 {
		t.Errorf("сводка: %+v", out)
	}
	toolsCall(t, build, `{"hours":168}`, nil)
	if d := f.builds[1][1].Sub(f.builds[1][0]); d != 168*time.Hour {
		t.Errorf("hours=168: %v", d)
	}

	// Модель не ответила, но сводка сохранена — ответ с error, не ошибка.
	f.build = func(from, to time.Time) (trivia.Summary, error) {
		sm := triviatest.SampleSummary(to)
		sm.ID, sm.Text, sm.Error = 5, "", "модель: таймаут"
		return sm, errors.New("модель: таймаут")
	}
	var partial toolsSummaryOut
	toolsCall(t, build, `{"hours":1}`, &partial)
	if partial.ID != 5 || partial.Error != "модель: таймаут" {
		t.Errorf("сводка без текста: %+v", partial)
	}

	f.build = func(from, to time.Time) (trivia.Summary, error) {
		return trivia.Summary{}, errors.New("агрегат: база закрыта")
	}
	if msg := toolsCallErr(t, build, `{}`); !strings.Contains(msg, "сводка не собрана: агрегат: база закрыта") {
		t.Errorf("сбой: %q", msg)
	}

	f.build = func(from, to time.Time) (trivia.Summary, error) { return trivia.Summary{}, daemon.ErrBudget }
	f.status.Spent = 0.5
	msg := toolsCallErr(t, build, `{}`)
	if !strings.Contains(msg, "лимит") || !strings.Contains(msg, "$0.5000 из $0.50") {
		t.Errorf("лимит: %q", msg)
	}

	n := len(f.builds)
	for args, want := range map[string]string{
		`{"hours":0}`:   "от 1 до 168",
		`{"hours":169}`: "от 1 до 168",
		`{"hours":-3}`:  "от 1 до 168",
		`{"hours":1.5}`: "аргументы не разобрались",
		`{"days":1}`:    "аргументы не разобрались",
	} {
		if msg := toolsCallErr(t, build, args); !strings.Contains(msg, want) {
			t.Errorf("%s: %q, ждали «%s»", args, msg, want)
		}
	}
	if len(f.builds) != n {
		t.Errorf("неверные аргументы дошли до сборки: %d вызовов", len(f.builds)-n)
	}
}

func TestToolsScheduleStatus(t *testing.T) {
	f := toolsSetup(t)
	status := toolsByName(daemon.Tools(f))[daemon.ToolScheduleStatus]

	var out struct {
		Now        string  `json:"now"`
		Location   string  `json:"location"`
		Budget     float64 `json:"budget_usd"`
		Spent      float64 `json:"spent_today_usd"`
		BudgetText string  `json:"budget_text"`
		Jobs       []struct {
			Name     string        `json:"name"`
			Every    string        `json:"every"`
			Daily    string        `json:"daily"`
			Paid     bool          `json:"paid"`
			Running  bool          `json:"running"`
			Next     string        `json:"next"`
			Last     *schedule.Run `json:"last"`
			NextText string        `json:"next_text"`
			LastText string        `json:"last_text"`
		} `json:"jobs"`
	}
	raw := toolsCall(t, status, `{}`, &out)
	if out.Now != "2026-09-24T12:30:00+03:00" || out.Location != "MSK" || out.Budget != 0.5 || out.Spent != 0.1234 ||
		out.BudgetText != "потрачено $0.1234 из $0.50 за сутки, осталось $0.3766" || len(out.Jobs) != 3 {
		t.Fatalf("статус: %s", raw)
	}
	if strings.Count(raw, `"jobs"`) != 1 {
		t.Errorf("задания продублированы: %s", raw)
	}
	is := out.Jobs[0]
	if is.Name != "issue" || is.Every != "1h0m0s" || !is.Paid || is.Next != "2026-09-24T13:00:00+03:00" ||
		is.NextText != "24.09.2026 13:00 (через 30 мин)" {
		t.Errorf("выпуск: %+v", is)
	}
	if is.Last == nil || is.Last.Ref != "issue:1" ||
		is.LastText != "24.09.2026 11:00 (1 ч 30 мин назад): ok — Манул (Otocolobus manul): 3 факта; $0.0070" {
		t.Errorf("последний выпуск: %+v / %q", is.Last, is.LastText)
	}
	if !strings.Contains(raw, `"started":"2026-09-24T11:00:00+03:00"`) {
		t.Errorf("время запуска не в поясе демона: %s", raw)
	}
	if sm := out.Jobs[1]; sm.Daily != "09:00" || sm.NextText != "25.09.2026 09:00 (через 20 ч 30 мин)" || sm.Last != nil || sm.LastText != "" {
		t.Errorf("сводка: %+v", sm)
	}

	f.status.Jobs[0].Running = true
	f.status.Budget, f.status.Spent = 0, 1.5
	toolsCall(t, status, ``, &out)
	if out.Jobs[0].NextText != "идёт сейчас" || out.BudgetText != "без лимита; за сутки потрачено $1.5000" {
		t.Errorf("идёт, без лимита: %+v", out)
	}
	f.status.Budget = 0.5
	toolsCall(t, status, ``, &out)
	if !strings.HasPrefix(out.BudgetText, "лимит исчерпан") {
		t.Errorf("лимит исчерпан: %q", out.BudgetText)
	}

	if msg := toolsCallErr(t, status, `{"job":"issue"}`); !strings.Contains(msg, "аргументы не разобрались") {
		t.Errorf("лишний аргумент: %q", msg)
	}
	f.statusErr = errors.New("база закрыта")
	if msg := toolsCallErr(t, status, `{}`); !strings.Contains(msg, "база закрыта") {
		t.Errorf("сбой: %q", msg)
	}
}

func TestToolsRunNow(t *testing.T) {
	f := toolsSetup(t)
	toolsSeed(t, f)
	run := toolsByName(daemon.Tools(f))[daemon.ToolRunNow]
	started := toolsNow.Add(-2 * time.Minute).UTC()
	f.runs[daemon.JobIssue] = schedule.Run{ID: 8, Job: daemon.JobIssue, Trigger: schedule.TriggerManual,
		Scheduled: started, Started: started, Finished: started.Add(95 * time.Second), Status: schedule.RunOK,
		Outcome: schedule.Outcome{CostUSD: 0.007, Ref: "issue:3", Detail: "Манул (Otocolobus manul): 3 факта"}}
	f.runs[daemon.JobSummary] = schedule.Run{ID: 9, Job: daemon.JobSummary, Trigger: schedule.TriggerManual,
		Started: started, Status: schedule.RunBudget}
	f.runs[daemon.JobMDD] = schedule.Run{ID: 10, Job: daemon.JobMDD, Trigger: schedule.TriggerManual,
		Started: started, Finished: started.Add(time.Second), Status: schedule.RunFailed,
		Error: "MDD: 503", Outcome: schedule.Outcome{Detail: "архив не скачался"}}

	type runOut struct {
		Run struct {
			ID       int64   `json:"id"`
			Job      string  `json:"job"`
			Trigger  string  `json:"trigger"`
			Status   string  `json:"status"`
			Ref      string  `json:"ref"`
			Detail   string  `json:"detail"`
			CostUSD  float64 `json:"cost_usd"`
			Error    string  `json:"error"`
			Started  string  `json:"started"`
			Finished string  `json:"finished"`
			Took     string  `json:"took"`
		} `json:"run"`
		Issue *toolsIssue `json:"issue"`
		Hint  string      `json:"hint"`
	}

	var ok runOut
	toolsCall(t, run, `{"job":"issue"}`, &ok)
	if r := ok.Run; r.ID != 8 || r.Status != "ok" || r.Ref != "issue:3" || r.CostUSD != 0.007 || r.Took != "1m35s" ||
		r.Started != "2026-09-24T12:28:00+03:00" || r.Trigger != "manual" || r.Detail == "" {
		t.Errorf("запуск: %+v", r)
	}
	if ok.Issue == nil || ok.Issue.ID != 3 || ok.Issue.SciName != "Otocolobus manul" || len(ok.Issue.Facts) != 3 ||
		ok.Issue.Facts[0].Sources[0].Title == "" || ok.Issue.Dropped != nil || ok.Hint != "" {
		t.Errorf("выпуск: %+v, %q", ok.Issue, ok.Hint)
	}

	var budget runOut
	toolsCall(t, run, `{"job":" Summary "}`, &budget)
	if budget.Run.Status != "budget" || budget.Issue != nil ||
		!strings.Contains(budget.Hint, "потрачено $0.1234 из $0.50") || !strings.Contains(budget.Hint, "не запускалось") {
		t.Errorf("лимит: %+v", budget)
	}

	var failed runOut
	toolsCall(t, run, `{"job":"mdd"}`, &failed)
	if failed.Run.Status != "failed" || failed.Run.Error != "MDD: 503" || !strings.Contains(failed.Hint, "сбоем") {
		t.Errorf("сбой: %+v", failed)
	}

	// Ref на выпуск, которого нет, — запуск всё равно виден.
	f.runs[daemon.JobIssue] = schedule.Run{Job: daemon.JobIssue, Status: schedule.RunOK, Outcome: schedule.Outcome{Ref: "issue:99"}}
	var lost runOut
	toolsCall(t, run, `{"job":"issue"}`, &lost)
	if lost.Issue != nil || !strings.Contains(lost.Hint, "Выпуск 99 не прочитался") {
		t.Errorf("пропавший выпуск: %+v", lost)
	}

	if msg := toolsCallErr(t, run, `{"job":"trivia"}`); !strings.Contains(msg, "задания «trivia» нет; есть: issue, summary, mdd") {
		t.Errorf("неизвестное: %q", msg)
	}
	f.runErr = schedule.ErrBusy
	if msg := toolsCallErr(t, run, `{"job":"issue"}`); !strings.Contains(msg, "уже идёт") || !strings.Contains(msg, daemon.ToolFactsLatest) {
		t.Errorf("занято: %q", msg)
	}
	f.runErr = errors.New("журнал: база закрыта")
	if msg := toolsCallErr(t, run, `{"job":"issue"}`); !strings.Contains(msg, "не запущено: журнал") {
		t.Errorf("сбой журнала: %q", msg)
	}
	f.runErr = nil

	calls := len(f.runCalls)
	for args, want := range map[string]string{
		`{}`:                         "нужен аргумент job",
		``:                           "нужен аргумент job",
		`{"job":""}`:                 "нужен аргумент job",
		`{"job":1}`:                  "аргументы не разобрались",
		`{"job":"issue","now":true}`: "аргументы не разобрались",
	} {
		if msg := toolsCallErr(t, run, args); !strings.Contains(msg, want) {
			t.Errorf("%s: %q, ждали «%s»", args, msg, want)
		}
	}
	if len(f.runCalls) != calls {
		t.Errorf("неверные аргументы дошли до планировщика")
	}
}

// TestToolsLocalFallback — расписание недоступно: инструменты чтения
// работают, время — в поясе процесса.
func TestToolsLocalFallback(t *testing.T) {
	f := toolsSetup(t)
	toolsSeed(t, f)
	f.statusErr = errors.New("база закрыта")
	var out toolsLatestOut
	toolsCall(t, toolsByName(daemon.Tools(f))[daemon.ToolFactsLatest], `{"limit":1}`, &out)
	want := toolsNow.Add(-time.Hour).In(time.Local).Format(time.RFC3339)
	if len(out.Issues) != 1 || out.Issues[0].CreatedAt != want {
		t.Errorf("время: %+v, ждали %s", out.Issues, want)
	}
}

// TestToolsOverMCP — инструменты демона, зарегистрированные в MCP-сервере
// проекта, отвечают через клиентскую сессию SDK так же, как в процессе;
// ошибки приходят результатом с IsError и тем же текстом.
func TestToolsOverMCP(t *testing.T) {
	ctx := context.Background()
	f := toolsSetup(t)
	toolsSeed(t, f)
	toolsSaveSummaries(t, f, 2)
	f.runs[daemon.JobIssue] = schedule.Run{ID: 8, Job: daemon.JobIssue, Trigger: schedule.TriggerManual,
		Started: toolsNow, Finished: toolsNow.Add(time.Minute), Status: schedule.RunOK,
		Outcome: schedule.Outcome{Ref: "issue:3", CostUSD: 0.007}}
	local := daemon.Tools(f)
	srv := mcp.NewServer(local, mcp.ServerOptions{})

	ct, stt := sdk.NewInMemoryTransports()
	ss, err := srv.SDK().Connect(ctx, stt, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cl := sdk.NewClient(&sdk.Implementation{Name: "daemon-test", Version: "0"}, nil)
	cs, err := cl.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	remote := map[string]*sdk.Tool{}
	for _, tl := range list.Tools {
		remote[tl.Name] = tl
	}
	for _, tl := range local {
		s := tl.Spec()
		r, ok := remote[s.Name]
		if !ok {
			t.Fatalf("%s не зарегистрирован в MCP", s.Name)
		}
		if r.Description != s.Description {
			t.Errorf("%s: описание разошлось", s.Name)
		}
		schema, err := json.Marshal(r.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		if string(tools.Canon(schema)) != string(tools.Canon(s.Parameters)) {
			t.Errorf("%s: схема разошлась:\n  mcp %s\nlocal %s", s.Name, schema, s.Parameters)
		}
	}

	byName := toolsByName(local)
	for _, c := range []struct {
		name, args string
		fail       bool
	}{
		{daemon.ToolFactsLatest, `{}`, false},
		{daemon.ToolFactsGet, `{"species":"Манул"}`, false},
		{daemon.ToolFactsGet, `{"id":99}`, true},
		{daemon.ToolFactsSearch, `{"text":"манул","limit":2}`, false},
		{daemon.ToolSummaryGet, `{"list":true}`, false},
		{daemon.ToolSummaryGet, `{}`, false},
		{daemon.ToolSummaryBuild, `{"hours":0}`, true},
		{daemon.ToolScheduleStatus, `{}`, false},
		{daemon.ToolRunNow, `{"job":"issue"}`, false},
		{daemon.ToolRunNow, `{"job":"nope"}`, true},
	} {
		want, lerr := byName[c.name].Call(ctx, json.RawMessage(c.args))
		if (lerr != nil) != c.fail {
			t.Fatalf("%s(%s) в процессе: %v", c.name, c.args, lerr)
		}
		if lerr != nil {
			want = lerr.Error()
		}
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: c.name, Arguments: json.RawMessage(c.args)})
		if err != nil {
			t.Fatalf("%s(%s) через MCP: %v", c.name, c.args, err)
		}
		if res.IsError != c.fail || len(res.Content) != 1 {
			t.Fatalf("%s(%s): IsError=%v, %d блоков", c.name, c.args, res.IsError, len(res.Content))
		}
		if got := res.Content[0].(*sdk.TextContent).Text; got != want {
			t.Errorf("%s(%s) разошлись:\n  mcp %s\nlocal %s", c.name, c.args, got, want)
		}
	}
}
