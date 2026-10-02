package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/daemon"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule/clocktest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// laneDaemon — единственная дорожка И-8: демон, а не диалог.
const laneDaemon = "демон"

// Trivia — И-8: «Интересные факты» по расписанию. Демон собирается над
// своим каталогом с НАСТОЯЩИМИ источниками и моделью, но на подставных
// часах: «час» проходит за миг, поэтому сутки работы укладываются в
// минуты прогона, а проверяется то же, что увидит человек через месяц.
//
// Сценарий:
//  1. Первый старт: справочник MDD (из MDDPath или загрузкой), затем
//     Issues выпусков по расписанию — часы сдвигаются на час после каждого.
//  2. Демон остановлен, часы ушли на 5 часов вперёд, демон поднят: ровно
//     один догоняющий запуск, второго немедленного нет.
//  3. Лимит на сутки опущен ниже уже потраченного: следующий платный слот
//     записан как budget, выпуска нет.
//  4. Сводка по запросу за весь прогон.
//  5. Инструменты демона по MCP (Streamable HTTP, с токеном): facts_latest,
//     facts_get по русскому названию, run_now при исчерпанном лимите.
//
// Жёсткие проверки: все плановые запуски выпуска ok; виды не повторяются;
// у каждого факта есть источник из досье; выпусков ok — не меньше двух
// третей; догон — ровно один; budget без выпуска и без расхода; агрегат
// сводки совпадает с хранилищем и её числа прошли сверку; MCP отдаёт те же
// выпуски, что лежат в базе.
type Trivia struct {
	// Issues — сколько выпусков собрать в первой части; 0 → 3.
	Issues int
	// MDDPath — готовая база со справочником (копируется); пусто — из
	// переменной TRIVIA_MDD_DB, иначе справочник скачивается.
	MDDPath string
}

// NewTrivia — И-8 с настройками по умолчанию.
func NewTrivia() *Trivia { return &Trivia{Issues: 3} }

func (t *Trivia) ID() string    { return "И-8" }
func (t *Trivia) Title() string { return "Интересные факты по расписанию" }

// trivRig — демон испытания и его события.
type trivRig struct {
	env    *Env
	dir    string
	clock  *clocktest.Fake
	budget float64
	events chan schedule.Run

	d      *daemon.Daemon
	cancel context.CancelFunc
	done   chan error
}

func (g *trivRig) start(ctx context.Context) error {
	start := g.clock.Now()
	d, err := daemon.Open(ctx, daemon.Config{
		DataDir: g.dir, Every: time.Hour, Budget: g.budget, Location: time.UTC,
		// Суточные задания — далеко от окна прогона (≈ 9 «часов»), чтобы
		// сводка и проверка релиза не вклинились в счёт выпусков.
		SummaryAt: start.Add(-2 * time.Hour).Format("15:04"),
		MDDAt:     start.Add(-3 * time.Hour).Format("15:04"),
		LLM:       g.env.LLM, Model: g.env.Model, Clock: g.clock,
		OnRun: func(r schedule.Run) { g.events <- r },
	})
	if err != nil {
		return err
	}
	rctx, cancel := context.WithCancel(ctx)
	g.d, g.cancel, g.done = d, cancel, make(chan error, 1)
	go func() { g.done <- d.Run(rctx) }()
	return nil
}

func (g *trivRig) stop() {
	if g.d == nil {
		return
	}
	g.cancel()
	<-g.done
	g.d.Close()
	g.d = nil
}

// wait ждёт запуск задания job; прочие события (mdd при первом старте)
// складываются в other.
func (g *trivRig) wait(ctx context.Context, job string, other *[]schedule.Run) (schedule.Run, error) {
	timeout := time.NewTimer(g.env.timeout())
	defer timeout.Stop()
	for {
		select {
		case r := <-g.events:
			if r.Job == job {
				return r, nil
			}
			*other = append(*other, r)
		case <-timeout.C:
			return schedule.Run{}, fmt.Errorf("И-8: не дождался запуска %s", job)
		case <-ctx.Done():
			return schedule.Run{}, ctx.Err()
		}
	}
}

// idle — планировщик ждёт следующего слота (стоит на подставных часах).
func (g *trivRig) idle(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	for g.clock.Waiters() == 0 {
		if time.Now().After(deadline) {
			return errors.New("И-8: планировщик не встал в ожидание слота")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil
}

func (t *Trivia) Run(ctx context.Context, s *Stand, r *Result) error {
	if s.env.LLM == nil {
		return errors.New("И-8: стенду не передан клиент модели (Env.LLM)")
	}
	n := t.Issues
	if n <= 0 {
		n = 3
	}
	dir := filepath.Join(s.dir, "daemon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mddPath := t.MDDPath
	if mddPath == "" {
		mddPath = os.Getenv("TRIVIA_MDD_DB")
	}
	if mddPath != "" {
		if err := trivCopy(mddPath, filepath.Join(dir, daemon.DBFile)); err != nil {
			return fmt.Errorf("И-8: копия справочника: %w", err)
		}
	}
	r.Lanes = append(r.Lanes, LaneInfo{Name: laneDaemon, Note: "animals-mcp -daemon над своим каталогом, настоящие источники и модель, подставные часы",
		Diff: "не диалог: выпуски по расписанию"})

	// Часы — сегодня в 10:00 UTC, а не настоящее «сейчас». Сценарий
	// проходит на подставных часах около девяти часов: начатый вечером, он
	// переходил полночь, суточный лимит обнулялся, и проверки лимита
	// проваливались в зависимости от времени запуска. С 10:00 до 19:00 нет ни
	// полуночи, ни плановых заданий (mdd в 04:00, сводка в 09:00). Цена
	// считается по тарифу этого подставного момента.
	g := &trivRig{env: s.env, dir: dir, clock: clocktest.NewFake(trivStart(time.Now())),
		budget: 1, events: make(chan schedule.Run, 64)}
	defer g.stop()
	began := g.clock.Now()

	// 1. Первый старт и выпуски по расписанию.
	var other []schedule.Run
	if err := g.start(ctx); err != nil {
		return err
	}
	var scheduled []schedule.Run
	var wall time.Duration // настоящее время сборки: подставные часы в запуске стоят
	for i := 0; i < n; i++ {
		if i > 0 {
			if err := g.idle(ctx); err != nil {
				return err
			}
			g.clock.Advance(time.Hour)
		}
		t0 := time.Now()
		run, err := g.wait(ctx, daemon.JobIssue, &other)
		if err != nil {
			return err
		}
		wall += time.Since(t0)
		scheduled = append(scheduled, run)
		s.env.logf("  И-8: выпуск %d/%d — %s: %s ($%.4f)", i+1, n, run.Status, run.Detail, run.CostUSD)
	}
	okRuns := 0
	for _, run := range scheduled {
		if run.Status == schedule.RunOK {
			okRuns++
		}
	}
	r.atLeast("плановые запуски выпуска завершились ok", laneDaemon, okRuns, n, n)
	r.metric("время выпуска, среднее (настоящее)", laneDaemon, "%.1f с", wall.Seconds()/float64(n))
	for _, o := range other {
		r.note("первый старт: %s %s — %s", o.Job, o.Status, o.Detail)
	}

	// 2. Простой 5 часов и догон.
	if err := g.idle(ctx); err != nil {
		return err
	}
	g.stop()
	g.clock.Advance(5 * time.Hour)
	if err := g.start(ctx); err != nil {
		return err
	}
	catch, err := g.wait(ctx, daemon.JobIssue, &other)
	if err != nil {
		return err
	}
	if err := g.idle(ctx); err != nil {
		return err
	}
	extra := len(g.events)
	r.yes("после простоя 5 ч — ровно один догоняющий выпуск", laneDaemon,
		catch.Trigger == schedule.TriggerCatchUp && extra == 0,
		fmt.Sprintf("первый запуск: %s, %s; лишних событий: %d", catch.Trigger, catch.Status, extra))

	issues, _, err := g.d.Trivia.Issues(ctx, trivia.IssueQuery{Limit: 100})
	if err != nil {
		return err
	}
	// 3. Сводка по запросу за весь прогон — до шага с лимитом: при
	// исчерпанном лимите демон её честно не соберёт.
	sum, err := g.d.BuildSummary(ctx, began, g.clock.Now().Add(time.Minute))
	if err != nil && sum.ID == 0 {
		return fmt.Errorf("И-8: сводка: %w", err)
	}
	a := sum.Aggregate
	r.yes("агрегат сводки совпадает с хранилищем", laneDaemon,
		a.Issues == len(issues),
		fmt.Sprintf("выпусков в агрегате %d, в базе %d", a.Issues, len(issues)))
	r.yes("текст сводки есть и прошёл сверку чисел с агрегатом", laneDaemon, sum.Text != "" && sum.Error == "", sum.Error)
	r.Samples = append(r.Samples, Sample{Topic: "сводка", Lane: laneDaemon, User: "summary_build за прогон", Reply: sum.Text})
	r.metric("сводка: цена", laneDaemon, "$%.4f", sum.Cost.USD)

	// 4. Лимит: опущен ниже потраченного за сутки.
	st, err := g.d.Status(ctx)
	if err != nil {
		return err
	}
	spent := st.Spent
	issuesBefore := trivCountIssues(ctx, g.d)
	g.stop()
	g.budget = spent / 2
	if err := g.start(ctx); err != nil {
		return err
	}
	if err := g.idle(ctx); err != nil {
		return err
	}
	g.clock.Advance(time.Hour)
	capped, err := g.wait(ctx, daemon.JobIssue, &other)
	if err != nil {
		return err
	}
	r.yes("лимит исчерпан — слот записан как budget, выпуска и расхода нет", laneDaemon,
		capped.Status == schedule.RunBudget && capped.CostUSD == 0 && trivCountIssues(ctx, g.d) == issuesBefore,
		fmt.Sprintf("%s: %s", capped.Status, capped.Detail))

	// Качество выпусков — по базе.
	t.judgeIssues(r, issues)

	// 5. MCP по HTTP.
	if err := t.checkMCP(ctx, r, g.d, issues); err != nil {
		r.note("MCP: %v", err)
		r.yes("инструменты демона по MCP отдают выпуски из базы", laneDaemon, false, err.Error())
	}
	return nil
}

// judgeIssues — проверки и числа по сохранённым выпускам.
func (t *Trivia) judgeIssues(r *Result, issues []trivia.Issue) {
	species := map[int]int{}
	var dup, orphan []string
	ok, facts, dropped := 0, 0, 0
	var cost float64
	for _, is := range issues {
		species[is.SpeciesID]++
		if species[is.SpeciesID] == 2 {
			dup = append(dup, is.SciName)
		}
		if is.Status == trivia.IssueOK {
			ok++
		}
		ids := map[string]bool{}
		for _, m := range is.Sources {
			ids[m.ID] = true
		}
		for _, f := range is.Facts {
			good := len(f.Sources) > 0
			for _, sid := range f.Sources {
				good = good && ids[sid]
			}
			if !good {
				orphan = append(orphan, is.SciName+": "+trivShort(f.Text))
			}
		}
		facts += len(is.Facts)
		dropped += len(is.Dropped)
		cost += is.Cost.USD
		if len(is.Facts) > 0 {
			r.Samples = append(r.Samples, Sample{Topic: "выпуск", Lane: laneDaemon, User: is.SciName,
				Reply: is.Title + "\n\n" + is.Facts[0].Text})
		}
	}
	r.zero("повторы вида среди выпусков", laneDaemon, len(dup), dup)
	r.zero("факты без источника из досье", laneDaemon, len(orphan), orphan)
	r.atLeast("выпусков со статусом ok", laneDaemon, ok, len(issues), (2*len(issues)+2)/3)
	if len(issues) > 0 {
		r.metric("выпусков", laneDaemon, "%d", len(issues))
		r.metric("цена выпуска, среднее", laneDaemon, "$%.4f", cost/float64(len(issues)))
		r.metric("фактов на выпуск", laneDaemon, "%.1f", float64(facts)/float64(len(issues)))
	}
	if facts+dropped > 0 {
		r.metric("доля отброшенных фактов", laneDaemon, "%.0f %%", 100*float64(dropped)/float64(facts+dropped))
	}
}

// checkMCP поднимает HTTP-сервер демона в процессе и ходит в него
// клиентом SDK с токеном — тем же путём, что mcp-list и приложение.
func (t *Trivia) checkMCP(ctx context.Context, r *Result, d *daemon.Daemon, issues []trivia.Issue) error {
	const token = "bench-token"
	ts := append(mdd.Tools(d.MDD), daemon.Tools(d)...)
	srv := mcp.NewServer(ts, mcp.ServerOptions{})
	hs := httptest.NewServer(srv.HTTPHandler(mcp.HTTPOptions{Token: token}))
	defer hs.Close()
	transport, _, err := mcp.HTTPDialer(hs.URL, token, nil)(ctx)
	if err != nil {
		return err
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "bench", Version: "1"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		return err
	}
	defer cs.Close()
	call := func(name string, args map[string]any) (string, bool, error) {
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return "", false, err
		}
		var sb strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String(), res.IsError, nil
	}

	text, isErr, err := call(daemon.ToolFactsLatest, map[string]any{"limit": 20, "status": []string{"ok", "thin", "failed"}})
	if err != nil || isErr {
		return fmt.Errorf("facts_latest: %v %s", err, text)
	}
	var latest struct {
		Issues []struct {
			ID     int64  `json:"id"`
			NameRu string `json:"name_ru"`
		} `json:"issues"`
	}
	if err := json.Unmarshal([]byte(text), &latest); err != nil {
		return fmt.Errorf("facts_latest: %w", err)
	}
	want, got := trivIDs(issues), []int64{}
	for _, is := range latest.Issues {
		got = append(got, is.ID)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	r.yes("facts_latest по MCP отдаёт те же выпуски, что в базе", laneDaemon, fmt.Sprint(got) == fmt.Sprint(want),
		fmt.Sprintf("MCP %v, база %v", got, want))

	// facts_get по русскому названию — первый выпуск, у которого оно есть.
	for _, is := range issues {
		if is.NameRu == "" || is.Status == trivia.IssueFailed {
			continue
		}
		text, isErr, err := call(daemon.ToolFactsGet, map[string]any{"species": is.NameRu})
		var one struct {
			ID int64 `json:"id"`
		}
		if err == nil && !isErr {
			err = json.Unmarshal([]byte(text), &one)
		}
		r.yes("facts_get по русскому названию находит выпуск", laneDaemon, err == nil && !isErr && one.ID == is.ID,
			fmt.Sprintf("«%s»: ждали %d, получили %d %s", is.NameRu, is.ID, one.ID, trivShort(text)))
		break
	}

	// run_now при исчерпанном лимите — ответ со статусом budget, не ошибка.
	text, isErr, err = call(daemon.ToolRunNow, map[string]any{"job": daemon.JobIssue})
	r.yes("run_now при исчерпанном лимите — budget, без выпуска", laneDaemon,
		err == nil && !isErr && strings.Contains(text, `"status":"budget"`), trivShort(text))
	return nil
}

func trivCountIssues(ctx context.Context, d *daemon.Daemon) int {
	_, total, err := d.Trivia.Issues(ctx, trivia.IssueQuery{Limit: 1})
	if err != nil {
		return -1
	}
	return total
}

func trivIDs(issues []trivia.Issue) []int64 {
	out := make([]int64, 0, len(issues))
	for _, is := range issues {
		out = append(out, is.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func trivShort(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return string(r)
}

// trivCopy копирует базу вместе с журналом WAL, если он есть.
func trivCopy(src, dst string) error {
	for _, suffix := range []string{"", "-wal"} {
		in, err := os.Open(src + suffix)
		if err != nil {
			if suffix != "" && os.IsNotExist(err) {
				continue
			}
			return err
		}
		out, err := os.Create(dst + suffix)
		if err != nil {
			in.Close()
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// trivStart — начало подставных часов И-8: сегодня (UTC) в 10:00.
func trivStart(now time.Time) time.Time {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d, 10, 0, 0, 0, time.UTC)
}
