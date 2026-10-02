package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// AgentConfig — исполнитель-модель: модели даются три инструмента
// конвейера (описания — как их отдал демон в tools/list), задача словами,
// и она сама вызывает их по порядку, передавая данные по ref.
type AgentConfig struct {
	LLM   llm.Chatter
	Model string // пусто — llm.DefaultModel
	// Tools — search, summarize, save_to_file от демона (feed.Remote.Tools
	// или mcp.Client). Других инструментов модели не дают.
	Tools    []tools.Tool
	MaxSteps int // 0 — 8
}

// defaultAgentSteps — сколько ответов модели ждать: три вызова, итоговая
// строка и запас на исправление ошибок.
const defaultAgentSteps = 8

// ErrIncomplete — исполнитель-модель закончил, не пройдя цепочку до файла.
var ErrIncomplete = errors.New("pipeline: цепочка не пройдена до конца")

// CheckQuery — проверка search у исполнителя-модели: искала ли модель тот
// вид, что просили.
const CheckQuery = "запрос как задан"

// agentSystem — задача модели-исполнителя. Данные — только по ref: модель
// не видит досье и фактов целиком (см. forModel), пересказывать ей нечего.
const agentSystem = `Ты — исполнитель конвейера из трёх инструментов. Выполни цепочку строго по порядку:
1. search — найди досье о виде (query — вид как в задаче, или random=true, если просят случайный вид).
2. summarize — передай ref, равный digest из ответа search.
3. save_to_file — передай ref, равный digest из ответа summarize, и format из задачи.

Данные между шагами передавай только по ref: ровно тот digest, что вернул предыдущий шаг, без сокращений и правок. Не выдумывай ref, не передавай input, не пересказывай данные и ничего не добавляй от себя. Каждый инструмент вызывай по одному разу; если инструмент вернул ошибку — прочитай её, исправь вызов и повтори.
После успешного save_to_file ответь одной короткой строкой: путь к сохранённому файлу.`

// RunAgent — исполнитель-модель. После того как модель закончила, код
// проверяет её цепочку так же, как Run: порядок инструментов, вид данных,
// Input шага = Digest предыдущего. Trace.Mode = ModeAgent, CostUSD — вместе
// с расходом самой модели-исполнителя.
//
// Шаги нумеруются по порядку вызовов модели; неудачные вызовы остаются в
// следе (модель могла исправиться). Итог OK, только если среди успешных
// вызовов есть цепочка search → summarize → save_to_file, где вход каждого
// шага — выход предыдущего и File.Chain с ними совпадает. Успешные вызовы
// вне этой цепочки помечаются проверкой «в итоговой цепочке» (✗), но итог
// не портят. Модель передаёт данные только по ref: Request.Pass
// выставляется в ref.
func RunAgent(ctx context.Context, cfg AgentConfig, req Request, onStep func(Step)) (Trace, error) {
	tr := Trace{Request: req, Mode: ModeAgent, Started: time.Now()}
	a := &agentRun{tr: &tr, onStep: onStep}
	finish := func(err error) (Trace, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		tr.Finished = time.Now()
		tr.Took = tr.Finished.Sub(tr.Started)
		tr.OK = err == nil
		if err != nil {
			tr.Error = err.Error()
		}
		out := tr
		out.Steps = make([]Step, len(tr.Steps))
		for i, s := range tr.Steps {
			out.Steps[i] = copyStep(s)
		}
		return out, err
	}
	req, err := req.Normalize()
	req.Pass = PassRef
	tr.Request, a.req = req, req
	if err != nil {
		return finish(err)
	}
	if cfg.LLM == nil {
		return finish(errors.New("pipeline: исполнителю не задана модель"))
	}
	byName := map[string]tools.Tool{}
	for _, t := range cfg.Tools {
		if name := t.Spec().Name; ToolKind(name) != "" {
			byName[name] = t
		}
	}
	var ts []tools.Tool
	for _, name := range ToolNames {
		t, ok := byName[name]
		if !ok {
			return finish(fmt.Errorf("pipeline: исполнителю не дан инструмент %s — у демона нет конвейера?", name))
		}
		ts = append(ts, tools.Wrap(t, a.wrap(name)))
		byName[name] = ts[len(ts)-1]
	}
	model := cfg.Model
	if model == "" {
		model = llm.DefaultModel
	}
	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultAgentSteps
	}

	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: agentSystem},
		{Role: llm.RoleUser, Content: agentTask(req)},
	}
	defs := tools.Defs(ts)
	var modelCost float64
	addCost := func() {
		a.mu.Lock()
		tr.CostUSD += modelCost
		a.mu.Unlock()
	}
	for turn := 0; ; turn++ {
		if turn >= maxSteps {
			addCost()
			return finish(fmt.Errorf("%w: модель не закончила за %d ответов", ErrIncomplete, maxSteps))
		}
		resp, err := cfg.LLM.Chat(ctx, llm.Request{Model: model, Messages: msgs, Tools: defs, Temperature: 0})
		if err != nil {
			addCost()
			return finish(fmt.Errorf("pipeline: модель-исполнитель: %w", err))
		}
		// Цена — по запрошенной модели, как во всём проекте: API отвечает
		// своим именем модели, которого в таблице тарифов может не быть, и
		// тогда расход исполнителя тихо обнулялся бы.
		c := llm.PriceOf(model, resp.Usage, time.Now())
		if !c.Known && resp.Model != "" {
			c = llm.PriceOf(resp.Model, resp.Usage, time.Now())
		}
		modelCost += c.USD
		msg := resp.Message
		msg.Role = llm.RoleAssistant
		msgs = append(msgs, msg)
		if !resp.HasToolCalls() {
			break
		}
		for _, call := range msg.ToolCalls {
			content := a.call(ctx, byName, call)
			msgs = append(msgs, llm.Message{Role: llm.RoleTool, Content: content, ToolCallID: call.ID})
		}
	}
	addCost()
	return finish(a.verify())
}

// agentTask — задача словами.
func agentTask(req Request) string {
	what := fmt.Sprintf("вид «%s» (search с query=%q)", req.Query, req.Query)
	if req.Random {
		what = "случайный вид (search с random=true)"
	}
	return fmt.Sprintf("Собери и сохрани выпуск фактов: %s. Формат файла: %s.", what, req.Format)
}

// agentRun — след исполнителя-модели: вызовы инструментов пишутся шагами.
type agentRun struct {
	req    Request
	onStep func(Step)

	mu    sync.Mutex
	tr    *Trace
	files map[int]*File // N шага save_to_file → данные файла
}

// call исполняет вызов модели; ответ (или ошибка словами) уходит ей.
func (a *agentRun) call(ctx context.Context, byName map[string]tools.Tool, call llm.ToolCall) string {
	name := call.Function.Name
	t, ok := byName[name]
	if !ok {
		st := Step{Tool: name, Status: StepFailed, Args: LogArgs(json.RawMessage(call.Function.Arguments)),
			Started: time.Now(), Error: "такого инструмента у исполнителя нет"}
		a.add(st)
		return fmt.Sprintf("ошибка: инструмента %q нет; доступны: %s", name, strings.Join(ToolNames, ", "))
	}
	out, err := t.Call(tools.WithCallID(ctx, call.ID), json.RawMessage(call.Function.Arguments))
	if err != nil {
		return "ошибка: " + err.Error()
	}
	return out
}

// add дописывает шаг с номером по порядку и сообщает о нём; возвращает индекс.
func (a *agentRun) add(st Step) int {
	a.mu.Lock()
	st.N = len(a.tr.Steps) + 1
	a.tr.Steps = append(a.tr.Steps, st)
	i := len(a.tr.Steps) - 1
	a.mu.Unlock()
	a.emit(i)
	return i
}

func (a *agentRun) emit(i int) {
	if a.onStep == nil {
		return
	}
	a.mu.Lock()
	st := copyStep(a.tr.Steps[i])
	a.mu.Unlock()
	a.onStep(st)
}

// wrap — обёртка инструмента: шаг в след, разбор конверта, проверки
// передачи. Провал проверки — ошибка вызова: модель прочитает её словами.
func (a *agentRun) wrap(name string) func(ctx context.Context, args json.RawMessage, next tools.CallFunc) (string, error) {
	return func(ctx context.Context, args json.RawMessage, next tools.CallFunc) (string, error) {
		i := a.add(Step{Tool: name, Status: StepRunning, Args: LogArgs(args), Started: time.Now()})
		out, callErr := next(ctx, args)

		a.mu.Lock()
		st := &a.tr.Steps[i]
		st.Took = time.Since(st.Started)
		var err error
		if callErr != nil {
			err = &StepError{N: st.N, Tool: name, Err: callErr}
		} else {
			prevStep, prevN := a.input(i, name, args)
			var prev *Envelope
			var chain []string
			if prevStep != nil {
				prev = &Envelope{Kind: prevStep.Kind, Digest: prevStep.Digest, Input: prevStep.Input}
				if name == ToolSaveFile {
					chain = []string{prevStep.Input, prevStep.Digest}
				}
			}
			var file *File
			_, file, err = Inspect(st, json.RawMessage(out), ToolKind(name), prev, prevN, chain)
			a.tr.CostUSD += st.CostUSD
			if name == ToolSearch {
				st.Checks = append(st.Checks, a.queryCheck(args))
			}
			if err == nil && name != ToolSearch && prev == nil {
				err = fmt.Errorf("%w: до %s не было успешного %s", ErrChain, name, prevTool(name))
				st.Checks = append(st.Checks, Check{Name: "вход = выход предыдущего шага", Note: err.Error()})
			}
			if file != nil {
				if a.files == nil {
					a.files = map[int]*File{}
				}
				a.files[st.N] = file
			}
			if err != nil {
				err = &StepError{N: st.N, Tool: name, Err: err}
			}
		}
		if err != nil {
			st.Status, st.Error = StepFailed, err.Error()
		} else {
			st.Status = StepOK
		}
		a.mu.Unlock()
		a.emit(i)
		if err != nil {
			return "", err
		}
		return forModel(out), nil
	}
}

// input — шаг, чей выход модель передала шагу i: успешный шаг предыдущего
// инструмента с digest, равным ref (или отпечатку input), а если такого нет
// — последний успешный шаг предыдущего инструмента. Вызывается под a.mu.
func (a *agentRun) input(i int, name string, args json.RawMessage) (*Step, int) {
	want := prevTool(name)
	if want == "" {
		return nil, 0
	}
	passed := passedDigest(args)
	var last *Step
	for j := i - 1; j >= 0; j-- {
		s := &a.tr.Steps[j]
		if s.Tool != want || s.Status != StepOK {
			continue
		}
		if s.Digest == passed && passed != "" {
			return s, s.N
		}
		if last == nil {
			last = s
		}
	}
	if last == nil {
		return nil, 0
	}
	return last, last.N
}

// queryCheck — искала ли модель то, что просили.
func (a *agentRun) queryCheck(args json.RawMessage) Check {
	var p struct {
		Query  string `json:"query"`
		Random bool   `json:"random"`
	}
	_ = json.Unmarshal(args, &p)
	c := Check{Name: CheckQuery, OK: true}
	switch {
	case a.req.Random && !p.Random:
		c.OK, c.Note = false, fmt.Sprintf("просили случайный вид, а искали %q", p.Query)
	case !a.req.Random && !strings.EqualFold(strings.TrimSpace(p.Query), a.req.Query):
		c.OK, c.Note = false, fmt.Sprintf("просили %q, а искали %q", a.req.Query, p.Query)
	}
	return c
}

// verify — проверка цепочки кодом после того, как модель закончила.
func (a *agentRun) verify() error {
	a.mu.Lock()
	steps := a.tr.Steps
	find := func(before int, tool, digest string) int {
		for j := before - 1; j >= 0; j-- {
			s := steps[j]
			if s.Tool == tool && s.Status == StepOK && (digest == "" || s.Digest == digest) {
				return j
			}
		}
		return -1
	}
	var err error
	s := find(len(steps), ToolSaveFile, "")
	f, d := -1, -1
	switch {
	case s < 0:
		err = fmt.Errorf("%w: модель не сохранила файл — успешного вызова %s нет", ErrIncomplete, ToolSaveFile)
	default:
		if f = find(s, ToolSummarize, steps[s].Input); f < 0 {
			err = fmt.Errorf("%w: файл (шаг %d) получен не из успешного %s перед ним", ErrChain, steps[s].N, ToolSummarize)
		} else if d = find(f, ToolSearch, steps[f].Input); d < 0 {
			err = fmt.Errorf("%w: факты (шаг %d) получены не из успешного %s перед ними", ErrChain, steps[f].N, ToolSearch)
		}
	}
	if err == nil {
		file := a.files[steps[s].N]
		want := []string{steps[d].Digest, steps[f].Digest}
		if file == nil || !slices.Equal(file.Chain, want) {
			err = fmt.Errorf("%w: цепочка в файле не совпала с пройденной %s", ErrChain, shortChain(want))
		} else {
			a.tr.File = file
		}
		if err == nil && !checkOK(steps[d].Checks, CheckQuery) {
			err = fmt.Errorf("pipeline: модель искала не тот вид (шаг %d)", steps[d].N)
		}
	}
	var marked []int
	if err == nil {
		inChain := map[int]bool{d: true, f: true, s: true}
		for j := range steps {
			if steps[j].Status != StepOK {
				continue
			}
			c := Check{Name: CheckInChain, OK: inChain[j]}
			if !c.OK {
				c.Note = "лишний вызов — в итоговую цепочку не вошёл"
			}
			steps[j].Checks = append(steps[j].Checks, c)
			marked = append(marked, j)
		}
	}
	a.mu.Unlock()
	for _, j := range marked {
		a.emit(j)
	}
	return err
}

func checkOK(cs []Check, name string) bool {
	for _, c := range cs {
		if c.Name == name {
			return c.OK
		}
	}
	return true
}

// prevTool — чей выход принимает инструмент.
func prevTool(name string) string {
	switch name {
	case ToolSummarize:
		return ToolSearch
	case ToolSaveFile:
		return ToolSummarize
	}
	return ""
}

// passedDigest — что модель передала на вход: ref или отпечаток конверта input.
func passedDigest(args json.RawMessage) string {
	var p struct {
		Ref   string    `json:"ref"`
		Input *Envelope `json:"input"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return ""
	}
	if p.Ref != "" {
		return strings.TrimSpace(p.Ref)
	}
	if p.Input != nil {
		return p.Input.Digest
	}
	return ""
}

// forModel — ответ инструмента для модели. Досье и факты — без data: модели
// они не нужны (она передаёт их по ref), а десятки килобайт досье в
// контексте стоили бы денег и соблазняли бы пересказать их. Файл — целиком:
// в нём путь и начало текста.
func forModel(out string) string {
	var env Envelope
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.Kind == KindFile {
		return out
	}
	short := struct {
		Kind    string `json:"kind"`
		Digest  string `json:"digest"`
		Input   string `json:"input,omitempty"`
		Summary string `json:"summary,omitempty"`
		Note    string `json:"note"`
	}{env.Kind, env.Digest, env.Input, env.Summary,
		"data скрыта: следующему шагу передай ref, равный digest"}
	b, err := json.Marshal(short)
	if err != nil {
		return out
	}
	return string(b)
}
