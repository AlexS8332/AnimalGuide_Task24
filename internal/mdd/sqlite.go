package mdd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
)

// SQLiteComponent — имя компонента в schema_migrations: версия схемы справочника
// ведётся отдельно от таблиц фактов, планировщика и прочих.
const SQLiteComponent = "mdd"

// sqliteSteps — миграции схемы справочника. Только дописывать в конец:
// применённый шаг повторно не выполняется, правка старого до уже
// созданных баз не дойдёт.
//
// Устройство:
//   - mdd_species — вид одной строкой. Списки (прочие названия, страны,
//     континенты, области) — JSON-текстом: вид читается одним запросом без
//     соединений, а искать по ним помогают отдельные таблицы ниже;
//   - mdd_name — нормализованные названия (нижний регистр, «_» → пробел):
//     точный поиск Find идёт по индексу, поиск подстроки — по короткой
//     таблице, а не по JSON;
//   - mdd_country, mdd_realm — страна и область как ключ с индексом: поиск
//     по стране — точное сравнение, а не LIKE по строке со списком;
//   - mdd_release — одна строка (id = 1);
//   - mdd_change — изменения текущего релиза в порядке файла.
//
// Столбцы, по которым фильтрует Search без нормализации в Go (отряд,
// семейство, род, МСОП, категория изменения), объявлены COLLATE NOCASE:
// сравнение без учёта регистра и индекс работают вместе. NOCASE знает
// только ASCII — латыни и кодам статусов этого хватает.
var sqliteSteps = []string{
	`CREATE TABLE mdd_species (
		id                  INTEGER PRIMARY KEY,
		phylosort           INTEGER NOT NULL,
		sci_name            TEXT NOT NULL,
		common_name         TEXT NOT NULL,
		other_names         TEXT NOT NULL,
		ord                 TEXT NOT NULL COLLATE NOCASE,
		family              TEXT NOT NULL COLLATE NOCASE,
		subfamily           TEXT NOT NULL,
		genus               TEXT NOT NULL COLLATE NOCASE,
		epithet             TEXT NOT NULL,
		authority           TEXT NOT NULL,
		year                INTEGER NOT NULL,
		iucn                TEXT NOT NULL COLLATE NOCASE,
		extinct             INTEGER NOT NULL,
		domestic            INTEGER NOT NULL,
		countries           TEXT NOT NULL,
		countries_uncertain TEXT NOT NULL,
		continents          TEXT NOT NULL,
		realms              TEXT NOT NULL,
		type_locality       TEXT NOT NULL,
		distribution_notes  TEXT NOT NULL,
		taxonomy_notes      TEXT NOT NULL
	);
	CREATE INDEX mdd_species_phylosort ON mdd_species (phylosort, id);
	CREATE INDEX mdd_species_ord ON mdd_species (ord);
	CREATE INDEX mdd_species_family ON mdd_species (family);
	CREATE INDEX mdd_species_genus ON mdd_species (genus);

	CREATE TABLE mdd_name (
		species_id INTEGER NOT NULL REFERENCES mdd_species (id) ON DELETE CASCADE,
		name       TEXT NOT NULL,
		kind       INTEGER NOT NULL, -- 0 латинское, 1 основное английское, 2 прочее
		PRIMARY KEY (species_id, name)
	);
	CREATE INDEX mdd_name_name ON mdd_name (name);

	CREATE TABLE mdd_country (
		country    TEXT NOT NULL, -- нормализовано: нижний регистр
		species_id INTEGER NOT NULL REFERENCES mdd_species (id) ON DELETE CASCADE,
		uncertain  INTEGER NOT NULL,
		PRIMARY KEY (country, species_id)
	);
	CREATE INDEX mdd_country_species ON mdd_country (species_id);

	CREATE TABLE mdd_realm (
		realm      TEXT NOT NULL, -- нормализовано: нижний регистр
		species_id INTEGER NOT NULL REFERENCES mdd_species (id) ON DELETE CASCADE,
		PRIMARY KEY (realm, species_id)
	);
	CREATE INDEX mdd_realm_species ON mdd_realm (species_id);

	CREATE TABLE mdd_release (
		id           INTEGER PRIMARY KEY CHECK (id = 1),
		version      TEXT NOT NULL,
		release_date TEXT NOT NULL,
		citation     TEXT NOT NULL,
		remarks      TEXT NOT NULL,
		etag         TEXT NOT NULL,
		species      INTEGER NOT NULL,
		loaded_at    TEXT NOT NULL,
		source_url   TEXT NOT NULL,
		prev_version TEXT NOT NULL
	);

	CREATE TABLE mdd_change (
		seq       INTEGER PRIMARY KEY,
		old_name  TEXT NOT NULL,
		new_name  TEXT NOT NULL,
		comment   TEXT NOT NULL,
		category  TEXT NOT NULL COLLATE NOCASE,
		reference TEXT NOT NULL
	);
	CREATE INDEX mdd_change_category ON mdd_change (category);`,
}

// SQLite — Store поверх общей базы приложения. Базу открывает и закрывает
// владелец (db.Open): в ней живут и чужие таблицы.
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// NewSQLite доводит схему справочника до текущей версии и возвращает
// хранилище.
func NewSQLite(ctx context.Context, conn *sql.DB) (*SQLite, error) {
	if conn == nil {
		return nil, errors.New("mdd: база не открыта")
	}
	if err := db.Migrate(ctx, conn, SQLiteComponent, sqliteSteps); err != nil {
		return nil, fmt.Errorf("mdd: схема справочника: %w", err)
	}
	return &SQLite{db: conn}, nil
}

// sqliteKey — ключ для сравнения названий и стран: нижний регистр, «_» как
// пробел, пробелы схлопнуты. Одна функция и для записи, и для запроса —
// иначе «Otocolobus_manul» и «otocolobus  manul» разошлись бы. Регистр
// снимается в Go, а не lower() в SQLite: тот понимает только ASCII.
func sqliteKey(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(s, "_", " "))), " ")
}

// sqliteList — список в JSON-текст; пустой — пустая строка.
func sqliteList(v []string) string {
	if len(v) == 0 {
		return ""
	}
	b, _ := json.Marshal(v) // срез строк маршалится всегда
	return string(b)
}

func sqliteUnlist(s, field string, id int) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var v []string
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("mdd: вид %d, поле %s повреждено: %w", id, field, err)
	}
	return v, nil
}

func sqliteBool(b bool) int {
	if b {
		return 1
	}
	return 0
}

// sqliteCols — столбцы вида в порядке, который ждёт sqliteScan.
const sqliteCols = `s.id, s.phylosort, s.sci_name, s.common_name, s.other_names,
	s.ord, s.family, s.subfamily, s.genus, s.epithet, s.authority, s.year,
	s.iucn, s.extinct, s.domestic, s.countries, s.countries_uncertain,
	s.continents, s.realms, s.type_locality, s.distribution_notes, s.taxonomy_notes`

// sqliteScan читает вид из строки с sqliteCols; extra — столбцы после них.
func sqliteScan(row interface{ Scan(...any) error }, extra ...any) (Species, error) {
	var (
		s                             Species
		other, cs, csu, conts, realms string
		extinct, domestic             int
	)
	dest := []any{&s.ID, &s.Phylosort, &s.SciName, &s.CommonName, &other,
		&s.Order, &s.Family, &s.Subfamily, &s.Genus, &s.Epithet, &s.Authority, &s.Year,
		&s.IUCN, &extinct, &domestic, &cs, &csu,
		&conts, &realms, &s.TypeLocality, &s.DistributionNotes, &s.TaxonomyNotes}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return Species{}, err
	}
	s.Extinct, s.Domestic = extinct != 0, domestic != 0
	var err error
	if s.OtherCommonNames, err = sqliteUnlist(other, "other_names", s.ID); err != nil {
		return Species{}, err
	}
	if s.Countries, err = sqliteUnlist(cs, "countries", s.ID); err != nil {
		return Species{}, err
	}
	if s.CountriesUncertain, err = sqliteUnlist(csu, "countries_uncertain", s.ID); err != nil {
		return Species{}, err
	}
	if s.Continents, err = sqliteUnlist(conts, "continents", s.ID); err != nil {
		return Species{}, err
	}
	if s.Realms, err = sqliteUnlist(realms, "realms", s.ID); err != nil {
		return Species{}, err
	}
	return s, nil
}

// Release — текущий релиз; ErrNotFound, если справочник ещё не загружен.
func (st *SQLite) Release(ctx context.Context) (Release, error) {
	var (
		r      Release
		loaded string
	)
	err := st.db.QueryRowContext(ctx, `SELECT version, release_date, citation, remarks, etag,
		species, loaded_at, source_url, prev_version FROM mdd_release WHERE id = 1`).
		Scan(&r.Version, &r.Date, &r.Citation, &r.Remarks, &r.ETag,
			&r.Species, &loaded, &r.SourceURL, &r.PrevVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return Release{}, fmt.Errorf("релиз: %w", ErrNotFound)
	}
	if err != nil {
		return Release{}, fmt.Errorf("mdd: релиз: %w", err)
	}
	if loaded != "" {
		if r.LoadedAt, err = time.Parse(time.RFC3339Nano, loaded); err != nil {
			return Release{}, fmt.Errorf("mdd: релиз: время загрузки %q: %w", loaded, err)
		}
	}
	return r, nil
}

// Replace заменяет справочник целиком одной транзакцией. Благодаря WAL
// читатели до фиксации видят старый релиз, после — новый, и никогда —
// полупустую базу.
//
// Release.Species записывается как число видов в наборе: это то, что
// действительно загружено. Пустой LoadedAt заполняется текущим временем.
// Пустой набор видов — ошибка: скорее всего, сломался разбор архива, и
// стирать рабочий справочник из-за этого нельзя.
func (st *SQLite) Replace(ctx context.Context, d *Dataset) error {
	if d == nil || len(d.Species) == 0 {
		return errors.New("mdd: замена справочника: в наборе нет видов")
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mdd: замена справочника: %w", err)
	}
	defer tx.Rollback()

	// Дочерние таблицы — явно и первыми: так быстрее, чем каскад по
	// каждому виду.
	for _, t := range []string{"mdd_name", "mdd_country", "mdd_realm", "mdd_species", "mdd_change", "mdd_release"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("mdd: замена справочника: очистка %s: %w", t, err)
		}
	}
	if err := sqliteInsertSpecies(ctx, tx, d.Species); err != nil {
		return err
	}
	if err := sqliteInsertChanges(ctx, tx, d.Changes); err != nil {
		return err
	}

	r := d.Release
	r.Species = len(d.Species)
	if r.LoadedAt.IsZero() {
		r.LoadedAt = time.Now().UTC()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mdd_release (id, version, release_date,
		citation, remarks, etag, species, loaded_at, source_url, prev_version)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Version, r.Date, r.Citation, r.Remarks, r.ETag, r.Species,
		r.LoadedAt.Format(time.RFC3339Nano), r.SourceURL, r.PrevVersion); err != nil {
		return fmt.Errorf("mdd: замена справочника: релиз: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mdd: замена справочника: фиксация: %w", err)
	}
	return nil
}

func sqliteInsertSpecies(ctx context.Context, tx *sql.Tx, list []Species) error {
	insSpecies, err := tx.PrepareContext(ctx, `INSERT INTO mdd_species (id, phylosort,
		sci_name, common_name, other_names, ord, family, subfamily, genus, epithet,
		authority, year, iucn, extinct, domestic, countries, countries_uncertain,
		continents, realms, type_locality, distribution_notes, taxonomy_notes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("mdd: замена справочника: %w", err)
	}
	defer insSpecies.Close()
	insName, err := tx.PrepareContext(ctx, `INSERT INTO mdd_name (species_id, name, kind) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("mdd: замена справочника: %w", err)
	}
	defer insName.Close()
	insCountry, err := tx.PrepareContext(ctx, `INSERT INTO mdd_country (country, species_id, uncertain) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("mdd: замена справочника: %w", err)
	}
	defer insCountry.Close()
	insRealm, err := tx.PrepareContext(ctx, `INSERT INTO mdd_realm (realm, species_id) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("mdd: замена справочника: %w", err)
	}
	defer insRealm.Close()

	for i := range list {
		s := &list[i]
		if _, err := insSpecies.ExecContext(ctx, s.ID, s.Phylosort, s.SciName, s.CommonName,
			sqliteList(s.OtherCommonNames), s.Order, s.Family, s.Subfamily, s.Genus, s.Epithet,
			s.Authority, s.Year, strings.ToUpper(strings.TrimSpace(s.IUCN)),
			sqliteBool(s.Extinct), sqliteBool(s.Domestic),
			sqliteList(s.Countries), sqliteList(s.CountriesUncertain),
			sqliteList(s.Continents), sqliteList(s.Realms),
			s.TypeLocality, s.DistributionNotes, s.TaxonomyNotes); err != nil {
			return fmt.Errorf("mdd: замена справочника: вид %d (%s): %w", s.ID, s.SciName, err)
		}

		// Одно название может повториться (основное среди прочих) —
		// записываем его один раз, с самым «сильным» видом.
		seen := make(map[string]bool, 2+len(s.OtherCommonNames))
		addName := func(name string, kind int) error {
			k := sqliteKey(name)
			if k == "" || seen[k] {
				return nil
			}
			seen[k] = true
			if _, err := insName.ExecContext(ctx, s.ID, k, kind); err != nil {
				return fmt.Errorf("mdd: замена справочника: вид %d, название %q: %w", s.ID, name, err)
			}
			return nil
		}
		if err := addName(s.SciName, 0); err != nil {
			return err
		}
		if err := addName(s.CommonName, 1); err != nil {
			return err
		}
		for _, n := range s.OtherCommonNames {
			if err := addName(n, 2); err != nil {
				return err
			}
		}

		// Страна и в точном, и в «?»-списке — считаем точной.
		countries := make(map[string]bool, len(s.Countries)+len(s.CountriesUncertain))
		for _, c := range s.Countries {
			k := sqliteKey(c)
			if k == "" || countries[k] {
				continue
			}
			countries[k] = true
			if _, err := insCountry.ExecContext(ctx, k, s.ID, 0); err != nil {
				return fmt.Errorf("mdd: замена справочника: вид %d, страна %q: %w", s.ID, c, err)
			}
		}
		for _, c := range s.CountriesUncertain {
			k := sqliteKey(c)
			if k == "" || countries[k] {
				continue
			}
			countries[k] = true
			if _, err := insCountry.ExecContext(ctx, k, s.ID, 1); err != nil {
				return fmt.Errorf("mdd: замена справочника: вид %d, страна %q: %w", s.ID, c, err)
			}
		}

		realms := make(map[string]bool, len(s.Realms))
		for _, r := range s.Realms {
			k := sqliteKey(r)
			if k == "" || realms[k] {
				continue
			}
			realms[k] = true
			if _, err := insRealm.ExecContext(ctx, k, s.ID); err != nil {
				return fmt.Errorf("mdd: замена справочника: вид %d, область %q: %w", s.ID, r, err)
			}
		}
	}
	return nil
}

func sqliteInsertChanges(ctx context.Context, tx *sql.Tx, list []Change) error {
	if len(list) == 0 {
		return nil
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO mdd_change (seq, old_name, new_name,
		comment, category, reference) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("mdd: замена справочника: %w", err)
	}
	defer ins.Close()
	for i, c := range list {
		if _, err := ins.ExecContext(ctx, i+1, c.OldName, c.NewName, c.Comment, c.Category, c.Reference); err != nil {
			return fmt.Errorf("mdd: замена справочника: изменение %d: %w", i+1, err)
		}
	}
	return nil
}

// Get — вид по mdd-id.
func (st *SQLite) Get(ctx context.Context, id int) (Species, error) {
	s, err := sqliteScan(st.db.QueryRowContext(ctx,
		`SELECT `+sqliteCols+` FROM mdd_species s WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Species{}, fmt.Errorf("вид %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Species{}, fmt.Errorf("mdd: вид %d: %w", id, err)
	}
	return s, nil
}

// Find — вид по точному названию. Если одно название носят несколько видов
// (прочие английские названия бывают общими), побеждает латинское, затем
// основное английское, затем — первый в систематическом порядке.
func (st *SQLite) Find(ctx context.Context, name string) (Species, error) {
	k := sqliteKey(name)
	if k == "" {
		return Species{}, fmt.Errorf("пустое название: %w", ErrNotFound)
	}
	s, err := sqliteScan(st.db.QueryRowContext(ctx, `SELECT `+sqliteCols+`
		FROM mdd_name n JOIN mdd_species s ON s.id = n.species_id
		WHERE n.name = ? ORDER BY n.kind, s.phylosort, s.id LIMIT 1`, k))
	if errors.Is(err, sql.ErrNoRows) {
		return Species{}, fmt.Errorf("вид %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return Species{}, fmt.Errorf("mdd: вид %q: %w", name, err)
	}
	return s, nil
}

// Пределы выдачи Search.
const (
	sqliteDefaultLimit = 20
	sqliteMaxLimit     = 100
)

// Search — поиск по Query. Выборка и total считаются одним выражением: оно
// видит один снимок базы, и Replace между ними не вклинится.
func (st *SQLite) Search(ctx context.Context, q Query) ([]Species, int, error) {
	var (
		where []string
		args  []any
	)
	if k := sqliteKey(q.Text); k != "" {
		where = append(where, `EXISTS (SELECT 1 FROM mdd_name n WHERE n.species_id = s.id AND instr(n.name, ?) > 0)`)
		args = append(args, k)
	}
	for _, f := range []struct{ col, val string }{
		{"s.ord", q.Order}, {"s.family", q.Family}, {"s.genus", q.Genus},
	} {
		if v := strings.TrimSpace(f.val); v != "" {
			where = append(where, f.col+` = ?`)
			args = append(args, v)
		}
	}
	if k := sqliteKey(q.Country); k != "" {
		where = append(where, `s.id IN (SELECT species_id FROM mdd_country WHERE country = ?)`)
		args = append(args, k)
	}
	if k := sqliteKey(q.Realm); k != "" {
		where = append(where, `s.id IN (SELECT species_id FROM mdd_realm WHERE realm = ?)`)
		args = append(args, k)
	}
	var iucn []string
	for _, v := range q.IUCN {
		if v = strings.ToUpper(strings.TrimSpace(v)); v != "" {
			iucn = append(iucn, v)
		}
	}
	if len(iucn) > 0 {
		where = append(where, `s.iucn IN (?`+strings.Repeat(`, ?`, len(iucn)-1)+`)`)
		for _, v := range iucn {
			args = append(args, v)
		}
	}
	if q.Extinct != nil {
		where = append(where, `s.extinct = ?`)
		args = append(args, sqliteBool(*q.Extinct))
	}
	if q.Domestic != nil {
		where = append(where, `s.domestic = ?`)
		args = append(args, sqliteBool(*q.Domestic))
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}

	limit := q.Limit
	if limit <= 0 {
		limit = sqliteDefaultLimit
	}
	if limit > sqliteMaxLimit {
		limit = sqliteMaxLimit
	}
	offset := max(q.Offset, 0)

	// Подзапрос total не связан с внешним (свой псевдоним s), поэтому
	// SQLite считает его один раз.
	all := append(append([]any{}, args...), args...)
	all = append(all, limit, offset)
	rows, err := st.db.QueryContext(ctx, `SELECT `+sqliteCols+`,
		(SELECT count(*) FROM mdd_species s`+cond+`)
		FROM mdd_species s`+cond+`
		ORDER BY s.phylosort, s.id LIMIT ? OFFSET ?`, all...)
	if err != nil {
		return nil, 0, fmt.Errorf("mdd: поиск: %w", err)
	}
	defer rows.Close()

	list := make([]Species, 0, limit)
	total := 0
	for rows.Next() {
		s, err := sqliteScan(rows, &total)
		if err != nil {
			return nil, 0, fmt.Errorf("mdd: поиск: %w", err)
		}
		list = append(list, s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("mdd: поиск: %w", err)
	}
	rows.Close()

	// Страница за концом выдачи строк не дала — total считаем отдельно.
	if len(list) == 0 && offset > 0 {
		if err := st.db.QueryRowContext(ctx, `SELECT count(*) FROM mdd_species s`+cond, args...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("mdd: поиск: подсчёт: %w", err)
		}
	}
	return list, total, nil
}

// Changes — изменения текущего релиза в порядке файла; category пусто —
// все, иначе без учёта регистра; limit ≤ 0 — без ограничения.
func (st *SQLite) Changes(ctx context.Context, category string, limit int) ([]Change, error) {
	query := `SELECT old_name, new_name, comment, category, reference FROM mdd_change`
	var args []any
	if c := strings.TrimSpace(category); c != "" {
		query += ` WHERE category = ?`
		args = append(args, c)
	}
	query += ` ORDER BY seq`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := st.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("mdd: изменения: %w", err)
	}
	defer rows.Close()
	out := make([]Change, 0)
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.OldName, &c.NewName, &c.Comment, &c.Category, &c.Reference); err != nil {
			return nil, fmt.Errorf("mdd: изменения: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mdd: изменения: %w", err)
	}
	return out, nil
}

// IDs — mdd-id всех видов по возрастанию.
func (st *SQLite) IDs(ctx context.Context) ([]int, error) {
	rows, err := st.db.QueryContext(ctx, `SELECT id FROM mdd_species ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("mdd: список id: %w", err)
	}
	defer rows.Close()
	out := make([]int, 0, 8192)
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("mdd: список id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mdd: список id: %w", err)
	}
	return out, nil
}
