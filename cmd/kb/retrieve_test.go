package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
)

// Второй этап поиска в командах: search с флагами конвейера, calibrate,
// matrix и qa с режимами v23 — на настоящем корпусе и hash-эмбеддере.

func TestSearchPipeline(t *testing.T) {
	db := ragDB(t)
	code, out, errOut := runKB("search", "-db", db, "-embedder", "hash", "-index", "structure", "-rewrite", "code", "-filter", "-trace",
		"-k", "3", "Сколько часов в день кошачий медведь тратит на еду?")
	if code != exitOK {
		t.Fatalf("search: %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"[structure] rewrite code, filter; режим dense, hash-256", "запрос: Сколько часов в день кошачий медведь тратит на еду? малая панда\n", "в BM25: Сколько часов в день кошачий медведь тратит на еду? малая панда Ailurus fulgens",
		"раскрыто: кошачий медведь → малая панда", "порог 0.800 (умолчание) не применяется: вид назван в запросе (малая панда), лучший косинус", "отрыв от второго", "кандидатов ", "кандидаты (по порядку после реранкинга):", "✗ ", " 1. "} {
		if !strings.Contains(out, want) {
			t.Errorf("нет %q в\n%s", want, out)
		}
	}
	if strings.Contains(out, " 4. 0.") {
		t.Errorf("больше k:\n%s", out)
	}
	// Продолжение с -context; гибрид.
	code, out, _ = runKB("search", "-db", db, "-embedder", "hash", "-index", "structure", "-rewrite", "code", "-rerank", "hybrid",
		"-context", "Где в России водится харза?", "А сколько она весит?")
	if code != exitOK || !strings.Contains(out, "раскрыто: вид из контекста → харза") || !strings.Contains(out, "rerank hybrid") {
		t.Fatalf("продолжение: %d\n%s", code, out)
	}
	// Без эмбеддера — откат на BM25 с заметкой.
	code, out, _ = runKB("search", "-db", db, "-embedder", "none", "-index", "structure", "-filter", "сколько весит корсак")
	if code != exitOK || !strings.Contains(out, "BM25: абсолютного порога нет") {
		t.Fatalf("откат: %d\n%s", code, out)
	}
	for _, args := range [][]string{
		{"search", "-db", db, "-rewrite", "x", "манул"},
		{"search", "-db", db, "-rerank", "x", "манул"},
		{"search", "-db", db, "-mode", "bm25", "-filter", "манул"},
		{"search", "-db", db, "-k0", "0", "-filter", "манул"},
	} {
		if code, _, _ := runKB(args...); code != exitUsage {
			t.Errorf("%v: %d", args, code)
		}
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	if code, _, errOut := runKB("search", "-db", db, "-embedder", "hash", "-rerank", "llm", "манул"); code != exitUsage || !strings.Contains(errOut, "DEEPSEEK_API_KEY") {
		t.Fatalf("llm без ключа: %d %s", code, errOut)
	}
}

func TestCalibrateAndMatrix(t *testing.T) {
	db := ragDB(t)
	dir := t.TempDir()
	q := "../../eval/questions.json"
	md := filepath.Join(dir, "calibrate.md")
	code, out, errOut := runKB("calibrate", "-db", db, "-embedder", "hash", "-questions", q, "-out", md)
	if code != exitOK || !strings.Contains(out, "# Калибровка порога релевантности") || !strings.Contains(out, "Порог **") ||
		!strings.Contains(out, "В индекс не записан") {
		t.Fatalf("calibrate: %d\n%s\n%s", code, out, errOut)
	}
	var cal retrieve.Calibration
	raw, err := os.ReadFile(filepath.Join(dir, "calibrate.json"))
	if err != nil || json.Unmarshal(raw, &cal) != nil || cal.DevN != 19 || cal.OutN != 8 {
		t.Fatalf("calibrate.json: %v %+v", err, cal)
	}
	st, _ := kb.Open(t.Context(), db)
	ix, _ := st.Index(t.Context(), "structure")
	st.Close()
	if ix.MinScore != 0 {
		t.Fatal("порог записан без -write")
	}
	if code, out, _ := runKB("calibrate", "-db", db, "-embedder", "hash", "-questions", q, "-write"); code != exitOK || !strings.Contains(out, "Записан в индекс") {
		t.Fatalf("calibrate -write: %d\n%s", code, out)
	}
	st, _ = kb.Open(t.Context(), db)
	ix, _ = st.Index(t.Context(), "structure")
	st.Close()
	if ix.MinScore != cal.Chosen {
		t.Fatalf("в индексе %v, выбран %v", ix.MinScore, cal.Chosen)
	}
	if code, _, errOut := runKB("calibrate", "-db", db, "-embedder", "none", "-questions", q); code != exitFailed || !strings.Contains(errOut, "BM25") {
		t.Fatalf("calibrate по BM25: %d %s", code, errOut)
	}

	md = filepath.Join(dir, "rag", "filter.md")
	code, out, errOut = runKB("matrix", "-db", db, "-embedder", "hash", "-questions", q, "-k1", "3,5", "-splits", "dev,test,out", "-rrf", "-out", md)
	if code != exitOK || !strings.Contains(out, "- K1 = 5, dev+test, 27 вопросов с доказательством") || !strings.Contains(out, "JSON: "+filepath.Join(dir, "rag", "filter.json")) ||
		!strings.Contains(errOut, "конфигураций 6") {
		t.Fatalf("matrix: %d\n%s\n%s", code, out, errOut)
	}
	var m retrieve.Matrix
	raw, err = os.ReadFile(filepath.Join(dir, "rag", "filter.json"))
	if err != nil || json.Unmarshal(raw, &m) != nil || len(m.Rows) != 6*2*3 || m.Configs[5].Name != "rrf-only" || m.MinScore != cal.Chosen {
		t.Fatalf("filter.json: %v %d %+v", err, len(m.Rows), m.MinScore)
	}
	if b, err := os.ReadFile(md); err != nil || !strings.Contains(string(b), "## Вопросы test (K1 = 5)") {
		t.Fatalf("filter.md: %v", err)
	}
	// Откат на BM25 — отчёт по умолчанию не перетирается.
	questions, _ := filepath.Abs(q)
	t.Chdir(t.TempDir())
	code, _, errOut = runKB("matrix", "-db", db, "-embedder", "none", "-questions", questions, "-k1", "5", "-splits", "out")
	if code != exitFailed || !strings.Contains(errOut, "векторный поиск не состоялся") {
		t.Fatalf("matrix откат: %d %s", code, errOut)
	}
	if _, err := os.Stat(defaultMatrix); err == nil {
		t.Fatal("отчёт по BM25 записан")
	}
	for _, args := range [][]string{{"matrix", "-k1", "x"}, {"matrix", "-k1", "0"}, {"matrix", "-questions", "nope.json"}, {"calibrate", "-questions", "nope.json"}} {
		if code, _, _ := runKB(args...); code != exitUsage {
			t.Errorf("%v: %d", args, code)
		}
	}
}

// TestQAPipelined — kb qa -modes с режимами v23: тот же промпт, выдача —
// через конвейер.
func TestQAPipelined(t *testing.T) {
	f := &fakeDeepSeek{}
	f.serve(t)
	db := ragDB(t)
	dir := t.TempDir()
	code, out, errOut := runKB("qa", "-db", db, "-embedder", "hash", "-questions", "../../eval/questions.json",
		"-modes", "norag,rag,rag+filter,rag+rewrite,rag+both", "-judge=false", "-out", filepath.Join(dir, "qa.md"))
	if code != exitOK || !strings.Contains(errOut, "kb qa: rag+filter — порог 0.800 (умолчание)") || !strings.Contains(errOut, "kb qa: rag+both — порог 0.800 (умолчание)") ||
		strings.Contains(errOut, "rag+rewrite — порог") {
		t.Fatalf("qa: %d\n%s\n%s", code, out, errOut)
	}
	if f.count() != 50 || !strings.Contains(errOut, "T07 synonym: norag abstain, rag wrong, rag+filter wrong, rag+rewrite wrong, rag+both wrong") || !strings.Contains(out, "Доказательство в выдаче rag+rewrite") {
		t.Fatalf("qa: %d запросов\n%s\n%s", f.count(), out, errOut)
	}
	code, out, _ = runKB("ask", "-db", db, "-embedder", "hash", "-mode", "rag+rewrite", "Что ест кошачий медведь?")
	if code != exitOK || !strings.Contains(out, "== rag+rewrite ==") || !strings.Contains(out, "Запрос в поиск: Что ест кошачий медведь? малая панда\n") {
		t.Fatalf("ask rag+rewrite: %d\n%s", code, out)
	}
}
