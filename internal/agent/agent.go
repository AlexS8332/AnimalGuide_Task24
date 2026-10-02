// Package agent — цикл одного агента и контракты журнала. Пакет не знает ни
// о конкретных агентах, ни о хранилищах, ни об HTTP: его импортируют и
// агенты, и прогоны, и сервер.
//
// Журнал и результат — два потока с разными потребителями (ИП-9): события
// уходят в журнал хода, а промежуточные результаты (карточка по мере
// сборки) — отдельным каналом Publish.
package agent

import (
	"context"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tokens"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Виды событий журнала.
const (
	EventAgentStart = "agent.start"
	EventAgentDone  = "agent.done"
	EventAgentError = "agent.error"
	EventLLMRequest = "llm.request"
	EventLLMReply   = "llm.response"
	EventToolCall   = "tool.call"
	EventToolResult = "tool.result"
	EventToolError  = "tool.error"
	EventNote       = "note"
	// EventPrompt — что получает модель на старте агента: системный промпт,
	// блоки механизмов, новое сообщение, описания инструментов. В Detail —
	// JSON вида Prompt; интерфейс показывает его вкладкой «Промпты».
	EventPrompt = "prompt"
	// EventInjection — в ответе источника найдены признаки попытки
	// управлять агентом (ФТ-44). Это не блокировка, а видимость.
	EventInjection = "source.injection"
	// EventMechanism — событие обвязки: выключенный механизм, откат на
	// уровень ниже (ФТ-48), работа привратника, извлекателя, стража.
	EventMechanism = "mechanism"
)

// Event — запись журнала. Title — одна строка для ленты, Detail — текст под
// ней. Data — структурированные данные для интерфейса (правки памяти,
// переход подборки, вердикт стража): агенту не нужно знать их типы, их
// кладёт тот, кто их породил.
type Event struct {
	Seq     int        `json:"seq"`
	Time    time.Time  `json:"time"`
	Agent   string     `json:"agent"`
	Kind    string     `json:"kind"`
	Step    int        `json:"step,omitempty"`
	Title   string     `json:"title"`
	Detail  string     `json:"detail,omitempty"`
	Usage   *llm.Usage `json:"usage,omitempty"`
	Cost    *llm.Cost  `json:"cost,omitempty"`
	Seconds float64    `json:"seconds,omitempty"`
	// Tokens — оценка перед запросом и факт по ответу модели.
	Tokens *tokens.Tokens `json:"tokens,omitempty"`
	// Tool и CallID — какой вызов инструмента описывает событие. По CallID
	// значок «почему так» у факта карточки ведёт на событие журнала, откуда
	// факт пришёл (ФТ-12).
	Tool   string `json:"tool,omitempty"`
	CallID string `json:"callId,omitempty"`
	// Via — каким путём шёл вызов инструмента источника: в процессе или
	// через MCP-сервер.
	Via string `json:"via,omitempty"`
	// Final — вызов завершающего инструмента; Rejected — результат не
	// принят кодом.
	Final    bool `json:"final,omitempty"`
	Rejected bool `json:"rejected,omitempty"`
	// Hits — признаки попытки управлять агентом в тексте источника.
	Hits []tools.Hit `json:"hits,omitempty"`
	// Mechanism — механизм, которому принадлежит событие обвязки.
	Mechanism string `json:"mechanism,omitempty"`
	Data      any    `json:"data,omitempty"`
}

// Update — промежуточный результат хода: карточка, раздел, сравнение по
// мере сборки (ФТ-14). Kind различает, что пришло.
type Update struct {
	Kind string `json:"kind"`
	Data any    `json:"data"`
}

// Emitter принимает события журнала и промежуточные результаты. Реализация
// обязана быть безопасной для вызова из нескольких горутин: специалисты в
// команде работают параллельно.
type Emitter interface {
	Log(Event)
	Publish(Update)
}

type emitterKey struct{}

// WithEmitter кладёт журнал хода в контекст. Нужен тем, кто работает на
// границе хода и не получает эмиттер параметром: реализация пути до
// источников пишет в журнал, как подключилась, не расширяя интерфейс.
func WithEmitter(ctx context.Context, em Emitter) context.Context {
	return context.WithValue(ctx, emitterKey{}, em)
}

// EmitterFrom — журнал хода из контекста; Nop, если его там нет.
func EmitterFrom(ctx context.Context) Emitter {
	if em, ok := ctx.Value(emitterKey{}).(Emitter); ok && em != nil {
		return em
	}
	return Nop{}
}

// Nop — эмиттер, который всё отбрасывает.
type Nop struct{}

func (Nop) Log(Event)      {}
func (Nop) Publish(Update) {}

// Recorder — эмиттер, который всё запоминает: для тестов и для прогонов,
// где журнал нужен после хода.
type Recorder struct {
	Events  []Event
	Updates []Update
}

// Log копит события: удобно в тестах и там, где журнал нужен после хода.
func (r *Recorder) Log(e Event) { r.Events = append(r.Events, e) }

// Publish копит промежуточные результаты.
func (r *Recorder) Publish(u Update) { r.Updates = append(r.Updates, u) }

// Kinds — виды событий по порядку.
func (r *Recorder) Kinds() []string {
	out := make([]string, len(r.Events))
	for i, e := range r.Events {
		out[i] = e.Kind
	}
	return out
}

// Prompt — содержимое события EventPrompt.
type Prompt struct {
	Agent  string        `json:"agent"`
	System string        `json:"system"`
	Blocks []PromptBlock `json:"blocks"`
	User   string        `json:"user"`
	Tools  []PromptTool  `json:"tools"`
	// History — сколько сообщений прошлых ходов ушло модели перед новым
	// сообщением, и сколько в них символов.
	History      int             `json:"history"`
	HistoryRunes int             `json:"historyRunes"`
	Estimate     tokens.Estimate `json:"estimate"`
	Limit        int             `json:"limit,omitempty"`
	// Fingerprint — отпечаток описаний инструментов: по нему видно, что
	// дорожки стенда получили побайтно одно и то же.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// PromptBlock — блок механизма в том виде, в каком его получила модель.
type PromptBlock struct {
	Feature string `json:"feature"`
	Title   string `json:"title,omitempty"`
	Text    string `json:"text"`
	Tokens  int    `json:"tokens"`
}

// PromptTool — инструмент, доступный агенту, как он описан модели.
type PromptTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Final       bool   `json:"final,omitempty"`
	Via         string `json:"via,omitempty"`
}

// Stats — счётчики одного прогона цикла.
type Stats struct {
	Steps     int       `json:"steps"`
	ToolCalls int       `json:"toolCalls"`
	Usage     llm.Usage `json:"usage"`
	Cost      llm.Cost  `json:"cost"`
	Context   Context   `json:"context"`
	// Reminders — сколько раз модели напомнили, что закончить надо
	// завершающим инструментом; Rejected — сколько результатов код не
	// принял.
	Reminders int `json:"reminders,omitempty"`
	Rejected  int `json:"rejected,omitempty"`
}

// Add складывает счётчики нескольких прогонов (ход из нескольких агентов).
// Контекст берётся от самого тяжёлого прогона: разбивка по блокам нужна
// там, где запрос больше всего, — у привратника на 300 токенов её смотреть
// незачем.
func (s Stats) Add(o Stats) Stats {
	out := s
	out.Steps += o.Steps
	out.ToolCalls += o.ToolCalls
	out.Usage = s.Usage.Add(o.Usage)
	out.Cost = s.Cost.Add(o.Cost)
	out.Reminders += o.Reminders
	out.Rejected += o.Rejected
	if o.Context.Estimate.Total > out.Context.Estimate.Total {
		peak := out.Context.Peak
		out.Context = o.Context
		if peak > out.Context.Peak {
			out.Context.Peak = peak
		}
	}
	if o.Context.Peak > out.Context.Peak {
		out.Context.Peak = o.Context.Peak
	}
	return out
}

// Context — что занимало контекст в прогоне. Estimate — оценка на старте с
// разбивкой по блокам; Peak — оценка перед последним запросом; FirstPrompt
// — факт первого запроса, с ним сравнима оценка старта.
type Context struct {
	Estimate    tokens.Estimate `json:"estimate"`
	Peak        int             `json:"peak"`
	FirstPrompt int             `json:"firstPrompt"`
	CacheHit    int             `json:"cacheHit"`
	Limit       int             `json:"limit,omitempty"`
	// Trimmed — сколько сообщений истории выброшено, чтобы ход влез.
	Trimmed int `json:"trimmed,omitempty"`
}
