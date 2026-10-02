package trivia

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// sqliteIssueStep — шаг 2 схемы trivia: выпуски фактов.
//
// Столбцами — то, по чему фильтруют, сортируют и что показывает список без
// разбора JSON (вид, время, состояние, заголовок, стоимость). Остальное —
// факты, отброшенные, источники, наблюдения, расход по шагам, стоимость
// целиком — JSON: это снимок выпуска, он читается целиком и не фильтруется.
// cost_usd дублирует cost.usd, чтобы сводка суммировала расход одним SUM.
//
// search — поле поиска, посчитанное Go (issueSearchText): нижний регистр
// SQLite без ICU не знает кириллицы. Ищется instr — подстрока без шаблонов
// LIKE, так что «%» и «_» в запросе — обычные символы. Цена: правило поиска
// зашито в данные, его смена потребует шага миграции, пересчитывающего
// search. Индекса у search нет — подстроку он не ускорит, а выпусков
// набирается по 24 в сутки.
//
// pick_id без внешнего ключа на trivia_pick — ради одинакового поведения с
// Memory, которая связь не проверяет; выпуск без выбора Builder не создаёт.
// AUTOINCREMENT — по той же причине, что у выборов: ID выпуска уходит
// наружу (MCP, ссылки) и не должен переиспользоваться.
const sqliteIssueStep = `CREATE TABLE trivia_issue (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	pick_id      INTEGER NOT NULL,
	species_id   INTEGER NOT NULL,
	sci_name     TEXT NOT NULL,
	name_ru      TEXT NOT NULL,
	iucn         TEXT NOT NULL,
	ord          TEXT NOT NULL,    -- отряд; order — ключевое слово SQL
	family       TEXT NOT NULL,
	created_at   INTEGER NOT NULL, -- наносекунды Unix
	status       TEXT NOT NULL,
	error        TEXT NOT NULL,
	title        TEXT NOT NULL,
	lead         TEXT NOT NULL,
	cost_usd     REAL NOT NULL,
	took         INTEGER NOT NULL, -- наносекунды
	realms       TEXT NOT NULL,    -- JSON []string
	facts        TEXT NOT NULL,    -- JSON []Fact
	dropped      TEXT NOT NULL,    -- JSON []Fact
	sources      TEXT NOT NULL,    -- JSON []Material
	observations TEXT NOT NULL,    -- JSON Observations
	spend        TEXT NOT NULL,    -- JSON []Spend
	cost         TEXT NOT NULL,    -- JSON llm.Cost
	search       TEXT NOT NULL     -- issueSearchText
);
CREATE INDEX trivia_issue_created_at ON trivia_issue (created_at, id);
CREATE INDEX trivia_issue_species ON trivia_issue (species_id, created_at);
CREATE INDEX trivia_issue_status ON trivia_issue (status, created_at);`

var _ IssueStore = (*SQLite)(nil)

// sqliteIssueColumns — столбцы trivia_issue в порядке sqliteScanIssue.
const sqliteIssueColumns = `id, pick_id, species_id, sci_name, name_ru, iucn, ord, family,
	created_at, status, error, title, lead, took,
	realms, facts, dropped, sources, observations, spend, cost`

func (s *SQLite) SaveIssue(ctx context.Context, is Issue) (int64, error) {
	if err := issueValidate(is); err != nil {
		return 0, err
	}
	// Через issueClone — чтобы пустые срезы легли в JSON так же, как их
	// отдаёт Memory (null, а не []).
	is = issueClone(is)
	var (
		js     [7]string
		fields = [7]struct {
			name string
			v    any
		}{
			{"регионы", is.Realms}, {"факты", is.Facts}, {"отброшенные факты", is.Dropped},
			{"источники", is.Sources}, {"наблюдения", is.Observations},
			{"расход", is.Spend}, {"стоимость", is.Cost},
		}
	)
	for i, f := range fields {
		b, err := json.Marshal(f.v)
		if err != nil {
			// Например, NaN в стоимости: ошибка расчёта, а не данных выпуска.
			return 0, fmt.Errorf("trivia: выпуск о виде %d: %s: %w", is.SpeciesID, f.name, err)
		}
		js[i] = string(b)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO trivia_issue
		(pick_id, species_id, sci_name, name_ru, iucn, ord, family, created_at, status, error,
		title, lead, cost_usd, took, realms, facts, dropped, sources, observations, spend, cost, search)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		is.PickID, is.SpeciesID, is.SciName, is.NameRu, is.IUCN, is.Order, is.Family,
		is.CreatedAt.UnixNano(), is.Status, is.Error, is.Title, is.Lead, is.Cost.USD, int64(is.Took),
		js[0], js[1], js[2], js[3], js[4], js[5], js[6], issueSearchText(is))
	if err != nil {
		return 0, fmt.Errorf("trivia: выпуск о виде %d не сохранён: %w", is.SpeciesID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("trivia: выпуск о виде %d: id записи: %w", is.SpeciesID, err)
	}
	return id, nil
}

func (s *SQLite) Issue(ctx context.Context, id int64) (Issue, error) {
	is, err := sqliteScanIssue(s.db.QueryRowContext(ctx,
		`SELECT `+sqliteIssueColumns+` FROM trivia_issue WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Issue{}, ErrIssueNotFound
	}
	return is, err
}

func (s *SQLite) Issues(ctx context.Context, q IssueQuery) ([]Issue, int, error) {
	limit, offset := issueLimit(q)
	var (
		where []string
		args  []any
	)
	if q.SpeciesID != 0 {
		where = append(where, "species_id = ?")
		args = append(args, q.SpeciesID)
	}
	if len(q.Status) > 0 {
		where = append(where, "status IN (?"+strings.Repeat(", ?", len(q.Status)-1)+")")
		for _, st := range q.Status {
			args = append(args, st)
		}
	}
	if !q.Since.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, storeNanos(q.Since))
	}
	if !q.Until.IsZero() {
		where = append(where, "created_at < ?")
		args = append(args, storeNanos(q.Until))
	}
	if needle := issueNeedle(q); needle != "" {
		where = append(where, "instr(search, ?) > 0")
		args = append(args, needle)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}

	// Счёт и страница — в одной транзакции: иначе выпуск, сохранённый между
	// запросами, дал бы total, не сходящийся со страницей. Транзакция
	// обычная, не ReadOnly: база открыта с _txlock=immediate, и короткая
	// блокировка записи на два чтения дешевле, чем разбор, как драйвер
	// сочетает это с ReadOnly.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("trivia: выпуски: %w", err)
	}
	defer tx.Rollback()

	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM trivia_issue`+cond, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("trivia: выпуски: число: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+sqliteIssueColumns+` FROM trivia_issue`+cond+
		` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("trivia: выпуски: %w", err)
	}
	defer rows.Close()
	list := []Issue{}
	for rows.Next() {
		is, err := sqliteScanIssue(rows)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, is)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("trivia: выпуски: %w", err)
	}
	return list, total, nil
}

func (s *SQLite) Pick(ctx context.Context, id int64) (Pick, error) {
	p, err := sqliteScanPick(s.db.QueryRowContext(ctx,
		`SELECT `+sqlitePickColumns+` FROM trivia_pick WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Pick{}, ErrIssueNotFound
	}
	return p, err
}

// sqliteScanIssue читает выпуск из строки с sqliteIssueColumns.
// sql.ErrNoRows возвращается как есть.
func sqliteScanIssue(row sqliteScanner) (Issue, error) {
	var (
		is       Issue
		at, took int64
		js       [7]string
	)
	if err := row.Scan(&is.ID, &is.PickID, &is.SpeciesID, &is.SciName, &is.NameRu, &is.IUCN,
		&is.Order, &is.Family, &at, &is.Status, &is.Error, &is.Title, &is.Lead, &took,
		&js[0], &js[1], &js[2], &js[3], &js[4], &js[5], &js[6]); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Issue{}, err
		}
		return Issue{}, fmt.Errorf("trivia: выпуски: %w", err)
	}
	is.CreatedAt = sqliteTime(at)
	is.Took = time.Duration(took)
	fields := [7]struct {
		name string
		v    any
	}{
		{"регионы", &is.Realms}, {"факты", &is.Facts}, {"отброшенные факты", &is.Dropped},
		{"источники", &is.Sources}, {"наблюдения", &is.Observations},
		{"расход", &is.Spend}, {"стоимость", &is.Cost},
	}
	for i, f := range fields {
		if err := json.Unmarshal([]byte(js[i]), f.v); err != nil {
			return Issue{}, fmt.Errorf("trivia: выпуск %d: %s: %w", is.ID, f.name, err)
		}
	}
	return is, nil
}
