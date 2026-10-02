package corpus

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ManifestFile — оглавление снимка в каталоге корпуса.
const ManifestFile = "MANIFEST.json"

// LicenseFile — лицензия текстов корпуса.
const LicenseFile = "LICENSE.md"

// SourcesFile — список статей, по которому `kb fetch` снимает корпус. Лежит
// в том же каталоге, но документом не является: Load его не считает лишним.
const SourcesFile = "sources.json"

// idPattern — допустимый doc_id: он же имя файла, поэтому только латиница
// в нижнем регистре, цифры и дефис — без путей, точек и пробелов.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// load — реализация Load.
func load(dir string) ([]Doc, Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("корпус: манифест не прочитан: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, Manifest{}, fmt.Errorf("корпус: манифест повреждён: %w", err)
	}
	if m.Schema != Schema {
		return nil, Manifest{}, fmt.Errorf("корпус: схема манифеста %d, ожидается %d", m.Schema, Schema)
	}
	if len(m.Entries) == 0 {
		return nil, Manifest{}, errors.New("корпус: манифест пуст")
	}

	docs := make([]Doc, 0, len(m.Entries))
	listed := map[string]bool{}
	chars := 0
	for _, e := range m.Entries {
		if !idPattern.MatchString(e.ID) || e.File != e.ID+".json" {
			return nil, Manifest{}, fmt.Errorf("корпус: в манифесте странная строка %q → %q", e.ID, e.File)
		}
		if listed[e.File] {
			return nil, Manifest{}, fmt.Errorf("корпус: документ %s в манифесте дважды", e.ID)
		}
		listed[e.File] = true

		data, err := os.ReadFile(filepath.Join(dir, e.File))
		if err != nil {
			return nil, Manifest{}, fmt.Errorf("корпус: документ %s не прочитан: %w", e.ID, err)
		}
		if sum := sha256Hex(data); sum != e.SHA256 {
			// Самая важная проверка: правка руками или недокачанный файл —
			// это другой корпус, а индекс и разметка вопросов привязаны к
			// этому.
			return nil, Manifest{}, fmt.Errorf("корпус: %s изменён после снимка (sha256 %s, в манифесте %s)", e.File, short(sum), short(e.SHA256))
		}
		var d Doc
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, Manifest{}, fmt.Errorf("корпус: документ %s повреждён: %w", e.ID, err)
		}
		if d.Schema != Schema {
			return nil, Manifest{}, fmt.Errorf("корпус: у документа %s схема %d, ожидается %d", e.ID, d.Schema, Schema)
		}
		if d.ID != e.ID {
			return nil, Manifest{}, fmt.Errorf("корпус: в файле %s документ %q", e.File, d.ID)
		}
		if c := d.Chars(); c != e.Chars {
			return nil, Manifest{}, fmt.Errorf("корпус: у %s %d символов, в манифесте %d", e.ID, c, e.Chars)
		}
		chars += e.Chars
		docs = append(docs, d)
	}

	// Лишний документ в каталоге — тоже расхождение: его положили руками
	// или Save оборвался между файлами и манифестом.
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, Manifest{}, err
	}
	for _, f := range files {
		name := filepath.Base(f)
		if name == ManifestFile || name == SourcesFile || listed[name] {
			continue
		}
		return nil, Manifest{}, fmt.Errorf("корпус: файл %s не указан в манифесте", name)
	}

	sha, err := corpusSHA(m.Entries)
	if err != nil {
		return nil, Manifest{}, err
	}
	if sha != m.CorpusSHA {
		return nil, Manifest{}, fmt.Errorf("корпус: corpus_sha %s не сходится с манифестом (%s)", short(sha), short(m.CorpusSHA))
	}
	if chars != m.Chars {
		return nil, Manifest{}, fmt.Errorf("корпус: всего %d символов, в манифесте %d", chars, m.Chars)
	}
	return docs, m, nil
}

// save — реализация Save. Сначала документы, последним — манифест: если
// запись оборвётся посередине, Load увидит расхождение sha, а не тихо
// смешанный корпус.
func save(dir string, docs []Doc) (Manifest, error) {
	if len(docs) == 0 {
		return Manifest{}, errors.New("корпус: нечего сохранять")
	}
	seen := map[string]bool{}
	for _, d := range docs {
		if !idPattern.MatchString(d.ID) {
			return Manifest{}, fmt.Errorf("корпус: недопустимый doc_id %q", d.ID)
		}
		if seen[d.ID] {
			return Manifest{}, fmt.Errorf("корпус: doc_id %s повторяется", d.ID)
		}
		seen[d.ID] = true
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Manifest{}, err
	}

	// Документы, которые были в прошлом снимке и выпали из нового, надо
	// убрать: иначе Load счёл бы их лишними.
	var stale []string
	if raw, err := os.ReadFile(filepath.Join(dir, ManifestFile)); err == nil {
		var old Manifest
		if json.Unmarshal(raw, &old) == nil {
			for _, e := range old.Entries {
				if !seen[e.ID] && idPattern.MatchString(e.ID) && e.File == e.ID+".json" {
					stale = append(stale, e.File)
				}
			}
		}
	}

	m := Manifest{Schema: Schema, Entries: make([]Entry, 0, len(docs))}
	for _, d := range docs {
		d.Schema = Schema
		data, err := marshal(d)
		if err != nil {
			return Manifest{}, fmt.Errorf("корпус: %s: %w", d.ID, err)
		}
		file := d.ID + ".json"
		if err := writeAtomic(filepath.Join(dir, file), data); err != nil {
			return Manifest{}, err
		}
		e := Entry{
			ID: d.ID, File: file, Title: d.Title, Source: d.Source,
			RevID: d.RevID, Chars: d.Chars(), SHA256: sha256Hex(data),
		}
		m.Entries = append(m.Entries, e)
		m.Chars += e.Chars
	}
	m.Pages = pages(m.Chars)
	sha, err := corpusSHA(m.Entries)
	if err != nil {
		return Manifest{}, err
	}
	m.CorpusSHA = sha

	data, err := marshal(m)
	if err != nil {
		return Manifest{}, err
	}
	if err := writeAtomic(filepath.Join(dir, ManifestFile), data); err != nil {
		return Manifest{}, err
	}
	for _, f := range stale {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Manifest{}, err
		}
	}
	return m, nil
}

// pages — объём в страницах с одним знаком после запятой: дробный хвост
// длиннее только шумел бы в JSON и отчётах.
func pages(chars int) float64 {
	return math.Round(float64(chars)/PageChars*10) / 10
}

// corpusSHA — sha256 канонического JSON строк манифеста, упорядоченных по
// doc_id. Порядок манифеста (порядок списка статей) в сумму не входит:
// перестановка статей корпус не меняет.
func corpusSHA(entries []Entry) (string, error) {
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	data, err := json.Marshal(sorted)
	if err != nil {
		return "", err
	}
	return sha256Hex(data), nil
}

// marshal — JSON с отступом в два пробела и переводом строки в конце. HTML
// не экранируется: «<» и «&» в тексте статьи остаются читаемыми.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeAtomic пишет файл через временный рядом и переименование: читатель
// видит либо старый файл, либо новый целиком.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// short — начало хэша для сообщений об ошибке.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// writeLicense — реализация WriteLicense. Текст детерминирован (только из
// документов), поэтому повторный fetch той же ревизии не меняет файл.
func writeLicense(dir string, docs []Doc) error {
	var wiki, other []Doc
	for _, d := range docs {
		if d.Source == SourceWikipedia {
			wiki = append(wiki, d)
		} else {
			other = append(other, d)
		}
	}

	var b strings.Builder
	b.WriteString("# Лицензия текстов корпуса\n\n")
	b.WriteString("Тексты в этом каталоге — не часть кода проекта и распространяются на\n")
	b.WriteString("условиях своих источников. Лицензия кода к ним не относится.\n")
	if len(wiki) > 0 {
		b.WriteString("\n## Русская Википедия — CC BY-SA 4.0\n\n")
		b.WriteString("Статьи ru.wikipedia.org распространяются по лицензии Creative Commons\n")
		b.WriteString("«Атрибуция — С сохранением условий» 4.0 (CC BY-SA 4.0,\n")
		b.WriteString("https://creativecommons.org/licenses/by-sa/4.0/deed.ru). Авторы — участники\n")
		b.WriteString("Википедии; список авторов — в истории правок каждой статьи. Тексты\n")
		b.WriteString("изменены: оставлен только простой текст (без разметки, таблиц и\n")
		b.WriteString("иллюстраций), сняты знаки ударения, убраны служебные разделы\n")
		b.WriteString("(примечания, литература, ссылки и т. п.). Производные тексты\n")
		b.WriteString("распространяются на тех же условиях.\n\n")
		for _, d := range wiki {
			fmt.Fprintf(&b, "- «%s» — %s, ревизия %d (%s), снято %s\n", d.Title, d.URL, d.RevID, d.OldURL, d.Fetched)
		}
	}
	for _, d := range other {
		fmt.Fprintf(&b, "\n## %s — %s\n\n", d.Title, d.License)
		if d.Source == SourceMDD {
			b.WriteString("Документ собран кодом по шаблону из набора данных Mammal Diversity\n")
			b.WriteString("Database Американского общества маммалогов (ASM), который\n")
			b.WriteString("распространяется по лицензии Creative Commons «Атрибуция» 4.0\n")
			b.WriteString("(CC BY 4.0, https://creativecommons.org/licenses/by/4.0/deed.ru).\n\n")
		}
		release := ""
		if d.Source == SourceMDD {
			if v, date := mddRelease(d); v != "" {
				release = ", релиз MDD " + v
				if date != "" {
					release += " от " + date
				}
			}
		}
		fmt.Fprintf(&b, "- «%s» — %s%s, данные от %s\n", d.Title, d.URL, release, d.Fetched)
	}
	return writeAtomic(filepath.Join(dir, LicenseFile), []byte(b.String()))
}

// mddReleaseRe — версия и дата релиза из первой строки вступления MDDDoc
// («…собран по релизу MDD v2.5 от 2026-07-28…»).
var mddReleaseRe = regexp.MustCompile(`по релизу MDD (\S+?)(?: от (\d{4}-\d{2}-\d{2}))?(?:[ (]|\.(?:\s|$))`)

// mddRelease — версия и дата релиза MDD документа. Отдельных полей у Doc
// для них нет (формат документа ради атрибуции не меняем), а вступление
// пишет MDDDoc по шаблону — оттуда и берём; не нашлось — пусто.
func mddRelease(d Doc) (version, date string) {
	m := mddReleaseRe.FindStringSubmatch(d.Intro)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}
