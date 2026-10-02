package trivia

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// TestLiveIssue — сквозная сборка выпусков, как в демоне: выбор вида,
// досье, редактор, проверяющий, сохранение — на настоящих источниках и
// настоящей модели. Идёт только с TRIVIA_LIVE=1, базой MDD в
// TRIVIA_MDD_DB (копируется) и ключом DEEPSEEK_API_KEY в окружении.
// TRIVIA_LIVE_ISSUES — сколько выпусков, по умолчанию 3.
func TestLiveIssue(t *testing.T) {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if os.Getenv("TRIVIA_LIVE") != "1" || os.Getenv("TRIVIA_MDD_DB") == "" || key == "" {
		t.Skip("живой выпуск: задай TRIVIA_LIVE=1, TRIVIA_MDD_DB и DEEPSEEK_API_KEY")
	}
	ctx := context.Background()
	src := os.Getenv("TRIVIA_MDD_DB")
	dst := filepath.Join(t.TempDir(), "trivia.db")
	for _, suffix := range []string{"", "-wal"} {
		if err := checkCopyFile(src+suffix, dst+suffix); err != nil && (suffix == "" || !os.IsNotExist(err)) {
			t.Fatalf("копия базы: %v", err)
		}
	}
	conn, err := db.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	species, err := mdd.NewSQLite(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLite(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	model := llm.NewClient(key, os.Getenv("DEEPSEEK_BASE_URL"))

	var total llm.Cost
	n := checkEnvInt("TRIVIA_LIVE_ISSUES", 3)
	for i := 0; i < n; i++ {
		// Свежий Fetcher на выпуск — как будет в демоне.
		f := tools.NewFetcher()
		p := &Picker{Species: species, Checker: NewWebChecker(f), Store: store}
		pick, err := p.Pick(ctx)
		if err != nil {
			t.Fatalf("выбор: %v", err)
		}
		sp, err := species.Get(ctx, pick.SpeciesID)
		if err != nil {
			t.Fatal(err)
		}
		b := &Builder{Collector: NewWebCollector(f), Editor: &LLMEditor{LLM: model},
			Verifier: &LLMVerifier{LLM: model}, Store: store}
		is, err := b.Build(ctx, pick, sp)
		if err != nil {
			t.Errorf("выпуск %s: %v", pick.SciName, err)
		}
		total = total.Add(is.Cost)
		var sb strings.Builder
		for j, fct := range is.Facts {
			sb.WriteString("\n      " + string(rune('1'+j)) + ". " + fct.Text + " " + strings.Join(fct.Sources, ","))
		}
		for _, fct := range is.Dropped {
			sb.WriteString("\n      ✗ " + fct.Text + " — " + fct.Verdict)
		}
		var in, hit, out int
		for _, s := range is.Spend {
			in, hit, out = in+s.Usage.Prompt, hit+s.Usage.CacheHit, out+s.Usage.Completion
		}
		t.Logf("#%d %s (%s, %s) — %s, фактов %d, отброшено %d, $%.4f, токены %d (кэш %d) → %d, %v\n    %s\n    %s%s",
			is.ID, is.SciName, is.NameRu, is.IUCN, is.Status, len(is.Facts), len(is.Dropped),
			is.Cost.USD, in, hit, out, is.Took.Round(time.Millisecond), is.Title, is.Lead, sb.String())
	}
	t.Logf("всего $%.4f (%s)", total.USD, total.Tariff)
	list, all, err := store.Issues(ctx, IssueQuery{})
	if err != nil || all != n || len(list) != n {
		t.Fatalf("в базе %d выпусков (ошибка %v), ждали %d", all, err, n)
	}
}
