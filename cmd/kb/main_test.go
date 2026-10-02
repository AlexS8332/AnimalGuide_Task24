package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
)

func runKB(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := dispatch(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestDispatchHelp(t *testing.T) {
	code, out, _ := runKB("help")
	if code != exitOK || !strings.Contains(out, "fetch") || !strings.Contains(out, "kb <команда>") {
		t.Fatalf("help: %d\n%s", code, out)
	}
	code, out, _ = runKB("help", "fetch")
	if code != exitOK || !strings.Contains(out, "-mdd-db") || !strings.Contains(out, "-only") {
		t.Fatalf("help fetch: %d\n%s", code, out)
	}
	if code, _, errOut := runKB(); code != exitUsage || !strings.Contains(errOut, "Команды:") {
		t.Fatalf("без аргументов: %d", code)
	}
	if code, _, errOut := runKB("frobnicate"); code != exitUsage || !strings.Contains(errOut, "нет команды") {
		t.Fatalf("неизвестная команда: %d %s", code, errOut)
	}
	if code, _, _ := runKB("fetch", "-nope"); code != exitUsage {
		t.Fatalf("неизвестный флаг: %d", code)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("повторная регистрация не запаниковала")
		}
	}()
	register("fetch", "", nil)
}

// fakeWiki — подставная Википедия: статья по заголовку, revid растёт с
// каждым запросом (так видно, какие статьи сняты заново).
func fakeWiki(t *testing.T) (*httptest.Server, *atomic.Int64) {
	var rev atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		title := r.URL.Query().Get("titles")
		n := rev.Add(1)
		page := map[string]any{
			"title":     title,
			"extract":   title + " — хищник.\n\n\n== Описание ==\nТекст о «" + title + "».\n\n\n== Примечания ==\n\n\n== Литература ==\nКнига.",
			"revisions": []map[string]any{{"revid": 100 + n, "timestamp": "2026-01-01T00:00:00Z"}},
		}
		if title == "Снежный барс" {
			page["title"] = "Ирбис"
		}
		json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"pages": []any{page}}})
	}))
	t.Cleanup(srv.Close)
	return srv, &rev
}

// mddDB — файл SQLite с мини-набором MDD.
func mddDB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "trivia.db")
	conn, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	st, err := mdd.NewSQLite(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Replace(ctx, mddtest.Sample()); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSources(t *testing.T, dir string) {
	t.Helper()
	src := `{"schema": 1, "wikipedia": [
  {"id": "manul", "title": "Манул", "species": {"latin": "Otocolobus manul", "ru": "манул"}},
  {"id": "snow-leopard", "title": "Снежный барс", "species": null}
], "mdd": {"id": "mdd-carnivora", "title": "Хищные"}}`
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sources.json"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFetchEndToEnd(t *testing.T) {
	srv, _ := fakeWiki(t)
	dir := filepath.Join(t.TempDir(), "corpus")
	writeSources(t, dir)
	dbPath := mddDB(t)

	code, out, errOut := runKB("fetch", "-corpus", dir, "-wiki-base", srv.URL, "-pause", "0", "-mdd-db", dbPath)
	if code != exitOK {
		t.Fatalf("fetch: %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "«Снежный барс» → «Ирбис»") {
		t.Errorf("нет заметки о перенаправлении: %s", errOut)
	}
	docs, m, err := corpus.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 || docs[0].ID != "manul" || docs[1].Title != "Ирбис" || docs[2].ID != corpus.MDDDocID {
		t.Fatalf("документы: %+v", m.Entries)
	}
	if docs[0].Species == nil || docs[0].Species.Latin != "Otocolobus manul" || docs[1].Species != nil {
		t.Errorf("species: %+v %+v", docs[0].Species, docs[1].Species)
	}
	if len(docs[0].Sections) != 1 || docs[0].Sections[0].Title != "Описание" {
		t.Errorf("разделы: %+v", docs[0].Sections)
	}
	for _, want := range []string{"manul", "Ирбис", "rev 101", "mdd-carnivora", "Документов: 3", "corpus_sha: " + m.CorpusSHA, fmt.Sprintf("страниц: %.1f", m.Pages)} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
	if lic, err := os.ReadFile(filepath.Join(dir, "LICENSE.md")); err != nil || !strings.Contains(string(lic), "oldid=101") {
		t.Errorf("LICENSE.md: %v\n%s", err, lic)
	}

	// -only: остальные документы — из прежнего снимка, MDD без -mdd-db
	// остаётся прежним.
	code, out, errOut = runKB("fetch", "-corpus", dir, "-wiki-base", srv.URL, "-pause", "0", "-only", "snow-leopard")
	if code != exitOK {
		t.Fatalf("fetch -only: %d\n%s\n%s", code, out, errOut)
	}
	docs2, _, err := corpus.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if docs2[0].RevID != docs[0].RevID || docs2[1].RevID == docs[1].RevID || docs2[2].Intro != docs[2].Intro {
		t.Errorf("-only пересобрал не то: %d→%d, %d→%d", docs[0].RevID, docs2[0].RevID, docs[1].RevID, docs2[1].RevID)
	}
	if !strings.Contains(out, "(из прежнего снимка)") {
		t.Errorf("нет пометки прежних:\n%s", out)
	}

	// Повреждённый снимок с -only — ошибка, а не тихое узаконивание.
	p := filepath.Join(dir, "manul.json")
	raw, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(raw), "хищник", "травоядное", 1)), 0o644)
	code, _, errOut = runKB("fetch", "-corpus", dir, "-wiki-base", srv.URL, "-pause", "0", "-only", "snow-leopard")
	if code != exitFailed || !strings.Contains(errOut, "не прошёл проверку") {
		t.Fatalf("повреждённый снимок: %d %s", code, errOut)
	}

	// Неизвестный id в -only.
	if code, _, errOut := runKB("fetch", "-corpus", dir, "-only", "wolf"); code != exitUsage || !strings.Contains(errOut, "wolf") {
		t.Fatalf("неизвестный id: %d %s", code, errOut)
	}
	// Несуществующая база MDD не создаётся.
	missing := filepath.Join(t.TempDir(), "nope", "trivia.db")
	if code, _, _ := runKB("fetch", "-corpus", dir, "-wiki-base", srv.URL, "-pause", "0", "-mdd-db", missing); code != exitFailed {
		t.Fatalf("нет базы MDD: %d", code)
	}
	if _, err := os.Stat(filepath.Dir(missing)); err == nil {
		t.Error("fetch создал каталог несуществующей базы")
	}
}

func TestFetchWithoutMDDFirstTime(t *testing.T) {
	srv, _ := fakeWiki(t)
	dir := filepath.Join(t.TempDir(), "corpus")
	writeSources(t, dir)
	code, out, errOut := runKB("fetch", "-corpus", dir, "-wiki-base", srv.URL, "-pause", "0")
	if code != exitOK || !strings.Contains(errOut, "нужен -mdd-db") || !strings.Contains(out, "Документов: 2") {
		t.Fatalf("без MDD: %d\n%s\n%s", code, out, errOut)
	}
}
