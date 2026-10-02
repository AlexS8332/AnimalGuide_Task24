package daemon

import (
	"context"
	"strconv"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// Контракт этапа 6: инструменты MCP демона (tools.go) видят демон только
// через Service. Так их можно тестировать на Memory-хранилищах и фейковом
// планировщике, а настоящий Daemon реализует Service ниже.
//
// Инструменты (имена — константы в tools.go):
//   - facts_latest   — последние выпуски;
//   - facts_get      — выпуск по id или последний выпуск о виде (mdd-id,
//     латинское, английское или русское название);
//   - facts_search   — поиск по выпускам (текст, статус, период);
//   - summary_get    — сводка по id, последняя или список последних;
//   - summary_build  — собрать сводку за последние N часов (Write: платно);
//   - schedule_status — задания, следующий запуск, лимит и расход за сутки;
//   - run_now        — запустить задание сейчас (Write: платно).
type Service interface {
	Issues() trivia.IssueStore
	Summaries() trivia.SummaryStore
	Species() mdd.Store
	Status(ctx context.Context) (schedule.Status, error)
	RunNow(ctx context.Context, job string) (schedule.Run, error)
	// BuildSummary собирает и сохраняет сводку за [from, to) с Trigger
	// «manual», в обход планировщика, но с учётом дневного лимита: при
	// исчерпанном лимите — ошибка ErrBudget.
	BuildSummary(ctx context.Context, from, to time.Time) (trivia.Summary, error)
}

func (d *Daemon) Issues() trivia.IssueStore      { return d.Trivia }
func (d *Daemon) Summaries() trivia.SummaryStore { return d.Trivia }
func (d *Daemon) Species() mdd.Store             { return d.MDD }

func (d *Daemon) Status(ctx context.Context) (schedule.Status, error) { return d.Sched.Status(ctx) }

func (d *Daemon) RunNow(ctx context.Context, job string) (schedule.Run, error) {
	return d.Sched.RunNow(ctx, job)
}

// ErrBudget — дневной лимит исчерпан.
var ErrBudget = errorString("дневной лимит расходов на модель исчерпан")

type errorString string

func (e errorString) Error() string { return string(e) }

func (d *Daemon) BuildSummary(ctx context.Context, from, to time.Time) (trivia.Summary, error) {
	if st, err := d.Sched.Status(ctx); err == nil && st.Budget > 0 && st.Spent >= st.Budget {
		return trivia.Summary{}, ErrBudget
	}
	// Сводка по запросу идёт мимо планировщика, но её расход обязан попасть
	// в журнал: иначе дневной лимит его не увидит.
	started := d.cfg.Clock.Now()
	s, err := BuildSummary(ctx, d.Summary, from, to, schedule.TriggerManual)
	if s.ID != 0 || s.Cost.USD > 0 {
		r := schedule.Run{Job: JobSummary, Trigger: schedule.TriggerManual, Scheduled: started, Started: started,
			Finished: d.cfg.Clock.Now(), Status: schedule.RunOK,
			Outcome: schedule.Outcome{CostUSD: s.Cost.USD, Detail: summaryDetail(s)}}
		if s.ID != 0 {
			r.Ref = RefSummary + strconv.FormatInt(s.ID, 10)
		}
		if err != nil {
			r.Status, r.Error = schedule.RunFailed, err.Error()
		}
		if _, rerr := d.Runs.Record(context.WithoutCancel(ctx), r); rerr != nil {
			d.log.Warn("журнал сводки по запросу", "err", rerr)
		}
	}
	return s, err
}

var _ Service = (*Daemon)(nil)
