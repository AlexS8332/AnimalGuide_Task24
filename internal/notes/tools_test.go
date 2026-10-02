package notes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// kit — инструменты блокнота над временным каталогом, по имени.
type kit struct {
	t   *testing.T
	dir string
	by  map[string]tools.Tool
}

func newKit(t *testing.T) *kit {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "notes")
	k := &kit{t: t, dir: dir, by: map[string]tools.Tool{}}
	for _, tl := range Tools(dir) {
		k.by[tl.Spec().Name] = tl
	}
	return k
}

func (k *kit) call(name string, args any) (string, error) {
	k.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		k.t.Fatal(err)
	}
	return k.by[name].Call(context.Background(), raw)
}

func (k *kit) must(name string, args, out any) {
	k.t.Helper()
	text, err := k.call(name, args)
	if err != nil {
		k.t.Fatalf("%s: %v", name, err)
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		k.t.Fatalf("%s: ответ не JSON: %v\n%s", name, err, text)
	}
}

func (k *kit) open(title, species string) OpenResult {
	k.t.Helper()
	var r OpenResult
	k.must(ToolOpen, OpenArgs{Title: title, Species: species}, &r)
	return r
}

func wantErr(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("ошибки нет, ждали %q", parts)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("в ошибке нет %q: %v", p, err)
		}
	}
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestSpecs(t *testing.T) {
	ts := Tools(t.TempDir())
	if got := tools.Names(ts); !reflect.DeepEqual(got, ToolNames) {
		t.Fatalf("инструменты %v, ждали %v", got, ToolNames)
	}
	for _, tl := range ts {
		s := tl.Spec()
		if !s.Write {
			t.Errorf("%s без Write: блокнот меняет состояние", s.Name)
		}
		if !json.Valid(s.Parameters) {
			t.Errorf("%s: схема не JSON", s.Name)
		}
		if !strings.Contains(s.Description, ToolOpen) {
			t.Errorf("%s: в описании нет порядка работы", s.Name)
		}
	}
}

func TestFullCycle(t *testing.T) {
	k := newKit(t)
	op := k.open("Манул — паспорт вида", "Otocolobus manul")
	if !regexp.MustCompile(`^nb-[0-9a-f]{12}$`).MatchString(op.NotebookID) {
		t.Fatalf("notebook_id %q", op.NotebookID)
	}
	if filepath.Dir(op.Path) != mustAbs(t, k.dir) || !strings.HasSuffix(op.Path, ".md") ||
		!strings.Contains(filepath.Base(op.Path), "otocolobus-manul") {
		t.Errorf("path %q", op.Path)
	}
	// Идентификаторы случайны: второй блокнот — другой id.
	if op2 := k.open("Манул", "Otocolobus manul"); op2.NotebookID == op.NotebookID || op2.Path == op.Path {
		t.Errorf("два блокнота с одним id или путём: %v %v", op, op2)
	}

	sections := []AddArgs{
		{Heading: "Систематика", Text: "Род *Otocolobus*, семейство кошачьих.", Cites: []string{"mdd_get", "taxon_tree"}},
		{Heading: "Описание", Text: "Размером с домашнюю кошку.", Cites: []string{"read_wikipedia", "mdd_get", " "}},
		{Heading: "Названия\nи имена", Text: "Манул, палласов кот.", Cites: []string{"vernacular_names"}},
	}
	for i, s := range sections {
		s.NotebookID = op.NotebookID
		var r AddResult
		k.must(ToolAdd, s, &r)
		if r.Section != i+1 || r.Sections != i+1 || r.NotebookID != op.NotebookID {
			t.Errorf("раздел %d: %+v", i+1, r)
		}
	}

	var cl CloseResult
	k.must(ToolClose, CloseArgs{NotebookID: op.NotebookID}, &cl)
	if cl.Path != op.Path {
		t.Errorf("файл %q, обещан %q", cl.Path, op.Path)
	}
	if cl.Sections != 3 {
		t.Errorf("разделов %d", cl.Sections)
	}
	if want := []string{"mdd_get", "read_wikipedia", "taxon_tree", "vernacular_names"}; !reflect.DeepEqual(cl.Cites, want) {
		t.Errorf("cites %v, ждали %v", cl.Cites, want)
	}
	if got := fileSHA(t, cl.Path); got != cl.SHA256 {
		t.Errorf("sha256 файла %s, в ответе %s", got, cl.SHA256)
	}
	data, _ := os.ReadFile(cl.Path)
	if cl.Bytes != len(data) {
		t.Errorf("bytes %d, файл %d", cl.Bytes, len(data))
	}
	md := string(data)
	for _, want := range []string{"# Манул — паспорт вида\n", "Вид: *Otocolobus manul*", "## Систематика\n",
		"## Названия и имена\n", "Размером с домашнюю кошку.",
		"Источники: mdd_get, read_wikipedia, taxon_tree, vernacular_names\n"} {
		if !strings.Contains(md, want) {
			t.Errorf("в файле нет %q:\n%s", want, md)
		}
	}
	if !strings.HasPrefix(md, cl.Preview) || cl.Preview == "" {
		t.Errorf("preview не начало файла: %q", cl.Preview)
	}

	// После закрытия: ни раздела, ни второго закрытия.
	_, err := k.call(ToolAdd, AddArgs{NotebookID: op.NotebookID, Heading: "Ещё", Text: "текст"})
	wantErr(t, err, "уже закрыт", ToolOpen)
	_, err = k.call(ToolClose, CloseArgs{NotebookID: op.NotebookID})
	wantErr(t, err, "уже закрыт", cl.Path)
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestUnknownID(t *testing.T) {
	k := newKit(t)
	k.open("Манул", "Otocolobus manul")
	for _, id := range []string{"nb-000000000000", "manul", ""} {
		_, err := k.call(ToolAdd, AddArgs{NotebookID: id, Heading: "Раздел", Text: "текст"})
		wantErr(t, err, "сначала "+ToolOpen, "notebook_id из его ответа")
		_, err = k.call(ToolClose, CloseArgs{NotebookID: id})
		wantErr(t, err, "сначала "+ToolOpen)
	}
	if entries, _ := os.ReadDir(k.dir); len(entries) != 0 {
		t.Errorf("файлы без закрытия: %v", entries)
	}
}

func TestCloseEmpty(t *testing.T) {
	k := newKit(t)
	op := k.open("Манул", "Otocolobus manul")
	_, err := k.call(ToolClose, CloseArgs{NotebookID: op.NotebookID})
	wantErr(t, err, "нет ни одного раздела", ToolAdd)
	// Отказ не закрыл блокнот: раздел добавляется, закрытие проходит.
	var r AddResult
	k.must(ToolAdd, AddArgs{NotebookID: op.NotebookID, Heading: "Раздел", Text: "текст"}, &r)
	var cl CloseResult
	k.must(ToolClose, CloseArgs{NotebookID: op.NotebookID}, &cl)
	if !strings.Contains(string(mustRead(t, cl.Path)), "Источники: не указаны") {
		t.Error("без cites нет строки об источниках")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestValidation(t *testing.T) {
	k := newKit(t)
	_, err := k.call(ToolOpen, OpenArgs{})
	wantErr(t, err, "title", "species")
	_, err = k.call(ToolOpen, map[string]string{"name": "Манул"})
	wantErr(t, err, "аргументы не разобрались")

	op := k.open("Манул", "Otocolobus manul")
	id := op.NotebookID
	_, err = k.call(ToolAdd, AddArgs{NotebookID: id, Heading: "  ", Text: "текст"})
	wantErr(t, err, "heading")
	_, err = k.call(ToolAdd, AddArgs{NotebookID: id, Heading: "Раздел", Text: "\n "})
	wantErr(t, err, "text")
	_, err = k.call(ToolAdd, AddArgs{NotebookID: id, Heading: "Раздел", Text: strings.Repeat("ё", textMax+1)})
	wantErr(t, err, "предел 8000")
	_, err = k.call(ToolAdd, AddArgs{NotebookID: id, Heading: "Раздел", Text: "т", Cites: []string{strings.Repeat("x", 100)}})
	wantErr(t, err, "cites")
	_, err = k.call(ToolClose, CloseArgs{NotebookID: id, Format: "pdf"})
	wantErr(t, err, "md или json")

	// Ровно предел — можно; раздел сверх предела — нет.
	for i := 0; i < sectionsMax; i++ {
		if _, err := k.call(ToolAdd, AddArgs{NotebookID: id, Heading: "Раздел", Text: strings.Repeat("ё", textMax)}); err != nil {
			t.Fatalf("раздел %d: %v", i+1, err)
		}
	}
	_, err = k.call(ToolAdd, AddArgs{NotebookID: id, Heading: "Лишний", Text: "текст"})
	wantErr(t, err, "предел", ToolClose)
}

func TestPathEscape(t *testing.T) {
	k := newKit(t)
	root := mustAbs(t, k.dir)
	for _, c := range []OpenArgs{
		{Title: "x", Species: `../../../evil`},
		{Title: "x", Species: `..\..\evil`},
		{Title: "x", Species: `C:\Windows\evil`},
		{Title: "../..", Species: "/etc/passwd"},
		{Title: "..", Species: ".."},
	} {
		op := k.open(c.Title, c.Species)
		if filepath.Dir(op.Path) != root {
			t.Errorf("%+v: файл %q вне каталога %q", c, op.Path, root)
		}
		k.must(ToolAdd, AddArgs{NotebookID: op.NotebookID, Heading: "Р", Text: "т"}, &AddResult{})
		var cl CloseResult
		k.must(ToolClose, CloseArgs{NotebookID: op.NotebookID}, &cl)
		if filepath.Dir(cl.Path) != root {
			t.Errorf("%+v: записан %q вне каталога", c, cl.Path)
		}
		if strings.Contains(filepath.Base(cl.Path), "..") {
			t.Errorf("в имени осталось «..»: %q", cl.Path)
		}
	}
	// Кириллица — транслитом.
	op := k.open("Манул", "Манул")
	if !strings.HasPrefix(filepath.Base(op.Path), "manul-nb-") {
		t.Errorf("имя %q", filepath.Base(op.Path))
	}
}

func TestJSONFormat(t *testing.T) {
	k := newKit(t)
	op := k.open("Манул — паспорт вида", "Otocolobus manul")
	k.must(ToolAdd, AddArgs{NotebookID: op.NotebookID, Heading: "Питание", Text: "Пищухи и грызуны.",
		Cites: []string{"read_wikipedia"}}, &AddResult{})
	var cl CloseResult
	k.must(ToolClose, CloseArgs{NotebookID: op.NotebookID, Format: "JSON"}, &cl)
	if !strings.HasSuffix(cl.Path, ".json") || strings.TrimSuffix(cl.Path, ".json") != strings.TrimSuffix(op.Path, ".md") {
		t.Errorf("json-файл %q при обещанном %q", cl.Path, op.Path)
	}
	if got := fileSHA(t, cl.Path); got != cl.SHA256 {
		t.Errorf("sha256 %s ≠ %s", got, cl.SHA256)
	}
	var doc struct {
		NotebookID string `json:"notebook_id"`
		Title      string `json:"title"`
		Species    string `json:"species"`
		Sections   []struct {
			Heading string   `json:"heading"`
			Text    string   `json:"text"`
			Cites   []string `json:"cites"`
		} `json:"sections"`
		Cites []string `json:"cites"`
	}
	if err := json.Unmarshal(mustRead(t, cl.Path), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.NotebookID != op.NotebookID || doc.Species != "Otocolobus manul" || len(doc.Sections) != 1 ||
		doc.Sections[0].Heading != "Питание" || !reflect.DeepEqual(doc.Cites, []string{"read_wikipedia"}) {
		t.Errorf("json-блокнот: %+v", doc)
	}
}
