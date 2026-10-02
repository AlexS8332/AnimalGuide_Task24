package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

func TestCardSetDeleteAndVersion(t *testing.T) {
	c := NewCard(LayerLong, "me", "Я")
	if _, ok := c.Set("Интерес", "совы", 1, SourceExtract); !ok {
		t.Fatal("новая запись")
	}
	if old, ok := c.Set("интерес ", "совы", 2, SourceExtract); ok || old != "совы" {
		t.Fatal("то же значение — не правка")
	}
	if old, ok := c.Set("интерес", "хищные птицы", 3, SourceExtract); !ok || old != "совы" {
		t.Fatal("замещение")
	}
	if v, _ := c.Get("ИНТЕРЕС"); v != "хищные птицы" || c.Version != 2 || c.Entries[0].Since != 1 {
		t.Fatalf("карточка: %+v", c)
	}
	if _, ok := c.Set(" ", "x", 1, ""); ok {
		t.Fatal("пустой ключ")
	}
	if old, ok := c.Delete("интерес"); !ok || old != "хищные птицы" || !c.Empty() {
		t.Fatal("удаление")
	}
	if _, ok := c.Delete("нет"); ok {
		t.Fatal("удаление отсутствующего")
	}
}

func TestAddToListKeepsOrderAndCap(t *testing.T) {
	c := NewCard(LayerLong, "me", "")
	c.AddToList(KeyRead, "Рысь", 1)
	c.AddToList(KeyRead, "Манул", 2)
	if c.AddToList(KeyRead, "манул", 3) {
		// Повтор не добавляет, но переносит в конец только если порядок изменился.
	}
	if v, _ := c.Get(KeyRead); v != "Рысь; манул" && v != "Рысь; Манул" {
		t.Fatalf("список: %q", v)
	}
	for i := range maxListItems + 5 {
		c.AddToList(KeyRead, strings.Repeat("я", 3)+string(rune('а'+i%30))+string(rune('0'+i/30)), i)
	}
	v, _ := c.Get(KeyRead)
	if n := len(strings.Split(v, "; ")); n != maxListItems {
		t.Fatalf("потолок списка: %d", n)
	}
	if c.Entries[0].Source != SourceCode {
		t.Fatal("список ведёт код")
	}
	if c.AddToList(KeyRead, " ", 1) {
		t.Fatal("пустое название")
	}
}

func reserved(key string) string {
	switch key {
	case "длина", "обращение":
		return "профиль"
	case "латынь", "ареал":
		return "карточка животного"
	case KeyRead, KeyBookmarks:
		return "код"
	}
	return ""
}

func TestApplyRoutesAndGuards(t *testing.T) {
	long := NewCard(LayerLong, "me", "")
	work := NewCard(LayerWork, "c1", "хищники тайги")
	work.Set("регион", "тайга", 1, SourceExtract)
	patch := Patch{
		Set: []Op{
			{Layer: LayerLong, Key: "Интерес", Value: "хищные птицы"},
			{Layer: LayerLong, Key: "регион", Value: "тайга Сибири"}, // был в рабочей — переезд
			{Layer: LayerLong, Key: "длина", Value: "коротко"},       // профиль
			{Layer: LayerWork, Key: "латынь", Value: "Lynx lynx"},    // карточка
			{Layer: LayerLong, Key: KeyRead, Value: "всё"},           // код
			{Layer: "short", Key: "x", Value: "y"},
			{Layer: LayerWork, Key: "", Value: "y"},
		},
		Delete: []Op{{Layer: LayerLong, Key: "длина"}},
	}
	ch := Apply(&long, &work, patch, Rules{UseLong: true, UseWork: true, Reserved: reserved}, 5)
	if v, _ := long.Get("интерес"); v != "хищные птицы" {
		t.Fatal("запись в долговременную")
	}
	if v, _ := long.Get("регион"); v != "тайга Сибири" || work.Empty() == false {
		t.Fatalf("переезд: long %v, work %v", long.Entries, work.Entries)
	}
	var moves, skips int
	for _, c := range ch {
		switch c.Op {
		case OpMove:
			moves++
		case OpSkip:
			skips++
			if c.Reason == "" {
				t.Error("отклонение без причины")
			}
		}
	}
	if moves != 1 || skips != 4 {
		t.Fatalf("правки: %+v", ch)
	}
	if !strings.Contains(ch.Summary(), "перенесено") || len(ch.Applied()) != 2 {
		t.Fatalf("сводка: %s", ch.Summary())
	}
}

// Выключенный слой — не молчаливая дыра: сведение оседает в включённом
// слое, а правка говорит почему (ФТ-48).
func TestApplyDisabledLayerFallsBack(t *testing.T) {
	work := NewCard(LayerWork, "c1", "")
	ch := Apply(nil, &work, Patch{Set: []Op{{Layer: LayerLong, Key: "имя собаки", Value: "Бим"}}}, Rules{UseWork: true}, 1)
	if v, _ := work.Get("имя собаки"); v != "Бим" {
		t.Fatalf("сведение пропало: %+v", ch)
	}
	if len(ch) != 2 || ch[0].Op != OpSkip || !strings.Contains(ch[0].Reason, "осела в слое «рабочая»") || ch[1].Op != OpSet {
		t.Fatalf("правка: %+v", ch)
	}
	ch = Apply(nil, nil, Patch{Set: []Op{{Layer: LayerLong, Key: "a", Value: "b"}}}, Rules{}, 1)
	if len(ch) != 1 || ch[0].Op != OpSkip {
		t.Fatalf("оба слоя выключены: %+v", ch)
	}
}

func TestApplyDeleteAndTrim(t *testing.T) {
	long := NewCard(LayerLong, "me", "")
	for i := range 5 {
		long.Set(string(rune('а'+i)), "v", i+1, SourceExtract)
	}
	long.AddToList(KeyRead, "Рысь", 0)
	ch := Apply(&long, nil, Patch{Delete: []Op{{Key: "а"}, {Key: KeyRead}}}, Rules{UseLong: true, Reserved: reserved, MaxLong: 3}, 9)
	if _, ok := long.Get("а"); ok {
		t.Fatal("удаление")
	}
	if _, ok := long.Get(KeyRead); !ok {
		t.Fatal("список кода удалён извлекателем")
	}
	if len(long.Entries) != 3 {
		t.Fatalf("потолок: %v", long.SortedKeys())
	}
	trimmed := 0
	for _, c := range ch {
		if strings.Contains(c.Reason, "потолку") {
			trimmed++
		}
	}
	if trimmed != 2 {
		t.Fatalf("вытеснено: %+v", ch)
	}
}

func TestPromptsExplainThemselves(t *testing.T) {
	long := NewCard(LayerLong, "me", "")
	work := NewCard(LayerWork, "c", "совы")
	if long.Prompt() != "" || work.Prompt() != "" {
		t.Fatal("пустой слой даёт блок")
	}
	long.Set("интерес", "совы", 1, "")
	work.Set("собрано", "неясыть", 1, "")
	lp, wp := long.Prompt(), work.Prompt()
	if !strings.Contains(lp, "между разговорами") || !strings.Contains(wp, "по текущей подборке") || !strings.Contains(wp, "«совы»") {
		t.Fatalf("блоки:\n%s\n%s", lp, wp)
	}
	if lp[:40] == wp[:40] {
		t.Fatal("одинаковое пояснение на два блока (П-3)")
	}
}

func TestStoreUpdateWritesOnlyChanged(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(store.NewDir(dir))
	long, work, err := s.Update("me", "Я", "c1", "совы", func(l, w *Card) error {
		l.Set("интерес", "совы", 1, SourceExtract)
		return nil
	})
	if err != nil || long.Version != 1 || work.Version != 0 {
		t.Fatalf("%+v %+v %v", long, work, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "memory", "work", "c1.json")); !os.IsNotExist(err) {
		t.Fatal("неизменённый слой записан")
	}
	got, _ := s.Card(LayerLong, "me", "")
	if v, _ := got.Get("интерес"); v != "совы" || got.Title != "Я" {
		t.Fatalf("прочитано: %+v", got)
	}
	// Нет собеседника и подборки — слоёв нет.
	_, _, err = s.Update("", "", "", "", func(l, w *Card) error {
		if l != nil || w != nil {
			t.Fatal("слои без владельца")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Update("../x", "", "", "", func(*Card, *Card) error { return nil }); err == nil {
		t.Fatal("плохой идентификатор")
	}
}

func TestStoreManualEditsAndList(t *testing.T) {
	s := NewStore(store.NewDir(t.TempDir()))
	if _, ch, err := s.Put(LayerWork, "c1", "совы", "Собрано", "неясыть", 2); err != nil || ch.Reason == "" {
		t.Fatalf("Put: %v", err)
	}
	if _, _, err := s.Put(LayerWork, "c1", "", "собрано", "неясыть", 3); err == nil {
		t.Fatal("правка без изменения")
	}
	if _, _, err := s.Forget(LayerWork, "c1", "нет"); err == nil {
		t.Fatal("удаление отсутствующего")
	}
	if _, ch, err := s.Forget(LayerWork, "c1", "собрано"); err != nil || ch.Op != OpDelete {
		t.Fatalf("Forget: %v", err)
	}
	if c, added, err := s.AddToList("me", "", KeyBookmarks, "Манул", 1); err != nil || !added || c.Empty() {
		t.Fatalf("закладка: %v", err)
	}
	cards, problems := s.List(LayerWork)
	if len(cards) != 1 || len(problems) != 0 {
		t.Fatalf("List: %d %v", len(cards), problems)
	}
	if _, problems := s.List("short"); len(problems) != 1 {
		t.Fatal("неизвестный слой")
	}
	path, raw, err := s.Raw(LayerLong, "me")
	if err != nil || !strings.Contains(path, "long") || !strings.Contains(string(raw), `"schema": 1`) {
		t.Fatalf("Raw: %s %v", path, err)
	}
	if s.DisplayPath("x", "y") != "" || s.DisplayPath(LayerLong, "me") == "" {
		t.Fatal("DisplayPath")
	}
	if _, _, err := s.Raw("x", "y"); err == nil {
		t.Fatal("Raw неизвестного слоя")
	}
	if _, err := s.Card("x", "y", ""); err == nil {
		t.Fatal("Card неизвестного слоя")
	}
}

// Памятники слоёв прошлых упражнений читаются без потерь (ФТ-52).
func TestLegacyLayers(t *testing.T) {
	for _, name := range []string{"v1-long-31e800decb254c82-all.json", "v1-work-4fff0f9a160e32e7.json"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "legacy", "memory", name))
		if err != nil {
			t.Fatal(err)
		}
		var raw struct {
			Layer   string
			Entries []struct{ Key, Value string }
		}
		json.Unmarshal(data, &raw)
		k, _ := KindOf(raw.Layer)
		var c Card
		meta, err := store.Decode(k, data, &c)
		if err != nil || meta.Schema != 1 || meta.Migrated {
			t.Fatalf("%s: %+v %v", name, meta, err)
		}
		if len(c.Entries) != len(raw.Entries) || len(c.Entries) == 0 {
			t.Fatalf("%s: записей %d из %d", name, len(c.Entries), len(raw.Entries))
		}
		for i, e := range raw.Entries {
			if c.Entries[i].Key != e.Key || c.Entries[i].Value != e.Value {
				t.Fatalf("%s: запись %d изменилась", name, i)
			}
		}
	}
}

func TestTitles(t *testing.T) {
	if Title(LayerWork) != "рабочая" || Title("x") != "x" || TitleIn(LayerLong) != "в долговременной" || TitleIn("x") != "в слое x" {
		t.Fatal("названия слоёв")
	}
	if !EqualKey("Ёж", "еж") || clip("абв", 2) != "аб…" {
		t.Fatal("мелочи")
	}
	c := NewCard(LayerLong, "me", "")
	if c.Render() != "(пусто)" || c.Runes() != 0 {
		t.Fatal("пустой слой")
	}
	c.Set("a", "b", 1, "")
	if c.Clone().Entries[0].Key != "a" || c.Runes() != 2 {
		t.Fatal("клон")
	}
	if (Changes{}).Summary() != "без правок" {
		t.Fatal("пустая сводка")
	}
	s := Changes{{Op: OpDelete, Layer: LayerLong, Key: "a"}, {Op: OpSkip, Key: "b"}}.Summary()
	if !strings.Contains(s, "удалено") || !strings.Contains(s, "отклонено") {
		t.Fatal(s)
	}
	if _, err := KindOf("short"); err == nil {
		t.Fatal("KindOf")
	}
}
