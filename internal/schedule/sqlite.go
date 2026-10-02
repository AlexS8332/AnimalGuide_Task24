package schedule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
)

// SQLiteComponent — имя компонента в schema_migrations.
const SQLiteComponent = "schedule"

// sqliteSteps — миграции схемы schedule. Только дописывать в конец:
// применённый шаг повторно не выполняется.
//
// Время — INTEGER, наносекунды Unix (UTC по определению), как в trivia:
// сравнение диапазонов (Spent, Since/Until) — сравнение чисел, индекс
// работает, и точность полная — время, сохранённое и прочитанное, равно
// исходному по Equal. Иначе граница суток в Spent («ровно в полночь»)
// зависела бы от усечения. Цена — диапазон 1678–2262 годов.
//
// finished — NULL, пока запуск идёт: «не закончен» отличим от «закончен в
// нулевой момент». Столбец причины называется triggered_by: TRIGGER в
// SQLite — ключевое слово.
//
// Индексы: (job, started, id) — Last и журнал одного задания, новые первыми;
// (started, id) — Spent за сутки и общий журнал. id в хвосте — второй ключ
// порядка «Started, затем ID», сортировка без временного B-дерева.
//
// AUTOINCREMENT: ID запуска не переиспользуется — на него ссылаются строки
// журнала процесса и ленты интерфейса.
var sqliteSteps = []string{
	`CREATE TABLE schedule_run (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		job          TEXT NOT NULL,
		triggered_by TEXT NOT NULL,
		scheduled    INTEGER NOT NULL, -- наносекунды Unix
		started      INTEGER NOT NULL, -- наносекунды Unix
		finished     INTEGER,          -- наносекунды Unix; NULL — идёт
		status       TEXT NOT NULL,
		error        TEXT NOT NULL,
		cost_usd     REAL NOT NULL,
		ref          TEXT NOT NULL,
		detail       TEXT NOT NULL
	);
	CREATE INDEX schedule_run_job_started ON schedule_run (job, started, id);
	CREATE INDEX schedule_run_started ON schedule_run (started, id);`,
}

// SQLite — RunStore поверх общей базы приложения. Базу открывает и
// закрывает владелец (db.Open): в ней живут и чужие таблицы.
type SQLite struct {
	db *sql.DB
}

var _ RunStore = (*SQLite)(nil)

// NewSQLite доводит схему schedule до текущей версии и возвращает журнал.
// Повторный вызов на той же базе ничего не меняет.
func NewSQLite(ctx context.Context, conn *sql.DB) (*SQLite, error) {
	if conn == nil {
		return nil, errors.New("schedule: база не открыта")
	}
	if err := db.Migrate(ctx, conn, SQLiteComponent, sqliteSteps); err != nil {
		return nil, fmt.Errorf("schedule: схема: %w", err)
	}
	return &SQLite{db: conn}, nil
}

const sqliteRunColumns = `id, job, triggered_by, scheduled, started, finished,
	status, error, cost_usd, ref, detail`

func sqliteTime(ns int64) time.Time { return time.Unix(0, ns).UTC() }

type sqliteScanner interface {
	Scan(dest ...any) error
}

// sqliteScanRun читает запуск из строки с sqliteRunColumns. sql.ErrNoRows
// возвращается как есть: «нет» решает вызывающий.
func sqliteScanRun(row sqliteScanner) (Run, error) {
	var (
		r                  Run
		scheduled, started int64
		finished           sql.NullInt64
	)
	err := row.Scan(&r.ID, &r.Job, &r.Trigger, &scheduled, &started, &finished,
		&r.Status, &r.Error, &r.CostUSD, &r.Ref, &r.Detail)
	if err != nil {
		return Run{}, err
	}
	r.Scheduled = sqliteTime(scheduled)
	r.Started = sqliteTime(started)
	if finished.Valid {
		r.Finished = sqliteTime(finished.Int64)
	}
	return r, nil
}

// sqliteFinished — время окончания для столбца: нулевое → NULL.
func sqliteFinished(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}

func (s *SQLite) insert(ctx context.Context, r Run) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO schedule_run
		(job, triggered_by, scheduled, started, finished, status, error, cost_usd, ref, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Job, r.Trigger, r.Scheduled.UnixNano(), r.Started.UnixNano(), sqliteFinished(r.Finished),
		r.Status, r.Error, r.CostUSD, r.Ref, r.Detail)
	if err != nil {
		return 0, fmt.Errorf("schedule: запуск %q не записан: %w", r.Job, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("schedule: запуск %q: id записи: %w", r.Job, err)
	}
	return id, nil
}

func (s *SQLite) Begin(ctx context.Context, r Run) (int64, error) {
	if err := storeValidateBegin(r); err != nil {
		return 0, err
	}
	r.Status = RunRunning
	r.Finished = time.Time{}
	r.Error = ""
	r.Outcome = Outcome{}
	return s.insert(ctx, r)
}

func (s *SQLite) Record(ctx context.Context, r Run) (int64, error) {
	if err := storeValidateRecord(r); err != nil {
		return 0, err
	}
	return s.insert(ctx, r)
}

func (s *SQLite) Finish(ctx context.Context, r Run) error {
	if err := storeValidateFinish(r); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE schedule_run
		SET finished = ?, status = ?, error = ?, cost_usd = ?, ref = ?, detail = ?
		WHERE id = ? AND status = ?`,
		r.Finished.UnixNano(), r.Status, r.Error, r.CostUSD, r.Ref, r.Detail, r.ID, RunRunning)
	if err != nil {
		return fmt.Errorf("schedule: итог запуска %d не записан: %w", r.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("schedule: итог запуска %d: %w", r.ID, err)
	}
	if n == 1 {
		return nil
	}
	// Ничего не обновилось — выясняем почему, чтобы ошибка говорила по делу.
	var status string
	err = s.db.QueryRowContext(ctx, `SELECT status FROM schedule_run WHERE id = ?`, r.ID).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("schedule: итог запуска %d: нет такого запуска", r.ID)
	case err != nil:
		return fmt.Errorf("schedule: итог запуска %d: %w", r.ID, err)
	}
	return fmt.Errorf("schedule: итог запуска %d: запуск уже завершён (%s)", r.ID, status)
}

func (s *SQLite) Runs(ctx context.Context, q RunQuery) ([]Run, error) {
	var (
		where []string
		args  []any
	)
	if q.Job != "" {
		where = append(where, "job = ?")
		args = append(args, q.Job)
	}
	if len(q.Status) > 0 {
		where = append(where, "status IN (?"+strings.Repeat(", ?", len(q.Status)-1)+")")
		for _, st := range q.Status {
			args = append(args, st)
		}
	}
	if !q.Since.IsZero() {
		where = append(where, "started >= ?")
		args = append(args, storeNanos(q.Since))
	}
	if !q.Until.IsZero() {
		where = append(where, "started < ?")
		args = append(args, storeNanos(q.Until))
	}
	query := `SELECT ` + sqliteRunColumns + ` FROM schedule_run`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY started DESC, id DESC LIMIT ?`
	args = append(args, storeLimit(q.Limit))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("schedule: журнал запусков: %w", err)
	}
	defer rows.Close()
	list := []Run{}
	for rows.Next() {
		r, err := sqliteScanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("schedule: журнал запусков: %w", err)
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schedule: журнал запусков: %w", err)
	}
	return list, nil
}

func (s *SQLite) Last(ctx context.Context, job string) (Run, bool, error) {
	r, err := sqliteScanRun(s.db.QueryRowContext(ctx, `SELECT `+sqliteRunColumns+`
		FROM schedule_run WHERE job = ? AND status NOT IN (?, ?)
		ORDER BY started DESC, id DESC LIMIT 1`, job, RunBudget, RunSkipped))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Run{}, false, nil
	case err != nil:
		return Run{}, false, fmt.Errorf("schedule: последний запуск %q: %w", job, err)
	}
	return r, true, nil
}

func (s *SQLite) Spent(ctx context.Context, since, until time.Time) (float64, error) {
	query := `SELECT COALESCE(SUM(cost_usd), 0) FROM schedule_run WHERE started >= ?`
	args := []any{storeNanos(since)}
	if !until.IsZero() {
		query += ` AND started < ?`
		args = append(args, storeNanos(until))
	}
	var sum float64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&sum); err != nil {
		return 0, fmt.Errorf("schedule: расход: %w", err)
	}
	return sum, nil
}

func (s *SQLite) Abandon(ctx context.Context, at time.Time) (int, error) {
	if err := storeCheckTime("время остановки", at); err != nil {
		return 0, fmt.Errorf("schedule: незавершённые запуски: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE schedule_run
		SET status = ?, error = ?, finished = ? WHERE status = ?`,
		RunFailed, abandonError, at.UnixNano(), RunRunning)
	if err != nil {
		return 0, fmt.Errorf("schedule: незавершённые запуски: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("schedule: незавершённые запуски: %w", err)
	}
	return int(n), nil
}
