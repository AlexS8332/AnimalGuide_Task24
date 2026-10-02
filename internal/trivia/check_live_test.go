package trivia

import (
	"context"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// TestLiveCheck — замер WebChecker на настоящих источниках и настоящем
// справочнике: доли причин отказа и время проверки. Нужен, чтобы выбрать
// порядок этапов в Check и порог наблюдений. Идёт только с TRIVIA_LIVE=1 и
// базой MDD в TRIVIA_MDD_DB; запросы последовательные — источники
// бесплатные, и нагружать их не стоит.
//
// Кроме итога Check, для каждого вида досчитывается и этап, до которого
// Check не дошёл: так доля отказов каждого этапа видна независимо от
// порядка.
func TestLiveCheck(t *testing.T) {
	if os.Getenv("TRIVIA_LIVE") != "1" {
		t.Skip("живая проверка: задай TRIVIA_LIVE=1 и TRIVIA_MDD_DB")
	}
	path := os.Getenv("TRIVIA_MDD_DB")
	if path == "" {
		t.Skip("TRIVIA_MDD_DB не задан")
	}
	ctx := context.Background()
	store := checkLiveOpenCopy(t, ctx, path)

	ids, err := store.IDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// TRIVIA_LIVE_N и TRIVIA_LIVE_SEED — выборка побольше или другая для
	// повторного замера; по умолчанию 25 видов с зерном 18.
	sample := checkEnvInt("TRIVIA_LIVE_N", 25)
	rng := rand.New(rand.NewSource(int64(checkEnvInt("TRIVIA_LIVE_SEED", 18))))
	pick := []int{1006010, 1000001} // манул и утконос — заведомо пригодные
	seen := map[int]bool{1006010: true, 1000001: true}
	for len(pick) < sample+2 {
		id := ids[rng.Intn(len(ids))]
		if !seen[id] {
			seen[id] = true
			pick = append(pick, id)
		}
	}

	const minOcc = 50
	reasons := map[string]int{}
	var (
		durs             []time.Duration
		noArt, gbifFail  int
		artTime, gbiTime time.Duration
	)
	t.Logf("%-8s %-34s %-4s %-12s %-3s %9s %7s  %s", "id", "вид", "ok", "причина", "яз", "набл.", "мс", "статья")
	for _, id := range pick {
		sp, err := store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		// Свой Fetcher на вид: время Check — без кэша прошлых видов.
		c := NewWebChecker(tools.NewFetcher())
		start := time.Now()
		e, err := c.Check(ctx, sp, minOcc)
		d := time.Since(start)
		if err != nil {
			t.Errorf("%d %s: %v", id, sp.SciName, err)
			continue
		}
		durs = append(durs, d)
		reasons[e.Reason]++

		// Независимые исходы этапов: повтор идёт через кэш Fetcher, а
		// недошедший этап — сетью; время этапов меряется отдельно.
		var probe Eligibility
		s := time.Now()
		gr, gerr := c.webCheckGBIF(ctx, sp, minOcc, &probe)
		gbiTime += time.Since(s)
		s = time.Now()
		ar, aerr := c.webCheckArticle(ctx, sp, minOcc, &probe)
		artTime += time.Since(s)
		if gerr != nil || aerr != nil {
			t.Errorf("%d: этапы: %v / %v", id, gerr, aerr)
		}
		if ar != ReasonOK {
			noArt++
		}
		if gr != ReasonOK {
			gbifFail++
		}
		t.Logf("%-8d %-34s %-4v %-12s %-3s %9d %7d  %s (en: %s; gbif=%s, art=%s)",
			id, sp.SciName, e.OK, e.Reason, probe.WikiLang, probe.Occurrences, d.Milliseconds(),
			probe.WikiTitle, probe.EnTitle, webOr(gr, "ok"), webOr(ar, "ok"))
	}

	n := len(durs)
	if n == 0 {
		t.Fatal("ни одной проверки")
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	t.Logf("итог Check на %d видах (порог %d): %v", n, minOcc, reasons)
	t.Logf("медиана Check %v, минимум %v, максимум %v", durs[n/2], durs[0], durs[n-1])
	t.Logf("этапы независимо: нет статьи %d/%d, отказ GBIF (no_gbif+few_records) %d/%d", noArt, n, gbifFail, n)
	t.Logf("время этапов в сумме (с кэшем после Check): статья %v, GBIF %v", artTime, gbiTime)
}

func webOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// checkLiveOpenCopy открывает копию базы: db.Open включает WAL, а NewSQLite
// доводит схему — рабочую базу справочника тест не трогает вовсе.
func checkLiveOpenCopy(t *testing.T, ctx context.Context, path string) *mdd.SQLite {
	t.Helper()
	dir := t.TempDir()
	dst := filepath.Join(dir, "mdd.db")
	for _, suffix := range []string{"", "-wal"} {
		if err := checkCopyFile(path+suffix, dst+suffix); err != nil {
			if suffix == "" || !os.IsNotExist(err) {
				t.Fatalf("копия базы: %v", err)
			}
		}
	}
	conn, err := db.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	store, err := mdd.NewSQLite(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func checkCopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func checkEnvInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}
