package pipeline_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
)

// testClock — подставные часы хранилища: срок жизни проверяется без ожидания.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Общий договор Artifacts для обеих реализаций.
func TestArtifactsConformance(t *testing.T) {
	impls := map[string]func(t *testing.T, clock func() time.Time) pipeline.Artifacts{
		"Memory": func(t *testing.T, clock func() time.Time) pipeline.Artifacts {
			m := pipeline.NewMemory()
			m.Now = clock
			return m
		},
		"SQLite": func(t *testing.T, clock func() time.Time) pipeline.Artifacts {
			conn, err := db.Open(t.Context(), db.Memory)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			s, err := pipeline.NewSQLite(t.Context(), conn)
			if err != nil {
				t.Fatal(err)
			}
			// Повторная миграция на той же базе ничего не ломает.
			if _, err := pipeline.NewSQLite(t.Context(), conn); err != nil {
				t.Fatal(err)
			}
			s.Now = clock
			return s
		},
	}
	for name, mk := range impls {
		t.Run(name, func(t *testing.T) { artifactsConformance(t, mk) })
	}
}

func artifactsConformance(t *testing.T, mk func(*testing.T, func() time.Time) pipeline.Artifacts) {
	ctx := context.Background()
	clock := &testClock{t: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
	a := mk(t, clock.Now)

	dossier, err := pipeline.Seal(pipeline.KindDossier, map[string]any{"b": 2, "a": "манул"}, "")
	if err != nil {
		t.Fatal(err)
	}
	dossier.Summary = "Манул: 2 материала"
	if err := a.Put(ctx, dossier); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := a.Put(ctx, dossier); err != nil {
		t.Fatalf("повторный Put — ошибка: %v", err)
	}
	got, err := a.Get(ctx, dossier.Digest)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Kind != dossier.Kind || got.Digest != dossier.Digest || got.Summary != dossier.Summary {
		t.Errorf("Get = %+v, ждали %+v", got, dossier)
	}
	if err := got.Open(pipeline.KindDossier); err != nil {
		t.Errorf("прочитанный конверт не открывается: %v", err)
	}
	// ref без приставки и с пробелами — тот же отпечаток.
	if _, err := a.Get(ctx, "  "+strings.TrimPrefix(dossier.Digest, "sha256:")+" "); err != nil {
		t.Errorf("Get без приставки: %v", err)
	}

	facts, _ := pipeline.Seal(pipeline.KindFacts, map[string]int{"n": 5}, dossier.Digest)
	if err := a.Put(ctx, facts); err != nil {
		t.Fatalf("Put facts: %v", err)
	}
	if got, err := a.Get(ctx, facts.Digest); err != nil || got.Input != dossier.Digest {
		t.Errorf("Get facts = %+v, %v", got, err)
	}

	// Нет такого — ErrRef.
	if _, err := a.Get(ctx, "sha256:"+strings.Repeat("0", 64)); !errors.Is(err, pipeline.ErrRef) {
		t.Errorf("неизвестный ref: %v", err)
	}
	// Файл и испорченный конверт не хранятся.
	file, _ := pipeline.Seal(pipeline.KindFile, map[string]string{"path": "x"}, facts.Digest)
	if err := a.Put(ctx, file); !errors.Is(err, pipeline.ErrKind) {
		t.Errorf("Put file: %v", err)
	}
	bad := dossier
	bad.Data = []byte(`{"a":"манул","b":3}`)
	if err := a.Put(ctx, bad); !errors.Is(err, pipeline.ErrDigest) {
		t.Errorf("Put испорченного: %v", err)
	}

	// Срок жизни: через неделю с лишним — как нет; Put другого убирает.
	clock.Add(pipeline.ArtifactTTL - time.Hour)
	if err := a.Put(ctx, facts); err != nil { // освежили факты
		t.Fatal(err)
	}
	clock.Add(2 * time.Hour)
	if _, err := a.Get(ctx, dossier.Digest); !errors.Is(err, pipeline.ErrRef) {
		t.Errorf("просроченное досье: %v", err)
	}
	if _, err := a.Get(ctx, facts.Digest); err != nil {
		t.Errorf("освежённые факты пропали: %v", err)
	}
	other, _ := pipeline.Seal(pipeline.KindDossier, map[string]int{"x": 1}, "")
	if err := a.Put(ctx, other); err != nil {
		t.Fatal(err)
	}
	clock.Add(-2 * time.Hour) // часы назад: убранное не воскресает
	if _, err := a.Get(ctx, dossier.Digest); !errors.Is(err, pipeline.ErrRef) {
		t.Errorf("просроченное досье не убрано при Put: %v", err)
	}
}
