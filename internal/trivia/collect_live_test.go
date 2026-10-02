package trivia

import (
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// TestLiveCollect — сборка досье на настоящих источниках: манул, лев,
// Akodon surdus (статья только в англовики) и два случайных пригодных
// вида. Идёт только с TRIVIA_LIVE=1 и базой MDD в TRIVIA_MDD_DB (читается
// копия). С TRIVIA_WRITE_DOSSIER=1 досье манула и Akodon surdus
// сохраняются в testdata — по ним офлайн-тесты редактора и проверяющего.
func TestLiveCollect(t *testing.T) {
	if os.Getenv("TRIVIA_LIVE") != "1" {
		t.Skip("живая сборка: задай TRIVIA_LIVE=1 и TRIVIA_MDD_DB")
	}
	path := os.Getenv("TRIVIA_MDD_DB")
	if path == "" {
		t.Skip("TRIVIA_MDD_DB не задан")
	}
	ctx := context.Background()
	store := checkLiveOpenCopy(t, ctx, path)

	type target struct {
		sp   mdd.Species
		file string
	}
	var targets []target
	manul, err := store.Get(ctx, 1006010)
	if err != nil {
		t.Fatal(err)
	}
	targets = append(targets, target{manul, "dossier_manul.json"})
	for _, name := range []string{"Panthera leo", "Akodon surdus"} {
		sp, err := store.Find(ctx, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		file := ""
		if name == "Akodon surdus" {
			file = "dossier_akodon.json"
		}
		targets = append(targets, target{sp, file})
	}

	f := tools.NewFetcher()
	checker := NewWebChecker(f)
	collector := NewWebCollector(f)

	// Два случайных пригодных вида: кандидаты берутся, пока Check не
	// скажет «пригоден» (порог — как у Picker по умолчанию).
	ids, err := store.IDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(int64(checkEnvInt("TRIVIA_LIVE_SEED", time.Now().Day()))))
	checks := map[int]Eligibility{}
	for found, tries := 0, 0; found < 2 && tries < 40; tries++ {
		sp, err := store.Get(ctx, ids[rng.Intn(len(ids))])
		if err != nil {
			t.Fatal(err)
		}
		e, err := checker.Check(ctx, sp, Defaults().MinOccurrences)
		if err != nil || !e.OK {
			continue
		}
		checks[sp.ID] = e
		targets = append(targets, target{sp: sp})
		found++
	}

	t.Logf("%-26s %-3s %-22s %4s %8s %6s %5s %6s", "вид", "яз", "NameRu", "мат", "всего", "окно", "вне", "мс")
	for _, tg := range targets {
		sp := tg.sp
		e, ok := checks[sp.ID]
		if !ok {
			// Известные виды проверяются без порога: Akodon surdus может не
			// набрать 50 наблюдений, а досье по нему нужно ради en-статьи.
			e, err = checker.Check(ctx, sp, 0)
			if err != nil {
				t.Errorf("%s: проверка: %v", sp.SciName, err)
				continue
			}
		}
		p := Pick{SpeciesID: sp.ID, SciName: sp.SciName, IUCN: sp.IUCN, PickedAt: time.Now(), Attempts: 1, Eligibility: e}
		d, err := collector.Collect(ctx, p, sp)
		if err != nil {
			t.Errorf("%s: %v", sp.SciName, err)
			continue
		}
		t.Logf("%-26s %-3s %-22s %4d %8d %6d %5d %6d", sp.SciName, e.WikiLang, d.NameRu, len(d.Materials),
			d.Observations.Total, d.Observations.Recent, len(d.Observations.OutOfRange), d.Took.Milliseconds())
		for _, m := range d.Materials {
			t.Logf("    %-3s %-9s %5d  %s", m.ID, m.Kind, utf8.RuneCountInString(m.Text), m.Title)
		}
		for _, o := range d.Observations.OutOfRange {
			t.Logf("    вне ареала: %s %s %d", o.Code, o.Name, o.Count)
		}
		if tg.file != "" && os.Getenv("TRIVIA_WRITE_DOSSIER") == "1" {
			data, err := json.MarshalIndent(d, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join("testdata", tg.file), append(data, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("    записано testdata/%s", tg.file)
		}
	}
}
