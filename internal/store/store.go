// Package store — файлы данных и их версии. Он знает про каталоги, JSON,
// номер формата и миграции, но не знает ни одного вида данных: диалог,
// подборка, профиль и свод объявляют себя описанием Kind, а читают и пишут
// через общий Dir.
//
// Правила формата (ТЗ, 4.12):
//
//   - каждый файл несёт "schema": N — номер версии формата, свой у каждого
//     вида файла; файл без поля считается версией 1;
//   - чтение поднимает старую версию до текущей цепочкой миграций, запись
//     всегда в текущей версии;
//   - перед первой перезаписью старого формата рядом кладётся копия
//     <имя>.v<N>.bak — чтение само по себе файл не трогает;
//   - файл версии новее текущей не читается вовсе, битый — пропускается с
//     предупреждением, а не роняет приложение.
//
// Поле schema вставляет сама запись: у типов данных его нет, и забыть его
// выставить нельзя.
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/paths"
)

// SchemaKey — имя поля с номером формата.
const SchemaKey = "schema"

var (
	// ErrNewer — файл записан программой новее этой. Читать его нельзя:
	// поля, смысла которых код не знает, при записи молча пропали бы.
	ErrNewer = errors.New("файл записан более новой версией программы")
	// ErrCorrupt — файл не разбирается как JSON-объект.
	ErrCorrupt = errors.New("файл повреждён")
	// ErrBadKey — ключ не годится как имя файла.
	ErrBadKey = errors.New("некорректный идентификатор")
	// ErrNoMigration — в цепочке миграций дыра. Это ошибка программиста,
	// но упасть приложению из-за неё нельзя: файл просто не читается.
	ErrNoMigration = errors.New("нет миграции формата")
)

// Migration поднимает документ ровно на одну версию: из N в N+1. Работает
// над сырым объектом — типы данных знают только текущий формат, а старый
// описан здесь, рядом с тем, кто его помнит.
type Migration func(doc map[string]json.RawMessage) error

// Kind — вид файла: где лежит, какой формат текущий и как поднять старый.
type Kind struct {
	// Name — вид файла словами для ошибок и журнала: «диалог», «подборка».
	Name string
	// Dir — каталог относительно корня хранилища.
	Dir string
	// Current — текущая версия формата, от 1.
	Current int
	// Migrate — миграции по исходной версии: Migrate[1] поднимает 1 → 2.
	Migrate map[int]Migration
	// ValidKey — годится ли ключ как имя файла; пусто — paths.ValidID.
	ValidKey func(string) bool
}

func (k *Kind) valid(key string) bool {
	if k.ValidKey != nil {
		return k.ValidKey(key)
	}
	return paths.ValidID(key)
}

func (k *Kind) current() int {
	if k.Current < 1 {
		return 1
	}
	return k.Current
}

// Meta — что было с файлом при чтении.
type Meta struct {
	// Schema — версия формата на диске.
	Schema int `json:"schema"`
	// Migrated — файл поднят миграцией и при следующей записи сменит
	// формат (перед этим рядом ляжет копия).
	Migrated bool `json:"migrated,omitempty"`
}

// Dir — корень хранилища. Все операции атомарны относительно друг друга:
// запись идёт во временный файл и переименованием, чтобы обрыв процесса не
// оставил полуфайл вместо данных.
type Dir struct {
	mu   sync.Mutex
	root string
}

// NewDir привязывает хранилище к каталогу; сам каталог создаётся при первой
// записи.
func NewDir(root string) *Dir {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Dir{root: root}
}

// Root — корень хранилища.
func (d *Dir) Root() string { return d.root }

// KindDir — каталог вида файлов.
func (d *Dir) KindDir(k *Kind) string { return filepath.Join(d.root, k.Dir) }

// Path — файл по ключу.
func (d *Dir) Path(k *Kind, key string) string {
	return filepath.Join(d.KindDir(k), key+".json")
}

// DisplayPath — файл для показа человеку.
func (d *Dir) DisplayPath(k *Kind, key string) string { return paths.Display(d.Path(k, key)) }

// DisplayDir — каталог вида для показа человеку.
func (d *Dir) DisplayDir(k *Kind) string { return paths.Display(d.KindDir(k)) }

// Has — есть ли файл.
func (d *Dir) Has(k *Kind, key string) bool {
	if !k.valid(key) {
		return false
	}
	_, err := os.Stat(d.Path(k, key))
	return err == nil
}

// Read читает файл, поднимает его до текущего формата и разбирает в v.
// Отсутствие файла — ошибка, для которой errors.Is(err, os.ErrNotExist).
func (d *Dir) Read(k *Kind, key string, v any) (Meta, error) {
	if !k.valid(key) {
		return Meta{}, fmt.Errorf("%w: %s %q", ErrBadKey, k.Name, key)
	}
	d.mu.Lock()
	data, err := os.ReadFile(d.Path(k, key))
	d.mu.Unlock()
	if err != nil {
		return Meta{}, err
	}
	meta, err := Decode(k, data, v)
	if err != nil {
		return meta, fmt.Errorf("%s %s: %w", k.Name, key, err)
	}
	return meta, nil
}

// Decode — то же, что Read, но над байтами: так проверяются миграции и
// памятники старых форматов без каталога.
func Decode(k *Kind, data []byte, v any) (Meta, error) {
	doc, schema, err := parse(data)
	if err != nil {
		return Meta{}, err
	}
	meta := Meta{Schema: schema}
	cur := k.current()
	if schema > cur {
		return meta, fmt.Errorf("%w: формат %d, программа знает до %d", ErrNewer, schema, cur)
	}
	for from := schema; from < cur; from++ {
		m, ok := k.Migrate[from]
		if !ok {
			return meta, fmt.Errorf("%w %d → %d", ErrNoMigration, from, from+1)
		}
		if err := m(doc); err != nil {
			return meta, fmt.Errorf("миграция %d → %d: %w", from, from+1, err)
		}
		meta.Migrated = true
	}
	delete(doc, SchemaKey)
	raw, err := json.Marshal(doc)
	if err != nil {
		return meta, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return meta, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return meta, nil
}

// parse разбирает файл как объект и достаёт номер формата. Файл без поля —
// версия 1: так выглядят все файлы, записанные до появления версий.
func parse(data []byte) (map[string]json.RawMessage, int, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil || doc == nil {
		if err == nil {
			err = errors.New("не объект")
		}
		return nil, 0, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	schema := 1
	if raw, ok := doc[SchemaKey]; ok {
		if err := json.Unmarshal(raw, &schema); err != nil || schema < 1 {
			return nil, 0, fmt.Errorf("%w: поле schema = %s", ErrCorrupt, string(raw))
		}
	}
	return doc, schema, nil
}

// SchemaOf — версия формата файла на диске без разбора всего содержимого.
func SchemaOf(data []byte) (int, error) {
	_, schema, err := parse(data)
	return schema, err
}

// Write записывает v в текущем формате. Если на диске лежит файл старого
// формата и копии ещё нет, он сначала копируется в <ключ>.v<N>.bak: первая
// миграция — единственный момент, когда старые байты ещё можно сохранить.
func (d *Dir) Write(k *Kind, key string, v any) error {
	if !k.valid(key) {
		return fmt.Errorf("%w: %s %q", ErrBadKey, k.Name, key)
	}
	data, err := Encode(k, v)
	if err != nil {
		return fmt.Errorf("%s %s: %w", k.Name, key, err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	dir := d.KindDir(k)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("каталог %s: %w", paths.Display(dir), err)
	}
	path := d.Path(k, key)
	if old, err := os.ReadFile(path); err == nil {
		d.backupLocked(k, key, old)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("запись %s: %w", paths.Display(tmp), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("замена %s: %w", paths.Display(path), err)
	}
	return nil
}

// backupLocked кладёт копию старого файла рядом, если его формат старше
// текущего. Битый файл тоже копируется — под своим именем: перезапись
// уничтожила бы то единственное, по чему можно понять, что случилось.
// Неудача копии запись не останавливает: данные в новом формате важнее.
func (d *Dir) backupLocked(k *Kind, key string, old []byte) {
	schema, err := SchemaOf(old)
	var name string
	switch {
	case err != nil:
		name = key + ".broken.bak"
	case schema < k.current():
		name = fmt.Sprintf("%s.v%d.bak", key, schema)
	default:
		return
	}
	bak := filepath.Join(d.KindDir(k), name)
	if _, err := os.Stat(bak); err == nil {
		return
	}
	_ = os.WriteFile(bak, old, 0o644)
}

// Encode сериализует v в текущем формате: "schema" первым полем, дальше
// поля типа в их порядке, с отступами — файлы читают люди.
func Encode(k *Kind, v any) ([]byte, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("сериализация: %w", err)
	}
	body = bytes.TrimSpace(body)
	if len(body) < 2 || body[0] != '{' {
		return nil, errors.New("сериализация: хранить можно только объект")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("сериализация: %w", err)
	}
	if _, clash := probe[SchemaKey]; clash {
		return nil, fmt.Errorf("сериализация: у типа своё поле %q, а его ставит хранилище", SchemaKey)
	}
	head := fmt.Sprintf("{\n  %q: %d", SchemaKey, k.current())
	rest := bytes.TrimSpace(body[1:])
	if len(rest) == 1 { // пустой объект: осталась только «}»
		return []byte(head + "\n}\n"), nil
	}
	var out bytes.Buffer
	out.WriteString(head)
	out.WriteString(",\n  ")
	out.Write(rest)
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// List — ключи всех читаемых файлов вида по алфавиту. Нечитаемые (битые,
// из будущего, с чужим именем) не прячутся: они возвращаются проблемами,
// чтобы их показать, но остальным не мешают.
func (d *Dir) List(k *Kind) ([]string, []error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	entries, err := os.ReadDir(d.KindDir(k))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("каталог %s: %w", d.DisplayDir(k), err)}
	}
	var keys []string
	var problems []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		key := strings.TrimSuffix(name, ".json")
		if !k.valid(key) {
			problems = append(problems, fmt.Errorf("%s: имя файла не годится как идентификатор", name))
			continue
		}
		data, err := os.ReadFile(filepath.Join(d.KindDir(k), name))
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", name, err))
			continue
		}
		schema, err := SchemaOf(data)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if schema > k.current() {
			problems = append(problems, fmt.Errorf("%s: %w (формат %d, программа знает до %d) — файл не читается",
				name, ErrNewer, schema, k.current()))
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, problems
}

// Raw — файл как есть: интерфейс показывает, что лежит на диске.
func (d *Dir) Raw(k *Kind, key string) ([]byte, error) {
	if !k.valid(key) {
		return nil, fmt.Errorf("%w: %s %q", ErrBadKey, k.Name, key)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return os.ReadFile(d.Path(k, key))
}

// Delete удаляет файл. Отсутствие файла ошибкой не считается; копии старых
// форматов остаются — это память о данных, а не сами данные.
func (d *Dir) Delete(k *Kind, key string) error {
	if !k.valid(key) {
		return fmt.Errorf("%w: %s %q", ErrBadKey, k.Name, key)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.Remove(d.Path(k, key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Rename — поле переименовывается миграцией как удаление и добавление
// (ТЗ, 4.12: «просто переименовать» нельзя — меняется смысл для читателя
// старого кода). Помощник для миграций: переносит значение, если нового
// поля ещё нет.
func Rename(doc map[string]json.RawMessage, from, to string) {
	v, ok := doc[from]
	if !ok {
		return
	}
	delete(doc, from)
	if _, taken := doc[to]; !taken {
		doc[to] = v
	}
}

// Set — помощник для миграций: записать значение поля.
func Set(doc map[string]json.RawMessage, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	doc[key] = raw
	return nil
}

// Get — помощник для миграций: прочитать поле, если оно есть.
func Get(doc map[string]json.RawMessage, key string, v any) (bool, error) {
	raw, ok := doc[key]
	if !ok || string(raw) == "null" {
		return false, nil
	}
	return true, json.Unmarshal(raw, v)
}
