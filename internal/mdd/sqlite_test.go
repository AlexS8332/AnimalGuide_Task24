package mdd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
)

// Имена помощников с префиксом sqlT — в пакете mdd тесты пишут и другие.

func sqlTManul() Species {
	return Species{
		ID: 1006010, Phylosort: 2300, SciName: "Otocolobus manul",
		CommonName: "Pallas's Cat", OtherCommonNames: []string{"Manul"},
		Order: "Carnivora", Family: "Felidae", Subfamily: "Felinae",
		Genus: "Otocolobus", Epithet: "manul", Authority: "(Pallas, 1776)", Year: 1776,
		IUCN:               "LC",
		Countries:          []string{"Kazakhstan", "Mongolia", "China", "Russia", "Iran"},
		CountriesUncertain: []string{"Azerbaijan", "Tajikistan"},
		Continents:         []string{"Asia"},
		Realms:             []string{"Palearctic", "Indomalayan"},
		TypeLocality:       "Russia, Transbaikalia, Dzhida River",
	}
}

func sqlTPlatypus() Species {
	return Species{
		ID: 1000001, Phylosort: 1, SciName: "Ornithorhynchus anatinus",
		CommonName: "Platypus", OtherCommonNames: []string{"Duck-billed Platypus"},
		Order: "Monotremata", Family: "Ornithorhynchidae",
		Genus: "Ornithorhynchus", Epithet: "anatinus", Authority: "(G. K. Shaw, 1799)", Year: 1799,
		IUCN: "NT", Countries: []string{"Australia"}, Continents: []string{"Oceania"},
		Realms: []string{"Australasian"},
	}
}

func sqlTSnowLeopard() Species {
	return Species{
		ID: 1006050, Phylosort: 2400, SciName: "Panthera uncia",
		CommonName: "Snow Leopard", OtherCommonNames: []string{"Ounce"},
		Order: "Carnivora", Family: "Felidae", Subfamily: "Pantherinae",
		Genus: "Panthera", Epithet: "uncia", Authority: "(Schreber, 1775)", Year: 1775,
		IUCN:      "VU",
		Countries: []string{"Kazakhstan", "Mongolia", "China", "Tajikistan"},
		Realms:    []string{"Palearctic"},
	}
}

func sqlTCat() Species {
	return Species{
		ID: 1006100, Phylosort: 2350, SciName: "Felis catus",
		CommonName: "Domestic Cat", Order: "Carnivora", Family: "Felidae",
		Genus: "Felis", Epithet: "catus", Authority: "Linnaeus, 1758", Year: 1758,
		IUCN: "NE", Domestic: true,
	}
}

func sqlTThylacine() Species {
	return Species{
		ID: 1000500, Phylosort: 300, SciName: "Thylacinus cynocephalus",
		CommonName: "Thylacine", OtherCommonNames: []string{"Tasmanian Tiger", "Tasmanian Wolf"},
		Order: "Dasyuromorphia", Family: "Thylacinidae",
		Genus: "Thylacinus", Epithet: "cynocephalus", Year: 1808,
		IUCN: "EX", Extinct: true, Countries: []string{"Australia"}, Realms: []string{"Australasian"},
	}
}

func sqlTDataset() *Dataset {
	return &Dataset{
		Release: Release{
			Version: "v2.5", Date: "2026-07-28", Citation: "MDD 2026",
			ETag: `"abc"`, SourceURL: DefaultURL, PrevVersion: "v2.4",
			LoadedAt: time.Date(2026, 9, 1, 10, 0, 0, 123, time.UTC),
			Species:  999, // Replace пишет фактическое число видов
		},
		Species: []Species{sqlTManul(), sqlTPlatypus(), sqlTSnowLeopard(), sqlTCat(), sqlTThylacine()},
		Changes: []Change{
			{NewName: "Sorex novus", Category: "de novo", Reference: "Smith 2025"},
			{OldName: "Myotis a", NewName: "Myotis b", Category: "split", Comment: "разделён"},
			{OldName: "Myotis c", Category: "lump"},
			{NewName: "Crocidura nova", Category: "De Novo"},
		},
	}
}

// sqlTStores — одно и то же хранилище на базе в памяти и на файле.
func sqlTStores(t *testing.T) map[string]*SQLite {
	t.Helper()
	out := map[string]*SQLite{}
	for name, path := range map[string]string{
		"memory": db.Memory,
		"file":   filepath.Join(t.TempDir(), "guide.db"),
	} {
		conn, err := db.Open(context.Background(), path)
		if err != nil {
			t.Fatalf("%s: Open: %v", name, err)
		}
		t.Cleanup(func() { conn.Close() })
		st, err := NewSQLite(context.Background(), conn)
		if err != nil {
			t.Fatalf("%s: NewSQLite: %v", name, err)
		}
		out[name] = st
	}
	return out
}

func sqlTIDs(list []Species) []int {
	ids := make([]int, len(list))
	for i, s := range list {
		ids[i] = s.ID
	}
	return ids
}

func sqlTPtr(b bool) *bool { return &b }

func TestSQLiteEmpty(t *testing.T) {
	ctx := context.Background()
	for name, st := range sqlTStores(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := st.Release(ctx); !errors.Is(err, ErrNotFound) {
				t.Errorf("Release на пустой базе: %v", err)
			}
			if _, err := st.Get(ctx, 1006010); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get: %v", err)
			}
			if _, err := st.Find(ctx, "Pallas's Cat"); !errors.Is(err, ErrNotFound) {
				t.Errorf("Find: %v", err)
			}
			ids, err := st.IDs(ctx)
			if err != nil || len(ids) != 0 {
				t.Errorf("IDs: %v, %v", ids, err)
			}
			list, total, err := st.Search(ctx, Query{})
			if err != nil || len(list) != 0 || total != 0 {
				t.Errorf("Search: %v %d %v", list, total, err)
			}
			ch, err := st.Changes(ctx, "", 0)
			if err != nil || len(ch) != 0 {
				t.Errorf("Changes: %v %v", ch, err)
			}
			if err := st.Replace(ctx, &Dataset{}); err == nil {
				t.Error("Replace пустым набором должен давать ошибку")
			}
			if err := st.Replace(ctx, nil); err == nil {
				t.Error("Replace(nil) должен давать ошибку")
			}
		})
	}
}

func TestSQLiteStore(t *testing.T) {
	ctx := context.Background()
	for name, st := range sqlTStores(t) {
		t.Run(name, func(t *testing.T) {
			d := sqlTDataset()
			if err := st.Replace(ctx, d); err != nil {
				t.Fatalf("Replace: %v", err)
			}

			r, err := st.Release(ctx)
			if err != nil {
				t.Fatalf("Release: %v", err)
			}
			want := d.Release
			want.Species = len(d.Species)
			if !r.LoadedAt.Equal(want.LoadedAt) {
				t.Errorf("LoadedAt %v, хотим %v", r.LoadedAt, want.LoadedAt)
			}
			r.LoadedAt, want.LoadedAt = time.Time{}, time.Time{}
			if r != want {
				t.Errorf("Release:\n got %+v\nwant %+v", r, want)
			}

			// Get возвращает вид целиком, как записан.
			got, err := st.Get(ctx, 1006010)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !reflect.DeepEqual(got, sqlTManul()) {
				t.Errorf("Get:\n got %+v\nwant %+v", got, sqlTManul())
			}
			if got.URL() != "https://www.mammaldiversity.org/taxon/1006010/" {
				t.Errorf("URL %s", got.URL())
			}
			cat, _ := st.Get(ctx, 1006100)
			if !reflect.DeepEqual(cat, sqlTCat()) {
				t.Errorf("Get без списков:\n got %+v\nwant %+v", cat, sqlTCat())
			}
			if _, err := st.Get(ctx, 42); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get несуществующего: %v", err)
			}

			// Find: латинское через пробел и «_», основное и прочие английские, регистр.
			for _, n := range []string{"Otocolobus manul", "otocolobus_manul", "  OTOCOLOBUS   Manul ",
				"pallas's cat", "MANUL"} {
				s, err := st.Find(ctx, n)
				if err != nil || s.ID != 1006010 {
					t.Errorf("Find(%q) = %d, %v", n, s.ID, err)
				}
			}
			if s, err := st.Find(ctx, "tasmanian tiger"); err != nil || s.ID != 1000500 {
				t.Errorf("Find прочего названия: %d, %v", s.ID, err)
			}
			for _, n := range []string{"Otocolobus", "pallas", "", "  "} {
				if _, err := st.Find(ctx, n); !errors.Is(err, ErrNotFound) {
					t.Errorf("Find(%q) — хотим ErrNotFound, получили %v", n, err)
				}
			}

			ids, err := st.IDs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(ids, []int{1000001, 1000500, 1006010, 1006050, 1006100}) {
				t.Errorf("IDs %v", ids)
			}

			all, err := st.Changes(ctx, "", 0)
			if err != nil || !reflect.DeepEqual(all, d.Changes) {
				t.Errorf("Changes все: %+v, %v", all, err)
			}
			novo, _ := st.Changes(ctx, "DE NOVO", 0)
			if len(novo) != 2 || novo[0].NewName != "Sorex novus" || novo[1].NewName != "Crocidura nova" {
				t.Errorf("Changes de novo: %+v", novo)
			}
			if one, _ := st.Changes(ctx, "", 1); len(one) != 1 || one[0].NewName != "Sorex novus" {
				t.Errorf("Changes limit 1: %+v", one)
			}
			if none, _ := st.Changes(ctx, "merge", 0); len(none) != 0 {
				t.Errorf("Changes неизвестной категории: %+v", none)
			}
		})
	}
}

func TestSQLiteSearch(t *testing.T) {
	ctx := context.Background()
	for name, st := range sqlTStores(t) {
		t.Run(name, func(t *testing.T) {
			if err := st.Replace(ctx, sqlTDataset()); err != nil {
				t.Fatal(err)
			}
			cases := []struct {
				name  string
				q     Query
				ids   []int
				total int
			}{
				// Порядок — phylosort: утконос 1, тилацин 300, манул 2300, кошка 2350, ирбис 2400.
				{"всё", Query{}, []int{1000001, 1000500, 1006010, 1006100, 1006050}, 5},
				{"текст латинский", Query{Text: "OTOCOLOBUS_MAN"}, []int{1006010}, 1},
				{"текст английский", Query{Text: "cat"}, []int{1006010, 1006100}, 2},
				{"текст прочее название", Query{Text: "tasmanian"}, []int{1000500}, 1},
				{"текст с пробелом", Query{Text: "snow  leo"}, []int{1006050}, 1},
				{"отряд", Query{Order: "carnivora"}, []int{1006010, 1006100, 1006050}, 3},
				{"семейство", Query{Family: "FELIDAE"}, []int{1006010, 1006100, 1006050}, 3},
				{"род", Query{Genus: "panthera"}, []int{1006050}, 1},
				{"страна", Query{Country: "kazakhstan"}, []int{1006010, 1006050}, 2},
				{"страна среди неуверенных", Query{Country: "Tajikistan"}, []int{1006010, 1006050}, 2},
				{"только неуверенная", Query{Country: "Azerbaijan"}, []int{1006010}, 1},
				{"страна — точное имя, не подстрока", Query{Country: "Kazakh"}, nil, 0},
				{"область", Query{Realm: "australasian"}, []int{1000001, 1000500}, 2},
				{"МСОП любой из", Query{IUCN: []string{"lc", "VU", ""}}, []int{1006010, 1006050}, 2},
				{"вымершие", Query{Extinct: sqlTPtr(true)}, []int{1000500}, 1},
				{"не вымершие", Query{Extinct: sqlTPtr(false)}, []int{1000001, 1006010, 1006100, 1006050}, 4},
				{"домашние", Query{Domestic: sqlTPtr(true)}, []int{1006100}, 1},
				{"сочетание", Query{Family: "Felidae", Country: "Mongolia", Domestic: sqlTPtr(false), Text: "leopard"}, []int{1006050}, 1},
				{"ничего", Query{Text: "dragon"}, nil, 0},
				{"limit", Query{Limit: 2}, []int{1000001, 1000500}, 5},
				{"offset", Query{Limit: 2, Offset: 2}, []int{1006010, 1006100}, 5},
				{"offset за концом", Query{Offset: 10}, nil, 5},
				{"отрицательные", Query{Limit: -1, Offset: -3}, []int{1000001, 1000500, 1006010, 1006100, 1006050}, 5},
			}
			for _, c := range cases {
				list, total, err := st.Search(ctx, c.q)
				if err != nil {
					t.Errorf("%s: %v", c.name, err)
					continue
				}
				if ids := sqlTIDs(list); total != c.total || !(len(ids) == 0 && len(c.ids) == 0) && !reflect.DeepEqual(ids, c.ids) {
					t.Errorf("%s: %v (total %d), хотим %v (total %d)", c.name, ids, total, c.ids, c.total)
				}
			}
			// Найденный вид — целиком, со списками.
			list, _, _ := st.Search(ctx, Query{Text: "manul"})
			if len(list) != 1 || !reflect.DeepEqual(list[0], sqlTManul()) {
				t.Errorf("Search отдаёт вид не целиком: %+v", list)
			}
		})
	}
}

// sqlTSynthetic — n синтетических видов, похожих на настоящие по объёму.
func sqlTSynthetic(n, base int) *Dataset {
	d := &Dataset{Release: Release{Version: fmt.Sprintf("v-synth-%d", base)}}
	iucn := []string{"LC", "NT", "VU", "EN", "CR", "DD"}
	countries := []string{"Kazakhstan", "Mongolia", "China", "Russia", "Brazil", "Peru", "Kenya", "India"}
	for i := 0; i < n; i++ {
		d.Species = append(d.Species, Species{
			ID: base + i, Phylosort: i, SciName: fmt.Sprintf("Genus%d species%d", i/7, i),
			CommonName:       fmt.Sprintf("Synthetic Mammal %d", i),
			OtherCommonNames: []string{fmt.Sprintf("Other Name %d", i), fmt.Sprintf("Alt %d", i)},
			Order:            fmt.Sprintf("Order%d", i%27), Family: fmt.Sprintf("Family%d", i%160),
			Genus: fmt.Sprintf("Genus%d", i/7), Epithet: fmt.Sprintf("species%d", i),
			Authority: "Someone, 1900", Year: 1900, IUCN: iucn[i%len(iucn)],
			Countries:          []string{countries[i%8], countries[(i+1)%8], countries[(i+3)%8]},
			CountriesUncertain: []string{countries[(i+5)%8]},
			Continents:         []string{"Asia"}, Realms: []string{"Palearctic"},
			TypeLocality:      "Somewhere, near a river",
			DistributionNotes: "Widespread in the lowlands; recorded from several provinces.",
			TaxonomyNotes:     "Formerly included in another genus; see reference.",
		})
	}
	for i := 0; i < 300; i++ {
		d.Changes = append(d.Changes, Change{NewName: fmt.Sprintf("New name %d", i), Category: "de novo"})
	}
	return d
}

// Повторный Replace заменяет справочник целиком; тот же тест — замер
// скорости на объёме настоящего MDD (~7000 видов).
func TestSQLiteReplaceWhole(t *testing.T) {
	ctx := context.Background()
	for name, st := range sqlTStores(t) {
		t.Run(name, func(t *testing.T) {
			if err := st.Replace(ctx, sqlTDataset()); err != nil {
				t.Fatal(err)
			}
			const n = 7000
			start := time.Now()
			if err := st.Replace(ctx, sqlTSynthetic(n, 2000000)); err != nil {
				t.Fatalf("Replace: %v", err)
			}
			t.Logf("Replace %d видов (%s): %v", n, name, time.Since(start))

			if _, err := st.Get(ctx, 1006010); !errors.Is(err, ErrNotFound) {
				t.Errorf("старый вид остался: %v", err)
			}
			if _, err := st.Find(ctx, "Pallas's Cat"); !errors.Is(err, ErrNotFound) {
				t.Errorf("старое название осталось: %v", err)
			}
			if list, total, _ := st.Search(ctx, Query{Country: "Australia"}); total != 0 {
				t.Errorf("старая страна осталась: %v", sqlTIDs(list))
			}
			ids, _ := st.IDs(ctx)
			if len(ids) != n {
				t.Errorf("видов %d, хотим %d", len(ids), n)
			}
			r, err := st.Release(ctx)
			if err != nil || r.Version != "v-synth-2000000" || r.Species != n || r.LoadedAt.IsZero() {
				t.Errorf("Release: %+v, %v", r, err)
			}
			if ch, _ := st.Changes(ctx, "split", 0); len(ch) != 0 {
				t.Errorf("старые изменения остались: %+v", ch)
			}

			start = time.Now()
			list, total, err := st.Search(ctx, Query{Country: "brazil", IUCN: []string{"LC", "EN"}, Limit: 100})
			t.Logf("Search по стране среди %d (%s): %v, total %d", n, name, time.Since(start), total)
			if err != nil || len(list) == 0 || total < len(list) {
				t.Errorf("Search: %d/%d, %v", len(list), total, err)
			}
			start = time.Now()
			_, total, _ = st.Search(ctx, Query{Text: "mammal 69"})
			t.Logf("Search подстроки среди %d (%s): %v, total %d", n, name, time.Since(start), total)
			if total != 111 { // 69, 690…699, 6900…6999
				t.Errorf("Search подстроки: total %d", total)
			}
			if s, err := st.Find(ctx, "genus3_species21"); err != nil || s.ID != 2000021 {
				t.Errorf("Find: %d, %v", s.ID, err)
			}

			// Сбой посреди Replace (дубль id) откатывает всё: справочник прежний.
			bad := sqlTDataset()
			bad.Species = append(bad.Species, sqlTManul())
			if err := st.Replace(ctx, bad); err == nil {
				t.Fatal("дубль id должен давать ошибку")
			}
			if ids, _ := st.IDs(ctx); len(ids) != n {
				t.Errorf("после неудачного Replace видов %d, хотим %d", len(ids), n)
			}
		})
	}
}

// Читатели во время Replace видят либо старый набор, либо новый — не
// полупустую базу. На файле: WAL даёт читателям снимок.
func TestSQLiteReplaceConcurrentRead(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "guide.db")
	conn, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	st, err := NewSQLite(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	a, b := sqlTSynthetic(3000, 1000), sqlTSynthetic(2000, 100000)
	if err := st.Replace(ctx, a); err != nil {
		t.Fatal(err)
	}

	var (
		stop  atomic.Bool
		reads atomic.Int64
		wg    sync.WaitGroup
	)
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_, total, err := st.Search(ctx, Query{Limit: 1})
				if err != nil {
					t.Errorf("Search: %v", err)
					return
				}
				if total != 3000 && total != 2000 {
					t.Errorf("читатель увидел %d видов", total)
					return
				}
				ids, err := st.IDs(ctx)
				if err != nil {
					t.Errorf("IDs: %v", err)
					return
				}
				if len(ids) != 3000 && len(ids) != 2000 {
					t.Errorf("читатель увидел %d id", len(ids))
					return
				}
				reads.Add(1)
			}
		}()
	}
	for i := 0; i < 6; i++ {
		d := a
		if i%2 == 0 {
			d = b
		}
		if err := st.Replace(ctx, d); err != nil {
			t.Errorf("Replace: %v", err)
		}
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("чтений во время замен: %d", reads.Load())
}

// Схема применяется один раз: второе хранилище на той же базе её не ломает,
// данные сохраняются между открытиями файла.
func TestSQLiteReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "guide.db")
	open := func() (*sql.DB, *SQLite) {
		conn, err := db.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		st, err := NewSQLite(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		return conn, st
	}
	conn, st := open()
	if err := st.Replace(ctx, sqlTDataset()); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSQLite(ctx, conn); err != nil {
		t.Fatalf("повторный NewSQLite: %v", err)
	}
	conn.Close()

	conn, st = open()
	defer conn.Close()
	if s, err := st.Get(ctx, 1000001); err != nil || s.CommonName != "Platypus" {
		t.Errorf("после переоткрытия: %+v, %v", s, err)
	}
	if v, _ := db.Version(ctx, conn, SQLiteComponent); v != len(sqliteSteps) {
		t.Errorf("версия схемы %d", v)
	}
}

func TestSQLiteNilDB(t *testing.T) {
	if _, err := NewSQLite(context.Background(), nil); err == nil {
		t.Fatal("nil база должна давать ошибку")
	}
}
