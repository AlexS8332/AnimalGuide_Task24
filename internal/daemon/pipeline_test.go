package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/daemon"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// pipelineDaemon — демон на временном каталоге с загруженным справочником;
// источники конвейера — фейки без сети.
func pipelineDaemon(t *testing.T, budget float64) (*daemon.Daemon, map[string]tools.Tool, string) {
	t.Helper()
	dir := t.TempDir()
	d, err := daemon.Open(t.Context(), daemon.Config{DataDir: dir, LLM: jobsFakeLLM(false, nil), Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.MDD.Replace(t.Context(), mddtest.Sample()); err != nil {
		t.Fatal(err)
	}
	src := &jobsFakeSources{eligible: true}
	deps := d.PipelineDeps()
	deps.Checker = func(*tools.Fetcher) trivia.Checker { return jobsFakeChecker{src} }
	deps.Collector = func(*tools.Fetcher) trivia.Collector { return jobsFakeCollector{} }
	ts := map[string]tools.Tool{}
	for _, tl := range pipeline.Tools(deps) {
		ts[tl.Spec().Name] = tl
	}
	return d, ts, dir
}

func pipelineCall(t *testing.T, ts map[string]tools.Tool, name, args string) (pipeline.Envelope, error) {
	t.Helper()
	out, err := ts[name].Call(context.Background(), json.RawMessage(args))
	if err != nil {
		return pipeline.Envelope{}, err
	}
	var env pipeline.Envelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return env, nil
}

// Конвейер над настоящим демоном: выходы шагов — в его SQLite (ref),
// файл — в <data>/exports, расход summarize — в журнале запусков под
// Job=pipeline, откуда его видят дневной лимит и сводка, но не планировщик.
func TestPipelineDeps(t *testing.T) {
	ctx := context.Background()
	d, ts, dir := pipelineDaemon(t, 0)

	dossier, err := pipelineCall(t, ts, pipeline.ToolSearch, `{"query":"Pallas's Cat"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Artifacts.Get(ctx, dossier.Digest); err != nil {
		t.Fatalf("досье не в базе демона: %v", err)
	}
	facts, err := pipelineCall(t, ts, pipeline.ToolSummarize, `{"ref":"`+dossier.Digest+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	file, err := pipelineCall(t, ts, pipeline.ToolSaveFile, `{"ref":"`+facts.Digest+`","name":"manul"}`)
	if err != nil {
		t.Fatal(err)
	}
	var fd pipeline.File
	json.Unmarshal(file.Data, &fd)
	if fd.Path != daemon.ExportsDir+"/manul.md" {
		t.Errorf("путь файла %s", fd.Path)
	}
	if _, err := os.Stat(filepath.Join(dir, daemon.ExportsDir, "manul.md")); err != nil {
		t.Errorf("файла в каталоге данных нет: %v", err)
	}

	runs, err := d.Runs.Runs(ctx, schedule.RunQuery{Job: pipeline.JobPipeline})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("записей конвейера в журнале %d", len(runs))
	}
	r := runs[0]
	if r.Status != schedule.RunOK || r.Trigger != schedule.TriggerManual || r.CostUSD != facts.CostUSD ||
		r.CostUSD <= 0 || r.Detail != facts.Summary {
		t.Errorf("запись журнала: %+v", r)
	}

	// Лимит видит расход; у планировщика задания pipeline нет.
	st, err := d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(st.Spent-facts.CostUSD) > 1e-12 {
		t.Errorf("Spent %v, расход summarize %v", st.Spent, facts.CostUSD)
	}
	for _, js := range st.Jobs {
		if js.Name == pipeline.JobPipeline {
			t.Error("в schedule_status появилось задание pipeline")
		}
		if js.Last != nil {
			t.Errorf("у задания %s последним запуском стала запись конвейера: %+v", js.Name, js.Last)
		}
	}

	// Сводка: отдельная строка разбивки и расход в общей сумме.
	agg, err := d.Summary.Aggregate(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range agg.Runs {
		if c.Key == pipeline.JobPipeline+"/"+schedule.RunOK && c.Count == 1 {
			found = true
		}
	}
	if !found || math.Abs(agg.CostUSD-facts.CostUSD) > 1e-12 || len(agg.Failures) != 0 {
		t.Errorf("агрегат: runs=%+v cost=%v failures=%v", agg.Runs, agg.CostUSD, agg.Failures)
	}
}

func TestPipelineBudget(t *testing.T) {
	d, ts, _ := pipelineDaemon(t, 1e-9)
	dossier, err := pipelineCall(t, ts, pipeline.ToolSearch, `{"query":"Otocolobus manul"}`)
	if err != nil {
		t.Fatal(err)
	}
	// Первый summarize проходит (потрачено 0) и исчерпывает лимит.
	if _, err := pipelineCall(t, ts, pipeline.ToolSummarize, `{"ref":"`+dossier.Digest+`"}`); err != nil {
		t.Fatal(err)
	}
	_, err = pipelineCall(t, ts, pipeline.ToolSummarize, `{"ref":"`+dossier.Digest+`"}`)
	if !errors.Is(err, daemon.ErrBudget) || !strings.Contains(err.Error(), "потрачено $") {
		t.Errorf("исчерпанный лимит: %v", err)
	}
	runs, _ := d.Runs.Runs(context.Background(), schedule.RunQuery{Job: pipeline.JobPipeline})
	if len(runs) != 1 {
		t.Errorf("отказ по лимиту записан в журнал: %d записей", len(runs))
	}
}
