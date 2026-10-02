package features

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogIsConsistent(t *testing.T) {
	r := Catalog()
	if len(r.All()) < 10 {
		t.Fatalf("в реестре %d механизмов", len(r.All()))
	}
	if err := r.Validate(r.Defaults()); err != nil {
		t.Fatalf("умолчания не согласованы: %v", err)
	}
	for _, m := range r.All() {
		if m.Title == "" || m.About == "" || m.Fallback == "" || m.Since == "" {
			t.Errorf("%s: у механизма не заполнено описание (Title/About/Fallback/Since)", m.Name)
		}
	}
}

// Порядок блоков — ФТ-34: свод → профиль → долговременная → состояние →
// рабочая → карточка фактов.
func TestCatalogBlockOrderFollowsStability(t *testing.T) {
	r := Catalog()
	want := []Name{Charter, Profile, MemoryLong, CollectionState, MemoryWork, Facts}
	var blocks []Block
	for i := len(want) - 1; i >= 0; i-- { // нарочно в обратном порядке
		blocks = append(blocks, Block{Feature: want[i], Text: string(want[i])})
	}
	got := r.Order(blocks, r.AllOn())
	if len(got) != len(want) {
		t.Fatalf("блоков %d, ждали %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Feature != want[i] {
			t.Fatalf("место %d: %s, ждали %s", i, got[i].Feature, want[i])
		}
	}
}

func TestOrderDropsDisabledEmptyAndUnknown(t *testing.T) {
	r := Catalog()
	s := r.AllOn().With(Profile, false)
	got := r.Order([]Block{
		{Feature: Profile, Text: "профиль"},
		{Feature: Charter, Text: "   "},
		{Feature: "nope", Text: "x"},
		{Feature: Extract, Text: "не блок"},
		{Feature: MemoryLong, Text: "память"},
	}, s)
	if len(got) != 1 || got[0].Feature != MemoryLong {
		t.Fatalf("остались %+v", got)
	}
}

func TestNewRejectsBrokenRegistries(t *testing.T) {
	cases := map[string][]Mechanism{
		"без имени":         {{Title: "x"}},
		"дубль":             {{Name: "a"}, {Name: "a"}},
		"блок без места":    {{Name: "a", Kind: KindBlock}},
		"место занято":      {{Name: "a", Kind: KindBlock, Place: 100}, {Name: "b", Kind: KindBlock, Place: 100}},
		"неизвестная связь": {{Name: "a", Requires: []Name{"b"}}},
	}
	for name, ms := range cases {
		if _, err := New(ms...); err == nil {
			t.Errorf("%s: реестр принят", name)
		}
	}
}

func TestDefaultsAreFullMap(t *testing.T) {
	r := Catalog()
	d := r.Defaults()
	for _, m := range r.All() {
		if !d.Known(m.Name) {
			t.Fatalf("%s нет в наборе по умолчанию", m.Name)
		}
		if d.On(m.Name) != m.Default {
			t.Fatalf("%s: %v, ждали %v", m.Name, d.On(m.Name), m.Default)
		}
	}
}

func TestParse(t *testing.T) {
	r := Catalog()
	s, err := r.Parse("-guard, +profile", r.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if s.On(Guard) || !s.On(Profile) {
		t.Fatalf("набор %v", s)
	}
	s, err = r.Parse("none,charter", r.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Names(); len(got) != 1 || got[0] != Charter {
		t.Fatalf("none,charter → %v", got)
	}
	s, _ = r.Parse("all", Set{})
	if len(s.Names()) != len(r.All()) {
		t.Fatalf("all → %v", s.Names())
	}
	s, _ = r.Parse("none,default", Set{})
	if s.String() != r.Defaults().String() {
		t.Fatalf("default → %v", s)
	}
	if _, err := r.Parse("+gaurd", r.Defaults()); err == nil || !strings.Contains(err.Error(), "неизвестный механизм") {
		t.Fatalf("опечатка принята: %v", err)
	}
	// Пустая строка — набор не меняется, но дописывается до полной карты.
	s, _ = r.Parse("", NewSet(map[Name]bool{Charter: true}))
	if !s.On(Charter) || !s.Known(Guard) || s.On(Guard) {
		t.Fatalf("пустая строка: %v", s.Map())
	}
}

func TestValidateRequirements(t *testing.T) {
	r := Catalog()
	s := r.Defaults().With(Charter, false)
	err := r.Validate(s)
	if err == nil || !strings.Contains(err.Error(), "guard требует charter") {
		t.Fatalf("страж без свода принят: %v", err)
	}
}

func TestDiffCountsOnlyRegistry(t *testing.T) {
	r := Catalog()
	a := r.Defaults()
	b := a.With(Guard, false).With("unknown-future", true)
	d := r.Diff(a, b)
	if len(d) != 1 || d[0] != Guard {
		t.Fatalf("Diff = %v", d)
	}
	if len(r.Diff(a, a)) != 0 {
		t.Fatal("разница набора с самим собой")
	}
}

func TestSetJSONKeepsUnknownAndSortsKeys(t *testing.T) {
	var s Set
	if err := json.Unmarshal([]byte(`{"zeta":true,"charter":false,"future.x":true}`), &s); err != nil {
		t.Fatal(err)
	}
	if !s.On("future.x") || s.On(Charter) || !s.Known(Charter) {
		t.Fatalf("прочитано %v", s.Map())
	}
	data, _ := json.Marshal(s)
	if string(data) != `{"charter":false,"future.x":true,"zeta":true}` {
		t.Fatalf("записано %s", data)
	}
	var empty Set
	data, _ = json.Marshal(empty)
	if string(data) != "{}" || !empty.Empty() {
		t.Fatalf("пустой набор: %s", data)
	}
	if err := json.Unmarshal([]byte(`[1]`), &s); err == nil {
		t.Fatal("массив принят за набор")
	}
}

func TestWithDoesNotMutate(t *testing.T) {
	a := NewSet(map[Name]bool{Charter: true})
	b := a.With(Charter, false)
	if !a.On(Charter) || b.On(Charter) {
		t.Fatal("With изменил исходный набор")
	}
	m := b.Map()
	m[Charter] = true
	if b.On(Charter) {
		t.Fatal("Map отдал внутреннюю карту")
	}
}

func TestComplete(t *testing.T) {
	r := Catalog()
	s := r.Complete(NewSet(map[Name]bool{Guard: true}))
	if !s.On(Guard) || !s.Known(Charter) || s.On(Charter) {
		t.Fatalf("Complete: %v", s.Map())
	}
}

func TestDescribeAndLabels(t *testing.T) {
	r := Catalog()
	st := r.Describe(r.Defaults().With(Guard, false))
	found := false
	for _, x := range st {
		if x.Name == Guard {
			found = true
			if x.On {
				t.Fatal("страж отмечен включённым")
			}
		}
	}
	if !found {
		t.Fatal("страж не описан")
	}
	if got := (KindBlock | KindCall).Labels(); strings.Join(got, ",") != "блок,вызов модели" {
		t.Fatalf("Labels = %v", got)
	}
	data, _ := json.Marshal(KindCheck)
	if string(data) != `["проверка"]` {
		t.Fatalf("Kind в JSON: %s", data)
	}
	if r2, _ := r.Get(Charter); r2.Place != PlaceCharter {
		t.Fatal("Get")
	}
}

// Цены блоков в реестре — измерения И-6, а не оценки: у каждого механизма с
// блоком цена есть, и сумма блоков оставляет место под системный промпт,
// описания инструментов и окно в пределах постоянной части (6 тыс.,
// раздел 10 ТЗ).
func TestCatalogBlockCostsMeasured(t *testing.T) {
	sum := 0
	for _, m := range Catalog().All() {
		if !m.Kind.Has(KindBlock) {
			continue
		}
		if m.Cost.Tokens <= 0 {
			t.Errorf("%s: блок без цены", m.Name)
		}
		sum += m.Cost.Tokens
	}
	if sum > 3000 {
		t.Fatalf("блоки по реестру ≈%d токенов: постоянной части в 6 тыс. не хватит на промпт и инструменты", sum)
	}
}

// MCP — транспорт: нового блока нет, модель ничего не платит, по
// умолчанию выключен, пока не прошёл своё испытание.
func TestMCPIsTransport(t *testing.T) {
	m, ok := Catalog().Get(MCP)
	if !ok || m.Default || m.Place != PlaceNone || !m.Kind.Has(KindTransport) || m.Kind.Has(KindBlock) {
		t.Fatalf("mcp: %+v", m)
	}
	if m.Cost.Tokens != 0 || m.Cost.Requests != 0 || m.Cost.Churn != ChurnNone || m.Cost.Note == "" || m.Fallback == "" {
		t.Fatalf("цена mcp: %+v", m.Cost)
	}
	if Catalog().Defaults().On(MCP) {
		t.Fatal("mcp включён у нового диалога")
	}
}
