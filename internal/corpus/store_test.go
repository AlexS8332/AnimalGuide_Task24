package corpus

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sampleDocs — два маленьких документа: статья с разделами и документ MDD.
func sampleDocs() []Doc {
	return []Doc{
		{
			Schema: Schema, ID: "manul", Source: SourceWikipedia, Title: "Манул",
			URL: "https://ru.wikipedia.org/wiki/Манул", RevID: 42,
			OldURL: "https://ru.wikipedia.org/w/index.php?oldid=42", Fetched: "2026-10-01T00:00:00Z",
			License: LicenseWikipedia,
			Species: &Species{Latin: "Otocolobus manul", Ru: "манул", Aliases: []string{"палласов кот"}},
			Intro:   "Манул — хищное млекопитающее.\nВторой абзац.",
			Sections: []Section{
				{Path: []string{"Биология"}, Title: "Биология", Level: 2},
				{Path: []string{"Биология", "Питание"}, Title: "Питание", Level: 3, Text: "Питается пищухами & грызунами <обычно>."},
			},
		},
		{
			Schema: Schema, ID: MDDDocID, Source: SourceMDD, Title: MDDTitle,
			URL: "https://www.mammaldiversity.org", Fetched: "2026-09-25T07:13:29Z", License: LicenseMDD,
			Intro:    "Этот документ собран по релизу MDD v2.5 от 2026-07-28 (предыдущий релиз — v2.4).",
			Sections: []Section{{Path: []string{"Семейства хищных"}, Title: "Семейства хищных", Level: 2, Text: "Кошачьи (Felidae) — 46 видов."}},
		},
	}
}

func TestDocTextStable(t *testing.T) {
	// Формат канонического текста — основа всех смещений: этот тест
	// должен падать при любой его правке.
	want := "Манул — хищное млекопитающее.\nВторой абзац." +
		"\n\n## Биология" +
		"\n\n## Биология › Питание\nПитается пищухами & грызунами <обычно>."
	d := sampleDocs()[0]
	if got := d.Text(); got != want {
		t.Fatalf("Text:\n%q\nждали\n%q", got, want)
	}
	if d.Chars() != len([]rune(want)) {
		t.Fatalf("Chars = %d", d.Chars())
	}
	if d.Text() != d.Text() {
		t.Fatal("Text не детерминирован")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	docs := sampleDocs()
	m, err := Save(dir, docs)
	if err != nil {
		t.Fatal(err)
	}
	if m.Schema != Schema || len(m.Entries) != 2 || m.CorpusSHA == "" {
		t.Fatalf("манифест: %+v", m)
	}
	if m.Entries[0].ID != "manul" || m.Entries[0].File != "manul.json" || m.Entries[0].RevID != 42 {
		t.Fatalf("строка манифеста: %+v", m.Entries[0])
	}
	if want := docs[0].Chars() + docs[1].Chars(); m.Chars != want {
		t.Fatalf("Chars = %d, ждали %d", m.Chars, want)
	}
	if m.Pages != pages(m.Chars) {
		t.Fatalf("Pages = %v", m.Pages)
	}

	got, lm, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, docs) {
		t.Fatalf("Load вернул не то:\n%+v\n%+v", got, docs)
	}
	if !reflect.DeepEqual(lm, m) {
		t.Fatalf("манифест после Load: %+v", lm)
	}

	// Файл читаемый: отступ в два пробела, HTML не экранирован.
	raw, err := os.ReadFile(filepath.Join(dir, "manul.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\n  \"doc_id\": \"manul\"") || !strings.Contains(string(raw), "& грызунами <обычно>") {
		t.Fatalf("формат файла:\n%s", raw)
	}

	// corpus_sha не зависит от порядка документов, а повторное
	// сохранение того же даёт тот же манифест.
	m2, err := Save(t.TempDir(), []Doc{docs[1], docs[0]})
	if err != nil {
		t.Fatal(err)
	}
	if m2.CorpusSHA != m.CorpusSHA {
		t.Fatal("corpus_sha зависит от порядка")
	}
	m3, err := Save(dir, docs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m3, m) {
		t.Fatal("повторное сохранение изменило манифест")
	}
	// Изменение текста меняет corpus_sha.
	docs[0].Intro += "!"
	m4, err := Save(t.TempDir(), docs)
	if err != nil {
		t.Fatal(err)
	}
	if m4.CorpusSHA == m.CorpusSHA {
		t.Fatal("corpus_sha не заметил правку")
	}
}

func TestLoadDetectsTampering(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(t *testing.T, dir string)
		want   string
	}{
		{"правка документа", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "manul.json")
			raw, _ := os.ReadFile(p)
			os.WriteFile(p, []byte(strings.Replace(string(raw), "пищухами", "мышами", 1)), 0o644)
		}, "изменён после снимка"},
		{"документ удалён", func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, "manul.json"))
		}, "не прочитан"},
		{"лишний документ", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "wolf.json"), []byte("{}"), 0o644)
		}, "не указан в манифесте"},
		{"правка corpus_sha", func(t *testing.T, dir string) {
			p := filepath.Join(dir, ManifestFile)
			raw, _ := os.ReadFile(p)
			s := string(raw)
			i := strings.Index(s, `"corpus_sha": "`) + len(`"corpus_sha": "`)
			s = s[:i] + "0" + s[i+1:]
			if s[i] == raw[i] {
				s = s[:i] + "1" + s[i+1:]
			}
			os.WriteFile(p, []byte(s), 0o644)
		}, "corpus_sha"},
		{"правка sha строки", func(t *testing.T, dir string) {
			p := filepath.Join(dir, ManifestFile)
			raw, _ := os.ReadFile(p)
			s := strings.Replace(string(raw), `"sha256": "`, `"sha256": "00`, 1)
			os.WriteFile(p, []byte(s), 0o644)
		}, "изменён после снимка"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := Save(dir, sampleDocs()); err != nil {
				t.Fatal(err)
			}
			c.break_(t, dir)
			_, _, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ошибка %v, ждали «%s»", err, c.want)
			}
		})
	}
}

func TestLoadIgnoresSourcesAndReportsMissingManifest(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Load(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("без манифеста: %v, ждали os.ErrNotExist", err)
	}
	if _, err := Save(dir, sampleDocs()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, SourcesFile), []byte(`{"schema":1}`), 0o644)
	if err := WriteLicense(dir, sampleDocs()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir); err != nil {
		t.Fatalf("sources.json и LICENSE.md помешали: %v", err)
	}
}

func TestSaveRemovesDroppedDocsAndRejectsBadIDs(t *testing.T) {
	dir := t.TempDir()
	docs := sampleDocs()
	if _, err := Save(dir, docs); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(dir, docs[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, MDDDocID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("выпавший документ остался: %v", err)
	}
	if got, _, err := Load(dir); err != nil || len(got) != 1 {
		t.Fatalf("Load после сокращения: %v, %d", err, len(got))
	}

	for _, id := range []string{"", "../x", "Manul", "a b", "a.json"} {
		bad := sampleDocs()[:1]
		bad[0].ID = id
		if _, err := Save(t.TempDir(), bad); err == nil {
			t.Errorf("doc_id %q принят", id)
		}
	}
	if _, err := Save(t.TempDir(), []Doc{docs[0], docs[0]}); err == nil {
		t.Error("повтор doc_id принят")
	}
	if _, err := Save(t.TempDir(), nil); err == nil {
		t.Error("пустой корпус принят")
	}
}

func TestWriteLicense(t *testing.T) {
	dir := t.TempDir()
	if err := WriteLicense(dir, sampleDocs()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, LicenseFile))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{
		"CC BY-SA 4.0", "CC BY 4.0", "«Манул»",
		"https://ru.wikipedia.org/w/index.php?oldid=42", "ревизия 42",
		"https://www.mammaldiversity.org, релиз MDD v2.5 от 2026-07-28, данные от 2026-09-25T07:13:29Z",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("в LICENSE.md нет %q:\n%s", want, s)
		}
	}
}
