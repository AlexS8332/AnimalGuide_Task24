package trivia

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
)

// База, созданная программой этапа 2 (только шаг 1 схемы), доводится до
// шага 2 без потери выборов, и выпуск ссылается на старый выбор. Тест
// внутренний: шаги схемы не экспортируются, а triviatest отсюда не
// импортировать — был бы цикл.
func TestSQLiteMigrateFromPicksOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	at := time.Date(2026, 9, 20, 8, 30, 0, 42, time.UTC)

	conn, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, conn, SQLiteComponent, sqliteSteps[:1]); err != nil {
		t.Fatal(err)
	}
	old := &SQLite{db: conn}
	p := Pick{SpeciesID: 1006010, SciName: "Otocolobus manul", IUCN: "LC", PickedAt: at, Attempts: 2,
		Rejected: []Rejection{{SpeciesID: 7, SciName: "Castor fiber", Reason: ReasonNoArticle}},
		Eligibility: Eligibility{SpeciesID: 1006010, SciName: "Otocolobus manul", CheckedAt: at, OK: true,
			WikiLang: "ru", WikiTitle: "Манул", Occurrences: 1800},
		Weights: WeightsReport{"LC": 0.75, "EN": 0.25}}
	if p.ID, err = old.SavePick(ctx, p); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	conn, err = db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if v, err := db.Version(ctx, conn, SQLiteComponent); err != nil || v != 1 {
		t.Fatalf("версия старой базы %d, %v; ждали 1", v, err)
	}
	st, err := NewSQLite(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := db.Version(ctx, conn, SQLiteComponent); err != nil || v != len(sqliteSteps) {
		t.Errorf("версия после миграции %d, %v; ждали %d", v, err, len(sqliteSteps))
	}

	got, err := st.Pick(ctx, p.ID)
	if err != nil {
		t.Fatalf("Pick(%d) после миграции: %v", p.ID, err)
	}
	if got.SciName != p.SciName || !got.PickedAt.Equal(at) || len(got.Rejected) != 1 ||
		got.Weights["EN"] != 0.25 || !got.Eligibility.CheckedAt.Equal(at) {
		t.Errorf("выбор после миграции: %+v", got)
	}
	if list, err := st.Picks(ctx, 0); err != nil || len(list) != 1 {
		t.Errorf("Picks после миграции: %d, %v", len(list), err)
	}

	id, err := st.SaveIssue(ctx, Issue{PickID: p.ID, SpeciesID: p.SpeciesID, SciName: p.SciName,
		NameRu: "Манул", CreatedAt: at.Add(time.Minute), Status: IssueThin,
		Facts: []Fact{{Text: "Зрачки круглые.", Sources: []string{"S2"}}}})
	if err != nil {
		t.Fatal(err)
	}
	list, total, err := st.Issues(ctx, IssueQuery{Text: "МАНУЛ"})
	if err != nil || total != 1 || len(list) != 1 || list[0].ID != id || list[0].PickID != p.ID {
		t.Errorf("Issues после миграции: %+v, total %d, %v", list, total, err)
	}
}
