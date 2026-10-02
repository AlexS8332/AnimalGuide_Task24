// Package mdd — справочный слой Mammal Diversity Database (MDD): эталонный
// список видов млекопитающих, который выпускает Американское общество
// маммалогов (https://www.mammaldiversity.org).
//
// API у сайта нет, зато весь набор данных лежит одним архивом в их
// GitHub-репозитории: CSV со всеми видами, список изменений относительно
// прошлого релиза и release.toml с версией. Пакет скачивает архив (с
// проверкой ETag — релизы выходят раз в несколько месяцев), разбирает его и
// кладёт в SQLite; инструменты отдают из базы вид, поиск и изменения.
//
// Три части пакета:
//   - загрузка и разбор архива — Fetch и Parse;
//   - хранилище — интерфейс Store, реализация на SQLite — SQLite;
//   - инструменты для модели и MCP — Tools.
package mdd

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// DefaultURL — архив набора данных. raw.githubusercontent.com отдаёт ETag:
// по нему проверка «не вышел ли релиз» стоит одного запроса без тела.
const DefaultURL = "https://raw.githubusercontent.com/mammaldiversity/mammaldiversity.github.io/refs/heads/master/assets/data/MDD.zip"

// SiteBase — страница вида на сайте: SiteBase + "/taxon/" + id.
const SiteBase = "https://www.mammaldiversity.org"

// ErrNotFound — вида (или релиза) нет в базе.
var ErrNotFound = errors.New("mdd: не найдено")

// Species — один вид. Поля — подмножество 52 колонок MDD_v*_species.csv,
// нужное справочнику; «NA» в CSV превращается в пустую строку.
type Species struct {
	// ID — mdd-id, устойчивый между релизами (колонка id).
	ID int `json:"id"`
	// Phylosort — систематический порядок MDD (колонка phylosort).
	Phylosort int `json:"-"`
	// SciName — латинское название через пробел: «Otocolobus manul»
	// (в CSV — через подчёркивание, колонка sciName).
	SciName string `json:"sci_name"`
	// CommonName — основное английское название (mainCommonName).
	CommonName string `json:"common_name,omitempty"`
	// OtherCommonNames — прочие английские названия (otherCommonNames, через «|»).
	OtherCommonNames []string `json:"other_common_names,omitempty"`

	Order     string `json:"order"`
	Family    string `json:"family"`
	Subfamily string `json:"subfamily,omitempty"`
	Genus     string `json:"genus"`
	Epithet   string `json:"epithet"` // specificEpithet

	// Authority — «Linnaeus, 1758»; в скобках, если authorityParentheses=1
	// (вид описан в другом роде): «(Pallas, 1776)».
	Authority string `json:"authority,omitempty"`
	Year      int    `json:"year,omitempty"`

	// IUCN — код статуса МСОП: LC, NT, VU, EN, CR, EW, EX, DD, NE. В CSV
	// бывает «NT (as Bison bison)» — оценка под другим именем; пояснение
	// отбрасывается, иначе фильтр по статусу таких видов не находит.
	IUCN     string `json:"iucn,omitempty"`
	Extinct  bool   `json:"extinct"`  // extinct = 1
	Domestic bool   `json:"domestic"` // domestic = 1

	// Countries — страны, где вид точно есть; CountriesUncertain — страны
	// со знаком «?» в countryDistribution (знак снимается). Пометка
	// «Domesticated» у домашних видов страной не считается и отбрасывается.
	Countries          []string `json:"countries,omitempty"`
	CountriesUncertain []string `json:"countries_uncertain,omitempty"`
	Continents         []string `json:"continents,omitempty"` // continentDistribution
	Realms             []string `json:"realms,omitempty"`     // biogeographicRealm

	TypeLocality      string `json:"type_locality,omitempty"`
	DistributionNotes string `json:"distribution_notes,omitempty"`
	TaxonomyNotes     string `json:"taxonomy_notes,omitempty"`
}

// URL — страница вида на сайте MDD.
func (s Species) URL() string { return SiteBase + "/taxon/" + strconv.Itoa(s.ID) + "/" }

// Release — релиз набора данных (release.toml + сведения о загрузке).
type Release struct {
	Version     string    `json:"version"`      // «v2.5»
	Date        string    `json:"release_date"` // «2026-07-28», как в release.toml
	Citation    string    `json:"citation,omitempty"`
	Remarks     string    `json:"remarks,omitempty"`
	ETag        string    `json:"etag,omitempty"` // ETag архива, из которого загружено
	Species     int       `json:"species"`        // сколько видов загружено
	LoadedAt    time.Time `json:"loaded_at"`
	SourceURL   string    `json:"source_url,omitempty"`
	PrevVersion string    `json:"prev_version,omitempty"` // версия, с которой сравнивает Diff (из имени файла Diff_v2.4-v2.5.csv)
}

// Change — строка Diff_vA-vB.csv: что изменилось в систематике между
// соседними релизами. OldName пусто — вид новый; NewName пусто — вид убран.
type Change struct {
	OldName   string `json:"old_name,omitempty"` // через пробел, «NA» → ""
	NewName   string `json:"new_name,omitempty"`
	Comment   string `json:"comment,omitempty"`
	Category  string `json:"category,omitempty"` // «de novo», «split», «lump», …
	Reference string `json:"reference,omitempty"`
}

// Dataset — разобранный архив целиком.
type Dataset struct {
	Release Release
	Species []Species
	Changes []Change
}

// Query — поиск видов. Пустые поля не ограничивают. Строки сравниваются
// без учёта регистра.
type Query struct {
	// Text — подстрока латинского или английского названия (основного или
	// прочих). Подчёркивание равно пробелу.
	Text    string
	Order   string
	Family  string
	Genus   string
	Country string // точное имя страны, как в MDD: «Kazakhstan»; ищет и среди «?»
	Realm   string
	IUCN    []string // любой из статусов
	// Extinct и Domestic: nil — не важно.
	Extinct  *bool
	Domestic *bool

	Limit  int // 0 — по умолчанию 20; предел 100
	Offset int
}

// Store — хранилище справочного слоя. Реализация в продукте — SQLite,
// в тестах инструментов — Memory.
type Store interface {
	// Release — текущий загруженный релиз; ErrNotFound, если базы ещё нет.
	Release(ctx context.Context) (Release, error)
	// Replace заменяет набор данных целиком одной транзакцией: читатели
	// видят либо старый релиз, либо новый. Пустой набор (nil или без видов)
	// — ошибка: сломанный разбор не должен стереть справочник.
	// Release.Species ставит сам Replace (число видов), пустой LoadedAt —
	// текущим временем.
	Replace(ctx context.Context, d *Dataset) error
	// Get — вид по mdd-id; ErrNotFound, если нет.
	Get(ctx context.Context, id int) (Species, error)
	// Find — вид по точному названию: латинскому (пробел или «_»), основному
	// или прочему английскому; без учёта регистра и пробелов по краям.
	// Прочие английские названия бывают общими у нескольких видов: тогда
	// латинское важнее основного, основное — прочего, а при равенстве
	// выигрывает первый в систематическом порядке. ErrNotFound, если нет.
	Find(ctx context.Context, name string) (Species, error)
	// Search — поиск; total — сколько всего подходит без Limit/Offset.
	// Limit 0 → 20 и предел 100 применяет само хранилище.
	// Порядок — phylosort (систематический порядок MDD), затем id.
	Search(ctx context.Context, q Query) (list []Species, total int, err error)
	// Changes — изменения текущего релиза в порядке Diff-файла; category
	// без учёта регистра, пусто — все; limit ≤ 0 — без ограничения.
	Changes(ctx context.Context, category string, limit int) ([]Change, error)
	// IDs — mdd-id всех видов: из них планировщик выбирает случайный.
	IDs(ctx context.Context) ([]int, error)
}
