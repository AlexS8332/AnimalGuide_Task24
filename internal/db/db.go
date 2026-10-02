// Package db — общая база SQLite приложения: открытие с нужными прагмами и
// версии схем по компонентам.
//
// В одном файле живут таблицы разных частей программы: справочник MDD,
// выпуски фактов, журнал планировщика, сводки. Каждая часть держит свою
// схему сама — объявляет шаги миграции и вызывает Migrate со своим именем
// компонента. Так добавление таблиц планировщика не требует трогать mdd, а
// версия схемы одного компонента не зависит от других.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // драйвер SQLite на чистом Go: cgo в сборке недоступен
)

// Memory — путь для базы в памяти (тесты). Живёт, пока открыт *sql.DB.
const Memory = ":memory:"

// ErrNewer — в базе схема компонента новее, чем знает программа. Работать с
// такой базой нельзя: старый код не знает, что значат новые столбцы, и при
// записи испортил бы данные.
var ErrNewer = errors.New("база новее программы")

// pragmas — настройки каждого соединения. Через DSN, а не одним Exec после
// открытия: пул database/sql открывает соединения когда захочет, и
// прагма, выполненная на одном из них, на остальные не действует.
//
//   - busy_timeout — писатель ждёт блокировку до 5 с, а не падает сразу
//     с «database is locked»;
//   - journal_mode=WAL — читатели не ждут писателя и видят снимок базы до
//     его транзакции, поэтому замена справочника не видна наполовину;
//   - synchronous=NORMAL — в режиме WAL безопасно и заметно быстрее FULL;
//   - foreign_keys — SQLite по умолчанию их не проверяет.
//
// _txlock=immediate — транзакция берёт блокировку записи сразу на BEGIN.
// С отложенной (по умолчанию) две транзакции, начавшие с чтения, при
// попытке записать получают SQLITE_BUSY немедленно, и busy_timeout не
// помогает.
const pragmas = "_pragma=busy_timeout(5000)" +
	"&_pragma=journal_mode(WAL)" +
	"&_pragma=synchronous(NORMAL)" +
	"&_pragma=foreign_keys(1)" +
	"&_txlock=immediate"

// Open открывает (и при нужде создаёт вместе с каталогом) базу по пути.
// Путь Memory — база в памяти: у неё каждое соединение своё, поэтому пул
// ограничен одним соединением, иначе второй запрос увидел бы пустую базу.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("путь к базе пуст")
	}

	var dsn string
	if path == Memory {
		// WAL базе в памяти не нужен (и не включится), остальное — то же.
		dsn = "file::memory:?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"
	} else {
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("каталог базы %s не создался: %w", dir, err)
			}
		}
		dsn = "file:" + filepath.ToSlash(path) + "?" + pragmas
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("база %s не открылась: %w", path, err)
	}
	if path == Memory {
		db.SetMaxOpenConns(1)
		// Закрытое простаивающее соединение унесло бы с собой всю базу.
		db.SetConnMaxIdleTime(0)
		db.SetConnMaxLifetime(0)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("база %s не отвечает: %w", path, err)
	}
	return db, nil
}

// Migrate доводит схему компонента до len(steps). steps[i] поднимает схему
// с версии i до i+1; шаг может состоять из нескольких выражений. Каждый шаг
// применяется в своей транзакции вместе с записью новой версии: упавший шаг
// не оставляет схему наполовину и не засчитывается.
//
// Уже применённые шаги не повторяются, поэтому шаги только добавляют в
// конец, а старые не правят. Если в базе версия больше len(steps) —
// ErrNewer.
func Migrate(ctx context.Context, db *sql.DB, component string, steps []string) error {
	if strings.TrimSpace(component) == "" {
		return errors.New("миграция: не указан компонент")
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		component TEXT PRIMARY KEY,
		version   INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("миграция %s: таблица версий: %w", component, err)
	}

	for {
		done, err := migrateStep(ctx, db, component, steps)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// migrateStep применяет один следующий шаг. Версия читается внутри той же
// транзакции (она берёт блокировку записи сразу): две программы, открывшие
// базу одновременно, не применят один шаг дважды.
func migrateStep(ctx context.Context, db *sql.DB, component string, steps []string) (done bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("миграция %s: %w", component, err)
	}
	defer tx.Rollback()

	var v int
	err = tx.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations WHERE component = ?`, component).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("миграция %s: версия схемы: %w", component, err)
	}
	switch {
	case v > len(steps):
		return false, fmt.Errorf("миграция %s: версия схемы в базе %d, программа знает %d: %w",
			component, v, len(steps), ErrNewer)
	case v == len(steps):
		return true, nil
	}

	if _, err := tx.ExecContext(ctx, steps[v]); err != nil {
		return false, fmt.Errorf("миграция %s: шаг %d → %d: %w", component, v, v+1, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (component, version) VALUES (?, ?)
		ON CONFLICT (component) DO UPDATE SET version = excluded.version`, component, v+1); err != nil {
		return false, fmt.Errorf("миграция %s: запись версии %d: %w", component, v+1, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("миграция %s: шаг %d → %d: %w", component, v, v+1, err)
	}
	return false, nil
}

// Version — текущая версия схемы компонента; 0, если он ещё не мигрировал.
func Version(ctx context.Context, db *sql.DB, component string) (int, error) {
	var v int
	err := db.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations WHERE component = ?`, component).Scan(&v)
	switch {
	case err == nil:
		return v, nil
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case strings.Contains(err.Error(), "no such table"):
		return 0, nil
	}
	return 0, fmt.Errorf("версия схемы %s: %w", component, err)
}
