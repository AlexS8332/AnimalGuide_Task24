package trivia_test

import (
	"path/filepath"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia/triviatest"
)

// Сводки: Memory и SQLite (в памяти и на файле) проходят одни проверки.
func TestSummaryStoreConformance(t *testing.T) {
	t.Run("Memory", func(t *testing.T) {
		triviatest.SummaryStoreConformance(t, func(t *testing.T) trivia.SummaryStore { return trivia.NewMemory() })
	})
	t.Run("SQLiteMemory", func(t *testing.T) {
		triviatest.SummaryStoreConformance(t, func(t *testing.T) trivia.SummaryStore {
			return sqliteNew(t, sqliteOpen(t, db.Memory))
		})
	})
	t.Run("SQLiteFile", func(t *testing.T) {
		triviatest.SummaryStoreConformance(t, func(t *testing.T) trivia.SummaryStore {
			return sqliteNew(t, sqliteOpen(t, filepath.Join(t.TempDir(), "trivia.db")))
		})
	})
}
