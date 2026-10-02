package trivia

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
)

// SQLiteComponent — имя компонента в schema_migrations. Одно на весь пакет:
// следующие этапы (выпуски фактов, журнал запусков, сводки) добавят свои
// таблицы новыми шагами в sqliteSteps, а не отдельным компонентом.
const SQLiteComponent = "trivia"

// sqliteSteps — миграции схемы trivia. Только дописывать в конец:
// применённый шаг повторно не выполняется, правка старого до уже созданных
// баз не дойдёт.
//
// Время — INTEGER, наносекунды Unix (UTC по определению). Почему так:
//   - сравнение по диапазону (since, notBefore) — сравнение чисел, индекс
//     работает, и ничто не зависит от формата строки: у RFC3339Nano
//     переменная ширина дробной части, и «…:00.5Z» лексикографически
//     больше «…:00.25Z» лишь случайно, а «…:00Z» > «…:00.1Z» уже неверно;
//   - точность полная: time.Time, сохранённый и прочитанный, равен
//     исходному по Equal. С миллисекундами граница «ровно равно» ломалась
//     бы: since с наносекундами оказывался бы позже усечённой записи;
//   - цена — диапазон 1678–2262 годов (UnixNano), за ним сохранение
//     отказывает (см. storeCheckTime). Для моментов выбора этого хватает.
//
// Зона не хранится: время читается в UTC. Контракт сравнивает через Equal.
//
// trivia_pick — выбор одной строкой. Отвергнутые кандидаты, веса и итог
// проверки выбранного — JSON: это снимок «как выбирали», читается целиком и
// не фильтруется. AUTOINCREMENT, а не просто rowid: id выбора не
// переиспользуется даже после удаления строк — на него будут ссылаться
// выпуски фактов.
//
// trivia_check — кэш проверок, одна строка на вид; поля Eligibility —
// столбцами: сводке «почему отбраковали» нужны reason и ok без разбора JSON.
//
// Внешних ключей на mdd_species нет намеренно: новый релиз MDD заменяет
// справочник целиком, вид может исчезнуть или сменить id, а история
// выборов и кэш проверок от этого не должны ни пропасть, ни мешать замене.
var sqliteSteps = []string{
	`CREATE TABLE trivia_pick (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		species_id  INTEGER NOT NULL,
		sci_name    TEXT NOT NULL,
		iucn        TEXT NOT NULL,
		picked_at   INTEGER NOT NULL, -- наносекунды Unix
		attempts    INTEGER NOT NULL,
		rejected    TEXT NOT NULL,    -- JSON []Rejection
		eligibility TEXT NOT NULL,    -- JSON Eligibility
		weights     TEXT NOT NULL     -- JSON WeightsReport
	);
	CREATE INDEX trivia_pick_picked_at ON trivia_pick (picked_at, id);
	CREATE INDEX trivia_pick_species ON trivia_pick (species_id, picked_at);

	CREATE TABLE trivia_check (
		species_id  INTEGER PRIMARY KEY,
		sci_name    TEXT NOT NULL,
		checked_at  INTEGER NOT NULL, -- наносекунды Unix
		ok          INTEGER NOT NULL,
		reason      TEXT NOT NULL,
		wiki_lang   TEXT NOT NULL,
		wiki_title  TEXT NOT NULL,
		wiki_url    TEXT NOT NULL,
		en_title    TEXT NOT NULL,
		gbif_key    INTEGER NOT NULL,
		occurrences INTEGER NOT NULL
	);
	CREATE INDEX trivia_check_checked_at ON trivia_check (checked_at);`,

	// Шаг 2 — выпуски фактов; почему таблица устроена так — sqlite_issue.go.
	sqliteIssueStep,

	// Шаг 3 — сводки; устройство таблицы — sqlite_summary.go.
	sqliteSummaryStep,
}

// SQLite — PickStore поверх общей базы приложения. Базу открывает и
// закрывает владелец (db.Open): в ней живут и чужие таблицы.
type SQLite struct {
	db *sql.DB
}

var _ PickStore = (*SQLite)(nil)

// NewSQLite доводит схему trivia до текущей версии и возвращает хранилище.
// Повторный вызов на той же базе ничего не меняет.
func NewSQLite(ctx context.Context, conn *sql.DB) (*SQLite, error) {
	if conn == nil {
		return nil, errors.New("trivia: база не открыта")
	}
	if err := db.Migrate(ctx, conn, SQLiteComponent, sqliteSteps); err != nil {
		return nil, fmt.Errorf("trivia: схема: %w", err)
	}
	return &SQLite{db: conn}, nil
}

// sqliteTime — наносекунды Unix обратно во время (UTC).
func sqliteTime(ns int64) time.Time { return time.Unix(0, ns).UTC() }

// sqliteScanner — общее у *sql.Row и *sql.Rows: одно чтение строки на оба
// случая (список выборов и выбор по ID).
type sqliteScanner interface {
	Scan(dest ...any) error
}

// sqlitePickColumns — столбцы trivia_pick в порядке sqliteScanPick.
const sqlitePickColumns = `id, species_id, sci_name, iucn, picked_at,
	attempts, rejected, eligibility, weights`

// sqliteScanPick читает выбор из строки с sqlitePickColumns. sql.ErrNoRows
// возвращается как есть: «нет» решает вызывающий.
func sqliteScanPick(row sqliteScanner) (Pick, error) {
	var (
		p                          Pick
		at                         int64
		rejected, elig, weightsRaw string
	)
	if err := row.Scan(&p.ID, &p.SpeciesID, &p.SciName, &p.IUCN, &at,
		&p.Attempts, &rejected, &elig, &weightsRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Pick{}, err
		}
		return Pick{}, fmt.Errorf("trivia: выборы: %w", err)
	}
	p.PickedAt = sqliteTime(at)
	if err := json.Unmarshal([]byte(rejected), &p.Rejected); err != nil {
		return Pick{}, fmt.Errorf("trivia: выбор %d: отвергнутые кандидаты: %w", p.ID, err)
	}
	if err := json.Unmarshal([]byte(elig), &p.Eligibility); err != nil {
		return Pick{}, fmt.Errorf("trivia: выбор %d: итог проверки: %w", p.ID, err)
	}
	if err := json.Unmarshal([]byte(weightsRaw), &p.Weights); err != nil {
		return Pick{}, fmt.Errorf("trivia: выбор %d: веса статусов: %w", p.ID, err)
	}
	return p, nil
}

func (s *SQLite) SavePick(ctx context.Context, p Pick) (int64, error) {
	if err := storeValidatePick(p); err != nil {
		return 0, err
	}
	rejected, err := json.Marshal(p.Rejected)
	if err != nil {
		return 0, fmt.Errorf("trivia: выбор вида %d: отвергнутые кандидаты: %w", p.SpeciesID, err)
	}
	elig, err := json.Marshal(p.Eligibility)
	if err != nil {
		return 0, fmt.Errorf("trivia: выбор вида %d: итог проверки: %w", p.SpeciesID, err)
	}
	// Веса с NaN или бесконечностью JSON не примет — это и ошибка расчёта
	// вероятностей, так что отказ здесь уместен.
	weights, err := json.Marshal(p.Weights)
	if err != nil {
		return 0, fmt.Errorf("trivia: выбор вида %d: веса статусов: %w", p.SpeciesID, err)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO trivia_pick
		(species_id, sci_name, iucn, picked_at, attempts, rejected, eligibility, weights)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.SpeciesID, p.SciName, p.IUCN, p.PickedAt.UnixNano(), p.Attempts,
		string(rejected), string(elig), string(weights))
	if err != nil {
		return 0, fmt.Errorf("trivia: выбор вида %d не сохранён: %w", p.SpeciesID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("trivia: выбор вида %d: id записи: %w", p.SpeciesID, err)
	}
	return id, nil
}

func (s *SQLite) RecentSpecies(ctx context.Context, since time.Time) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT species_id FROM trivia_pick
		WHERE picked_at >= ? ORDER BY species_id`, storeNanos(since))
	if err != nil {
		return nil, fmt.Errorf("trivia: недавние виды: %w", err)
	}
	defer rows.Close()
	ids := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("trivia: недавние виды: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trivia: недавние виды: %w", err)
	}
	return ids, nil
}

func (s *SQLite) Picks(ctx context.Context, limit int) ([]Pick, error) {
	// LIMIT -1 в SQLite — без ограничения: один запрос на оба случая.
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+sqlitePickColumns+` FROM trivia_pick
		ORDER BY picked_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("trivia: выборы: %w", err)
	}
	defer rows.Close()
	list := []Pick{}
	for rows.Next() {
		p, err := sqliteScanPick(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trivia: выборы: %w", err)
	}
	return list, nil
}

func (s *SQLite) CachedCheck(ctx context.Context, speciesID int, notBefore time.Time) (Eligibility, bool, error) {
	var (
		e  Eligibility
		at int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT species_id, sci_name, checked_at, ok, reason,
		wiki_lang, wiki_title, wiki_url, en_title, gbif_key, occurrences
		FROM trivia_check WHERE species_id = ? AND checked_at >= ?`,
		speciesID, storeNanos(notBefore)).Scan(&e.SpeciesID, &e.SciName, &at, &e.OK, &e.Reason,
		&e.WikiLang, &e.WikiTitle, &e.WikiURL, &e.EnTitle, &e.GBIFKey, &e.Occurrences)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Eligibility{}, false, nil
	case err != nil:
		return Eligibility{}, false, fmt.Errorf("trivia: проверка вида %d из кэша: %w", speciesID, err)
	}
	e.CheckedAt = sqliteTime(at)
	return e, true, nil
}

func (s *SQLite) SaveCheck(ctx context.Context, e Eligibility) error {
	if err := storeValidateCheck(e); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO trivia_check
		(species_id, sci_name, checked_at, ok, reason, wiki_lang, wiki_title, wiki_url,
		en_title, gbif_key, occurrences) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.SpeciesID, e.SciName, e.CheckedAt.UnixNano(), e.OK, e.Reason, e.WikiLang, e.WikiTitle,
		e.WikiURL, e.EnTitle, e.GBIFKey, e.Occurrences); err != nil {
		return fmt.Errorf("trivia: проверка вида %d не сохранена: %w", e.SpeciesID, err)
	}
	return nil
}
