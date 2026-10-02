package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type note struct {
	Title string   `json:"title"`
	Tags  []string `json:"tags"`
}

// v2: поле text переименовано в title (удаление + добавление), tags появились
// с осмысленным нулём.
func noteKind() *Kind {
	return &Kind{
		Name: "заметка", Dir: "notes", Current: 2,
		Migrate: map[int]Migration{
			1: func(doc map[string]json.RawMessage) error {
				Rename(doc, "text", "title")
				if _, ok := doc["tags"]; !ok {
					return Set(doc, "tags", []string{})
				}
				return nil
			},
		},
	}
}

func TestWriteThenReadRoundTrip(t *testing.T) {
	d := NewDir(t.TempDir())
	k := noteKind()
	if err := d.Write(k, "a", note{Title: "рысь", Tags: []string{"кошачьи"}}); err != nil {
		t.Fatal(err)
	}
	var got note
	meta, err := d.Read(k, "a", &got)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "рысь" || len(got.Tags) != 1 {
		t.Fatalf("прочитано %+v", got)
	}
	if meta.Schema != 2 || meta.Migrated {
		t.Fatalf("meta = %+v, ждали текущую версию без миграции", meta)
	}
}

func TestWritePutsSchemaFirst(t *testing.T) {
	d := NewDir(t.TempDir())
	k := noteKind()
	if err := d.Write(k, "a", note{Title: "x"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := d.Raw(k, "a")
	if !strings.HasPrefix(string(raw), "{\n  \"schema\": 2,\n") {
		t.Fatalf("schema не первым полем:\n%s", raw)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("записан не JSON: %v", err)
	}
}

func TestEncodeEmptyObject(t *testing.T) {
	data, err := Encode(&Kind{Current: 3}, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]int
	if err := json.Unmarshal(data, &probe); err != nil || probe["schema"] != 3 {
		t.Fatalf("пустой объект: %s (%v)", data, err)
	}
}

func TestEncodeRejectsOwnSchemaField(t *testing.T) {
	type bad struct {
		Schema int `json:"schema"`
	}
	if _, err := Encode(&Kind{Current: 1}, bad{Schema: 5}); err == nil {
		t.Fatal("тип со своим полем schema должен отвергаться")
	}
}

func TestEncodeRejectsNonObject(t *testing.T) {
	if _, err := Encode(&Kind{Current: 1}, []int{1}); err == nil {
		t.Fatal("массив хранить нельзя")
	}
}

func TestFileWithoutSchemaIsVersionOneAndMigrates(t *testing.T) {
	dir := t.TempDir()
	d := NewDir(dir)
	k := noteKind()
	writeRaw(t, d, k, "old", `{"text":"манул"}`)

	var got note
	meta, err := d.Read(k, "old", &got)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Schema != 1 || !meta.Migrated {
		t.Fatalf("meta = %+v", meta)
	}
	if got.Title != "манул" || got.Tags == nil {
		t.Fatalf("данные потерялись при миграции: %+v", got)
	}
	// Чтение не трогает файл: ни копии, ни перезаписи.
	if _, err := os.Stat(filepath.Join(dir, "notes", "old.v1.bak")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("копия появилась от одного чтения")
	}
	raw, _ := d.Raw(k, "old")
	if string(raw) != `{"text":"манул"}` {
		t.Fatalf("чтение переписало файл: %s", raw)
	}
}

func TestFirstWriteOverOldFormatKeepsBackupOnce(t *testing.T) {
	dir := t.TempDir()
	d := NewDir(dir)
	k := noteKind()
	writeRaw(t, d, k, "old", `{"text":"манул"}`)

	var got note
	if _, err := d.Read(k, "old", &got); err != nil {
		t.Fatal(err)
	}
	if err := d.Write(k, "old", got); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(dir, "notes", "old.v1.bak")
	data, err := os.ReadFile(bak)
	if err != nil || string(data) != `{"text":"манул"}` {
		t.Fatalf("копия старого формата: %q, %v", data, err)
	}
	// Вторая запись — уже поверх нового формата: копия не переписывается.
	got.Title = "другое"
	if err := d.Write(k, "old", got); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(bak)
	if string(data) != `{"text":"манул"}` {
		t.Fatal("копия старого формата переписана")
	}
}

func TestNewerFileIsNotRead(t *testing.T) {
	d := NewDir(t.TempDir())
	k := noteKind()
	writeRaw(t, d, k, "future", `{"schema":7,"title":"x"}`)
	var got note
	_, err := d.Read(k, "future", &got)
	if !errors.Is(err, ErrNewer) {
		t.Fatalf("ждали ErrNewer, получили %v", err)
	}
	keys, problems := d.List(k)
	if len(keys) != 0 || len(problems) != 1 || !errors.Is(problems[0], ErrNewer) {
		t.Fatalf("List: ключи %v, проблемы %v", keys, problems)
	}
	if !strings.Contains(problems[0].Error(), "не читается") {
		t.Fatalf("про файл из будущего нужно отдельное сообщение: %v", problems[0])
	}
}

func TestCorruptFileIsSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	d := NewDir(dir)
	k := noteKind()
	writeRaw(t, d, k, "bad", `{не json`)
	writeRaw(t, d, k, "arr", `[1,2]`)
	if err := d.Write(k, "good", note{Title: "ok"}); err != nil {
		t.Fatal(err)
	}
	keys, problems := d.List(k)
	if len(keys) != 1 || keys[0] != "good" {
		t.Fatalf("ключи %v", keys)
	}
	if len(problems) != 2 {
		t.Fatalf("проблемы %v", problems)
	}
	var got note
	if _, err := d.Read(k, "bad", &got); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ждали ErrCorrupt, получили %v", err)
	}
	// Перезапись битого файла сохраняет его копию.
	if err := d.Write(k, "bad", note{Title: "починено"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "notes", "bad.broken.bak")); err != nil || string(data) != `{не json` {
		t.Fatalf("копия битого файла: %q, %v", data, err)
	}
}

func TestMissingMigrationIsError(t *testing.T) {
	k := &Kind{Name: "x", Current: 3, Migrate: map[int]Migration{1: func(map[string]json.RawMessage) error { return nil }}}
	var v map[string]any
	if _, err := Decode(k, []byte(`{}`), &v); !errors.Is(err, ErrNoMigration) {
		t.Fatalf("ждали ErrNoMigration, получили %v", err)
	}
}

func TestMigrationErrorIsReported(t *testing.T) {
	k := &Kind{Name: "x", Current: 2, Migrate: map[int]Migration{
		1: func(map[string]json.RawMessage) error { return errors.New("сломано") },
	}}
	var v map[string]any
	if _, err := Decode(k, []byte(`{}`), &v); err == nil || !strings.Contains(err.Error(), "сломано") {
		t.Fatalf("ошибка миграции потерялась: %v", err)
	}
}

func TestBadSchemaValueIsCorrupt(t *testing.T) {
	var v map[string]any
	for _, raw := range []string{`{"schema":"два"}`, `{"schema":0}`, `null`} {
		if _, err := Decode(&Kind{Current: 1}, []byte(raw), &v); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s: ждали ErrCorrupt, получили %v", raw, err)
		}
	}
}

func TestBadKeysAreRejected(t *testing.T) {
	d := NewDir(t.TempDir())
	k := noteKind()
	for _, key := range []string{"", "../x", "a.b", "с кириллицей"} {
		if err := d.Write(k, key, note{}); !errors.Is(err, ErrBadKey) {
			t.Fatalf("Write(%q): %v", key, err)
		}
		if _, err := d.Read(k, key, &note{}); !errors.Is(err, ErrBadKey) {
			t.Fatalf("Read(%q): %v", key, err)
		}
		if d.Has(k, key) {
			t.Fatalf("Has(%q)", key)
		}
	}
}

func TestCustomKeyValidator(t *testing.T) {
	d := NewDir(t.TempDir())
	k := &Kind{Name: "свод", Dir: "c", Current: 1, ValidKey: func(s string) bool { return s == "guide" }}
	if err := d.Write(k, "guide", note{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Write(k, "other", note{}); !errors.Is(err, ErrBadKey) {
		t.Fatalf("other: %v", err)
	}
}

func TestReadMissingIsNotExist(t *testing.T) {
	d := NewDir(t.TempDir())
	_, err := d.Read(noteKind(), "nope", &note{})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ждали os.ErrNotExist, получили %v", err)
	}
}

func TestDeleteAndHas(t *testing.T) {
	d := NewDir(t.TempDir())
	k := noteKind()
	if err := d.Delete(k, "nope"); err != nil {
		t.Fatalf("удаление отсутствующего: %v", err)
	}
	if err := d.Write(k, "a", note{}); err != nil {
		t.Fatal(err)
	}
	if !d.Has(k, "a") {
		t.Fatal("файла нет после записи")
	}
	if err := d.Delete(k, "a"); err != nil {
		t.Fatal(err)
	}
	if d.Has(k, "a") {
		t.Fatal("файл остался после удаления")
	}
	if err := d.Delete(k, "../a"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("Delete с плохим ключом: %v", err)
	}
}

func TestListIgnoresForeignFilesAndEmptyDir(t *testing.T) {
	dir := t.TempDir()
	d := NewDir(dir)
	k := noteKind()
	if keys, problems := d.List(k); keys != nil || problems != nil {
		t.Fatalf("пустой каталог: %v %v", keys, problems)
	}
	if err := d.Write(k, "b", note{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Write(k, "a", note{}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "notes", "readme.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes", "a.v1.bak"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes", "bad name.json"), []byte("{}"), 0o644)
	keys, problems := d.List(k)
	if strings.Join(keys, ",") != "a,b" {
		t.Fatalf("ключи %v", keys)
	}
	if len(problems) != 1 {
		t.Fatalf("проблемы %v", problems)
	}
}

func TestDisplayPathsAreRelative(t *testing.T) {
	d := NewDir(filepath.Join(".", "data"))
	k := noteKind()
	if got := d.DisplayPath(k, "a"); got != filepath.Join("data", "notes", "a.json") {
		t.Fatalf("DisplayPath = %q", got)
	}
	if got := d.DisplayDir(k); got != filepath.Join("data", "notes") {
		t.Fatalf("DisplayDir = %q", got)
	}
	if !filepath.IsAbs(d.Root()) {
		t.Fatal("корень хранится относительным")
	}
}

func TestMigrationHelpers(t *testing.T) {
	doc := map[string]json.RawMessage{"a": json.RawMessage(`1`), "b": json.RawMessage(`2`)}
	Rename(doc, "a", "b") // новое поле уже есть — значение не затирается
	if string(doc["b"]) != "2" || doc["a"] != nil {
		t.Fatalf("Rename при занятом имени: %v", doc)
	}
	Rename(doc, "missing", "c")
	if _, ok := doc["c"]; ok {
		t.Fatal("Rename отсутствующего создал поле")
	}
	var n int
	if ok, err := Get(doc, "b", &n); !ok || err != nil || n != 2 {
		t.Fatalf("Get: %v %v %d", ok, err, n)
	}
	if ok, _ := Get(doc, "none", &n); ok {
		t.Fatal("Get отсутствующего")
	}
	doc["nil"] = json.RawMessage(`null`)
	if ok, _ := Get(doc, "nil", &n); ok {
		t.Fatal("Get null должен считаться отсутствием")
	}
	if err := Set(doc, "c", []string{"x"}); err != nil || string(doc["c"]) != `["x"]` {
		t.Fatalf("Set: %v %s", err, doc["c"])
	}
}

func TestSchemaOf(t *testing.T) {
	for raw, want := range map[string]int{`{}`: 1, `{"schema":4}`: 4} {
		got, err := SchemaOf([]byte(raw))
		if err != nil || got != want {
			t.Fatalf("SchemaOf(%s) = %d, %v", raw, got, err)
		}
	}
}

func writeRaw(t *testing.T, d *Dir, k *Kind, key, content string) {
	t.Helper()
	if err := os.MkdirAll(d.KindDir(k), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.Path(k, key), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
