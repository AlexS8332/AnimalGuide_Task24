// Package pipeline — конвейер из трёх MCP-инструментов демона:
//
//	search       — получить данные: досье о виде (MDD, Википедия, GBIF), без модели;
//	summarize    — обработать: 3–5 проверенных фактов по досье (редактор + проверяющий);
//	save_to_file — сохранить: выпуск в файл Markdown или JSON в каталоге выгрузок демона.
//
// Каждый инструмент — самостоятельный MCP-инструмент: его можно позвать
// руками (mcp-list), моделью или исполнителем цепочки. Цепочку не ведёт
// сервер: её ведёт клиент (Run в runner.go — код, RunAgent в agent.go —
// модель), вызывая инструменты по MCP и передавая выход одного на вход
// следующего.
//
// # Передача данных
//
// Выход каждого шага — конверт (Envelope): вид данных (Kind), сами данные
// (Data), их отпечаток (Digest) и отпечаток входа (Input), из которого они
// получены. Отпечаток — sha256 канонического JSON данных (Digest): ключи по
// алфавиту, без пробелов, поэтому переупаковка JSON по дороге (map → JSON)
// его не меняет, а любая правка содержимого — меняет.
//
// Следующий шаг получает вход одним из двух способов:
//
//   - input — конверт целиком (так передаёт исполнитель-код). Инструмент
//     проверяет Kind и пересчитывает отпечаток Data: не совпал — ошибка
//     ErrDigest, данные испорчены по дороге;
//   - ref — только отпечаток (так удобно модели: не пересказывать десятки
//     килобайт досье). Демон держит выходы шагов в своём хранилище
//     (Artifacts) и достаёт их по ref.
//
// Ровно один из input и ref. Вход принятого шага попадает в его выход
// (Envelope.Input), и исполнитель сверяет цепочку: Input шага N+1 обязан
// совпасть с Digest шага N. Так корректность передачи проверяется с обеих
// сторон: сервер — что данные дошли целыми, клиент — что сервер
// обработал именно то, что ему передали.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// Имена инструментов конвейера.
const (
	ToolSearch    = "search"
	ToolSummarize = "summarize"
	ToolSaveFile  = "save_to_file"
)

// ToolNames — инструменты в порядке цепочки.
var ToolNames = []string{ToolSearch, ToolSummarize, ToolSaveFile}

// Виды данных в конверте.
const (
	KindDossier = "dossier" // выход search: Data — Dossier
	KindFacts   = "facts"   // выход summarize: Data — Facts
	KindFile    = "file"    // выход save_to_file: Data — File
)

// Форматы файла save_to_file.
const (
	FormatMarkdown = "md"
	FormatJSON     = "json"
)

// JobPipeline — имя, под которым платный шаг (summarize) пишется в журнал
// запусков демона: иначе его расход не увидел бы дневной лимит.
const JobPipeline = "pipeline"

// Ошибки передачи данных. Инструменты возвращают их текстом ошибки вызова
// (IsError в MCP) — текст начинается с сообщения ошибки, чтобы исполнитель
// и модель узнали причину.
var (
	// ErrDigest — отпечаток данных не совпал с заявленным: данные
	// испорчены по дороге.
	ErrDigest = errors.New("pipeline: отпечаток данных не совпал — данные изменены при передаче")
	// ErrKind — на вход пришли данные не того вида (например, досье в save_to_file).
	ErrKind = errors.New("pipeline: данные не того вида")
	// ErrRef — по ref ничего нет (демон перезапускался, или ref выдуман).
	ErrRef = errors.New("pipeline: данных с таким ref нет")
	// ErrInput — нет ни input, ни ref, или заданы оба.
	ErrInput = errors.New("pipeline: нужен ровно один вход — input (конверт предыдущего шага) или ref (его digest)")
	// ErrChain — исполнитель: вход шага не совпал с выходом предыдущего.
	ErrChain = errors.New("pipeline: цепочка разорвана — шаг обработал не те данные, что ему передали")
)

// Envelope — выход шага конвейера.
type Envelope struct {
	Kind   string          `json:"kind"`            // Kind*
	Digest string          `json:"digest"`          // Digest(Data)
	Input  string          `json:"input,omitempty"` // Digest входа; пусто у search
	Data   json.RawMessage `json:"data"`
	// Summary — одна строка для людей и журнала: «Манул (Otocolobus
	// manul): 9 материалов», «5 фактов из 6», «exports/…md, 2.1 КБ».
	Summary string `json:"summary,omitempty"`
	// CostUSD — расход шага на модель (только summarize).
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// Dossier — данные search: досье trivia как есть плюс как искали.
type Dossier struct {
	Query string `json:"query,omitempty"` // что искали; пусто при random
	// Resolved — как запрос превратился в вид: «mdd-id», «латинское
	// название», «английское название», «русская Википедия: Манул → Otocolobus
	// manul», «случайный вид».
	Resolved string         `json:"resolved"`
	Dossier  trivia.Dossier `json:"dossier"`
}

// Facts — данные summarize: выпуск (не сохранённый в ленту, ID = 0).
type Facts struct {
	Issue trivia.Issue `json:"issue"`
}

// File — данные save_to_file.
type File struct {
	Path   string `json:"path"`   // относительно каталога данных демона: exports/<имя>
	Format string `json:"format"` // Format*
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"` // файла на диске, после записи перечитан и сверен
	// Chain — отпечатки всей цепочки: досье → факты. Они же записаны в
	// сам файл (в Markdown — в подвале, в JSON — полем), так что по файлу
	// видно, из каких данных он получен.
	Chain   []string `json:"chain"`
	Preview string   `json:"preview"` // начало файла, до 1500 символов
}

// Digest — отпечаток данных: "sha256:" + hex от канонического JSON.
func Digest(data json.RawMessage) (string, error) {
	c, err := Canonical(data)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Canonical — JSON с ключами по алфавиту и без пробелов. Числа
// сохраняются как есть (UseNumber), иначе большие целые округлились бы
// через float64 и отпечаток «поплыл» бы.
func Canonical(data json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("pipeline: данные не JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("pipeline: после JSON-значения лишние данные")
	}
	return json.Marshal(v)
}

// Seal собирает конверт: кодирует data, считает отпечаток.
func Seal(kind string, data any, input string) (Envelope, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, err
	}
	d, err := Digest(raw)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Kind: kind, Digest: d, Input: input, Data: raw}, nil
}

// Open проверяет конверт: вид (kind, если не пуст) и отпечаток данных.
func (e Envelope) Open(kind string) error {
	if kind != "" && e.Kind != kind {
		return fmt.Errorf("%w: ждали %q, пришло %q", ErrKind, kind, e.Kind)
	}
	d, err := Digest(e.Data)
	if err != nil {
		return err
	}
	if d != e.Digest {
		return fmt.Errorf("%w: заявлен %s, по данным %s", ErrDigest, Short(e.Digest), Short(d))
	}
	return nil
}

// Short — отпечаток для людей: первые 12 знаков hex.
func Short(d string) string {
	h := strings.TrimPrefix(d, "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

// Artifacts — хранилище выходов шагов у демона (для ref). Реализации:
// Memory (тесты) и SQLite (демон, компонент миграций "pipeline").
type Artifacts interface {
	// Put сохраняет конверт под его Digest; повтор — не ошибка.
	Put(ctx context.Context, e Envelope) error
	// Get — конверт по digest; ErrRef, если нет.
	Get(ctx context.Context, digest string) (Envelope, error)
}

// ------------------------------------------------------------ исполнитель

// Caller — вызов инструмента MCP: args кодируется в JSON, ответ — текст
// результата (JSON). Ошибка инструмента (IsError) — ошибка с его текстом.
// Реализуют feed.Remote (приложение) и адаптер над mcp-клиентом (CLI).
type Caller interface {
	Call(ctx context.Context, tool string, args any) (json.RawMessage, error)
}

// Как передавать данные между шагами.
const (
	PassInline = "inline" // конверт целиком (input)
	PassRef    = "ref"    // только digest (ref)
)

// Request — что запустить.
type Request struct {
	Query  string `json:"query,omitempty"`  // вид; пусто и Random=false — ошибка
	Random bool   `json:"random,omitempty"` // случайный вид из MDD вместо Query
	Format string `json:"format,omitempty"` // Format*; пусто — md
	Pass   string `json:"pass,omitempty"`   // Pass*; пусто — inline
}

// Состояния шага.
const (
	StepPending = "pending"
	StepRunning = "running"
	StepOK      = "ok"
	StepFailed  = "failed"
)

// Step — след одного шага цепочки.
type Step struct {
	N      int    `json:"n"` // 1, 2, 3
	Tool   string `json:"tool"`
	Status string `json:"status"` // Step*
	// Args — аргументы вызова для журнала: конверт input заменён строкой
	// «<конверт dossier sha256:…>», иначе в журнал ушли бы килобайты.
	Args    json.RawMessage `json:"args,omitempty"`
	Kind    string          `json:"kind,omitempty"`
	Digest  string          `json:"digest,omitempty"`
	Input   string          `json:"input,omitempty"`
	Summary string          `json:"summary,omitempty"`
	CostUSD float64         `json:"cost_usd,omitempty"`
	Bytes   int             `json:"bytes,omitempty"` // размер ответа инструмента
	Started time.Time       `json:"started,omitzero"`
	Took    time.Duration   `json:"took,omitempty"`
	Error   string          `json:"error,omitempty"`
	// Checks — проверки передачи на этом шаге (что проверено и итог).
	Checks []Check `json:"checks,omitempty"`
}

// Check — одна проверка передачи данных.
type Check struct {
	Name string `json:"name"` // «отпечаток ответа», «вход = выход шага 1», «вид данных»
	OK   bool   `json:"ok"`
	Note string `json:"note,omitempty"`
}

// Trace — след всей цепочки.
type Trace struct {
	Request  Request       `json:"request"`
	Mode     string        `json:"mode"` // "code" | "agent"
	Steps    []Step        `json:"steps"`
	OK       bool          `json:"ok"`
	Error    string        `json:"error,omitempty"`
	File     *File         `json:"file,omitempty"` // данные последнего шага
	CostUSD  float64       `json:"cost_usd"`
	Took     time.Duration `json:"took"`
	Started  time.Time     `json:"started"`
	Finished time.Time     `json:"finished,omitzero"`
}
