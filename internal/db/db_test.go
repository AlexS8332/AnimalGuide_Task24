package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openFile(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "dir", "app.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func pragma(t *testing.T, conn interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, name string) string {
	t.Helper()
	var v string
	if err := conn.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func TestOpenEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), "  "); err == nil {
		t.Fatal("пустой путь должен давать ошибку")
	}
}

// Прагмы должны стоять на каждом соединении пула, а не только на первом.
func TestOpenPragmasOnEveryConnection(t *testing.T) {
	ctx := context.Background()
	db, _ := openFile(t)

	// Держим несколько соединений одновременно — пул вынужден открыть новые.
	var conns []*sql.Conn
	for i := 0; i < 4; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i, c := range conns {
		if v := pragma(t, c, "journal_mode"); !strings.EqualFold(v, "wal") {
			t.Errorf("соединение %d: journal_mode = %s, хотим wal", i, v)
		}
		if v := pragma(t, c, "foreign_keys"); v != "1" {
			t.Errorf("соединение %d: foreign_keys = %s", i, v)
		}
		if v := pragma(t, c, "busy_timeout"); v != "5000" {
			t.Errorf("соединение %d: busy_timeout = %s", i, v)
		}
		if v := pragma(t, c, "synchronous"); v != "1" { // 1 = NORMAL
			t.Errorf("соединение %d: synchronous = %s", i, v)
		}
	}
}

// У базы в памяти все запросы должны видеть одну и ту же базу.
func TestOpenMemoryIsOneDatabase(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Memory)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v := pragma(t, db, "foreign_keys"); v != "1" {
		t.Errorf("foreign_keys = %s", v)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (x INT)`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := db.ExecContext(ctx, `INSERT INTO t VALUES (1)`); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Fatalf("строк %d, хотим 8", n)
	}

	// Вторая база в памяти — отдельная.
	other, err := Open(ctx, Memory)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.ExecContext(ctx, `SELECT count(*) FROM t`); err == nil {
		t.Fatal("вторая база в памяти видит таблицу первой")
	}
}

func TestMigrate(t *testing.T) {
	ctx := context.Background()
	db, path := openFile(t)

	steps := []string{
		`CREATE TABLE a (id INTEGER PRIMARY KEY, name TEXT);
		 CREATE INDEX a_name ON a (name);`,
		`ALTER TABLE a ADD COLUMN extra TEXT`,
	}
	if err := Migrate(ctx, db, "comp", steps[:1]); err != nil {
		t.Fatalf("первый шаг: %v", err)
	}
	if v, _ := Version(ctx, db, "comp"); v != 1 {
		t.Fatalf("версия %d, хотим 1", v)
	}
	if err := Migrate(ctx, db, "comp", steps); err != nil {
		t.Fatalf("второй шаг: %v", err)
	}
	if v, _ := Version(ctx, db, "comp"); v != 2 {
		t.Fatalf("версия %d, хотим 2", v)
	}
	// Повторный вызов ничего не делает (иначе ALTER упал бы на дубле столбца).
	if err := Migrate(ctx, db, "comp", steps); err != nil {
		t.Fatalf("повтор: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO a (name, extra) VALUES ('x', 'y')`); err != nil {
		t.Fatalf("схема не та: %v", err)
	}

	// Компоненты независимы.
	if err := Migrate(ctx, db, "other", []string{`CREATE TABLE b (x INT)`}); err != nil {
		t.Fatal(err)
	}
	if v, _ := Version(ctx, db, "other"); v != 1 {
		t.Fatalf("other: версия %d", v)
	}
	if v, _ := Version(ctx, db, "comp"); v != 2 {
		t.Fatalf("comp после other: версия %d", v)
	}

	// Программа старее базы.
	err := Migrate(ctx, db, "comp", steps[:1])
	if !errors.Is(err, ErrNewer) {
		t.Fatalf("хотим ErrNewer, получили %v", err)
	}

	// После переоткрытия версия та же.
	db.Close()
	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if v, _ := Version(ctx, db2, "comp"); v != 2 {
		t.Fatalf("после переоткрытия версия %d", v)
	}
}

// Упавший шаг откатывается целиком и не засчитывается; предыдущие остаются.
func TestMigrateFailedStepRollsBack(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Memory)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	steps := []string{
		`CREATE TABLE a (x INT)`,
		`CREATE TABLE b (x INT); CREATE TABLE a (x INT)`, // второе выражение падает
	}
	err = Migrate(ctx, db, "c", steps)
	if err == nil || !strings.Contains(err.Error(), "шаг 1 → 2") {
		t.Fatalf("ошибка: %v", err)
	}
	if v, _ := Version(ctx, db, "c"); v != 1 {
		t.Fatalf("версия %d, хотим 1", v)
	}
	if _, err := db.ExecContext(ctx, `SELECT * FROM b`); err == nil {
		t.Fatal("таблица b из упавшего шага осталась")
	}

	// Исправленный шаг применяется.
	steps[1] = `CREATE TABLE b (x INT)`
	if err := Migrate(ctx, db, "c", steps); err != nil {
		t.Fatal(err)
	}
	if v, _ := Version(ctx, db, "c"); v != 2 {
		t.Fatalf("версия %d, хотим 2", v)
	}
}

func TestMigrateEmptyComponent(t *testing.T) {
	db, err := Open(context.Background(), Memory)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(context.Background(), db, "", nil); err == nil {
		t.Fatal("пустой компонент должен давать ошибку")
	}
}

func TestVersionWithoutTable(t *testing.T) {
	db, err := Open(context.Background(), Memory)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	v, err := Version(context.Background(), db, "x")
	if err != nil || v != 0 {
		t.Fatalf("Version = %d, %v", v, err)
	}
}

// Две программы мигрируют одну базу одновременно — шаг применяется один раз.
func TestMigrateConcurrent(t *testing.T) {
	ctx := context.Background()
	_, path := openFile(t)
	steps := []string{`CREATE TABLE a (x INT)`, `CREATE TABLE b (x INT)`}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := Open(ctx, path)
			if err != nil {
				t.Error(err)
				return
			}
			defer db.Close()
			if err := Migrate(ctx, db, "c", steps); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
