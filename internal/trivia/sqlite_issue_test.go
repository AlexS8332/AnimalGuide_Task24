package trivia_test

import (
	"path/filepath"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia/triviatest"
)

// Выпуски в SQLite — в памяти и на файле, как и выборы.
func TestSQLiteIssueConformance(t *testing.T) {
	type store = interface {
		trivia.IssueStore
		trivia.PickStore
	}
	t.Run("Memory", func(t *testing.T) {
		triviatest.IssueStoreConformance(t, func(t *testing.T) store {
			return sqliteNew(t, sqliteOpen(t, db.Memory))
		})
	})
	t.Run("File", func(t *testing.T) {
		triviatest.IssueStoreConformance(t, func(t *testing.T) store {
			return sqliteNew(t, sqliteOpen(t, filepath.Join(t.TempDir(), "trivia.db")))
		})
	})
}
