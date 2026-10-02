package mdd

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// loaderMiniFiles — файлы testdata/mini: урезанный настоящий архив MDD v2.5.
// В git лежат читаемые файлы, а zip собирается в памяти — так правка
// тестовых данных видна в диффе.
func loaderMiniFiles(t *testing.T) map[string][]byte {
	t.Helper()
	root := filepath.Join("testdata", "mini")
	files := make(map[string][]byte)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatalf("testdata/mini: %v", err)
	}
	return files
}

// loaderZip собирает zip из набора «путь → содержимое» в порядке имён,
// чтобы архив от прогона к прогону был одинаковым.
func loaderZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func loaderMiniZip(t *testing.T) []byte {
	t.Helper()
	return loaderZip(t, loaderMiniFiles(t))
}

func loaderSpecies(t *testing.T, d *Dataset, name string) Species {
	t.Helper()
	for _, s := range d.Species {
		if s.SciName == name {
			return s
		}
	}
	t.Fatalf("вида %q нет среди %d", name, len(d.Species))
	return Species{}
}

func TestParseMini(t *testing.T) {
	d, err := Parse(loaderMiniZip(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	r := d.Release
	if r.Version != "v2.5" || r.Date != "2026-07-28" || r.PrevVersion != "v2.4" {
		t.Errorf("релиз: %+v", r)
	}
	if !strings.Contains(r.Citation, "zenodo") || !strings.Contains(r.Remarks, "6,904") {
		t.Errorf("citation/remarks: %q / %q", r.Citation, r.Remarks)
	}
	if r.Species != 12 || len(d.Species) != 12 {
		t.Errorf("видов %d (Release.Species %d), ждали 12", len(d.Species), r.Species)
	}

	m := loaderSpecies(t, d, "Otocolobus manul")
	want := Species{
		ID:                 1006010,
		Phylosort:          25,
		SciName:            "Otocolobus manul",
		CommonName:         "Pallas's Cat",
		OtherCommonNames:   []string{"Manul", "Steppe Cat"},
		Order:              "Carnivora",
		Family:             "Felidae",
		Subfamily:          "Felinae",
		Genus:              "Otocolobus",
		Epithet:            "manul",
		Authority:          "(Pallas, 1776)",
		Year:               1776,
		IUCN:               "LC",
		Countries:          m.Countries, // проверены отдельно ниже
		CountriesUncertain: []string{"Azerbaijan", "Tajikistan", "Uzbekistan"},
		Continents:         []string{"Asia"},
		Realms:             []string{"Palearctic"},
		TypeLocality:       "S of Lake Baikal, Russia.",
		DistributionNotes:  "recently re-discovered in Armenia",
		TaxonomyNotes:      "moved from Felis to Otocolobus",
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("манул:\n got %+v\nwant %+v", m, want)
	}
	if !slices.Contains(m.Countries, "Kazakhstan") || len(m.Countries) != 13 {
		t.Errorf("страны манула: %v", m.Countries)
	}
	for _, c := range m.Countries {
		if strings.Contains(c, "?") || slices.Contains(m.CountriesUncertain, c) {
			t.Errorf("в Countries попала сомнительная страна %q", c)
		}
	}
	if m.URL() != "https://www.mammaldiversity.org/taxon/1006010/" {
		t.Errorf("URL: %s", m.URL())
	}

	p := loaderSpecies(t, d, "Ornithorhynchus anatinus")
	if p.ID != 1000001 || p.Authority != "(G. K. Shaw, 1799)" || p.CommonName != "Platypus" || p.Order != "Monotremata" {
		t.Errorf("утконос: %+v", p)
	}
	if p.Subfamily != "" {
		t.Errorf("NA в subfamily не стал пустой строкой: %q", p.Subfamily)
	}

	if u := loaderSpecies(t, d, "Panthera uncia"); u.IUCN != "VU" || len(u.CountriesUncertain) != 0 || u.Extinct || u.Domestic {
		t.Errorf("ирбис: %+v", u)
	}
	if c := loaderSpecies(t, d, "Felis catus"); !c.Domestic || c.Extinct || c.Authority != "Linnaeus, 1758" || c.Realms != nil || c.Countries != nil {
		t.Errorf("кошка: %+v", c)
	}
	if th := loaderSpecies(t, d, "Thylacinus cynocephalus"); !th.Extinct || th.IUCN != "EX" {
		t.Errorf("сумчатый волк: %+v", th)
	}
	// МСОП оценивал бизона как Bison bison: в CSV «NT (as Bison bison)».
	if b := loaderSpecies(t, d, "Bos bison"); b.IUCN != "NT" {
		t.Errorf("бизон: IUCN %q, ждали NT", b.IUCN)
	}
	// В настоящем v2.5 многострочных полей нет, но формат их допускает:
	// в testdata в taxonomyNotes волка вставлен перевод строки.
	w := loaderSpecies(t, d, "Canis lupus")
	if !strings.Contains(w.TaxonomyNotes, ";\nthe subspecies chanco") || w.CommonName != "Gray Wolf" {
		t.Errorf("многострочное поле волка: %q", w.TaxonomyNotes)
	}

	if len(d.Changes) != 5 {
		t.Fatalf("изменений %d, ждали 5: %+v", len(d.Changes), d.Changes)
	}
	cats := map[string]Change{}
	for _, c := range d.Changes {
		cats[c.Category] = c
	}
	nv := cats["de novo"]
	if nv.OldName != "" || nv.NewName != "Afronycteris rautenbachi" || nv.Comment != "recently described" || !strings.Contains(nv.Reference, "Zootaxa") {
		t.Errorf("de novo: %+v", nv)
	}
	if l := cats["lump"]; l.OldName != "Abrothrix gossei" || l.NewName != "Abrothrix dolichonyx" {
		t.Errorf("lump: %+v", l)
	}
	for _, c := range []string{"split", "name change", "genus change"} {
		if _, ok := cats[c]; !ok {
			t.Errorf("нет изменения категории %q", c)
		}
	}
}

func TestParseSkipsJunkAndOtherFiles(t *testing.T) {
	files := loaderMiniFiles(t)
	// Мусор, который совпал бы с шаблоном по имени, если бы не __MACOSX,
	// и файлы, которые справочнику не нужны.
	files["__MACOSX/MDD/MDD_v2.5_1species.csv"] = []byte("мусор")
	files["MDD/Diff-AllChanges_v2.4-v2.5.csv"] = []byte("не CSV \"")
	files["MDD/Species_Syn_Current_v2.5.csv"] = []byte("не CSV \"")
	d, err := Parse(loaderZip(t, files))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Species) != 12 || len(d.Changes) != 5 {
		t.Errorf("видов %d, изменений %d", len(d.Species), len(d.Changes))
	}
}

func TestParseWithoutReleaseAndDiff(t *testing.T) {
	files := loaderMiniFiles(t)
	delete(files, "MDD/release.toml")
	delete(files, "MDD/Diff_v2.4-v2.5.csv")
	d, err := Parse(loaderZip(t, files))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Версия берётся из имени файла видов.
	if d.Release.Version != "v2.5" || d.Release.PrevVersion != "" || d.Changes != nil {
		t.Errorf("релиз %+v, изменений %d", d.Release, len(d.Changes))
	}
}

func TestParseErrors(t *testing.T) {
	mini := loaderMiniFiles(t)
	species := "MDD/MDD_v2.5_12species.csv"

	without := func(drop string) map[string][]byte {
		out := make(map[string][]byte)
		for k, v := range mini {
			if k != drop {
				out[k] = v
			}
		}
		return out
	}
	renameColumn := func(from, to string) map[string][]byte {
		out := without("")
		data := string(out[species])
		header, rest, _ := strings.Cut(data, "\n")
		cols := strings.Split(header, ",")
		for i, c := range cols {
			if c == from {
				cols[i] = to
			}
		}
		out[species] = []byte(strings.Join(cols, ",") + "\n" + rest)
		return out
	}
	withSpecies := func(csv string) map[string][]byte {
		out := without(species)
		out[species] = []byte(csv)
		return out
	}

	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"пустой", nil, "пуст"},
		{"не zip", []byte("<html>Not Found</html>"), "zip"},
		{"обрезанный zip", loaderMiniZip(t)[:1000], "zip"},
		{"нет файла видов", loaderZip(t, without(species)), "нет файла видов"},
		{"нет колонки family", loaderZip(t, renameColumn("family", "familia")), `"family"`},
		{"нет колонки id", loaderZip(t, renameColumn("id", "mddID")), `"id"`},
		{"id не число", loaderZip(t, withSpecies("sciName,id,order,family\nA_b,x,O,F\n")), "id"},
		{"повтор id", loaderZip(t, withSpecies("sciName,id,order,family\nA_b,1,O,F\nA_c,1,O,F\n")), "повторяется"},
		{"строка съехала", loaderZip(t, withSpecies("sciName,id,order,family\nA_b,1,O\n")), "wrong number of fields"},
		{"только заголовок", loaderZip(t, withSpecies("sciName,id,order,family\n")), "ни одного"},
		{"два файла видов", loaderZip(t, func() map[string][]byte {
			m := without("")
			m["MDD/MDD_v2.6_12species.csv"] = mini[species]
			return m
		}()), "два файла"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Parse(tc.data)
			if err == nil {
				t.Fatalf("ждали ошибку, получили %d видов", len(d.Species))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ошибка %q не содержит %q", err, tc.want)
			}
		})
	}
}

func TestParseReleaseTOML(t *testing.T) {
	r := parseReleaseTOML([]byte("# шапка\r\n[other]\r\nversion = \"v0\"\r\n[metadata]\r\n" +
		"version = \"v3.0\" # комментарий\r\nrelease_date = '2027-01-02'\r\n" +
		"remarks = \"с \\\"кавычками\\\" и # решёткой\"\r\ncitation = bare # хвост\r\n"))
	want := Release{Version: "v3.0", Date: "2027-01-02", Remarks: `с "кавычками" и # решёткой`, Citation: "bare"}
	if r != want {
		t.Errorf("got %+v\nwant %+v", r, want)
	}
}

// TestParseRealArchive — разбор настоящего архива MDD целиком. Архив в
// репозиторий не кладётся (15 МБ): путь к нему — в MDD_ZIP.
func TestParseRealArchive(t *testing.T) {
	p := os.Getenv("MDD_ZIP")
	if p == "" {
		t.Skip("MDD_ZIP не задан — разбор настоящего архива пропущен")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	d, err := Parse(data)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	t.Logf("%s: %d видов, %d изменений, релиз %s (от %s), разбор %v",
		filepath.Base(p), len(d.Species), len(d.Changes), d.Release.Version, d.Release.PrevVersion, elapsed)

	if d.Release.Species != len(d.Species) || d.Release.Version == "" {
		t.Errorf("релиз: %+v", d.Release)
	}
	// В v2.5 ровно 6904 вида; у другого релиза число будет другим, но
	// сойтись оно должно с числом в имени файла видов.
	if d.Release.Version == "v2.5" && len(d.Species) != 6904 {
		t.Errorf("видов %d, в v2.5 их 6904", len(d.Species))
	}
	ids := make(map[int]bool, len(d.Species))
	for _, s := range d.Species {
		if ids[s.ID] {
			t.Errorf("id %d повторяется", s.ID)
		}
		ids[s.ID] = true
		if s.SciName == "" || s.Order == "" || s.Family == "" {
			t.Errorf("вид %d без названия или систематики: %+v", s.ID, s)
		}
		if strings.Contains(s.SciName, "_") {
			t.Errorf("подчёркивание в названии: %q", s.SciName)
		}
	}
	m := loaderSpecies(t, d, "Otocolobus manul")
	if m.ID != 1006010 || !slices.Contains(m.CountriesUncertain, "Azerbaijan") {
		t.Errorf("манул: %+v", m)
	}
}

// TestParseBOM — CSV, пересохранённый в Excel, начинается с BOM: первая
// колонка всё равно должна находиться по имени.
func TestParseBOM(t *testing.T) {
	files := loaderMiniFiles(t)
	name := "MDD/MDD_v2.5_12species.csv"
	files[name] = append([]byte(string(rune(0xFEFF))), files[name]...)
	d, err := Parse(loaderZip(t, files))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if loaderSpecies(t, d, "Otocolobus manul").ID != 1006010 {
		t.Error("манул не нашёлся")
	}
}
