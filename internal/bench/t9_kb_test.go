package bench

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
)

// TestKBTrial — И-9 на настоящем корпусе и hash-эмбеддере: все жёсткие
// проверки проходят, метрики сравнения в отчёте.
func TestKBTrial(t *testing.T) {
	env := &Env{Root: t.TempDir()}
	s := &Stand{env: env, dir: t.TempDir()}
	tr := &KB{CorpusDir: "../../corpus", Questions: "../../eval/questions.json", Embedder: embed.Hash{}}
	r := &Result{ID: tr.ID(), Title: tr.Title()}
	if err := tr.Run(context.Background(), s, r); err != nil {
		t.Fatal(err)
	}
	if r.Count(Fail) != 0 {
		for _, c := range r.Checks {
			t.Logf("%s: %s (%s) %s", c.Status, c.What, c.Got, c.Note)
		}
		t.Fatal("есть проваленные проверки")
	}
	if len(r.Checks) < 9 {
		t.Fatalf("проверок %d", len(r.Checks))
	}
	var whats []string
	for _, c := range r.Checks {
		whats = append(whats, c.What)
	}
	joined := strings.Join(whats, "\n")
	for _, want := range []string{"объём корпуса", "повторы chunk_id", "двух сборок", "чужого раздела", "Verify", "recall@5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("нет проверки %q", want)
		}
	}
	metrics := map[string]bool{}
	for _, m := range r.Metrics {
		metrics[m.What+"|"+m.Lane] = true
	}
	for _, want := range []string{"чанков|structure", "чанков|fixed", "recall@1/3/5, MRR — test|structure dense", "recall@1/3/5, MRR — dev|fixed bm25"} {
		if !metrics[want] {
			t.Errorf("нет метрики %q", want)
		}
	}
	if len(r.Notes) < 3 {
		t.Fatalf("вывод не попал в заметки: %q", r.Notes)
	}
}

// TestKBTrialNoEmbedder — без эмбеддера индекс собирается для BM25, а
// сравнение по recall — «не определено» с причиной.
func TestKBTrialNoEmbedder(t *testing.T) {
	t.Setenv(embed.EnvBaseURL, "http://127.0.0.1:1")
	s := &Stand{env: &Env{}, dir: t.TempDir()}
	tr := &KB{CorpusDir: "../../corpus", Questions: "../../eval/questions.json"}
	r := &Result{}
	if err := tr.Run(context.Background(), s, r); err != nil {
		t.Fatal(err)
	}
	if r.Count(Fail) != 0 || r.Count(Pending) != 1 {
		t.Fatalf("проверки: fail %d, pending %d", r.Count(Fail), r.Count(Pending))
	}
	// Числа — по строкам BM25, а не «0.00 против 0.00» из пустых dense.
	re := regexp.MustCompile(`BM25: (\d\.\d\d) против (\d\.\d\d)`)
	for _, c := range r.Checks {
		if c.Status != Pending {
			continue
		}
		if !strings.Contains(c.Note, "недоступен") {
			t.Fatalf("причина: %q", c.Note)
		}
		m := re.FindStringSubmatch(c.Note)
		if m == nil || m[1] == "0.00" || m[2] == "0.00" {
			t.Fatalf("recall BM25 в причине: %q", c.Note)
		}
	}
}

// TestKBTrialSharedCache — общий кэш эмбеддингов: векторы второго прогона
// берутся из kb.db первого (без промахов), а сама общая база не меняется.
func TestKBTrialSharedCache(t *testing.T) {
	run := func(cache string) (*Result, string) {
		t.Helper()
		s := &Stand{env: &Env{}, dir: t.TempDir()}
		tr := &KB{CorpusDir: "../../corpus", Questions: "../../eval/questions.json", Embedder: embed.Hash{}, CacheDB: cache}
		r := &Result{}
		if err := tr.Run(context.Background(), s, r); err != nil {
			t.Fatal(err)
		}
		if r.Count(Fail) != 0 {
			t.Fatalf("проваленные проверки: %+v", r.Checks)
		}
		return r, filepath.Join(s.Dir(), "kb.db")
	}
	metric := func(r *Result, what string) string {
		for _, m := range r.Metrics {
			if m.What == what {
				return m.Value
			}
		}
		return ""
	}
	first, shared := run("-")
	if got := metric(first, "кэш эмбеддингов (первая сборка)"); got == "" || strings.HasSuffix(got, " 0 промахов") {
		t.Fatalf("без общего кэша: %q", got)
	}
	// kb.db первого прогона — общий кэш второго.
	before, err := os.ReadFile(shared)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := run(shared)
	if got := metric(second, "кэш эмбеддингов (первая сборка)"); !strings.HasSuffix(got, " 0 промахов") {
		t.Fatalf("с общим кэшем: %q", got)
	}
	if got := metric(second, "общий кэш эмбеддингов (только чтение)"); !strings.Contains(got, "hash-256") {
		t.Fatalf("метрика общего кэша: %q", got)
	}
	after, _ := os.ReadFile(shared)
	if !bytes.Equal(before, after) {
		t.Fatal("общая база изменилась")
	}
	// Нет файла — заметка, а не ошибка.
	third, _ := run(filepath.Join(t.TempDir(), "nope.db"))
	if !strings.Contains(strings.Join(third.Notes, "\n"), "не открылся") {
		t.Fatalf("заметки: %q", third.Notes)
	}
}

// TestKBTrialBadCorpus — каталога корпуса нет: проверка провалена, а не
// поломка стенда.
func TestKBTrialBadCorpus(t *testing.T) {
	s := &Stand{env: &Env{}, dir: t.TempDir()}
	r := &Result{}
	if err := (&KB{CorpusDir: t.TempDir(), Embedder: embed.Hash{}}).Run(context.Background(), s, r); err != nil {
		t.Fatal(err)
	}
	if r.Verdict() != Fail {
		t.Fatalf("вердикт %s", r.Verdict())
	}
}
