package schedule_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule/runtest"
)

func TestMemoryConformance(t *testing.T) {
	runtest.RunStoreConformance(t, func(t *testing.T) schedule.RunStore {
		return schedule.NewMemory()
	})
}

func sqliteOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func sqliteNew(t *testing.T, conn *sql.DB) *schedule.SQLite {
	t.Helper()
	st, err := schedule.NewSQLite(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// SQLite — и в памяти (одно соединение), и на файле (WAL, пул соединений):
// параллельная запись ведёт себя по-разному.
func TestSQLiteConformance(t *testing.T) {
	t.Run("Memory", func(t *testing.T) {
		runtest.RunStoreConformance(t, func(t *testing.T) schedule.RunStore {
			return sqliteNew(t, sqliteOpen(t, db.Memory))
		})
	})
	t.Run("File", func(t *testing.T) {
		runtest.RunStoreConformance(t, func(t *testing.T) schedule.RunStore {
			return sqliteNew(t, sqliteOpen(t, filepath.Join(t.TempDir(), "schedule.db")))
		})
	})
}

func TestSQLiteNilDB(t *testing.T) {
	if _, err := schedule.NewSQLite(context.Background(), nil); err == nil {
		t.Error("NewSQLite(nil): нет ошибки")
	}
}

// Повторное открытие той же базы не повторяет миграцию и не теряет журнал.
func TestSQLiteReopen(t *testing.T) {
	ctx := context.Background()
	conn := sqliteOpen(t, filepath.Join(t.TempDir(), "schedule.db"))
	st := sqliteNew(t, conn)
	if _, err := st.Record(ctx, schedule.Run{Job: "issue", Trigger: schedule.TriggerSchedule,
		Scheduled: t0, Started: t0, Status: schedule.RunOK}); err != nil {
		t.Fatal(err)
	}
	st2 := sqliteNew(t, conn)
	list, err := st2.Runs(ctx, schedule.RunQuery{})
	if err != nil || len(list) != 1 {
		t.Fatalf("после повторного NewSQLite: %d записей, %v", len(list), err)
	}
	if v, err := db.Version(ctx, conn, schedule.SQLiteComponent); err != nil || v != 1 {
		t.Errorf("версия схемы: %d, %v", v, err)
	}
}
