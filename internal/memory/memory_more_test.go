package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

// Ключи слоя по алфавиту — для отчёта: порядок записей в файле не важен.
func TestSortedKeys(t *testing.T) {
	c := NewCard(LayerLong, "me", "")
	for _, k := range []string{"интересы", "город", "закладки"} {
		c.Set(k, "x", 1, SourceHuman)
	}
	got := c.SortedKeys()
	if strings.Join(got, ",") != "город,закладки,интересы" {
		t.Fatalf("ключи: %v", got)
	}
	if len(NewCard(LayerWork, "c", "").SortedKeys()) != 0 {
		t.Fatal("пустой слой")
	}
}

// Ключи сравниваются без регистра, краевых пробелов и «ё»; значение режется
// по потолку с многоточием.
func TestKeysAndClip(t *testing.T) {
	if !EqualKey(" Ещё ", "еще") || EqualKey("ёж", "уж") {
		t.Fatal("EqualKey")
	}
	c := NewCard(LayerLong, "me", "")
	if _, changed := c.Set("  Любимое   животное ", strings.Repeat("я", DefaultMaxValueRunes+10), 1, SourceHuman); !changed {
		t.Fatal("запись")
	}
	e := c.Entries[0]
	if e.Key != "любимое животное" || len([]rune(e.Value)) != DefaultMaxValueRunes+1 || !strings.HasSuffix(e.Value, "…") {
		t.Fatalf("ключ %q, длина %d", e.Key, len([]rune(e.Value)))
	}
	if _, changed := c.Set("", "x", 1, SourceHuman); changed {
		t.Fatal("пустой ключ")
	}
	if _, changed := c.Set("x", "   ", 1, SourceHuman); changed {
		t.Fatal("пустое значение")
	}
	if c.Runes() != len([]rune(e.Key))+len([]rune(e.Value)) {
		t.Fatal("Runes")
	}
}

// Списочная запись не растёт без предела по длине: старые названия уходят.
func TestAddToListRuneCap(t *testing.T) {
	c := NewCard(LayerLong, "me", "")
	long := strings.Repeat("д", 100)
	for i := 0; i < 20; i++ {
		c.AddToList(KeyBookmarks, long+string(rune('а'+i)), i)
	}
	v, _ := c.Get(KeyBookmarks)
	if n := len([]rune(v)); n > DefaultMaxValueRunes*4 {
		t.Fatalf("список длиной %d символов", n)
	}
	if !strings.HasSuffix(v, long+string(rune('а'+19))) {
		t.Fatal("последнее название выпало")
	}
	// То же название последним — не правка.
	ver := c.Version
	if c.AddToList(KeyBookmarks, long+string(rune('а'+19)), 30) || c.Version != ver {
		t.Fatal("повтор последнего названия засчитан правкой")
	}
}

// Правки хода одной строкой для журнала, по видам.
func TestChangesSummary(t *testing.T) {
	cs := Changes{
		{Op: OpSet, Layer: LayerLong, Key: "интересы"},
		{Op: OpDelete, Layer: LayerWork, Key: "собрано"},
		{Op: OpMove, Layer: LayerLong, Key: "город"},
		{Op: OpSkip, Layer: LayerLong, Key: "закладки"},
	}
	want := "в долговременной «интересы», удалено «собрано» в рабочей, «город» перенесено в долговременной, отклонено «закладки»"
	if got := cs.Summary(); got != want {
		t.Fatalf("сводка:\n%q\nждали\n%q", got, want)
	}
	if got := cs.Applied(); len(got) != 3 {
		t.Fatalf("применённые: %+v", got)
	}
	if (Changes{}).Summary() != "без правок" {
		t.Fatal("пустая сводка")
	}
	if Title("x") != "x" || TitleIn("x") != "в слое x" {
		t.Fatal("незнакомый слой словами")
	}
}

// Слой без пояснения (незнакомый) отдаётся как есть; пустой — пустой строкой.
func TestPromptUnknownLayerAndEmpty(t *testing.T) {
	c := NewCard("short", "x", "")
	if c.Prompt() != "" {
		t.Fatal("пустой слой в запросе")
	}
	c.Set("ключ", "значение", 1, SourceHuman)
	if got := c.Prompt(); got != c.Render() || !strings.Contains(got, "ключ: значение") {
		t.Fatalf("незнакомый слой: %q", got)
	}
	w := NewCard(LayerWork, "c1", "")
	w.Set("собрано", "неясыть", 1, SourceHuman)
	if strings.Contains(w.Prompt(), "«") {
		t.Fatalf("рабочая память без названия подборки: %q", w.Prompt())
	}
}

// Ошибка правки — ничего не пишется ни в один слой.
func TestUpdateErrorWritesNothing(t *testing.T) {
	s := NewStore(store.NewDir(t.TempDir()))
	boom := errors.New("извлекатель ошибся")
	_, _, err := s.Update("me", "", "abcdef12", "совы", func(long, work *Card) error {
		long.Set("интересы", "совы", 1, SourceExtract)
		work.Set("собрано", "сипуха", 1, SourceExtract)
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("ошибка: %v", err)
	}
	if cards, _ := s.List(LayerLong); len(cards) != 0 {
		t.Fatal("записана долговременная")
	}
	if cards, _ := s.List(LayerWork); len(cards) != 0 {
		t.Fatal("записана рабочая")
	}
	// Без собеседника и подборки слоёв нет: fn получает nil.
	_, _, err = s.Update("", "", "", "", func(long, work *Card) error {
		if long != nil || work != nil {
			return errors.New("слой без владельца")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Битый файл слоя — ошибка чтения и правки, файл не затирается.
func TestBrokenLayerFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(store.NewDir(dir))
	path := filepath.Join(dir, "memory", "work", "c1.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{битый"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Card(LayerWork, "c1", ""); err == nil {
		t.Fatal("битый слой прочитан")
	}
	if _, _, err := s.Update("me", "", "c1", "", func(*Card, *Card) error { return nil }); err == nil {
		t.Fatal("правка поверх битой рабочей памяти")
	}
	if _, _, err := s.Put(LayerWork, "c1", "", "k", "v", 1); err == nil {
		t.Fatal("правка руками поверх битого файла")
	}
	if cards, problems := s.List(LayerWork); len(cards) != 0 || len(problems) != 1 {
		t.Fatalf("список: %d, проблем %d", len(cards), len(problems))
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{битый" {
		t.Fatal("битый файл затёрт")
	}
	// Битая долговременная память тоже останавливает правку.
	lpath := filepath.Join(dir, "memory", "long", "me.json")
	os.MkdirAll(filepath.Dir(lpath), 0o755)
	os.WriteFile(lpath, []byte("[]"), 0o600)
	if _, _, err := s.Update("me", "", "", "", func(*Card, *Card) error { return nil }); err == nil {
		t.Fatal("правка поверх битой долговременной памяти")
	}
}

// Правка руками в неизвестном слое — ошибка, а не файл в чужом каталоге.
func TestManualEditUnknownLayer(t *testing.T) {
	s := NewStore(store.NewDir(t.TempDir()))
	if _, _, err := s.Put("short", "me", "", "k", "v", 1); err == nil {
		t.Fatal("правка в неизвестном слое")
	}
	if _, _, err := s.Forget("short", "me", "k"); err == nil {
		t.Fatal("удаление в неизвестном слое")
	}
	if _, _, err := s.Put(LayerLong, "../me", "", "k", "v", 1); err == nil {
		t.Fatal("опасный идентификатор")
	}
	// Повторная закладка того же названия — не правка и не запись.
	s.AddToList("me", "", KeyBookmarks, "Манул", 1)
	if _, added, err := s.AddToList("me", "", KeyBookmarks, "Манул", 2); err != nil || added {
		t.Fatalf("повтор закладки: %v %v", added, err)
	}
}
