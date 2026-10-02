package mddtest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
)

// Conformance проверяет контракт mdd.Store на любой реализации. newStore
// должен каждый раз отдавать новое пустое хранилище (закрытие — через
// t.Cleanup). Проверяется только то, что обещают комментарии интерфейса и
// Query: порядок изменений, поведение Changes при limit ≤ 0 и ошибки
// Search на пустой базе контрактом не заданы и здесь не проверяются.
func Conformance(t *testing.T, newStore func(t *testing.T) mdd.Store) {
	ctx := context.Background()
	loaded := func(t *testing.T) (mdd.Store, *mdd.Dataset) {
		t.Helper()
		st := newStore(t)
		d := Sample()
		if err := st.Replace(ctx, d); err != nil {
			t.Fatalf("Replace: %v", err)
		}
		return st, Sample()
	}

	t.Run("Empty", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.Release(ctx); !errors.Is(err, mdd.ErrNotFound) {
			t.Errorf("Release на пустой базе: %v, ждали ErrNotFound", err)
		}
		if _, err := st.Get(ctx, Manul); !errors.Is(err, mdd.ErrNotFound) {
			t.Errorf("Get на пустой базе: %v", err)
		}
		if _, err := st.Find(ctx, "Otocolobus manul"); !errors.Is(err, mdd.ErrNotFound) {
			t.Errorf("Find на пустой базе: %v", err)
		}
		if list, total, err := st.Search(ctx, mdd.Query{}); err == nil && (len(list) != 0 || total != 0) {
			t.Errorf("Search на пустой базе: %d видов, total %d", len(list), total)
		}
		if ids, err := st.IDs(ctx); err == nil && len(ids) != 0 {
			t.Errorf("IDs на пустой базе: %v", ids)
		}
		if ch, err := st.Changes(ctx, "", 10); err == nil && len(ch) != 0 {
			t.Errorf("Changes на пустой базе: %v", ch)
		}
	})

	t.Run("Release", func(t *testing.T) {
		st, want := loaded(t)
		got, err := st.Release(ctx)
		if err != nil {
			t.Fatal(err)
		}
		w := want.Release
		if got.Version != w.Version || got.Date != w.Date || got.PrevVersion != w.PrevVersion ||
			got.ETag != w.ETag || got.Citation != w.Citation || got.Remarks != w.Remarks ||
			got.SourceURL != w.SourceURL || got.Species != len(want.Species) || !got.LoadedAt.Equal(w.LoadedAt) {
			t.Errorf("релиз:\n got %+v\nwant %+v", got, w)
		}
	})

	t.Run("Get", func(t *testing.T) {
		st, want := loaded(t)
		for _, s := range want.Species {
			got, err := st.Get(ctx, s.ID)
			if err != nil {
				t.Errorf("Get(%d): %v", s.ID, err)
				continue
			}
			if msg := diffSpecies(got, s); msg != "" {
				t.Errorf("Get(%d): %s", s.ID, msg)
			}
		}
		if _, err := st.Get(ctx, 42); !errors.Is(err, mdd.ErrNotFound) {
			t.Errorf("Get несуществующего: %v", err)
		}
	})

	t.Run("Find", func(t *testing.T) {
		st, _ := loaded(t)
		cases := map[string]int{
			"Otocolobus manul":         Manul,
			"otocolobus_manul":         Manul,
			"OTOCOLOBUS MANUL":         Manul,
			"Pallas's Cat":             Manul,
			"pallas's cat":             Manul,
			"Manul":                    Manul,
			"steppe cat":               Manul,
			"Ornithorhynchus_anatinus": Platypus,
			"duck-billed platypus":     Platypus,
			"Snow Leopard":             SnowLeopard,
			"panthera uncia":           SnowLeopard,
			"Tasmanian Tiger":          Thylacine,
			"Domestic Cat":             DomesticCat,
		}
		for name, id := range cases {
			got, err := st.Find(ctx, name)
			if err != nil || got.ID != id {
				t.Errorf("Find(%q) = %d, %v; ждали %d", name, got.ID, err, id)
			}
		}
		for _, name := range []string{"Pallas", "Otocolobus", "manul cat", "Манул", ""} {
			if got, err := st.Find(ctx, name); !errors.Is(err, mdd.ErrNotFound) {
				t.Errorf("Find(%q) = %d, %v; ждали ErrNotFound", name, got.ID, err)
			}
		}
		// Найденный вид — полный, а не только ID.
		got, _ := st.Find(ctx, "Manul")
		if msg := diffSpecies(got, byID(Sample(), Manul)); msg != "" {
			t.Errorf("Find(Manul): %s", msg)
		}
	})

	t.Run("Search", func(t *testing.T) {
		st, want := loaded(t)
		yes, no := true, false
		cases := []struct {
			name string
			q    mdd.Query
			ids  []int
		}{
			{"всё", mdd.Query{}, SampleOrder},
			{"text в английском", mdd.Query{Text: "cat"}, []int{DomesticCat, Manul}},
			{"text в латинском с _", mdd.Query{Text: "otocolobus_ma"}, []int{Manul}},
			{"text регистр", mdd.Query{Text: "SNOW"}, []int{SnowLeopard}},
			{"text в прочих названиях", mdd.Query{Text: "tasmanian tiger"}, []int{Thylacine}},
			{"text нет", mdd.Query{Text: "манул"}, nil},
			{"order", mdd.Query{Order: "carnivora"}, []int{GiantPanda, RedFox, DomesticCat, Lynx, Manul, Lion, SnowLeopard}},
			{"family", mdd.Query{Family: "FELIDAE"}, []int{DomesticCat, Lynx, Manul, Lion, SnowLeopard}},
			{"family не подстрока", mdd.Query{Family: "Felid"}, nil},
			{"genus", mdd.Query{Genus: "panthera"}, []int{Lion, SnowLeopard}},
			{"country", mdd.Query{Country: "kazakhstan"}, []int{RedFox, Lynx, Manul, SnowLeopard}},
			{"country под вопросом", mdd.Query{Country: "Azerbaijan"}, []int{RedFox, Lynx, Manul}},
			{"country только под вопросом", mdd.Query{Country: "greece"}, []int{RedFox, Lynx}},
			{"country точно", mdd.Query{Country: "Kazakh"}, nil},
			{"realm", mdd.Query{Realm: "palearctic"}, []int{GiantPanda, RedFox, Lynx, Manul, SnowLeopard}},
			{"realm второй в списке", mdd.Query{Realm: "Indomalaya"}, []int{Lion}},
			{"iucn любой из", mdd.Query{IUCN: []string{"vu", "CR"}}, []int{GiantPanda, Lion, SnowLeopard, BlackRhino}},
			{"extinct", mdd.Query{Extinct: &yes}, []int{Thylacine}},
			{"не extinct", mdd.Query{Extinct: &no}, without(SampleOrder, Thylacine)},
			{"domestic", mdd.Query{Domestic: &yes}, []int{DomesticCat}},
			{"не domestic", mdd.Query{Domestic: &no}, without(SampleOrder, DomesticCat)},
			{"вместе", mdd.Query{Family: "Felidae", IUCN: []string{"VU"}, Country: "India"}, []int{Lion, SnowLeopard}},
			{"вместе пусто", mdd.Query{Genus: "Panthera", Domestic: &yes}, nil},
		}
		for _, c := range cases {
			list, total, err := st.Search(ctx, c.q)
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
				continue
			}
			if got := ids(list); !reflect.DeepEqual(got, orEmpty(c.ids)) || total != len(c.ids) {
				t.Errorf("%s: %v (total %d), ждали %v", c.name, got, total, c.ids)
			}
		}
		// Виды в выдаче — полные.
		list, _, _ := st.Search(ctx, mdd.Query{Genus: "Otocolobus"})
		if len(list) == 1 {
			if msg := diffSpecies(list[0], byID(want, Manul)); msg != "" {
				t.Errorf("вид в выдаче: %s", msg)
			}
		}
	})

	t.Run("Pagination", func(t *testing.T) {
		st, _ := loaded(t)
		n := len(SampleOrder)
		var all []int
		for off := 0; off < n+3; off += 3 {
			list, total, err := st.Search(ctx, mdd.Query{Limit: 3, Offset: off})
			if err != nil {
				t.Fatal(err)
			}
			if total != n {
				t.Errorf("offset %d: total %d, ждали %d", off, total, n)
			}
			wantLen := min(3, max(0, n-off))
			if len(list) != wantLen {
				t.Errorf("offset %d: %d видов, ждали %d", off, len(list), wantLen)
			}
			all = append(all, ids(list)...)
		}
		if !reflect.DeepEqual(all, SampleOrder) {
			t.Errorf("страницы вместе: %v, ждали %v", all, SampleOrder)
		}
		// Limit 0 — по умолчанию 20, то есть весь набор; огромный — не больше 100.
		for _, limit := range []int{0, 1000} {
			list, total, err := st.Search(ctx, mdd.Query{Limit: limit})
			if err != nil || len(list) != n || total != n {
				t.Errorf("limit %d: %d видов, total %d, %v", limit, len(list), total, err)
			}
		}
		// total считается по фильтру, а не по странице.
		list, total, err := st.Search(ctx, mdd.Query{Family: "Felidae", Limit: 2, Offset: 1})
		if err != nil || total != 5 || !reflect.DeepEqual(ids(list), []int{Lynx, Manul}) {
			t.Errorf("фильтр со страницей: %v, total %d, %v", ids(list), total, err)
		}
	})

	t.Run("DefaultLimit", func(t *testing.T) {
		st := newStore(t)
		d := Sample()
		// 130 копий одного вида с разными id и названиями: хватает проверить и 20, и 100.
		base := d.Species[0]
		d.Species = nil
		for i := 0; i < 130; i++ {
			s := base
			s.ID = 2000000 + i
			s.SciName = fmt.Sprintf("Genus species%03d", i)
			s.CommonName, s.OtherCommonNames = "", nil
			d.Species = append(d.Species, s)
		}
		d.Release.Species = len(d.Species)
		if err := st.Replace(ctx, d); err != nil {
			t.Fatal(err)
		}
		for limit, want := range map[int]int{0: 20, 5: 5, 100: 100, 101: 100, 1000: 100} {
			list, total, err := st.Search(ctx, mdd.Query{Limit: limit})
			if err != nil || len(list) != want || total != 130 {
				t.Errorf("limit %d: %d видов, total %d, %v; ждали %d", limit, len(list), total, err, want)
			}
		}
	})

	t.Run("Order", func(t *testing.T) {
		st := newStore(t)
		d := Sample()
		// Порядок входа не должен влиять на выдачу.
		for i, j := 0, len(d.Species)-1; i < j; i, j = i+1, j-1 {
			d.Species[i], d.Species[j] = d.Species[j], d.Species[i]
		}
		if err := st.Replace(ctx, d); err != nil {
			t.Fatal(err)
		}
		list, _, err := st.Search(ctx, mdd.Query{})
		if err != nil || !reflect.DeepEqual(ids(list), SampleOrder) {
			t.Errorf("порядок: %v, ждали %v (%v)", ids(list), SampleOrder, err)
		}
	})

	t.Run("Changes", func(t *testing.T) {
		st, want := loaded(t)
		all, err := st.Changes(ctx, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		if !sameChanges(all, want.Changes) {
			t.Errorf("все изменения:\n got %+v\nwant %+v", all, want.Changes)
		}
		novo, err := st.Changes(ctx, "de novo", 100)
		if err != nil || len(novo) != 2 {
			t.Fatalf("de novo: %+v, %v", novo, err)
		}
		for _, c := range novo {
			if c.Category != "de novo" || c.OldName != "" || c.NewName == "" {
				t.Errorf("de novo: %+v", c)
			}
		}
		if one, err := st.Changes(ctx, "", 1); err != nil || len(one) != 1 {
			t.Errorf("limit 1: %+v, %v", one, err)
		}
		if two, err := st.Changes(ctx, "de novo", 1); err != nil || len(two) != 1 || two[0].Category != "de novo" {
			t.Errorf("категория с limit 1: %+v, %v", two, err)
		}
		if none, err := st.Changes(ctx, "no such category", 10); err != nil || len(none) != 0 {
			t.Errorf("нет категории: %+v, %v", none, err)
		}
	})

	t.Run("IDs", func(t *testing.T) {
		st, _ := loaded(t)
		got, err := st.IDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sort.Ints(got)
		want := append([]int(nil), SampleOrder...)
		sort.Ints(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("IDs: %v, ждали %v", got, want)
		}
	})

	t.Run("ReplaceAgain", func(t *testing.T) {
		st, _ := loaded(t)
		d := Sample()
		d.Release.Version, d.Release.PrevVersion, d.Release.Date = "v2.6", "v2.5", "2027-01-15"
		var keep []mdd.Species
		for _, s := range d.Species {
			if s.Family == "Felidae" {
				keep = append(keep, s)
			}
		}
		d.Species = keep
		d.Release.Species = len(keep)
		d.Changes = d.Changes[3:]
		if err := st.Replace(ctx, d); err != nil {
			t.Fatal(err)
		}
		rel, err := st.Release(ctx)
		if err != nil || rel.Version != "v2.6" || rel.PrevVersion != "v2.5" || rel.Species != len(keep) {
			t.Errorf("релиз после замены: %+v, %v", rel, err)
		}
		if _, err := st.Get(ctx, Platypus); !errors.Is(err, mdd.ErrNotFound) {
			t.Errorf("старый вид по id: %v", err)
		}
		if _, err := st.Find(ctx, "Platypus"); !errors.Is(err, mdd.ErrNotFound) {
			t.Errorf("старый вид по названию: %v", err)
		}
		list, total, err := st.Search(ctx, mdd.Query{})
		if err != nil || total != len(keep) || !reflect.DeepEqual(ids(list), []int{DomesticCat, Lynx, Manul, Lion, SnowLeopard}) {
			t.Errorf("поиск после замены: %v, total %d, %v", ids(list), total, err)
		}
		if got, err := st.IDs(ctx); err != nil || len(got) != len(keep) {
			t.Errorf("IDs после замены: %v, %v", got, err)
		}
		if ch, err := st.Changes(ctx, "", 100); err != nil || !sameChanges(ch, d.Changes) {
			t.Errorf("изменения после замены: %+v, %v", ch, err)
		}
	})

	t.Run("ReplaceEmpty", func(t *testing.T) {
		st, _ := loaded(t)
		empty := Sample()
		empty.Species = nil
		for _, d := range []*mdd.Dataset{nil, empty} {
			if err := st.Replace(ctx, d); err == nil {
				t.Errorf("Replace пустого набора прошёл без ошибки")
			}
		}
		if _, err := st.Get(ctx, Manul); err != nil {
			t.Errorf("после отвергнутого Replace справочник пропал: %v", err)
		}
	})

	t.Run("ReplaceCopies", func(t *testing.T) {
		st := newStore(t)
		d := Sample()
		if err := st.Replace(ctx, d); err != nil {
			t.Fatal(err)
		}
		// Вызывающий портит свой набор после Replace — хранилище не меняется.
		for i := range d.Species {
			d.Species[i].SciName = "Broken name"
			if len(d.Species[i].Countries) > 0 {
				d.Species[i].Countries[0] = "Nowhere"
			}
			if len(d.Species[i].OtherCommonNames) > 0 {
				d.Species[i].OtherCommonNames[0] = "Broken"
			}
		}
		d.Changes[0].NewName = "Broken"
		d.Release.Version = "broken"
		got, err := st.Get(ctx, Manul)
		if err != nil {
			t.Fatal(err)
		}
		if msg := diffSpecies(got, byID(Sample(), Manul)); msg != "" {
			t.Errorf("хранилище изменилось вслед за набором: %s", msg)
		}
		// Получатель портит выданный вид — следующий Get его не видит.
		got.Countries[0] = "Nowhere"
		got.OtherCommonNames[0] = "Broken"
		again, _ := st.Get(ctx, Manul)
		if msg := diffSpecies(again, byID(Sample(), Manul)); msg != "" {
			t.Errorf("хранилище изменилось вслед за выданным видом: %s", msg)
		}
		if rel, _ := st.Release(ctx); rel.Version != "v2.5" {
			t.Errorf("релиз изменился вслед за набором: %q", rel.Version)
		}
		if ch, _ := st.Changes(ctx, "", 100); !sameChanges(ch, Sample().Changes) {
			t.Errorf("изменения испорчены: %+v", ch)
		}
	})
}

func byID(d *mdd.Dataset, id int) mdd.Species {
	for _, s := range d.Species {
		if s.ID == id {
			return s
		}
	}
	panic(fmt.Sprintf("вида %d в наборе нет", id))
}

func ids(list []mdd.Species) []int {
	out := []int{}
	for _, s := range list {
		out = append(out, s.ID)
	}
	return out
}

func orEmpty(v []int) []int {
	if v == nil {
		return []int{}
	}
	return v
}

func without(list []int, id int) []int {
	out := []int{}
	for _, v := range list {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

// diffSpecies сравнивает виды, не различая nil и пустой срез: хранилище
// вправе вернуть любой из них.
func diffSpecies(got, want mdd.Species) string {
	g, w := normSpecies(got), normSpecies(want)
	if reflect.DeepEqual(g, w) {
		return ""
	}
	return fmt.Sprintf("\n got %+v\nwant %+v", g, w)
}

func normSpecies(s mdd.Species) mdd.Species {
	for _, p := range []*[]string{&s.OtherCommonNames, &s.Countries, &s.CountriesUncertain, &s.Continents, &s.Realms} {
		if len(*p) == 0 {
			*p = nil
		}
	}
	return s
}

// sameChanges — те же изменения без учёта порядка: порядок контракт не
// задаёт.
func sameChanges(got, want []mdd.Change) bool {
	key := func(c mdd.Change) string { return fmt.Sprintf("%q", c) }
	a, b := make([]string, len(got)), make([]string, len(want))
	for i, c := range got {
		a[i] = key(c)
	}
	for i, c := range want {
		b[i] = key(c)
	}
	sort.Strings(a)
	sort.Strings(b)
	return reflect.DeepEqual(a, b)
}
