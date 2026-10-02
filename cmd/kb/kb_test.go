package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed/embedtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
)

// Подкоманды базы знаний на настоящем корпусе (31 документ режется и
// кодируется hash-эмбеддером за секунду) во временной базе.

const repoCorpus = "../../corpus"

func TestIndexSearchStatsEval(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kb.db")

	code, out, errOut := runKB("index", "-db", dbPath, "-corpus", repoCorpus, "-embedder", "hash")
	if code != exitOK {
		t.Fatalf("index: %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"Корпус: 31 документов", "structure (max 1200, min 200): чанков", "fixed", "hash-256", "промахов"} {
		if !strings.Contains(out, want) {
			t.Errorf("index: нет %q в\n%s", want, out)
		}
	}
	// fixed при all — окно по медиане structure, перекрытие 15 %.
	st, err := kb.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := st.Chunks(t.Context(), "structure", "")
	fi, err := st.Index(t.Context(), "fixed")
	st.Close()
	if err != nil || fi.Params.Size != kb.MedianChars(sc) || fi.Params.Overlap != fi.Params.Size*15/100 {
		t.Fatalf("fixed: %+v (медиана structure %d), %v", fi.Params, kb.MedianChars(sc), err)
	}

	// Повторная сборка — всё из кэша.
	code, out, _ = runKB("index", "-db", dbPath, "-corpus", repoCorpus, "-embedder", "hash", "-strategy", "structure")
	if code != exitOK || !strings.Contains(out, " 0 промахов") {
		t.Fatalf("повторная сборка: %d\n%s", code, out)
	}

	code, out, _ = runKB("stats", "-db", dbPath)
	if code != exitOK || !strings.Contains(out, "Документов: 31") || !strings.Contains(out, "corpus_sha: ") ||
		!strings.Contains(out, "structure") || !strings.Contains(out, "fixed") {
		t.Fatalf("stats: %d\n%s", code, out)
	}

	code, out, _ = runKB("search", "-db", dbPath, "-embedder", "hash", "-k", "3", "чем питается манул")
	if code != exitOK || !strings.Contains(out, "[structure] режим dense, hash-256") || !strings.Contains(out, "[fixed]") ||
		!strings.Contains(out, " 1. ") || strings.Contains(out, " 4. ") {
		t.Fatalf("search: %d\n%s", code, out)
	}
	// Флаги после вопроса, BM25.
	code, out, _ = runKB("search", "-db", dbPath, "сколько весит снежный барс", "-mode", "bm25", "-index", "structure")
	if code != exitOK || !strings.Contains(out, "[structure] режим bm25") || strings.Contains(out, "[fixed]") ||
		!strings.Contains(out, "snow-leopard/structure/") {
		t.Fatalf("search bm25: %d\n%s", code, out)
	}
	// Без эмбеддера dense откатывается и говорит почему.
	code, out, _ = runKB("search", "-db", dbPath, "-embedder", "none", "-index", "fixed", "кошачий медведь")
	if code != exitOK || !strings.Contains(out, "откат: эмбеддер не задан") {
		t.Fatalf("search откат: %d\n%s", code, out)
	}

	md, js := filepath.Join(dir, "out", "chunking.md"), filepath.Join(dir, "out", "chunking.json")
	code, out, errOut = runKB("eval", "-db", dbPath, "-embedder", "hash", "-questions", "../../eval/questions.json",
		"-out", md, "-json", js, "-bm25")
	if code != exitOK || !strings.Contains(out, "- structure: recall@5") || !strings.Contains(out, "BM25 (справочно)") {
		t.Fatalf("eval: %d\n%s\n%s", code, out, errOut)
	}
	raw, err := os.ReadFile(md)
	if err != nil || !strings.Contains(string(raw), "## Вопросы test") {
		t.Fatalf("markdown: %v", err)
	}
	var r kb.Report
	raw, err = os.ReadFile(js)
	if err != nil || json.Unmarshal(raw, &r) != nil || len(r.Stats) != 2 || len(r.Retrieval) != 8 {
		t.Fatalf("json: %v %+v", err, r.Stats)
	}
	// KB_DB — путь базы по умолчанию.
	t.Setenv("KB_DB", dbPath)
	code, out, _ = runKB("stats")
	if code != exitOK || !strings.Contains(out, "Последнее сравнение") {
		t.Fatalf("stats после eval (KB_DB):\n%s", out)
	}

	// JSON — рядом с markdown, без -json.
	md2 := filepath.Join(dir, "other", "report.md")
	if code, out, errOut := runKB("eval", "-embedder", "hash", "-questions", "../../eval/questions.json", "-out", md2); code != exitOK {
		t.Fatalf("eval -out: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "other", "report.json")); err != nil {
		t.Fatalf("JSON рядом с markdown: %v", err)
	}
	// Dense откатился на BM25 — в отчёт по умолчанию не пишем.
	code, _, errOut = runKB("eval", "-embedder", "none", "-questions", "../../eval/questions.json")
	if code != exitFailed || !strings.Contains(errOut, "-out") || !strings.Contains(errOut, "BM25") {
		t.Fatalf("eval без эмбеддера в путь по умолчанию: %d\n%s", code, errOut)
	}
	if _, err := os.Stat("examples"); err == nil {
		t.Fatal("отчёт по BM25 записан в путь по умолчанию")
	}
	// С явным -out — можно.
	if code, _, errOut := runKB("eval", "-embedder", "none", "-questions", "../../eval/questions.json", "-out", md2); code != exitOK {
		t.Fatalf("eval без эмбеддера с -out: %d\n%s", code, errOut)
	}
}

func TestIndexHTTPAndErrors(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kb.db")
	fake := embedtest.Server(t)
	t.Setenv("EMBED_BASE_URL", fake.URL)
	t.Setenv("EMBED_MODEL", "")
	t.Setenv("EMBED_API_KEY", "")
	// auto: сайдкар отвечает — индекс с векторами модели сайдкара.
	code, out, errOut := runKB("index", "-db", dbPath, "-corpus", repoCorpus, "-strategy", "structure")
	if code != exitOK || !strings.Contains(out, "intfloat/multilingual-e5-base") || !strings.Contains(errOut, "эмбеддер: ") {
		t.Fatalf("index auto: %d\n%s\n%s", code, out, errOut)
	}
	// Адрес флагом -embed-url важнее EMBED_BASE_URL.
	t.Setenv("EMBED_BASE_URL", "http://127.0.0.1:1")
	code, out, errOut = runKB("index", "-db", dbPath, "-corpus", repoCorpus, "-strategy", "structure", "-embed-url", fake.URL)
	if code != exitOK || !strings.Contains(out, "intfloat/multilingual-e5-base") || !strings.Contains(errOut, fake.URL) {
		t.Fatalf("index -embed-url: %d\n%s\n%s", code, out, errOut)
	}
	// Сайдкар упал: auto собирает без векторов и предупреждает.
	t.Setenv("EMBED_BASE_URL", "http://127.0.0.1:1")
	code, out, errOut = runKB("index", "-db", dbPath, "-corpus", repoCorpus, "-strategy", "fixed", "-size", "500")
	if code != exitOK || !strings.Contains(out, "без векторов") || !strings.Contains(errOut, "недоступен") ||
		!strings.Contains(out, "(size 500, overlap 75)") {
		t.Fatalf("index без сайдкара: %d\n%s\n%s", code, out, errOut)
	}
	// -overlap 0 — без перекрытия.
	code, out, errOut = runKB("index", "-db", dbPath, "-corpus", repoCorpus, "-strategy", "fixed", "-size", "500", "-overlap", "0", "-embedder", "none")
	if code != exitOK || !strings.Contains(out, "(size 500, overlap 0)") {
		t.Fatalf("index без сайдкара: %d\n%s\n%s", code, out, errOut)
	}
	// http без проверки — ошибка кодирования честно роняет сборку.
	code, _, errOut = runKB("index", "-db", dbPath, "-corpus", repoCorpus, "-strategy", "fixed", "-embedder", "http")
	if code != exitFailed || !strings.Contains(errOut, "недоступен") {
		t.Fatalf("index http без сайдкара: %d\n%s", code, errOut)
	}

	for _, args := range [][]string{
		{"index", "-strategy", "magic"},
		{"index", "-embedder", "magic", "-db", dbPath},
		{"search", "-db", dbPath},
		{"search", "-db", dbPath, "-mode", "magic", "манул"},
		{"eval", "-db", dbPath, "-questions", filepath.Join(dir, "nope.json")},
	} {
		if code, _, _ := runKB(args...); code != exitUsage {
			t.Errorf("%v: код %d, ждали %d", args, code, exitUsage)
		}
	}
	missing := filepath.Join(dir, "missing.db")
	for _, args := range [][]string{
		{"stats", "-db", missing},
		{"search", "-db", missing, "манул"},
		{"eval", "-db", missing, "-questions", "../../eval/questions.json"},
	} {
		code, _, errOut := runKB(args...)
		if code != exitFailed || !strings.Contains(errOut, "kb index") {
			t.Errorf("%v: код %d, %s", args, code, errOut)
		}
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("search/stats создали пустую базу")
	}
	if code, _, _ := runKB("index", "-db", dbPath, "-corpus", filepath.Join(dir, "nope")); code != exitFailed {
		t.Fatalf("нет корпуса: %d", code)
	}
	if code, out, _ := runKB("help", "index"); code != exitOK || !strings.Contains(out, "-strategy") {
		t.Fatalf("help index: %s", out)
	}
	if code, _, errOut := runKB("search", "-h"); code != exitOK || !strings.Contains(errOut, "-mode") {
		t.Fatalf("search -h: %d %s", code, errOut)
	}
}
