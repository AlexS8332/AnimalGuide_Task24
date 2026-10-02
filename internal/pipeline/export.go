package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// Пределы save_to_file.
const (
	exportPreviewRunes = 1500 // File.Preview
	exportNameMax      = 80   // имя файла без расширения, знаков
	exportTimeLayout   = "20060102-150405"
	exportHumanTime    = "02.01.2006 15:04 -07:00"
)

func toolSaveFile(d Deps) tools.Tool {
	return tools.Func{
		S: tools.Spec{
			Name: ToolSaveFile,
			Description: toolAbout +
				"Шаг 3 — сохранить. Записывает факты из " + ToolSummarize + " в файл в каталоге выгрузок демона " +
				"(exports рядом с его базой): format=md (по умолчанию) — Markdown для чтения: заголовок, вид " +
				"(латынь, русское название, МСОП, отряд и семейство), вступление, факты со ссылками на источники и " +
				"список источников; format=json — выпуск и цепочка отпечатков для программ. В конце файла — цепочка " +
				"«досье sha256:… → факты sha256:…»: по файлу видно, из каких данных он получен. name — имя файла " +
				"без каталога и расширения (латиница, цифры, - и _; кириллица переводится в латиницу), по умолчанию " +
				"<латинское название>-<дата-время>; файл с тем же именем перезаписывается. Принимает только конверт " +
				"kind=facts: ref — его digest или input — он целиком (досье сюда передавать нельзя — сначала " +
				ToolSummarize + "). После записи файл перечитывается и сверяется по sha256. Возвращает конверт " +
				"kind=file: data.path (относительно каталога данных демона), format, bytes, sha256 файла, chain — " +
				"отпечатки досье и фактов, preview — начало файла. Это последний шаг цепочки.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "ref": {"type": "string", "description": "digest конверта фактов из summarize: sha256:…"},
    "input": {"type": "object", "description": "Конверт фактов из summarize целиком, без изменений"},
    "format": {"type": "string", "enum": ["md", "json"], "description": "md — Markdown (по умолчанию), json — JSON"},
    "name": {"type": "string", "description": "Имя файла без каталога и расширения, например manul-facts; по умолчанию латинское название и время"}
  },
  "additionalProperties": false
}`),
			Untrusted: true,
			Write:     true,
			Via:       tools.ViaLocal,
		},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in toolInputArgs
			if err := toolParse(args, &in); err != nil {
				return "", err
			}
			env, err := saveFile(ctx, d, in)
			if err != nil {
				return "", err
			}
			return tools.Result(env)
		},
	}
}

// saveFile — шаг 3 целиком.
func saveFile(ctx context.Context, d Deps, in toolInputArgs) (Envelope, error) {
	format := strings.ToLower(strings.TrimSpace(in.Format))
	switch format {
	case "":
		format = FormatMarkdown
	case FormatMarkdown, FormatJSON:
	case "markdown":
		format = FormatMarkdown
	default:
		return Envelope{}, fmt.Errorf("save_to_file: неизвестный format %q; допустимы md и json", in.Format)
	}
	if strings.TrimSpace(d.ExportDir) == "" {
		return Envelope{}, errors.New("save_to_file: каталог выгрузок не задан")
	}
	// Имя проверяется до чтения входа: ошибка в имени не зависит от данных.
	base, err := exportBaseName(in.Name, format)
	if err != nil {
		return Envelope{}, err
	}

	env, err := openInput(ctx, d, in, KindFacts)
	if err != nil {
		if errors.Is(err, ErrKind) {
			err = kindHint(err, KindFacts)
		}
		return Envelope{}, err
	}
	var facts Facts
	if err := json.Unmarshal(env.Data, &facts); err != nil {
		return Envelope{}, fmt.Errorf("%w: data фактов не разобралось: %v", ErrKind, err)
	}
	is := facts.Issue
	if is.SciName == "" {
		return Envelope{}, fmt.Errorf("%w: в фактах нет вида", ErrKind)
	}

	// Цепочка — из конвертов, а не из данных: факты знают своё досье
	// только через Input конверта, который поставил summarize.
	var chain []string
	if env.Input != "" {
		chain = append(chain, env.Input)
	}
	chain = append(chain, env.Digest)

	now := d.now()
	if base == "" {
		base = exportSlug(is.SciName) + "-" + now.Format(exportTimeLayout)
	}
	var body []byte
	switch format {
	case FormatJSON:
		body, err = renderJSON(is, chain, now)
	default:
		body = renderMarkdown(is, chain, now)
	}
	if err != nil {
		return Envelope{}, fmt.Errorf("save_to_file: %w", err)
	}

	name := base + "." + format
	full, err := exportPath(d.ExportDir, name)
	if err != nil {
		return Envelope{}, err
	}
	sum, err := writeVerified(full, body)
	if err != nil {
		return Envelope{}, fmt.Errorf("save_to_file: %w", err)
	}

	rel := path.Join(filepath.Base(filepath.Clean(d.ExportDir)), name)
	file := File{Path: rel, Format: format, Bytes: len(body), SHA256: sum, Chain: chain,
		Preview: exportPreview(body)}
	out, err := Seal(KindFile, file, env.Digest)
	if err != nil {
		return Envelope{}, fmt.Errorf("save_to_file: %w", err)
	}
	out.Summary = fmt.Sprintf("%s, %s", rel, exportSize(len(body)))
	return out, nil
}

// ---------------------------------------------------------------- имя файла

// exportBaseName — имя файла без расширения из аргумента name; пусто —
// имя по умолчанию выберет вызывающий. Каталоги в имени — ошибка, а не
// тихая чистка: «../../x» — это либо ошибка модели, либо попытка выйти из
// каталога выгрузок, и в обоих случаях молча писать в другое место нельзя.
func exportBaseName(name, format string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	// Двоеточие само по себе — обычная пунктуация («Манул: факты») и уйдёт
	// в «-»; каталогом оно становится только как диск в начале («c:x» на
	// Windows — путь относительно текущего каталога диска C).
	drive := len(name) >= 2 && name[1] == ':' &&
		(name[0] >= 'a' && name[0] <= 'z' || name[0] >= 'A' && name[0] <= 'Z')
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") || drive ||
		filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("save_to_file: name %q — только имя файла, без каталогов, «..» и дисков: "+
			"файлы пишутся в каталог выгрузок демона", name)
	}
	// Расширение, совпавшее с форматом, — не часть имени: «manul.md» при
	// format=md не должно стать «manul-md.md».
	if ext := strings.ToLower(filepath.Ext(name)); ext == "."+format || ext == ".markdown" {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	slug := exportSlug(name)
	if slug == "" {
		return "", fmt.Errorf("save_to_file: из name %q не получилось имени файла: нужны латинские буквы, "+
			"цифры, кириллица, - или _", name)
	}
	return slug, nil
}

// exportSlug — имя файла: строчная латиница, цифры, «-» и «_»; кириллица —
// транслитом, прочее — «-»; повторы «-» схлопываются, по краям их нет.
// Точек нет вовсе: ни «..», ни скрытых файлов, ни второго расширения.
func exportSlug(s string) string {
	var b strings.Builder
	dash := false
	put := func(r rune) {
		if r == '-' {
			if dash || b.Len() == 0 {
				return
			}
			dash = true
		} else {
			dash = false
		}
		b.WriteRune(r)
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			put(r)
		case translit[r] != "":
			for _, t := range translit[r] {
				put(t)
			}
		case r == 'ъ' || r == 'ь':
			// мягкий и твёрдый знаки в имени файла не нужны
		default:
			put('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if utf8.RuneCountInString(out) > exportNameMax {
		out = strings.Trim(out[:exportNameMax], "-_")
	}
	return out
}

// translit — строчная кириллица латиницей (упрощённая система, как в
// загранпаспорте): имени файла нужна читаемость, а не обратимость.
var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh", 'з': "z",
	'и': "i", 'й': "i", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r",
	'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch",
	'ы': "y", 'э': "e", 'ю': "iu", 'я': "ia",
}

// exportPath — полный путь файла строго внутри каталога выгрузок. Имя уже
// очищено exportSlug, но проверка повторяется на итоговом пути: защита от
// обхода не должна держаться на одной функции.
func exportPath(dir, name string) (string, error) {
	root, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return "", fmt.Errorf("save_to_file: каталог выгрузок: %w", err)
	}
	full := filepath.Join(root, name)
	rel, err := filepath.Rel(root, full)
	if err != nil || rel != name || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("save_to_file: имя %q выводит за каталог выгрузок", name)
	}
	return full, nil
}

// ------------------------------------------------------------------ запись

// writeVerified пишет файл атомарно (временный файл в том же каталоге →
// rename) и перечитывает его: sha256 прочитанного обязан совпасть с
// sha256 записанного. Возвращает sha256 файла на диске (hex, как у
// sha256sum и Get-FileHash).
//
// Временный файл — в том же каталоге, иначе rename между томами не
// атомарен (и на Windows вовсе не сработает). Читатель, открывший файл во
// время записи, видит либо старую версию, либо новую, но не половину.
func writeVerified(full string, body []byte) (string, error) {
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("каталог выгрузок %s не создался: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("временный файл: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // после rename его уже нет — ошибка не важна
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return "", fmt.Errorf("запись: %w", err)
	}
	// Sync до rename: иначе после сбоя питания на диске мог бы оказаться
	// файл нужного имени, но пустой.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("запись: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("запись: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return "", fmt.Errorf("запись %s: %w", filepath.Base(full), err)
	}

	want := sha256.Sum256(body)
	got, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("файл %s не перечитался: %w", filepath.Base(full), err)
	}
	have := sha256.Sum256(got)
	if have != want {
		return "", fmt.Errorf("файл %s на диске не совпал с записанным: sha256 %s, ждали %s",
			filepath.Base(full), hex.EncodeToString(have[:]), hex.EncodeToString(want[:]))
	}
	return hex.EncodeToString(have[:]), nil
}

func exportPreview(body []byte) string {
	s := string(body)
	if utf8.RuneCountInString(s) <= exportPreviewRunes {
		return s
	}
	return string([]rune(s)[:exportPreviewRunes]) + "…"
}

// exportSize — «2.1 КБ», «812 Б».
func exportSize(n int) string {
	if n < 1024 {
		return strconv.Itoa(n) + " Б"
	}
	return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " КБ"
}

// ------------------------------------------------------------------ формат

// iucnRu — статусы МСОП словами.
var iucnRu = map[string]string{
	"LC": "вызывающие наименьшие опасения", "NT": "близкие к уязвимому положению", "VU": "уязвимый вид",
	"EN": "вымирающий вид", "CR": "на грани исчезновения", "EW": "исчезнувший в дикой природе",
	"EX": "исчезнувший", "DD": "недостаточно данных", "NE": "не оценивался",
}

// renderMarkdown — выпуск для чтения. Тексты пришли от модели: переводы
// строк внутри факта схлопываются, иначе факт развалил бы нумерованный
// список.
func renderMarkdown(is trivia.Issue, chain []string, saved time.Time) []byte {
	var b bytes.Buffer
	title := oneLine(is.Title)
	if title == "" {
		title = speciesLabel(is.NameRu, is.SciName)
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	sp := "*" + is.SciName + "*"
	if ru := oneLine(is.NameRu); ru != "" {
		sp = ru + " — " + sp
	}
	fmt.Fprintf(&b, "- **Вид:** %s\n", sp)
	if is.IUCN != "" {
		st := is.IUCN
		if ru := iucnRu[is.IUCN]; ru != "" {
			st += " — " + ru
		}
		fmt.Fprintf(&b, "- **Статус МСОП:** %s\n", st)
	}
	if is.Order != "" || is.Family != "" {
		fmt.Fprintf(&b, "- **Отряд / семейство:** %s / %s\n", orDash(is.Order), orDash(is.Family))
	}
	if len(is.Realms) > 0 {
		fmt.Fprintf(&b, "- **Области:** %s\n", strings.Join(is.Realms, ", "))
	}
	if is.SpeciesID != 0 {
		fmt.Fprintf(&b, "- **mdd-id:** %d\n", is.SpeciesID)
	}
	b.WriteString("\n")
	if lead := oneLine(is.Lead); lead != "" {
		b.WriteString(lead + "\n\n")
	}

	byID := make(map[string]trivia.Material, len(is.Sources))
	for _, m := range is.Sources {
		byID[m.ID] = m
	}
	b.WriteString("## Факты\n\n")
	for i, f := range is.Facts {
		var refs []string
		for _, id := range f.Sources {
			if u, ok := mdURL(byID[id].URL); ok {
				refs = append(refs, "["+id+"]("+u+")")
			} else {
				refs = append(refs, id)
			}
		}
		fmt.Fprintf(&b, "%d. %s", i+1, oneLine(f.Text))
		if len(refs) > 0 {
			b.WriteString(" — " + strings.Join(refs, ", "))
		}
		b.WriteString("\n")
	}

	if len(is.Sources) > 0 {
		b.WriteString("\n## Источники\n\n")
		for _, m := range is.Sources {
			t := oneLine(m.Title)
			if t == "" {
				t = m.Kind
			}
			if u, ok := mdURL(m.URL); ok {
				fmt.Fprintf(&b, "- **%s** [%s](%s)\n", m.ID, t, u)
			} else {
				fmt.Fprintf(&b, "- **%s** %s\n", m.ID, t)
			}
		}
	}

	b.WriteString("\n---\n\n")
	b.WriteString("Цепочка: " + chainText(chain))
	fmt.Fprintf(&b, ", собрано %s, сохранено %s.\n",
		is.CreatedAt.In(saved.Location()).Format(exportHumanTime), saved.Format(exportHumanTime))
	return b.Bytes()
}

// chainText — «досье sha256:… → факты sha256:…» (отпечатки полностью: по
// ним файл сверяют с конвертами).
func chainText(chain []string) string {
	labels := []string{"досье", "факты"}
	if len(chain) == 1 {
		labels = []string{"факты"}
	}
	parts := make([]string, 0, len(chain))
	for i, d := range chain {
		l := "шаг"
		if i < len(labels) {
			l = labels[i]
		}
		parts = append(parts, l+" "+d)
	}
	return strings.Join(parts, " → ")
}

// renderJSON — выпуск для программ: issue как в конверте facts и цепочка.
func renderJSON(is trivia.Issue, chain []string, saved time.Time) ([]byte, error) {
	out := struct {
		Issue   trivia.Issue `json:"issue"`
		Chain   []string     `json:"chain"`
		SavedAt time.Time    `json:"saved_at"`
	}{Issue: is, Chain: chain, SavedAt: saved.Round(0)}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// mdURL — адрес для ссылки Markdown: только http(s); скобки и пробелы
// кодируются («Рысь_(род)» у Википедии обычное дело), иначе они закрыли бы
// ссылку раньше времени. ok=false — ссылкой не делать.
func mdURL(u string) (string, bool) {
	if !(strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) || strings.IndexFunc(u, unicode.IsControl) >= 0 || strings.ContainsAny(u, "<>") {
		return "", false
	}
	return strings.NewReplacer("(", "%28", ")", "%29", " ", "%20").Replace(u), true
}
