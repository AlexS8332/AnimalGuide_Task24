package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// DBFile — имя базы демона в каталоге данных: справочник MDD, выборы,
// выпуски, сводки и журнал запусков живут в одной SQLite.
const DBFile = "trivia.db"

// Значения по умолчанию. Интервал выпуска — час (постановка задания);
// сводка утром, проверка релиза MDD ночью, когда никто не ждёт ответа.
const (
	DefaultEvery     = time.Hour
	DefaultSummaryAt = "09:00"
	DefaultMDDAt     = "04:00"
	DefaultBudget    = 0.50
)

// Config — настройки демона. Нулевые поля — значения по умолчанию.
type Config struct {
	DataDir   string
	Every     time.Duration
	SummaryAt string
	MDDAt     string
	// Budget — лимит расходов на модель в сутки, долларов; < 0 — без лимита.
	Budget   float64
	Location *time.Location
	LLM      llm.Chatter
	Model    string
	MDDURL   string
	Clock    schedule.Clock
	Log      *slog.Logger
	// OnRun — уведомление о каждом запуске (интерфейс, журнал процесса).
	OnRun func(schedule.Run)
}

// Daemon — собранный демон: база, хранилища, задания, планировщик.
type Daemon struct {
	Conn    *sql.DB
	MDD     *mdd.SQLite
	Trivia  *trivia.SQLite
	Runs    *schedule.SQLite
	Sched   *schedule.Scheduler
	Summary SummaryDeps
	// Artifacts — выходы шагов конвейера search → summarize → save_to_file
	// для передачи по ref (компонент миграций "pipeline" в той же базе).
	Artifacts *pipeline.SQLite

	cfg Config
	log *slog.Logger
}

// Open открывает базу и собирает демон. Планировщик не запускается — это
// делает Run.
func Open(ctx context.Context, c Config) (*Daemon, error) {
	if c.LLM == nil {
		return nil, errors.New("демону нужен клиент модели")
	}
	if c.Every == 0 {
		c.Every = DefaultEvery
	}
	if c.SummaryAt == "" {
		c.SummaryAt = DefaultSummaryAt
	}
	if c.MDDAt == "" {
		c.MDDAt = DefaultMDDAt
	}
	if c.Budget == 0 {
		c.Budget = DefaultBudget
	}
	if c.Budget < 0 {
		c.Budget = 0 // у планировщика 0 — без лимита
	}
	if c.Location == nil {
		c.Location = time.Local
	}
	if c.Clock == nil {
		c.Clock = schedule.System
	}
	log := c.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	conn, err := db.Open(ctx, filepath.Join(c.DataDir, DBFile))
	if err != nil {
		return nil, err
	}
	d := &Daemon{Conn: conn, cfg: c, log: log}
	fail := func(err error) (*Daemon, error) { conn.Close(); return nil, err }
	if d.MDD, err = mdd.NewSQLite(ctx, conn); err != nil {
		return fail(err)
	}
	if d.Trivia, err = trivia.NewSQLite(ctx, conn); err != nil {
		return fail(err)
	}
	if d.Artifacts, err = pipeline.NewSQLite(ctx, conn); err != nil {
		return fail(err)
	}
	d.Artifacts.Now = c.Clock.Now
	if d.Runs, err = schedule.NewSQLite(ctx, conn); err != nil {
		return fail(err)
	}

	agg := &trivia.Aggregator{Issues: d.Trivia, Picks: d.Trivia, Runs: RunSourceOf(d.Runs), Location: c.Location}
	d.Summary = SummaryDeps{
		Aggregate:  agg.Aggregate,
		Summarizer: &trivia.LLMSummarizer{LLM: c.LLM, Model: c.Model},
		Store:      d.Trivia,
		Now:        c.Clock.Now,
	}
	jobs := []schedule.Job{
		IssueJob(IssueDeps{Species: d.MDD, Picks: d.Trivia, Issues: d.Trivia, LLM: c.LLM, Model: c.Model, Log: log, Now: c.Clock.Now}, c.Every),
		MDDJob(d.MDD, mdd.SyncOptions{URL: c.MDDURL}, c.MDDAt),
		SummaryJob(d.Summary, c.SummaryAt),
	}
	d.Sched, err = schedule.New(d.Runs, jobs, schedule.Options{
		Clock: c.Clock, Location: c.Location, Budget: c.Budget, OnRun: c.OnRun,
	})
	if err != nil {
		return fail(err)
	}
	return d, nil
}

// Run работает до отмены ctx. Если справочник MDD пуст (первый запуск),
// он загружается сразу, а не в ближайшие 04:00: без справочника задание
// выпуска только пропускало бы свои слоты.
func (d *Daemon) Run(ctx context.Context) error {
	if _, err := d.MDD.Release(ctx); errors.Is(err, mdd.ErrNotFound) {
		d.log.Info("справочник MDD пуст — загружаю до старта планировщика")
		if _, err := d.Sched.RunNow(ctx, JobMDD); err != nil {
			return fmt.Errorf("первая загрузка MDD: %w", err)
		}
	}
	return d.Sched.Run(ctx)
}

// Close закрывает базу.
func (d *Daemon) Close() error { return d.Conn.Close() }
