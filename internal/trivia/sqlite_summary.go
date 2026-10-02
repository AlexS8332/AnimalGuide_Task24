package trivia

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// sqliteSummaryStep — шаг 3 схемы trivia: сводки.
//
// Столбцами — время (период и создание: по created_at сортируется список и
// ищется последняя сводка), кто запустил, текст, ошибка и стоимость одним
// числом. Агрегат, расход и стоимость целиком — JSON: это снимок, он
// читается целиком. Имена from_at/to_at/trigger_by — потому что from, to и
// trigger — ключевые слова SQL. AUTOINCREMENT — как у выпусков: ID сводки
// уходит наружу и не переиспользуется.
const sqliteSummaryStep = `CREATE TABLE trivia_summary (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	from_at    INTEGER NOT NULL, -- наносекунды Unix
	to_at      INTEGER NOT NULL, -- наносекунды Unix
	created_at INTEGER NOT NULL, -- наносекунды Unix
	trigger_by TEXT NOT NULL,    -- schedule | manual
	text       TEXT NOT NULL,
	error      TEXT NOT NULL,
	cost_usd   REAL NOT NULL,
	aggregate  TEXT NOT NULL,    -- JSON Aggregate
	spend      TEXT NOT NULL,    -- JSON Spend
	cost       TEXT NOT NULL     -- JSON llm.Cost
);
CREATE INDEX trivia_summary_created_at ON trivia_summary (created_at, id);`

var _ SummaryStore = (*SQLite)(nil)

const sqliteSummaryColumns = `id, from_at, to_at, created_at, trigger_by, text, error, aggregate, spend, cost`

func (s *SQLite) SaveSummary(ctx context.Context, sum Summary) (int64, error) {
	if err := summaryValidate(sum); err != nil {
		return 0, err
	}
	agg, spend, cost, err := summaryJSON(sum)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO trivia_summary
		(from_at, to_at, created_at, trigger_by, text, error, cost_usd, aggregate, spend, cost)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sum.From.UnixNano(), sum.To.UnixNano(), sum.CreatedAt.UnixNano(), sum.Trigger,
		sum.Text, sum.Error, sum.Cost.USD, agg, spend, cost)
	if err != nil {
		return 0, fmt.Errorf("trivia: сводка не сохранена: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("trivia: сводка: id записи: %w", err)
	}
	return id, nil
}

func (s *SQLite) Summary(ctx context.Context, id int64) (Summary, error) {
	sum, err := sqliteScanSummary(s.db.QueryRowContext(ctx,
		`SELECT `+sqliteSummaryColumns+` FROM trivia_summary WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Summary{}, ErrIssueNotFound
	}
	return sum, err
}

func (s *SQLite) Summaries(ctx context.Context, limit int) ([]Summary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sqliteSummaryColumns+` FROM trivia_summary
		ORDER BY created_at DESC, id DESC LIMIT ?`, summaryLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("trivia: сводки: %w", err)
	}
	defer rows.Close()
	list := []Summary{}
	for rows.Next() {
		sum, err := sqliteScanSummary(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trivia: сводки: %w", err)
	}
	return list, nil
}

func (s *SQLite) LatestSummary(ctx context.Context) (Summary, bool, error) {
	sum, err := sqliteScanSummary(s.db.QueryRowContext(ctx, `SELECT `+sqliteSummaryColumns+
		` FROM trivia_summary ORDER BY created_at DESC, id DESC LIMIT 1`))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Summary{}, false, nil
	case err != nil:
		return Summary{}, false, err
	}
	return sum, true, nil
}

// sqliteScanSummary читает сводку из строки с sqliteSummaryColumns.
// sql.ErrNoRows возвращается как есть.
func sqliteScanSummary(row sqliteScanner) (Summary, error) {
	var (
		sum                Summary
		from, to, at       int64
		agg, spend, costJS string
	)
	if err := row.Scan(&sum.ID, &from, &to, &at, &sum.Trigger, &sum.Text, &sum.Error,
		&agg, &spend, &costJS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Summary{}, err
		}
		return Summary{}, fmt.Errorf("trivia: сводки: %w", err)
	}
	sum.From, sum.To, sum.CreatedAt = sqliteTime(from), sqliteTime(to), sqliteTime(at)
	if err := summaryUnJSON(&sum, agg, spend, costJS); err != nil {
		return Summary{}, err
	}
	return sum, nil
}
