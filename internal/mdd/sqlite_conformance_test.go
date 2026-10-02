package mdd_test

import (
	"context"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
)

// SQLite проходит тот же набор проверок контракта, что и Memory: две
// реализации Store обязаны вести себя одинаково.
func TestSQLiteConformance(t *testing.T) {
	mddtest.Conformance(t, func(t *testing.T) mdd.Store {
		conn, err := db.Open(context.Background(), db.Memory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		st, err := mdd.NewSQLite(context.Background(), conn)
		if err != nil {
			t.Fatal(err)
		}
		return st
	})
}
