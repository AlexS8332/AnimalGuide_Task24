package trivia

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
)

// База этапа 3 (шаги 1–2: выборы и выпуски) доводится до шага 3 без потери
// выборов и выпусков, а сводка сохраняется и читается рядом с ними.
func TestSQLiteMigrateToSummaries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	at := time.Date(2026, 9, 23, 10, 0, 0, 7, time.UTC)

	conn, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, conn, SQLiteComponent, sqliteSteps[:2]); err != nil {
		t.Fatal(err)
	}
	old := &SQLite{db: conn}
	p := Pick{SpeciesID: 1006010, SciName: "Otocolobus manul", IUCN: "LC", PickedAt: at, Attempts: 1,
		Eligibility: Eligibility{SpeciesID: 1006010, CheckedAt: at, OK: true}}
	if p.ID, err = old.SavePick(ctx, p); err != nil {
		t.Fatal(err)
	}
	isID, err := old.SaveIssue(ctx, Issue{PickID: p.ID, SpeciesID: p.SpeciesID, SciName: p.SciName,
		NameRu: "Манул", Order: "Carnivora", Realms: []string{"Palearctic"}, CreatedAt: at.Add(time.Minute),
		Status: IssueOK, Title: "Манул", Facts: []Fact{{Text: "Зрачки круглые.", Sources: []string{"S2"}}}})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	conn, err = db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if v, err := db.Version(ctx, conn, SQLiteComponent); err != nil || v != 2 {
		t.Fatalf("версия старой базы %d, %v; ждали 2", v, err)
	}
	st, err := NewSQLite(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := db.Version(ctx, conn, SQLiteComponent); err != nil || v != 3 || len(sqliteSteps) != 3 {
		t.Errorf("версия после миграции %d, %v; ждали 3 (шагов %d)", v, err, len(sqliteSteps))
	}
	if got, err := st.Pick(ctx, p.ID); err != nil || got.SciName != p.SciName || !got.PickedAt.Equal(at) {
		t.Errorf("выбор после миграции: %+v, %v", got, err)
	}
	if got, err := st.Issue(ctx, isID); err != nil || got.PickID != p.ID || len(got.Facts) != 1 {
		t.Errorf("выпуск после миграции: %+v, %v", got, err)
	}

	// Сводка по перенесённым данным.
	agg, err := (&Aggregator{Issues: st, Picks: st}).Aggregate(ctx, at, at.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if agg.Issues != 1 || agg.Picks != 1 || agg.Facts != 1 {
		t.Errorf("агрегат после миграции: %+v", agg)
	}
	sum := Summary{From: agg.From, To: agg.To, CreatedAt: agg.To, Trigger: SummaryTriggerSchedule,
		Aggregate: agg, Text: "Вышел 1 выпуск."}
	id, err := st.SaveSummary(ctx, sum)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.LatestSummary(ctx)
	if err != nil || !ok || got.ID != id || got.Aggregate.Issues != 1 || got.Text != sum.Text {
		t.Errorf("LatestSummary после миграции: %+v, ok=%v, %v", got, ok, err)
	}

	// Повторное открытие ничего не меняет.
	if _, err := NewSQLite(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if list, err := st.Summaries(ctx, 0); err != nil || len(list) != 1 {
		t.Errorf("Summaries после повторного NewSQLite: %d, %v", len(list), err)
	}
}
