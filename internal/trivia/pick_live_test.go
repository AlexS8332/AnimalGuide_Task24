package trivia

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// TestLivePick — сквозная проверка выбора на настоящем справочнике и
// настоящих источниках: Picker + WebChecker + SQLite в одной базе, как в
// демоне. Идёт только с TRIVIA_LIVE=1 и базой MDD в TRIVIA_MDD_DB (база
// копируется, рабочая не меняется). TRIVIA_LIVE_PICKS — сколько выборов
// подряд, по умолчанию 6.
func TestLivePick(t *testing.T) {
	if os.Getenv("TRIVIA_LIVE") != "1" || os.Getenv("TRIVIA_MDD_DB") == "" {
		t.Skip("живой выбор: задай TRIVIA_LIVE=1 и TRIVIA_MDD_DB")
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

	n := checkEnvInt("TRIVIA_LIVE_PICKS", 6)
	seen := map[int]bool{}
	for i := 0; i < n; i++ {
		// Свежий Fetcher на каждый выбор, как будет в демоне: кэш
		// Fetcher бессрочный, а число наблюдений со временем растёт.
		p := &Picker{Species: species, Checker: NewWebChecker(tools.NewFetcher()), Store: store}
		start := time.Now()
		pick, err := p.Pick(ctx)
		if err != nil {
			t.Fatalf("выбор %d: %v", i+1, err)
		}
		if seen[pick.SpeciesID] {
			t.Errorf("вид %s выбран повторно", pick.SciName)
		}
		seen[pick.SpeciesID] = true
		var rej []string
		for _, r := range pick.Rejected {
			rej = append(rej, r.SciName+":"+r.Reason)
		}
		e := pick.Eligibility
		t.Logf("%d. %-32s %-3s попыток %d, %s «%s», наблюдений %d, %v; отвергнуты: %s",
			i+1, pick.SciName, pick.IUCN, pick.Attempts, e.WikiLang, e.WikiTitle, e.Occurrences,
			time.Since(start).Round(time.Millisecond), strings.Join(rej, ", "))
	}
	picks, err := store.Picks(ctx, 0)
	if err != nil || len(picks) != n {
		t.Fatalf("в базе %d выборов (ошибка %v), ждали %d", len(picks), err, n)
	}
}
