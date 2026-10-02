// Package tools — инструменты, которые агент может вызывать через модель,
// и источники, которые их объявляют. Инструмент описывает себя для модели
// (имя, назначение, схема аргументов) и исполняет вызов; результат всегда
// текст с JSON — его удобно и отдать модели, и показать в журнале.
//
// Каждый инструмент описан один раз, здесь. Путь до него может быть разным
// — вызов в процессе или через MCP-сервер, — но описание, которое видит
// модель, одно и то же побайтно: иначе дорожки стенда отличались бы не
// путём, а текстом запроса.
package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

// Пути до инструмента. Модели не уходят — только в журнал.
const (
	ViaLocal = "local"
	ViaMCP   = "mcp"
)

// Spec — описание инструмента. Всё, что видит модель, плюс два признака для
// обвязки: Untrusted — ответ является содержимым внешнего источника (ФТ-41),
// Via — каким путём идёт вызов.
type Spec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Untrusted   bool            `json:"untrusted,omitempty"`
	// Write — вызов меняет состояние или тратит деньги (run_now запускает
	// платный выпуск). MCP-сервер снимает у такого инструмента пометку
	// «только чтение»; модели признак не уходит.
	Write bool   `json:"write,omitempty"`
	Via   string `json:"via,omitempty"`
}

// CallFunc — исполнение вызова.
type CallFunc func(ctx context.Context, args json.RawMessage) (string, error)

// Tool — один инструмент.
type Tool interface {
	Spec() Spec
	Call(ctx context.Context, args json.RawMessage) (string, error)
}

// Func — инструмент из описания и замыкания. Достаточно для всех
// инструментов проекта: состояние живёт в замыкании.
type Func struct {
	S  Spec
	Fn CallFunc
}

func (f Func) Spec() Spec { return f.S }
func (f Func) Call(ctx context.Context, args json.RawMessage) (string, error) {
	return f.Fn(ctx, args)
}

// Wrap оборачивает инструмент: описание копируется целиком, меняется только
// исполнение. Все обёртки — трекер, права этапа, счётчики — идут через него:
// обёртка, пересобирающая описание поле за полем, молча теряла бы новые
// поля.
func Wrap(t Tool, mw func(ctx context.Context, args json.RawMessage, next CallFunc) (string, error)) Tool {
	return Func{S: t.Spec(), Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
		return mw(ctx, args, t.Call)
	}}
}

// Source — источник инструментов (ФТ-5, точка роста Р-1). Новый источник —
// новая реализация, а не правка агентов.
type Source interface {
	ID() string
	Tools() []Tool
}

// StaticSource — источник из готового списка.
type StaticSource struct {
	Name string
	List []Tool
}

func (s StaticSource) ID() string    { return s.Name }
func (s StaticSource) Tools() []Tool { return s.List }

// SourceTools — шесть инструментов источников (ФТ-1) в порядке показа.
var SourceTools = []string{"search_wikipedia", "read_wikipedia", "match_taxon",
	"taxon_tree", "taxon_children", "vernacular_names"}

// IsSourceTool — инструмент ли это источников.
func IsSourceTool(name string) bool {
	for _, n := range SourceTools {
		if n == name {
			return true
		}
	}
	return false
}

// Canon — схема в каноническом виде: ключи по алфавиту, без пробелов. Одна
// и та же схема, прошедшая через MCP (JSON → map → JSON), и написанная
// руками выглядят по-разному, а разница в байтах запроса — это разница в
// токенах и в кэше префикса. Не JSON возвращается как есть.
func Canon(schema json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(schema)) == 0 {
		return json.RawMessage(`{"properties":{},"type":"object"}`)
	}
	var v any
	if err := json.Unmarshal(schema, &v); err != nil {
		return schema
	}
	out, err := json.Marshal(v) // map[string]any кодируется с ключами по алфавиту
	if err != nil {
		return schema
	}
	return out
}

// Defs собирает описания инструментов для запроса к модели — всегда в
// каноническом виде.
func Defs(ts []Tool) []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(ts))
	for _, t := range ts {
		s := t.Spec()
		defs = append(defs, llm.NewToolDef(s.Name, s.Description, Canon(s.Parameters)))
	}
	return defs
}

// Fingerprint — отпечаток набора инструментов: sha256 от описаний в том
// виде, в каком их получает модель. Совпадение отпечатков — это совпадение
// запроса побайтно.
func Fingerprint(ts []Tool) string {
	data, _ := json.Marshal(Defs(ts))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// Names — имена инструментов по порядку.
func Names(ts []Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Spec().Name
	}
	return out
}

// Registry — набор инструментов по имени. Агент собирает из него своё
// подмножество: у каждого типа агента свои инструменты.
type Registry struct {
	byName map[string]Tool
	order  []string
}

// NewRegistry собирает реестр из инструментов. Повтор имени — ошибка: два
// разных инструмента под одним именем модель различить не может.
func NewRegistry(ts ...Tool) (*Registry, error) {
	r := &Registry{byName: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		name := t.Spec().Name
		if _, dup := r.byName[name]; dup {
			return nil, fmt.Errorf("инструмент %q объявлен дважды", name)
		}
		r.byName[name] = t
		r.order = append(r.order, name)
	}
	return r, nil
}

// FromSources — реестр из инструментов нескольких источников.
func FromSources(srcs ...Source) (*Registry, error) {
	var all []Tool
	for _, s := range srcs {
		all = append(all, s.Tools()...)
	}
	return NewRegistry(all...)
}

// MustRegistry — то же для инструментов, собранных кодом: повтор имени там —
// ошибка программиста.
func MustRegistry(ts ...Tool) *Registry {
	r, err := NewRegistry(ts...)
	if err != nil {
		panic(err)
	}
	return r
}

// Get возвращает инструмент по имени.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Has — есть ли все инструменты с такими именами.
func (r *Registry) Has(names ...string) bool {
	for _, n := range names {
		if _, ok := r.byName[n]; !ok {
			return false
		}
	}
	return true
}

// Pick выбирает инструменты по именам и в их порядке: порядок в запросе
// задаёт агент, а не источник. Неизвестное имя — ошибка программиста,
// поэтому паника; реестры, пришедшие извне (MCP), проверяются через Has при
// подключении, до сборки агентов.
func (r *Registry) Pick(names ...string) []Tool {
	ts := make([]Tool, 0, len(names))
	for _, name := range names {
		t, ok := r.byName[name]
		if !ok {
			panic(fmt.Sprintf("инструмент %q не зарегистрирован", name))
		}
		ts = append(ts, t)
	}
	return ts
}

// Names — имена всех инструментов реестра по алфавиту.
func (r *Registry) Names() []string {
	names := append([]string(nil), r.order...)
	sort.Strings(names)
	return names
}

// All — все инструменты в порядке добавления.
func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}

// ParseArgs разбирает аргументы вызова. Пустая строка — допустимый вызов
// без аргументов: модели иногда так и присылают. Аргументы — недоверенный
// ввод (Р-2): разбираются строго в структуру, лишнее отбрасывается.
func ParseArgs(args json.RawMessage, target any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, target); err != nil {
		return fmt.Errorf("аргументы не разобрались: %w", err)
	}
	return nil
}

// Result сериализует результат инструмента для модели.
func Result(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("сериализация результата: %w", err)
	}
	return string(data), nil
}

// Truncate обрезает длинный текст по рунам. Обрыв помечается, иначе модель
// не узнает, что текста было больше.
func Truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + " …[обрезано]"
}

type callIDKey struct{}

// WithCallID кладёт в контекст идентификатор вызова модели: обёртки
// инструментов (трекер) связывают по нему факт с событием журнала.
func WithCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callIDKey{}, id)
}

// CallID — идентификатор вызова из контекста; пусто, если вызов сделан не
// моделью (например, кодом координатора).
func CallID(ctx context.Context) string {
	id, _ := ctx.Value(callIDKey{}).(string)
	return id
}
