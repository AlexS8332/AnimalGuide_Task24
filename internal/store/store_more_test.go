package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Вид без номера формата считается версией 1: запись ставит schema 1.
func TestKindWithoutCurrentIsVersionOne(t *testing.T) {
	d := NewDir(t.TempDir())
	k := &Kind{Name: "x", Dir: "x"}
	if err := d.Write(k, "a", note{Title: "t"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := d.Raw(k, "a")
	if s, err := SchemaOf(raw); err != nil || s != 1 {
		t.Fatalf("формат: %d %v", s, err)
	}
	var got note
	if meta, err := d.Read(k, "a", &got); err != nil || meta.Migrated || got.Title != "t" {
		t.Fatalf("чтение: %+v %v", meta, err)
	}
}

// Каталог данных занят файлом — запись возвращает ошибку, а не падает.
func TestWriteWhenRootIsFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(root, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := NewDir(root)
	err := d.Write(noteKind(), "a", note{Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "каталог") {
		t.Fatalf("запись в файл вместо каталога: %v", err)
	}
	// Список вида поверх файла — пусто, а не паника (на Windows такой путь
	// читается как несуществующий, на Unix — как ошибка каталога).
	if keys, _ := d.List(noteKind()); len(keys) != 0 {
		t.Fatalf("список: %v", keys)
	}
}

// Файл данных не заменяется (на его месте каталог) — ошибка, временный
// файл за собой не остаётся.
func TestWriteRenameFailureCleansTmp(t *testing.T) {
	d := NewDir(t.TempDir())
	k := noteKind()
	path := d.Path(k, "a")
	if err := os.MkdirAll(filepath.Join(path, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.Write(k, "a", note{Title: "t"}); err == nil {
		t.Fatal("запись поверх каталога прошла")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("временный файл остался: %v", err)
	}
	// Удалить непустой каталог нельзя — ошибка доходит.
	if err := d.Delete(k, "a"); err == nil {
		t.Fatal("удаление непустого каталога прошло молча")
	}
}

// Значение, которое не сериализуется, не пишется, и файл не появляется.
func TestWriteUnmarshalable(t *testing.T) {
	d := NewDir(t.TempDir())
	k := noteKind()
	if err := d.Write(k, "a", map[string]any{"f": func() {}}); err == nil {
		t.Fatal("функция сериализована")
	}
	if d.Has(k, "a") {
		t.Fatal("файл появился после неудачной записи")
	}
	if err := Set(map[string]json.RawMessage{}, "x", make(chan int)); err == nil {
		t.Fatal("Set: канал сериализован")
	}
}

// Файл того же формата при перезаписи копии не получает: копия — только
// перед первой сменой формата.
func TestSameFormatWriteMakesNoBackup(t *testing.T) {
	dir := t.TempDir()
	d := NewDir(dir)
	k := noteKind()
	for i := 0; i < 3; i++ {
		if err := d.Write(k, "a", note{Title: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "notes"))
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("рядом с файлом лишнее: %v", names)
	}
}

// Параллельные записи и чтения одного файла: файл всегда целый (запись
// через временный файл и переименование).
func TestConcurrentWritesStayWhole(t *testing.T) {
	d := NewDir(t.TempDir())
	k := noteKind()
	if err := d.Write(k, "a", note{Title: "start"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if err := d.Write(k, "a", note{Title: fmt.Sprint("w", i), Tags: []string{"x"}}); err != nil {
				errs <- err
			}
		}(i)
		go func() {
			defer wg.Done()
			var got note
			if _, err := d.Read(k, "a", &got); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Список пропускает подкаталоги и чужие расширения, но битое имя .json —
// проблема.
func TestListSkipsDirsAndReportsBadNames(t *testing.T) {
	d := NewDir(t.TempDir())
	k := noteKind()
	if err := d.Write(k, "ok", note{Title: "t"}); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(d.KindDir(k), "sub.json"), 0o755)
	os.WriteFile(filepath.Join(d.KindDir(k), "ok.json.tmp"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(d.KindDir(k), "плохое имя.json"), []byte("{}"), 0o644)
	keys, problems := d.List(k)
	if len(keys) != 1 || keys[0] != "ok" || len(problems) != 1 || !strings.Contains(problems[0].Error(), "плохое имя") {
		t.Fatalf("список: %v %v", keys, problems)
	}
}

// Корень хранилища — абсолютный путь: смена рабочего каталога его не
// сдвигает.
func TestNewDirIsAbsolute(t *testing.T) {
	d := NewDir("data")
	if !filepath.IsAbs(d.Root()) || filepath.Base(d.Root()) != "data" {
		t.Fatalf("корень: %q", d.Root())
	}
	if !strings.HasSuffix(d.Path(noteKind(), "a"), filepath.Join("notes", "a.json")) {
		t.Fatalf("путь: %q", d.Path(noteKind(), "a"))
	}
}

// Помощники миграций: null — это «поля нет», неверный тип — ошибка.
func TestGetNullAndWrongType(t *testing.T) {
	doc := map[string]json.RawMessage{"a": json.RawMessage("null"), "b": json.RawMessage(`"строка"`)}
	var n int
	if ok, err := Get(doc, "a", &n); ok || err != nil {
		t.Fatalf("null: %v %v", ok, err)
	}
	if ok, err := Get(doc, "b", &n); !ok || err == nil {
		t.Fatalf("неверный тип: %v %v", ok, err)
	}
	Rename(doc, "нет", "c")
	if _, ok := doc["c"]; ok {
		t.Fatal("Rename несуществующего поля создал новое")
	}
}
