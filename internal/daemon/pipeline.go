package daemon

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
)

// ExportsDir — каталог выгрузок save_to_file внутри каталога данных демона.
const ExportsDir = "exports"

// PipelineDeps — зависимости инструментов конвейера (search, summarize,
// save_to_file) поверх демона: тот же справочник и кэш проверок, что у
// выпуска, выходы шагов — в той же базе, файлы — в <DataDir>/exports.
//
// Расход summarize идёт мимо планировщика, как у сводки по запросу
// (BuildSummary): до модели проверяется дневной лимит, после — запись в
// журнал запусков под Job = pipeline.JobPipeline с Trigger manual. Из
// журнала расход попадает в Spent (лимит) и в агрегат сводки: там
// «pipeline/ok» — отдельная строка разбивки запусков, а упавший
// summarize — строка сбоев «pipeline: …». Планировщик такого задания не
// знает, поэтому schedule_status и слоты заданий записи конвейера не
// трогают.
//
// Источники — боевые (WebChecker, WebCollector, русская Википедия);
// тесты подменяют Checker, Collector и Wiki в возвращённой структуре.
func (d *Daemon) PipelineDeps() pipeline.Deps {
	return pipeline.Deps{
		Species:   d.MDD,
		Artifacts: d.Artifacts,
		Picks:     d.Trivia,
		LLM:       d.cfg.LLM,
		Model:     d.cfg.Model,
		ExportDir: filepath.Join(d.cfg.DataDir, ExportsDir),
		Budget:    d.pipelineBudget,
		Record:    d.pipelineRecord,
		Now:       d.cfg.Clock.Now,
	}
}

// pipelineBudget — лимит так же, как у BuildSummary: планировщик
// недоступен — не мешаем (его сбой виден в schedule_status), исчерпан —
// ErrBudget с цифрами: модели нужно сказать человеку, сколько потрачено и
// когда станет можно.
func (d *Daemon) pipelineBudget(ctx context.Context) error {
	st, err := d.Sched.Status(ctx)
	if err != nil || st.Budget <= 0 || st.Spent < st.Budget {
		return nil
	}
	return fmt.Errorf("%w: потрачено $%.4f из $%.2f за сутки; лимит обнулится в полночь (%s)",
		ErrBudget, st.Spent, st.Budget, st.Location)
}

// pipelineRecord — платный шаг конвейера в журнал запусков.
func (d *Daemon) pipelineRecord(ctx context.Context, r pipeline.RunRecord) {
	run := schedule.Run{Job: pipeline.JobPipeline, Trigger: schedule.TriggerManual,
		Scheduled: r.Started, Started: r.Started, Finished: r.Finished, Status: schedule.RunOK,
		Outcome: schedule.Outcome{CostUSD: r.CostUSD, Detail: r.Detail}}
	if r.Error != "" {
		run.Status, run.Error = schedule.RunFailed, r.Error
	}
	if _, err := d.Runs.Record(ctx, run); err != nil {
		d.log.Warn("журнал конвейера", "err", err, "cost_usd", r.CostUSD)
	}
}
