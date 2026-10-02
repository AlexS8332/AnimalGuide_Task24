package trivia_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia/triviatest"
)

// sqliteOpen — база для теста (путь db.Memory или файл), закрывается в
// конце теста.
func sqliteOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func sqliteNew(t *testing.T, conn *sql.DB) *trivia.SQLite {
	t.Helper()
	st, err := trivia.NewSQLite(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// SQLite проходит те же проверки контракта, что и Memory, — и в памяти, и
// на файле: у файловой базы WAL и пул из нескольких соединений, у базы в
// памяти одно соединение, и параллельная запись ведёт себя по-разному.
func TestSQLiteConformance(t *testing.T) {
	t.Run("Memory", func(t *testing.T) {
		triviatest.PickStoreConformance(t, func(t *testing.T) trivia.PickStore {
			return sqliteNew(t, sqliteOpen(t, db.Memory))
		})
	})
	t.Run("File", func(t *testing.T) {
		triviatest.PickStoreConformance(t, func(t *testing.T) trivia.PickStore {
			return sqliteNew(t, sqliteOpen(t, filepath.Join(t.TempDir(), "trivia.db")))
		})
	})
}

func TestSQLiteNilDB(t *testing.T) {
	if _, err := trivia.NewSQLite(context.Background(), nil); err == nil {
		t.Error("NewSQLite(nil): нет ошибки")
	}
}

// Повторное открытие той же базы не повторяет миграции и не теряет данные.
func TestSQLiteReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "trivia.db")
	at := time.Date(2026, 9, 24, 10, 0, 0, 1, time.UTC)

	conn := sqliteOpen(t, path)
	st := sqliteNew(t, conn)
	id, err := st.SavePick(ctx, triviatest.SamplePick(5, at))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCheck(ctx, triviatest.SampleCheck(5, at, true)); err != nil {
		t.Fatal(err)
	}
	// Второй NewSQLite на том же соединении.
	sqliteNew(t, conn)
	conn.Close()

	conn = sqliteOpen(t, path)
	st = sqliteNew(t, conn)
	if v, err := db.Version(ctx, conn, trivia.SQLiteComponent); err != nil || v != 3 {
		t.Errorf("версия схемы %d, %v; ждали 3 (выборы, выпуски, сводки)", v, err)
	}
	list, err := st.Picks(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != id || !list[0].PickedAt.Equal(at) {
		t.Errorf("после переоткрытия: %+v", list)
	}
	if _, ok, err := st.CachedCheck(ctx, 5, at); err != nil || !ok {
		t.Errorf("CachedCheck после переоткрытия: ok=%v, %v", ok, err)
	}
	// Новый выбор получает следующий ID, а не переиспользует.
	id2, err := st.SavePick(ctx, triviatest.SamplePick(6, at))
	if err != nil {
		t.Fatal(err)
	}
	if id2 <= id {
		t.Errorf("ID после переоткрытия %d, ждали больше %d", id2, id)
	}
}

// База новее программы — ошибка, а не молчаливая работа со старой схемой.
func TestSQLiteNewerSchema(t *testing.T) {
	ctx := context.Background()
	conn := sqliteOpen(t, db.Memory)
	sqliteNew(t, conn)
	if _, err := conn.ExecContext(ctx,
		`UPDATE schema_migrations SET version = 99 WHERE component = ?`, trivia.SQLiteComponent); err != nil {
		t.Fatal(err)
	}
	if _, err := trivia.NewSQLite(ctx, conn); !errors.Is(err, db.ErrNewer) {
		t.Errorf("NewSQLite на базе новее: %v, ждали ErrNewer", err)
	}
}

// Справочник MDD и trivia живут в одной базе: схемы не мешают друг другу,
// а замена справочника не трогает историю выборов.
func TestSQLiteWithMDD(t *testing.T) {
	ctx := context.Background()
	conn := sqliteOpen(t, filepath.Join(t.TempDir(), "app.db"))

	ref, err := mdd.NewSQLite(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := ref.Replace(ctx, mddtest.Sample()); err != nil {
		t.Fatal(err)
	}
	st := sqliteNew(t, conn)

	sp, err := ref.Get(ctx, mddtest.Manul)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	p := triviatest.SamplePick(sp.ID, at)
	p.SciName, p.IUCN = sp.SciName, sp.IUCN
	if _, err := st.SavePick(ctx, p); err != nil {
		t.Fatal(err)
	}

	// Повторная загрузка того же справочника (как при обновлении релиза).
	if err := ref.Replace(ctx, mddtest.Sample()); err != nil {
		t.Fatal(err)
	}
	ids, err := st.RecentSpecies(ctx, at.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != mddtest.Manul {
		t.Errorf("RecentSpecies после замены справочника: %v", ids)
	}
	for _, c := range []string{mdd.SQLiteComponent, trivia.SQLiteComponent} {
		if v, err := db.Version(ctx, conn, c); err != nil || v < 1 {
			t.Errorf("версия схемы %s: %d, %v", c, v, err)
		}
	}
	if _, err := ref.Get(ctx, mddtest.Manul); err != nil {
		t.Errorf("справочник после работы trivia: %v", err)
	}
}
