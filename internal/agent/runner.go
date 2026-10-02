package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tokens"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

const (
	// Сколько символов результата инструмента и ответа модели показывать в
	// журнале. Полный текст уходит модели, а в ленте нужен обозримый кусок.
	logDetailRunes = 6000

	defaultMaxSteps = 12

	// MaxReminders — сколько раз напоминать модели, что закончить надо
	// вызовом завершающего инструмента, прежде чем признать ход неудачным
	// (ФТ-8: «после двух — ход считается неудачным»).
	MaxReminders = 2
)

var (
	// ErrStepLimit — агент не уложился в лимит шагов.
	ErrStepLimit = errors.New("исчерпан лимит шагов агента")
	// ErrContextOverflow — запрос не влезает в свой лимит контекста.
	ErrContextOverflow = errors.New("запрос не влезает в контекст")
	// ErrProtocol — модель отвечает текстом вместо завершающего инструмента.
	ErrProtocol = errors.New("модель отвечает текстом вместо вызова завершающего инструмента")
)

// Что делать, когда запрос не влезает в лимит контекста (ФТ-33).
const (
	OverflowFail = "fail"
	OverflowTrim = "trim"
	OverflowOff  = "off"
)

// Finisher — завершающий инструмент: модель вызывает его, когда готова
// отдать результат. Handle проверяет результат кодом — по трекеру реальных
// вызовов, а не по словам модели (ИП-1). Ошибка Handle уходит модели
// результатом вызова, и цикл продолжается: отказ обязан объяснить, что
// сделать, чтобы результат приняли (ИП-8).
type Finisher struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Handle      func(ctx context.Context, callID string, args json.RawMessage) (any, error)
}

// Spec — описание агента для Runner.
type Spec struct {
	Name   string
	System string
	Tools  []tools.Tool
	Finish []Finisher
	// MaxSteps — предел запросов к модели за прогон.
	MaxSteps int
	// AllowText — можно закончить обычным текстом. Без него при заданных
	// завершающих инструментах текст — ошибка протокола: модели напоминают,
	// после MaxReminders напоминаний прогон неудачен.
	AllowText bool
	// MaxTokens — предел ответа модели; 0 — без предела.
	MaxTokens int
}

// Prepared — что уходит модели, уже после работы с памятью и профилем.
// Blocks — блоки механизмов в порядке реестра; History — окно сообщений,
// которые модель получит дословно.
type Prepared struct {
	Blocks  []features.Block
	History []llm.Message
	User    string
	// Features — набор механизмов диалога: по нему Runner решает, класть ли
	// пометку источника и искать ли признаки попытки управлять агентом.
	Features features.Set
	// Preload — вызовы, которые код делает до первого запроса к модели.
	Preload []Preload
}

// Preload — вызов инструмента агента, который код делает сам до первого
// запроса к модели. Годится для шага без выбора — того, что агент по своему
// промпту всё равно сделал бы первым (поиск по названию из запроса, раздел,
// чьё название совпало с темой): запрос к модели ради такого шага — чистый
// расход бюджета «запросов на ход».
//
// Вызов идёт тем же путём, что вызов моделью: инструмент из Spec.Tools (его
// видит трекер), тот же журнал, пометка источника и поиск попыток управлять
// агентом. Модель получает его как свой первый шаг — сообщение с вызовом и
// ответ инструмента, — и дальше решает сама: результат можно не
// использовать, а любой инструмент вызвать заново.
type Preload struct {
	Tool string
	Args string
}

// preloadID — id вызова кодом. Не счётчик: один и тот же вызов должен
// давать те же байты запроса на любом пути до источника и в любом прогоне
// (кэш префикса, сверка путей в И-7), а разные вызовы одного хода —
// разные id («почему так» ищет событие по id).
func preloadID(agent string, p Preload) string {
	sum := sha256.Sum256([]byte(agent + "\x00" + p.Tool + "\x00" + p.Args))
	return fmt.Sprintf("pre_%s_%x", p.Tool, sum[:6])
}

// Reply — итог прогона. Added — всё, что добавилось после истории:
// сообщение пользователя, ответы модели, ответы инструментов, итоговый
// текст. Final — результат принятого завершающего инструмента.
type Reply struct {
	Text        string        `json:"text"`
	Added       []llm.Message `json:"-"`
	Final       any           `json:"-"`
	FinalTool   string        `json:"finalTool,omitempty"`
	FinalCallID string        `json:"finalCallId,omitempty"`
	Stats       Stats         `json:"stats"`
}

// Runner исполняет цикл одного прогона: запрос к модели, вызовы
// инструментов, ответ. Один Runner обслуживает всех агентов приложения.
type Runner struct {
	LLM         llm.Chatter
	Model       string
	Temperature float64
	// ContextLimit — свой лимит контекста в токенах, 0 — не проверять.
	ContextLimit int
	// OnOverflow — OverflowFail (по умолчанию), OverflowTrim, OverflowOff.
	OnOverflow string
	// Calibration — поправка оценки по фактам; nil — без поправки.
	Calibration *tokens.Calibration
}

// Run ведёт прогон: модель получает системный промпт, блоки механизмов,
// окно истории и новое сообщение; цикл крутится, пока модель просит
// инструменты, и заканчивается ответом текстом или принятым завершающим
// инструментом.
func (r Runner) Run(ctx context.Context, spec Spec, in Prepared, em Emitter) (Reply, error) {
	if em == nil {
		em = Nop{}
	}
	if spec.MaxSteps <= 0 {
		spec.MaxSteps = defaultMaxSteps
	}
	history := in.History

	byName := make(map[string]tools.Tool, len(spec.Tools))
	for _, t := range spec.Tools {
		byName[t.Spec().Name] = t
	}
	defs := tools.Defs(spec.Tools)
	finishers := make(map[string]Finisher, len(spec.Finish))
	for _, f := range spec.Finish {
		finishers[f.Name] = f
		defs = append(defs, llm.NewToolDef(f.Name, f.Description, tools.Canon(f.Parameters)))
	}

	parts := tokens.Parts{System: spec.System, Blocks: in.Blocks, Tools: defs, History: history, User: in.User}
	est := tokens.Of(parts)
	var trimmed int
	if r.ContextLimit > 0 && r.Calibration.Apply(est.Total) > r.ContextLimit {
		switch r.OnOverflow {
		case OverflowOff:
		case OverflowTrim:
			history, trimmed = trimHistory(parts, r.ContextLimit, r.Calibration)
			parts.History = history
			est = tokens.Of(parts)
			em.Log(Event{Agent: spec.Name, Kind: EventNote,
				Title: fmt.Sprintf("история обрезана: выброшено сообщений %d, контекст ≈ %d токенов при лимите %d",
					trimmed, est.Total, r.ContextLimit),
				Detail: "Выброшены самые старые ходы целиком — от сообщения пользователя до следующего: " +
					"ответ инструмента без вызова, который его породил, API отвергает. Файл диалога не меняется."})
			if r.Calibration.Apply(est.Total) > r.ContextLimit {
				return Reply{}, overflowError(est.Total, r.ContextLimit)
			}
		default:
			return Reply{}, overflowError(r.Calibration.Apply(est.Total), r.ContextLimit)
		}
	}

	messages := make([]llm.Message, 0, len(history)+len(in.Blocks)+4)
	messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: spec.System})
	// Блоки идут отдельными системными сообщениями сразу за промптом, в
	// порядке реестра: от самого устойчивого к самому изменчивому (ИП-3).
	// Кэш префикса считается от начала запроса, и блок, поставленный раньше
	// своего места, обнулял бы скидку на каждой своей правке.
	for _, b := range in.Blocks {
		if strings.TrimSpace(b.Text) == "" {
			continue
		}
		messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: b.Text})
	}
	messages = append(messages, history...)
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: in.User})
	base := len(messages) - 1

	em.Log(Event{Agent: spec.Name, Kind: EventPrompt, Title: "промпт: " + spec.Name,
		Detail: promptDetail(spec, in, history, est, r.ContextLimit)})

	stats := Stats{Context: Context{Estimate: est, Limit: r.ContextLimit, Trimmed: trimmed}}
	reminders := 0

	if len(in.Preload) > 0 {
		call := llm.Message{Role: llm.RoleAssistant}
		var replies []llm.Message
		for _, p := range in.Preload {
			t, ok := byName[p.Tool]
			if !ok {
				em.Log(Event{Agent: spec.Name, Kind: EventNote, Tool: p.Tool,
					Title: "вызов " + p.Tool + " кодом пропущен: у агента нет такого инструмента"})
				continue
			}
			tc := llm.ToolCall{ID: preloadID(spec.Name, p), Type: "function",
				Function: llm.FunctionCall{Name: p.Tool, Arguments: p.Args}}
			call.ToolCalls = append(call.ToolCalls, tc)
			stats.ToolCalls++
			replies = append(replies, toolReply(tc.ID, callTool(ctx, spec.Name, 0, t, tc, in.Features, em, " кодом до первого запроса")))
		}
		if len(call.ToolCalls) > 0 {
			messages = append(messages, call)
			messages = append(messages, replies...)
		}
	}

	for step := 1; step <= spec.MaxSteps; step++ {
		stats.Steps = step
		stepEst := tokens.OfMessages(defs, messages)
		if stepEst > stats.Context.Peak {
			stats.Context.Peak = stepEst
		}
		if r.ContextLimit > 0 && r.OnOverflow != OverflowOff && r.Calibration.Apply(stepEst) > r.ContextLimit {
			return Reply{Stats: stats}, fmt.Errorf("шаг %d: %w", step, overflowError(stepEst, r.ContextLimit))
		}

		em.Log(Event{Agent: spec.Name, Kind: EventLLMRequest, Step: step,
			Title: fmt.Sprintf("запрос к модели, сообщений: %d (истории: %d), контекст ≈ %d токенов",
				len(messages), len(history), stepEst),
			Detail: lastMessageDetail(messages),
			Tokens: &tokens.Tokens{Estimated: stepEst}})

		started := time.Now()
		resp, err := r.LLM.Chat(ctx, llm.Request{
			Model:       r.Model,
			Messages:    messages,
			Tools:       defs,
			Temperature: r.Temperature,
			MaxTokens:   spec.MaxTokens,
		})
		elapsed := time.Since(started)
		if err != nil {
			return Reply{Stats: stats}, fmt.Errorf("шаг %d: %w", step, err)
		}
		if r.Calibration != nil {
			r.Calibration.Observe(stepEst, resp.Usage.Prompt)
		}

		cost := llm.PriceOf(r.Model, resp.Usage, started)
		stats.Usage = stats.Usage.Add(resp.Usage)
		stats.Cost = stats.Cost.Add(cost)
		if step == 1 {
			stats.Context.FirstPrompt = resp.Usage.Prompt
			stats.Context.CacheHit = resp.Usage.CacheHit
		}
		usage := resp.Usage
		tk := tokens.Compare(stepEst, resp.Usage.Prompt)
		em.Log(Event{Agent: spec.Name, Kind: EventLLMReply, Step: step,
			Title:   replyTitle(resp),
			Detail:  replyDetail(resp),
			Usage:   &usage,
			Cost:    &cost,
			Seconds: elapsed.Seconds(),
			Tokens:  &tk})

		messages = append(messages, resp.Message)

		if !resp.HasToolCalls() {
			if spec.AllowText || len(spec.Finish) == 0 {
				added := append([]llm.Message(nil), messages[base:]...)
				return Reply{Text: replyText(added), Added: added, Stats: stats}, nil
			}
			if reminders >= MaxReminders {
				stats.Reminders = reminders
				return Reply{Stats: stats}, fmt.Errorf("%w: после %d напоминаний", ErrProtocol, reminders)
			}
			reminders++
			stats.Reminders = reminders
			reminder := "Ответ текстом не принимается. Заверши работу вызовом одного из инструментов: " +
				strings.Join(finisherNames(spec.Finish), ", ") + "."
			em.Log(Event{Agent: spec.Name, Kind: EventNote, Step: step,
				Title: fmt.Sprintf("модель ответила текстом, напоминание %d из %d о завершающем инструменте", reminders, MaxReminders)})
			messages = append(messages, llm.Message{Role: llm.RoleUser, Content: reminder})
			continue
		}

		var final *Reply
		for _, call := range resp.Message.ToolCalls {
			stats.ToolCalls++
			name := call.Function.Name

			if f, ok := finishers[name]; ok {
				em.Log(Event{Agent: spec.Name, Kind: EventToolCall, Step: step, Tool: name, CallID: call.ID,
					Final: true, Title: "завершение: " + name, Detail: prettyJSON(call.Function.Arguments)})
				if final != nil {
					// Второй результат в том же ответе: принят первый.
					messages = append(messages, toolReply(call.ID, errorPayload(errors.New("результат уже принят этим же ответом"))))
					continue
				}
				res, err := f.Handle(ctx, call.ID, json.RawMessage(call.Function.Arguments))
				if err != nil {
					stats.Rejected++
					em.Log(Event{Agent: spec.Name, Kind: EventToolError, Step: step, Tool: name, CallID: call.ID,
						Final: true, Rejected: true, Title: name + ": результат не принят", Detail: err.Error()})
					messages = append(messages, toolReply(call.ID, errorPayload(err)))
					continue
				}
				em.Log(Event{Agent: spec.Name, Kind: EventToolResult, Step: step, Tool: name, CallID: call.ID,
					Final: true, Title: name + ": результат принят"})
				messages = append(messages, toolReply(call.ID, `{"accepted":true}`))
				final = &Reply{Final: res, FinalTool: name, FinalCallID: call.ID}
				continue
			}

			t, ok := byName[name]
			if !ok {
				err := fmt.Errorf("инструмента %s нет; доступны: %s", name, strings.Join(toolNames(spec), ", "))
				em.Log(Event{Agent: spec.Name, Kind: EventToolError, Step: step, Tool: name, CallID: call.ID,
					Title: "неизвестный инструмент " + name, Detail: err.Error()})
				messages = append(messages, toolReply(call.ID, errorPayload(err)))
				continue
			}
			messages = append(messages, toolReply(call.ID, callTool(ctx, spec.Name, step, t, call, in.Features, em, "")))
		}
		if final != nil {
			added := append([]llm.Message(nil), messages[base:]...)
			final.Added = added
			final.Text = replyText(added)
			final.Stats = stats
			return *final, nil
		}
	}

	return Reply{Stats: stats}, fmt.Errorf("%w (%d)", ErrStepLimit, spec.MaxSteps)
}

// callTool исполняет вызов инструмента — модели или кода до первого
// запроса — и возвращает то, что уйдёт модели: ответ с пометкой источника
// или ошибку словами (ФТ-2). Журнал у обоих путей один.
func callTool(ctx context.Context, agentName string, step int, t tools.Tool, call llm.ToolCall, fs features.Set, em Emitter, by string) string {
	s := t.Spec()
	name := call.Function.Name
	em.Log(Event{Agent: agentName, Kind: EventToolCall, Step: step, Tool: name, CallID: call.ID, Via: s.Via,
		Title: "вызов " + name + viaNote(s.Via) + by, Detail: prettyJSON(call.Function.Arguments)})

	started := time.Now()
	out, err := t.Call(tools.WithCallID(ctx, call.ID), json.RawMessage(call.Function.Arguments))
	elapsed := time.Since(started)
	if err != nil {
		em.Log(Event{Agent: agentName, Kind: EventToolError, Step: step, Tool: name, CallID: call.ID, Via: s.Via,
			Title: name + ": ошибка", Detail: err.Error(), Seconds: elapsed.Seconds()})
		return errorPayload(err)
	}
	em.Log(Event{Agent: agentName, Kind: EventToolResult, Step: step, Tool: name, CallID: call.ID, Via: s.Via,
		Title:   fmt.Sprintf("%s: %s", name, sizeLabel(out)),
		Detail:  tools.Truncate(prettyJSON(out), logDetailRunes),
		Seconds: elapsed.Seconds()})
	if s.Untrusted {
		if fs.On(features.Scan) {
			if hits := tools.ScanInjection(out); len(hits) > 0 {
				em.Log(Event{Agent: agentName, Kind: EventInjection, Step: step, Tool: name, CallID: call.ID,
					Mechanism: string(features.Scan), Hits: hits,
					Title:  fmt.Sprintf("в ответе %s похоже на указания агенту: %s", name, hitNames(hits)),
					Detail: "Фрагмент помечен, но не вырезан: это данные источника, и что с ними сделает агент — видно дальше по журналу."})
			}
		}
		if fs.On(features.Envelope) {
			out = tools.Envelope(name, out)
		}
	}
	return out
}

func viaNote(via string) string {
	if via == tools.ViaMCP {
		return " (через MCP)"
	}
	return ""
}

func hitNames(hits []tools.Hit) string {
	names := make([]string, len(hits))
	for i, h := range hits {
		names[i] = h.Pattern
	}
	return strings.Join(names, ", ")
}

// replyText собирает ответ пользователю из всех сообщений модели за прогон:
// модель нередко отвечает на часть вопроса в том же сообщении, в котором
// просит вызвать инструмент.
func replyText(added []llm.Message) string {
	var parts []string
	for _, m := range added {
		if m.Role == llm.RoleAssistant && strings.TrimSpace(m.Content) != "" {
			parts = append(parts, strings.TrimSpace(m.Content))
		}
	}
	return strings.Join(parts, "\n\n")
}

func overflowError(estimated, limit int) error {
	return fmt.Errorf("%w: ≈%d токенов при лимите %d (лишних ≈%d)",
		ErrContextOverflow, estimated, limit, estimated-limit)
}

// trimHistory выбрасывает самые старые ходы, пока запрос не влезет в лимит.
// Ход выбрасывается целиком, от сообщения пользователя до следующего: ответ
// инструмента без вызова, который его породил, API отвергает.
func trimHistory(parts tokens.Parts, limit int, cal *tokens.Calibration) ([]llm.Message, int) {
	history, dropped := parts.History, 0
	for len(history) > 0 {
		parts.History = history
		if cal.Apply(tokens.Of(parts).Total) <= limit {
			break
		}
		next := 1
		for next < len(history) && history[next].Role != llm.RoleUser {
			next++
		}
		dropped += next
		history = history[next:]
	}
	return history, dropped
}

// promptDetail — что уходит модели на старте, в том же виде, в каком это
// получает модель.
func promptDetail(spec Spec, in Prepared, history []llm.Message, est tokens.Estimate, limit int) string {
	p := Prompt{Agent: spec.Name, System: spec.System, User: in.User, Tools: []PromptTool{}, Blocks: []PromptBlock{},
		History: len(history), Estimate: est, Limit: limit, Fingerprint: tools.Fingerprint(spec.Tools)}
	for _, b := range in.Blocks {
		if strings.TrimSpace(b.Text) == "" {
			continue
		}
		p.Blocks = append(p.Blocks, PromptBlock{Feature: string(b.Feature), Title: b.Title, Text: b.Text, Tokens: est.Blocks[b.Feature]})
	}
	for _, m := range history {
		p.HistoryRunes += len([]rune(m.Content))
		for _, c := range m.ToolCalls {
			p.HistoryRunes += len([]rune(c.Function.Arguments))
		}
	}
	for _, t := range spec.Tools {
		s := t.Spec()
		p.Tools = append(p.Tools, PromptTool{Name: s.Name, Description: s.Description, Via: s.Via})
	}
	for _, f := range spec.Finish {
		p.Tools = append(p.Tools, PromptTool{Name: f.Name, Description: f.Description, Final: true})
	}
	data, err := json.Marshal(p)
	if err != nil {
		return spec.System + "\n\n" + in.User
	}
	return string(data)
}

func toolReply(callID, content string) llm.Message {
	return llm.Message{Role: llm.RoleTool, ToolCallID: callID, Content: content}
}

// errorPayload — ошибка в виде JSON: модели проще отличить её от результата
// (ФТ-2: ошибка источника возвращается словами, а не роняет ход).
func errorPayload(err error) string {
	data, _ := json.Marshal(map[string]string{"error": err.Error()})
	return string(data)
}

func finisherNames(fs []Finisher) []string {
	names := make([]string, 0, len(fs))
	for _, f := range fs {
		names = append(names, f.Name)
	}
	return names
}

func toolNames(spec Spec) []string {
	names := tools.Names(spec.Tools)
	return append(names, finisherNames(spec.Finish)...)
}

func replyTitle(resp llm.Response) string {
	if !resp.HasToolCalls() {
		return "ответ текстом"
	}
	names := make([]string, 0, len(resp.Message.ToolCalls))
	for _, c := range resp.Message.ToolCalls {
		names = append(names, c.Function.Name)
	}
	return "модель просит вызвать: " + strings.Join(names, ", ")
}

func replyDetail(resp llm.Response) string {
	if resp.Message.Content == "" {
		return ""
	}
	return tools.Truncate(resp.Message.Content, logDetailRunes)
}

// lastMessageDetail показывает, что ушло модели последним: на первом шаге
// это сообщение пользователя, дальше — результаты инструментов.
func lastMessageDetail(messages []llm.Message) string {
	if len(messages) == 0 {
		return ""
	}
	last := messages[len(messages)-1]
	if last.Role == llm.RoleUser {
		return tools.Truncate(last.Content, logDetailRunes)
	}
	return ""
}

func sizeLabel(s string) string {
	n := len([]rune(s))
	if n < 1000 {
		return fmt.Sprintf("%d символов", n)
	}
	return fmt.Sprintf("%.1f тыс. символов", float64(n)/1000)
}

// prettyJSON форматирует JSON для журнала; не JSON возвращается как есть.
func prettyJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(data)
}

// Tee — эмиттер, который пишет сразу в несколько. Потокобезопасен, если
// потокобезопасны получатели.
type Tee []Emitter

func (t Tee) Log(e Event) {
	for _, em := range t {
		em.Log(e)
	}
}

func (t Tee) Publish(u Update) {
	for _, em := range t {
		em.Publish(u)
	}
}

// Safe — эмиттер под замком: для получателей, которые сами не
// потокобезопасны (Recorder в тестах с параллельными специалистами).
type Safe struct {
	mu sync.Mutex
	E  Emitter
}

func (s *Safe) Log(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.E.Log(e)
}

func (s *Safe) Publish(u Update) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.E.Publish(u)
}
