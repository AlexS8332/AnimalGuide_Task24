package notes

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Пределы блокнота. Модель пишет текст сама, и без пределов один
// зациклившийся ход раздул бы файл и память процесса: сервер живёт, пока
// жив клиент, а блокноты — в памяти.
const (
	textMax     = 8000 // текст раздела, рун
	headingMax  = 200  // заголовок раздела, рун
	titleMax    = 200  // заголовок блокнота и вид, рун
	sectionsMax = 20   // разделов в блокноте
	citesMax    = 20   // ссылок в разделе
	citeMax     = 64   // имя инструмента в ссылке, знаков
	openMax     = 64   // открытых блокнотов одновременно
	previewMax  = 600  // CloseResult.Preview, рун
)

// Форматы файла.
const (
	formatMD   = "md"
	formatJSON = "json"
)

// section — раздел блокнота.
type section struct {
	Heading string   `json:"heading"`
	Text    string   `json:"text"`
	Cites   []string `json:"cites"`
}

// notebook — блокнот в памяти. После закрытия разделы отпускаются: от
// закрытого блокнота нужен только ответ «уже закрыт, файл такой-то».
type notebook struct {
	id, title, species string
	base               string // имя файла без расширения
	sections           []section
	closing            bool // идёт запись: nb_add и второй nb_close ждать не будут
	closed             bool
	path               string // после закрытия — записанный файл
}

// book — состояние сервера: каталог и блокноты под одним замком.
type book struct {
	dir string

	mu    sync.Mutex
	books map[string]*notebook
	open  int
}

// Tools — инструменты блокнота над каталогом dir (<data>/notes). Каталог
// создаётся при первом nb_close. Состояние открытых блокнотов — в памяти
// процесса.
//
// Все три помечены Write: каждый меняет состояние, и повтор вызова — не то
// же самое, что один вызов (второй блокнот, второй раздел, отказ «уже
// закрыт»). Ответы — JSON (OpenResult, AddResult, CloseResult); ошибки —
// словами, с подсказкой, какой вызов сделать.
func Tools(dir string) []tools.Tool {
	b := &book{dir: dir, books: map[string]*notebook{}}
	return []tools.Tool{b.toolOpen(), b.toolAdd(), b.toolClose()}
}

// toolAbout — общее начало описаний: что это за сервер и порядок работы.
const toolAbout = "Блокнот натуралиста: записи о виде, которые становятся файлом. Порядок работы: " +
	ToolOpen + " (получить notebook_id) → " + ToolAdd + " по разделу за вызов → " + ToolClose + " (записать файл). "

func (b *book) toolOpen() tools.Tool {
	return tools.Func{
		S: tools.Spec{
			Name: ToolOpen,
			Description: toolAbout + "Открывает новый пустой блокнот о виде. Звать один раз в начале, до " + ToolAdd +
				". Возвращает notebook_id — его нужно передавать в " + ToolAdd + " и " + ToolClose +
				" как есть (угадать его нельзя), — и path: куда ляжет файл после " + ToolClose + ".",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "Заголовок блокнота, например «Манул — паспорт вида»"},
    "species": {"type": "string", "description": "Латинское название вида, например Otocolobus manul"}
  },
  "required": ["title", "species"],
  "additionalProperties": false
}`),
			Write: true,
			Via:   tools.ViaLocal,
		},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in OpenArgs
			if err := parse(args, &in); err != nil {
				return "", err
			}
			res, err := b.openBook(in)
			if err != nil {
				return "", err
			}
			return tools.Result(res)
		},
	}
}

func (b *book) toolAdd() tools.Tool {
	return tools.Func{
		S: tools.Spec{
			Name: ToolAdd,
			Description: toolAbout + "Добавляет в открытый блокнот один раздел: заголовок, текст (Markdown) и cites — " +
				"имена инструментов, из ответов которых взяты сведения раздела (read_wikipedia, mdd_get, taxon_tree…). " +
				fmt.Sprintf("Текст раздела — до %d знаков, разделов — до %d. ", textMax, sectionsMax) +
				"Возвращает номер раздела и сколько их всего.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "notebook_id": {"type": "string", "description": "notebook_id из ответа nb_open"},
    "heading": {"type": "string", "description": "Заголовок раздела, например «Систематика»"},
    "text": {"type": "string", "description": "Текст раздела, Markdown"},
    "cites": {"type": "array", "items": {"type": "string"}, "description": "Имена инструментов, из ответов которых взяты сведения раздела"}
  },
  "required": ["notebook_id", "heading", "text", "cites"],
  "additionalProperties": false
}`),
			Write: true,
			Via:   tools.ViaLocal,
		},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in AddArgs
			if err := parse(args, &in); err != nil {
				return "", err
			}
			res, err := b.add(in)
			if err != nil {
				return "", err
			}
			return tools.Result(res)
		},
	}
}

func (b *book) toolClose() tools.Tool {
	return tools.Func{
		S: tools.Spec{
			Name: ToolClose,
			Description: toolAbout + "Закрывает блокнот и записывает его файлом: format md (по умолчанию) или json. " +
				"Звать один раз в конце, когда все разделы добавлены: после закрытия добавить раздел нельзя. " +
				"Возвращает путь к файлу, его sha256 и размер, число разделов, все ссылки cites и начало текста.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "notebook_id": {"type": "string", "description": "notebook_id из ответа nb_open"},
    "format": {"type": "string", "enum": ["md", "json"], "description": "Формат файла: md (по умолчанию) или json"}
  },
  "required": ["notebook_id"],
  "additionalProperties": false
}`),
			Write: true,
			Via:   tools.ViaLocal,
		},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in CloseArgs
			if err := parse(args, &in); err != nil {
				return "", err
			}
			res, err := b.close(in)
			if err != nil {
				return "", err
			}
			return tools.Result(res)
		},
	}
}

// ------------------------------------------------------------------ открытие

func (b *book) openBook(in OpenArgs) (OpenResult, error) {
	title, species := oneLine(in.Title), oneLine(in.Species)
	if title == "" && species == "" {
		return OpenResult{}, errors.New(ToolOpen + ": нужны title (заголовок блокнота) и species (латинское название вида)")
	}
	if utf8.RuneCountInString(title) > titleMax || utf8.RuneCountInString(species) > titleMax {
		return OpenResult{}, fmt.Errorf("%s: title и species — не длиннее %d знаков", ToolOpen, titleMax)
	}
	if title == "" {
		title = species
	}
	// Имя файла — из вида (по нему файл найдёт человек), иначе из заголовка.
	// Slug не пропускает ни разделителей каталогов, ни точек: «../..» в
	// названии вида становится просто дефисами. id в имени — чтобы два
	// блокнота об одном виде не затёрли друг друга.
	base := pipeline.Slug(species)
	if base == "" {
		base = pipeline.Slug(title)
	}
	if base == "" {
		base = "notebook"
	}
	id, err := newID()
	if err != nil {
		return OpenResult{}, err
	}
	base += "-" + id
	path, err := b.file(base, formatMD)
	if err != nil {
		return OpenResult{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.open >= openMax {
		return OpenResult{}, fmt.Errorf("%s: открыто уже %d блокнотов — закрой ненужные через %s", ToolOpen, openMax, ToolClose)
	}
	b.books[id] = &notebook{id: id, title: title, species: species, base: base}
	b.open++
	return OpenResult{NotebookID: id, Path: path}, nil
}

// newID — «nb-» и 12 hex-знаков из crypto/rand: 48 бит, модель их не
// угадает и не «вспомнит» из прошлого разговора.
func newID() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("%s: идентификатор блокнота: %w", ToolOpen, err)
	}
	return "nb-" + hex.EncodeToString(raw[:]), nil
}

// file — полный путь файла в каталоге блокнота. Имя уже безопасно (Slug),
// но проверка не лишняя: выход за каталог — это запись куда угодно на
// диске, и держать её должно не одно правило имени.
func (b *book) file(base, format string) (string, error) {
	root, err := filepath.Abs(filepath.Clean(b.dir))
	if err != nil {
		return "", fmt.Errorf("каталог блокнотов: %w", err)
	}
	name := base + "." + format
	full := filepath.Join(root, name)
	rel, err := filepath.Rel(root, full)
	if err != nil || rel != name || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("имя файла блокнота %q выводит за каталог блокнотов", name)
	}
	return full, nil
}

// ------------------------------------------------------------------ разделы

func (b *book) add(in AddArgs) (AddResult, error) {
	heading := oneLine(in.Heading)
	text := strings.TrimSpace(in.Text)
	switch {
	case heading == "":
		return AddResult{}, errors.New(ToolAdd + ": пустой heading — у раздела нужен заголовок")
	case text == "":
		return AddResult{}, errors.New(ToolAdd + ": пустой text — раздел без текста не добавляется")
	case utf8.RuneCountInString(heading) > headingMax:
		return AddResult{}, fmt.Errorf("%s: heading длиннее %d знаков", ToolAdd, headingMax)
	case utf8.RuneCountInString(text) > textMax:
		return AddResult{}, fmt.Errorf("%s: text — %d знаков, предел %d: сократи раздел или раздели на два",
			ToolAdd, utf8.RuneCountInString(text), textMax)
	}
	cites, err := cleanCites(in.Cites)
	if err != nil {
		return AddResult{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	nb, err := b.lookup(ToolAdd, in.NotebookID)
	if err != nil {
		return AddResult{}, err
	}
	if len(nb.sections) >= sectionsMax {
		return AddResult{}, fmt.Errorf("%s: в блокноте уже %d разделов — это предел; закрой блокнот через %s",
			ToolAdd, sectionsMax, ToolClose)
	}
	nb.sections = append(nb.sections, section{Heading: heading, Text: text, Cites: cites})
	n := len(nb.sections)
	return AddResult{NotebookID: nb.id, Section: n, Sections: n}, nil
}

// cleanCites — ссылки без пробелов по краям, пустых и повторов, в порядке
// модели.
func cleanCites(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		if len(c) > citeMax || strings.ContainsAny(c, "\r\n") {
			return nil, fmt.Errorf("%s: в cites — имена инструментов (read_wikipedia, mdd_get…), а не %q", ToolAdd, tools.Truncate(c, 40))
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) > citesMax {
		return nil, fmt.Errorf("%s: в cites больше %d имён", ToolAdd, citesMax)
	}
	return out, nil
}

// lookup — открытый блокнот по id; вызывается под замком. Тексты ошибок
// говорят модели, какой вызов сделать: выдуманный id — самая частая её
// ошибка в этом месте.
func (b *book) lookup(tool, id string) (*notebook, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%s: нет notebook_id — сначала %s, передай notebook_id из его ответа", tool, ToolOpen)
	}
	nb, ok := b.books[id]
	if !ok {
		return nil, fmt.Errorf("%s: блокнота %q нет — сначала %s, передай notebook_id из его ответа", tool, tools.Truncate(id, 40), ToolOpen)
	}
	if nb.closed {
		return nil, fmt.Errorf("%s: блокнот %s уже закрыт, файл %s; для новых записей открой новый через %s", tool, nb.id, nb.path, ToolOpen)
	}
	if nb.closing {
		return nil, fmt.Errorf("%s: блокнот %s сейчас закрывается", tool, nb.id)
	}
	return nb, nil
}

// ------------------------------------------------------------------ закрытие

func (b *book) close(in CloseArgs) (CloseResult, error) {
	format := strings.ToLower(strings.TrimSpace(in.Format))
	switch format {
	case "", formatMD, "markdown":
		format = formatMD
	case formatJSON:
	default:
		return CloseResult{}, fmt.Errorf("%s: format %q — нужен md или json", ToolClose, in.Format)
	}

	// Под замком — только проверка и снимок: запись на диск идёт без
	// него, а «closing» не даёт в это время дописать раздел или закрыть
	// блокнот второй раз.
	b.mu.Lock()
	nb, err := b.lookup(ToolClose, in.NotebookID)
	if err != nil {
		b.mu.Unlock()
		return CloseResult{}, err
	}
	if len(nb.sections) == 0 {
		b.mu.Unlock()
		return CloseResult{}, fmt.Errorf("%s: в блокноте %s нет ни одного раздела — добавь раздел %s, потом закрывай",
			ToolClose, nb.id, ToolAdd)
	}
	nb.closing = true
	snap := *nb
	snap.sections = append([]section(nil), nb.sections...)
	b.mu.Unlock()

	res, err := b.write(snap, format)

	b.mu.Lock()
	defer b.mu.Unlock()
	nb.closing = false
	if err != nil {
		// Блокнот остаётся открытым: модель может повторить nb_close.
		return CloseResult{}, err
	}
	nb.closed, nb.path, nb.sections = true, res.Path, nil
	b.open--
	return res, nil
}

func (b *book) write(nb notebook, format string) (CloseResult, error) {
	full, err := b.file(nb.base, format)
	if err != nil {
		return CloseResult{}, err
	}
	cites := allCites(nb.sections)
	var body []byte
	if format == formatJSON {
		body, err = renderJSON(nb, cites)
		if err != nil {
			return CloseResult{}, fmt.Errorf("%s: %w", ToolClose, err)
		}
	} else {
		body = renderMarkdown(nb, cites)
	}
	sum, err := pipeline.WriteVerified(full, body)
	if err != nil {
		return CloseResult{}, fmt.Errorf("%s: файл %s не записан: %w", ToolClose, full, err)
	}
	return CloseResult{NotebookID: nb.id, Path: full, SHA256: sum, Bytes: len(body),
		Sections: len(nb.sections), Cites: cites, Preview: preview(body)}, nil
}

// allCites — ссылки всех разделов без повторов, по алфавиту.
func allCites(ss []section) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range ss {
		for _, c := range s.Cites {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	sort.Strings(out)
	return out
}

// renderMarkdown — блокнот для чтения: заголовок, вид, разделы и в конце
// источники. Заголовки разделов — одной строкой (oneLine), иначе перевод
// строки в них развалил бы разметку; текст раздела — Markdown модели как
// есть.
func renderMarkdown(nb notebook, cites []string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n\n", nb.title)
	if nb.species != "" {
		fmt.Fprintf(&b, "Вид: *%s*\n\n", nb.species)
	}
	for _, s := range nb.sections {
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", s.Heading, s.Text)
	}
	if len(cites) > 0 {
		fmt.Fprintf(&b, "---\n\nИсточники: %s\n", strings.Join(cites, ", "))
	} else {
		b.WriteString("---\n\nИсточники: не указаны\n")
	}
	return b.Bytes()
}

// renderJSON — блокнот для программ: те же поля, что в Markdown.
func renderJSON(nb notebook, cites []string) ([]byte, error) {
	data, err := json.MarshalIndent(struct {
		NotebookID string    `json:"notebook_id"`
		Title      string    `json:"title"`
		Species    string    `json:"species"`
		Sections   []section `json:"sections"`
		Cites      []string  `json:"cites"`
	}{nb.id, nb.title, nb.species, nb.sections, cites}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func preview(body []byte) string {
	s := string(body)
	if utf8.RuneCountInString(s) <= previewMax {
		return s
	}
	return string([]rune(s)[:previewMax]) + "…"
}

// oneLine — строка без переводов строк и лишних пробелов.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// parse — аргументы строго: лишнее поле — ошибка словами. Модель, которая
// прислала «id» вместо «notebook_id», должна узнать об этом сразу, а не
// получить «блокнота "" нет».
func parse(args json.RawMessage, target any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("аргументы не разобрались: %w", err)
	}
	return nil
}
