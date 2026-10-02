package mdd

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// maxEntryBytes — предел распакованного размера одной нужной записи архива.
// Файл видов весит 9 МБ; предел защищает от архива-бомбы, а не от роста MDD.
const maxEntryBytes = 256 << 20

// Имена файлов меняются от релиза к релизу (MDD_v2.5_6904species.csv,
// Diff_v2.4-v2.5.csv), поэтому записи ищутся по шаблону. Diff-AllChanges
// шаблону Diff_ не подходит — он огромный и справочнику не нужен.
var (
	speciesFileRe = regexp.MustCompile(`^MDD_v[0-9][0-9.]*_[0-9]+species\.csv$`)
	diffFileRe    = regexp.MustCompile(`^Diff_(v[0-9][0-9.]*)-(v[0-9][0-9.]*)\.csv$`)
	versionRe     = regexp.MustCompile(`^MDD_(v[0-9][0-9.]*)_`)
)

// Parse разбирает архив MDD целиком в памяти. Из архива читаются только
// release.toml, файл видов и краткий список изменений; прочее (синонимы на
// 68 МБ, метаданные типовых экземпляров, мусор macOS) пропускается.
func Parse(data []byte) (*Dataset, error) {
	if len(data) == 0 {
		return nil, errors.New("архив MDD пуст")
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("архив MDD не читается как zip: %w", err)
	}

	var releaseFile, speciesFile, diffFile *zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || junkEntry(f.Name) {
			continue
		}
		base := path.Base(f.Name)
		switch {
		case base == "release.toml":
			releaseFile = f
		case speciesFileRe.MatchString(base):
			if speciesFile != nil {
				return nil, fmt.Errorf("в архиве MDD два файла видов: %s и %s", speciesFile.Name, f.Name)
			}
			speciesFile = f
		case diffFileRe.MatchString(base):
			if diffFile != nil {
				return nil, fmt.Errorf("в архиве MDD два файла изменений: %s и %s", diffFile.Name, f.Name)
			}
			diffFile = f
		}
	}
	if speciesFile == nil {
		return nil, errors.New("в архиве MDD нет файла видов MDD_v*_*species.csv")
	}

	d := &Dataset{}
	if releaseFile != nil {
		raw, err := readEntry(releaseFile)
		if err != nil {
			return nil, err
		}
		d.Release = parseReleaseTOML(raw)
	}
	// Без release.toml (или без версии в нём) версию даёт имя файла видов:
	// справочнику без версии не сказать, что вышел новый релиз.
	if d.Release.Version == "" {
		if m := versionRe.FindStringSubmatch(path.Base(speciesFile.Name)); m != nil {
			d.Release.Version = m[1]
		}
	}

	rc, err := openEntry(speciesFile)
	if err != nil {
		return nil, err
	}
	d.Species, err = parseSpecies(rc)
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path.Base(speciesFile.Name), err)
	}
	d.Release.Species = len(d.Species)

	// Файла изменений может не быть (скажем, у самого первого релиза) —
	// это не ошибка: справочник работает и без списка изменений.
	if diffFile != nil {
		m := diffFileRe.FindStringSubmatch(path.Base(diffFile.Name))
		prev, cur := m[1], m[2]
		d.Release.PrevVersion = prev
		rc, err := openEntry(diffFile)
		if err != nil {
			return nil, err
		}
		d.Changes, err = parseChanges(rc, prev, cur)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path.Base(diffFile.Name), err)
		}
	}
	return d, nil
}

// junkEntry — служебные записи, которые архиватор macOS кладёт рядом с
// файлами: каталог __MACOSX с «._имя» и .DS_Store. Файл «._MDD_v2.5_…csv»
// иначе совпал бы с шаблоном по базовому имени лишь случайно — отсекаем явно.
func junkEntry(name string) bool {
	if strings.HasPrefix(name, "__MACOSX/") || strings.Contains(name, "/__MACOSX/") {
		return true
	}
	base := path.Base(name)
	return base == ".DS_Store" || strings.HasPrefix(base, "._")
}

// openEntry открывает запись архива с пределом размера: заголовку zip
// о размере верить нельзя, поэтому предел ставится на сам поток.
func openEntry(f *zip.File) (io.ReadCloser, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("запись %s в архиве MDD не открывается: %w", f.Name, err)
	}
	return &limitedEntry{r: io.LimitReader(rc, maxEntryBytes+1), c: rc, name: f.Name}, nil
}

func readEntry(f *zip.File) ([]byte, error) {
	rc, err := openEntry(f)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// limitedEntry — поток записи, который превращает превышение предела в
// ошибку, а не в молча обрезанный CSV.
type limitedEntry struct {
	r    io.Reader
	c    io.Closer
	name string
	n    int64
}

func (l *limitedEntry) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.n += int64(n)
	if l.n > maxEntryBytes {
		return n, fmt.Errorf("запись %s в архиве MDD больше %d МБ", l.name, maxEntryBytes>>20)
	}
	if err != nil && err != io.EOF {
		err = fmt.Errorf("чтение записи %s в архиве MDD: %w", l.name, err)
	}
	return n, err
}

func (l *limitedEntry) Close() error { return l.c.Close() }

// parseReleaseTOML разбирает release.toml. Формат там простейший —
// «key = "value"» в секции [metadata], — и ради него не стоит тянуть
// библиотеку TOML. Неизвестные ключи и прочие секции пропускаются.
func parseReleaseTOML(data []byte) Release {
	var r Release
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.TrimSpace(strings.Trim(line, "[]"))
			continue
		}
		if section != "metadata" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = tomlValue(strings.TrimSpace(val))
		switch key {
		case "version":
			r.Version = val
		case "release_date":
			r.Date = val
		case "zenodo_citation", "citation":
			r.Citation = val
		case "remarks":
			r.Remarks = val
		}
	}
	return r
}

// tomlValue — значение строки TOML: строка в двойных кавычках (с экранами),
// в одинарных (как есть) или голое значение до комментария.
func tomlValue(v string) string {
	switch {
	case strings.HasPrefix(v, `"`):
		// Ищем закрывающую кавычку, пропуская экранированные: после неё
		// может идти комментарий.
		for i := 1; i < len(v); i++ {
			if v[i] == '\\' {
				i++
				continue
			}
			if v[i] == '"' {
				if s, err := strconv.Unquote(v[:i+1]); err == nil {
					return s
				}
				return v[1:i]
			}
		}
		return strings.TrimPrefix(v, `"`)
	case strings.HasPrefix(v, "'"):
		if i := strings.Index(v[1:], "'"); i >= 0 {
			return v[1 : i+1]
		}
		return strings.TrimPrefix(v, "'")
	default:
		if i := strings.Index(v, "#"); i >= 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
}

// columns — номера колонок CSV по имени заголовка: MDD не обещает порядок
// колонок, и между релизами он уже менялся.
type columns map[string]int

func newColumns(header []string) columns {
	c := make(columns, len(header))
	for i, h := range header {
		h = strings.TrimSpace(h)
		if i == 0 {
			h = strings.TrimPrefix(h, string(rune(0xFEFF))) // BOM, если CSV сохранили в Excel
		}
		if _, dup := c[h]; !dup {
			c[h] = i
		}
	}
	return c
}

// get — значение колонки; отсутствующая колонка и «NA» дают пустую строку.
func (c columns) get(rec []string, name string) string {
	i, ok := c[name]
	if !ok || i >= len(rec) {
		return ""
	}
	return na(rec[i])
}

func na(s string) string {
	s = strings.TrimSpace(s)
	if s == "NA" {
		return ""
	}
	return s
}

// requiredSpeciesColumns — без них вид не опознать и не разложить по
// систематике; остальные колонки необязательны и дают пустые поля.
var requiredSpeciesColumns = []string{"id", "sciName", "order", "family"}

func newCSVReader(r io.Reader) *csv.Reader {
	cr := csv.NewReader(r)
	// Число полей сверяется с заголовком: строка «съехала» — значит, файл
	// испорчен, и молча брать сдвинутые значения нельзя.
	cr.FieldsPerRecord = 0
	cr.ReuseRecord = true
	return cr
}

func parseSpecies(r io.Reader) ([]Species, error) {
	cr := newCSVReader(r)
	header, err := cr.Read()
	if err == io.EOF {
		return nil, errors.New("файл видов пуст")
	}
	if err != nil {
		return nil, fmt.Errorf("заголовок не разобрался: %w", err)
	}
	col := newColumns(header)
	for _, name := range requiredSpeciesColumns {
		if _, ok := col[name]; !ok {
			return nil, fmt.Errorf("нет обязательной колонки %q", name)
		}
	}

	var list []Species
	seen := make(map[int]string)
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("CSV не разобрался: %w", err)
		}
		line, _ := cr.FieldPos(0)
		s, err := speciesFromRecord(col, rec)
		if err != nil {
			return nil, fmt.Errorf("строка %d: %w", line, err)
		}
		if other, dup := seen[s.ID]; dup {
			return nil, fmt.Errorf("строка %d: id %d повторяется (%s и %s)", line, s.ID, other, s.SciName)
		}
		seen[s.ID] = s.SciName
		list = append(list, s)
	}
	if len(list) == 0 {
		return nil, errors.New("в файле видов нет ни одного вида")
	}
	return list, nil
}

func speciesFromRecord(col columns, rec []string) (Species, error) {
	var s Species
	id, err := strconv.Atoi(col.get(rec, "id"))
	if err != nil || id <= 0 {
		return s, fmt.Errorf("id %q не число", col.get(rec, "id"))
	}
	s.ID = id
	s.SciName = strings.ReplaceAll(col.get(rec, "sciName"), "_", " ")
	if s.SciName == "" {
		return s, fmt.Errorf("у id %d пустое sciName", id)
	}
	// phylosort и год — вспомогательные: кривое значение даёт 0, а не
	// отказ от всего релиза.
	s.Phylosort, _ = strconv.Atoi(col.get(rec, "phylosort"))
	s.CommonName = col.get(rec, "mainCommonName")
	s.OtherCommonNames = splitList(col.get(rec, "otherCommonNames"))

	s.Order = col.get(rec, "order")
	s.Family = col.get(rec, "family")
	s.Subfamily = col.get(rec, "subfamily")
	s.Genus = col.get(rec, "genus")
	s.Epithet = col.get(rec, "specificEpithet")

	author := col.get(rec, "authoritySpeciesAuthor")
	year := col.get(rec, "authoritySpeciesYear")
	s.Year, _ = strconv.Atoi(year)
	s.Authority = authority(author, year, col.get(rec, "authorityParentheses") == "1")

	s.IUCN = iucnCode(col.get(rec, "iucnStatus"))
	s.Extinct = col.get(rec, "extinct") == "1"
	s.Domestic = col.get(rec, "domestic") == "1"

	s.Countries, s.CountriesUncertain = splitCountries(col.get(rec, "countryDistribution"))
	s.Continents = splitList(col.get(rec, "continentDistribution"))
	s.Realms = splitList(col.get(rec, "biogeographicRealm"))

	s.TypeLocality = col.get(rec, "typeLocality")
	s.DistributionNotes = col.get(rec, "distributionNotes")
	s.TaxonomyNotes = col.get(rec, "taxonomyNotes")
	return s, nil
}

// authority — «Автор, год»; скобки по правилам номенклатуры означают, что
// вид описан в другом роде.
func authority(author, year string, parens bool) string {
	a := author
	switch {
	case author == "" && year == "":
		return ""
	case author == "":
		a = year
	case year != "":
		a = author + ", " + year
	}
	if parens {
		a = "(" + a + ")"
	}
	return a
}

// iucnCode — код статуса МСОП. У вида, которого МСОП оценивал под другим
// именем, MDD пишет «NT (as Bison bison)»; справочнику и фильтру по статусу
// нужен сам код, пояснение отбрасывается.
func iucnCode(s string) string {
	if i := strings.IndexAny(s, " ("); i > 0 {
		return s[:i]
	}
	return s
}

// splitList — список через «|» без пустых элементов и «NA».
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, "|") {
		if p = na(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitCountries делит страны на достоверные и сомнительные: «Azerbaijan?»
// в MDD значит «возможно, есть», и знак снимается, чтобы по стране можно
// было искать точным именем. «Domesticated» у домашних видов — не страна,
// а пометка: её несёт флаг Domestic, в списке стран она только мешала бы
// поиску и сводкам по странам.
func splitCountries(s string) (sure, uncertain []string) {
	for _, c := range splitList(s) {
		if strings.EqualFold(c, "Domesticated") {
			continue
		}
		if strings.Contains(c, "?") {
			if c = strings.TrimSpace(strings.ReplaceAll(c, "?", "")); c != "" {
				uncertain = append(uncertain, c)
			}
			continue
		}
		sure = append(sure, c)
	}
	return sure, uncertain
}

// parseChanges разбирает Diff_vA-vB.csv. Колонки названий называются по
// версиям («MDDv2.4_Name,MDDv2.5_Name»), поэтому ищутся по версиям из
// имени файла, а при расхождении — первые две колонки «…_Name».
func parseChanges(r io.Reader, prev, cur string) ([]Change, error) {
	cr := newCSVReader(r)
	header, err := cr.Read()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("заголовок не разобрался: %w", err)
	}
	col := newColumns(header)
	oldCol, newCol := "MDD"+prev+"_Name", "MDD"+cur+"_Name"
	_, okOld := col[oldCol]
	_, okNew := col[newCol]
	if !okOld || !okNew {
		var names []string
		for i, h := range header {
			if h = strings.TrimSpace(h); strings.HasSuffix(h, "_Name") && col[h] == i {
				names = append(names, h)
			}
		}
		if len(names) < 2 {
			return nil, fmt.Errorf("нет колонок %q и %q", oldCol, newCol)
		}
		oldCol, newCol = names[0], names[1]
	}

	var list []Change
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("CSV не разобрался: %w", err)
		}
		c := Change{
			OldName:   strings.ReplaceAll(col.get(rec, oldCol), "_", " "),
			NewName:   strings.ReplaceAll(col.get(rec, newCol), "_", " "),
			Comment:   col.get(rec, "Comment"),
			Category:  col.get(rec, "Category"),
			Reference: col.get(rec, "Reference"),
		}
		if c.OldName == "" && c.NewName == "" {
			continue
		}
		list = append(list, c)
	}
	return list, nil
}
