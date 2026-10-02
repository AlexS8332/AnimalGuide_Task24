package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Исполнители цепочки (Trace.Mode).
const (
	ModeCode  = "code"  // Run: цепочку ведёт код
	ModeAgent = "agent" // RunAgent: цепочку ведёт модель
)

// ErrRequest — запрос не собрать: нет вида, неизвестный формат или способ
// передачи. До демона такой запрос не доходит.
var ErrRequest = errors.New("pipeline: неверный запрос")

// Названия проверок передачи (Check.Name).
const (
	CheckEnvelope = "ответ — конверт"
	CheckKind     = "вид данных"
	CheckDigest   = "отпечаток ответа"
	CheckChain    = "цепочка в файле"
	// CheckInput — «вход = выход шага N»: номер шага подставляется.
	CheckInput = "вход = выход шага %d"
	// CheckInChain — только у исполнителя-модели: вошёл ли успешный вызов
	// в итоговую цепочку.
	CheckInChain = "в итоговой цепочке"
)

// ToolKind — какой вид данных отдаёт инструмент конвейера; пусто — не наш.
func ToolKind(tool string) string {
	switch tool {
	case ToolSearch:
		return KindDossier
	case ToolSummarize:
		return KindFacts
	case ToolSaveFile:
		return KindFile
	}
	return ""
}

// Normalize — запрос с умолчаниями (md, inline) или ErrRequest с
// причиной словами.
func (r Request) Normalize() (Request, error) {
	r.Query = strings.TrimSpace(r.Query)
	r.Format = strings.ToLower(strings.TrimSpace(r.Format))
	r.Pass = strings.ToLower(strings.TrimSpace(r.Pass))
	if r.Format == "" {
		r.Format = FormatMarkdown
	}
	if r.Pass == "" {
		r.Pass = PassInline
	}
	switch {
	case r.Random && r.Query != "":
		return r, fmt.Errorf("%w: задан и вид %q, и случайный вид — нужно что-то одно", ErrRequest, r.Query)
	case !r.Random && r.Query == "":
		return r, fmt.Errorf("%w: не задан вид — укажи название или случайный вид", ErrRequest)
	case r.Format != FormatMarkdown && r.Format != FormatJSON:
		return r, fmt.Errorf("%w: формат %q, нужен %s или %s", ErrRequest, r.Format, FormatMarkdown, FormatJSON)
	case r.Pass != PassInline && r.Pass != PassRef:
		return r, fmt.Errorf("%w: передача %q, нужна %s или %s", ErrRequest, r.Pass, PassInline, PassRef)
	}
	return r, nil
}

// Run — исполнитель-код: search → summarize → save_to_file по MCP через c,
// с проверками передачи на каждом шаге. onStep (может быть nil) зовётся при
// каждом изменении шага: pending → running → ok/failed. Trace возвращается
// всегда, даже с ошибкой: в нём видно, какой шаг упал.
//
// Ошибка шага оборачивает причину так, что errors.Is узнаёт ErrDigest,
// ErrKind, ErrChain (и ErrRef, ErrInput) — и когда её нашёл сам Run, и
// когда инструмент ответил ею словами.
func Run(ctx context.Context, c Caller, req Request, onStep func(Step)) (Trace, error) {
	tr := Trace{Request: req, Mode: ModeCode, Started: time.Now()}
	finish := func(err error) (Trace, error) {
		tr.Finished = time.Now()
		tr.Took = tr.Finished.Sub(tr.Started)
		tr.OK = err == nil
		if err != nil {
			tr.Error = err.Error()
		}
		return tr, err
	}
	req, err := req.Normalize()
	tr.Request = req
	if err != nil {
		return finish(err)
	}
	if c == nil {
		return finish(errors.New("pipeline: не задан клиент MCP"))
	}
	emit := func(i int) {
		if onStep != nil {
			onStep(copyStep(tr.Steps[i]))
		}
	}
	for i, name := range ToolNames {
		tr.Steps = append(tr.Steps, Step{N: i + 1, Tool: name, Status: StepPending})
	}
	for i := range tr.Steps {
		emit(i)
	}

	var prev []Envelope // выходы пройденных шагов по порядку
	for i := range tr.Steps {
		st := &tr.Steps[i]
		args, err := stepArgs(req, st.Tool, prev)
		if err != nil {
			return finish(err)
		}
		st.Args = LogArgs(args)
		st.Status, st.Started = StepRunning, time.Now()
		emit(i)

		raw, callErr := c.Call(ctx, st.Tool, args)
		st.Took = time.Since(st.Started)
		var env Envelope
		if callErr != nil {
			err = &StepError{N: st.N, Tool: st.Tool, Err: callErr}
		} else {
			var in *Envelope
			if len(prev) > 0 {
				in = &prev[len(prev)-1]
			}
			var chain []string
			if st.Tool == ToolSaveFile {
				chain = []string{prev[0].Digest, prev[1].Digest}
			}
			var file *File
			env, file, err = Inspect(st, raw, ToolKind(st.Tool), in, len(prev), chain)
			if file != nil {
				tr.File = file
			}
			tr.CostUSD += st.CostUSD
			if err != nil {
				err = &StepError{N: st.N, Tool: st.Tool, Err: err}
			}
		}
		if err != nil {
			st.Status, st.Error = StepFailed, err.Error()
			emit(i)
			return finish(err)
		}
		st.Status = StepOK
		emit(i)
		prev = append(prev, env)
	}
	return finish(nil)
}

// stepArgs — аргументы вызова шага: search — запрос, остальные — вход
// (конверт или ref) плюс format у save_to_file.
func stepArgs(req Request, tool string, prev []Envelope) (json.RawMessage, error) {
	args := map[string]any{}
	switch tool {
	case ToolSearch:
		if req.Random {
			args["random"] = true
		} else {
			args["query"] = req.Query
		}
	default:
		if len(prev) == 0 {
			return nil, fmt.Errorf("pipeline: у шага %s нет входа", tool)
		}
		in := prev[len(prev)-1]
		if req.Pass == PassRef {
			args["ref"] = in.Digest
		} else {
			args["input"] = in
		}
		if tool == ToolSaveFile {
			args["format"] = req.Format
		}
	}
	return json.Marshal(args)
}

// LogArgs — аргументы для журнала: конверт в input заменён строкой
// «<конверт dossier sha256:abc…>». Не JSON-объект возвращается как есть.
func LogArgs(args json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil || m == nil {
		return args
	}
	in, ok := m["input"]
	if !ok {
		return args
	}
	var env Envelope
	label := "<конверт>"
	if err := json.Unmarshal(in, &env); err == nil && (env.Kind != "" || env.Digest != "") {
		label = fmt.Sprintf("<конверт %s sha256:%s…>", env.Kind, Short(env.Digest))
	}
	m["input"] = marshalPlain(label)
	if out := marshalPlain(m); out != nil {
		return out
	}
	return args
}

// Inspect разбирает ответ инструмента и проверяет передачу, записывая
// проверки и поля ответа в st:
//
//   - ответ — конверт (JSON с kind, digest и data);
//   - вид данных — kind (если не пуст);
//   - отпечаток ответа — Digest пересчитан по Data (порча на обратном пути);
//   - вход = выход шага prevN — Input ответа совпал с prev.Digest (если prev
//     задан), иначе ErrChain: сервер обработал не то, что ему передали;
//   - цепочка в файле — File.Chain совпал с chain (если chain задан).
//
// Возвращает конверт, данные файла (если ответ — файл и они разобрались) и
// первую ошибку проверок.
func Inspect(st *Step, raw json.RawMessage, kind string, prev *Envelope, prevN int, chain []string) (Envelope, *File, error) {
	st.Bytes = len(raw)
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Kind == "" || env.Digest == "" || len(env.Data) == 0 {
		why := "нет kind, digest или data"
		if err != nil {
			why = err.Error()
		}
		st.Checks = append(st.Checks, Check{Name: CheckEnvelope, Note: why})
		return env, nil, fmt.Errorf("%w: ответ %s не конверт: %s", ErrKind, st.Tool, why)
	}
	st.Checks = append(st.Checks, Check{Name: CheckEnvelope, OK: true})
	st.Kind, st.Digest, st.Input, st.Summary, st.CostUSD = env.Kind, env.Digest, env.Input, env.Summary, env.CostUSD

	var first error
	check := func(name string, err error, note string) {
		c := Check{Name: name, OK: err == nil, Note: note}
		if err != nil {
			c.Note = err.Error()
			if first == nil {
				first = err
			}
		}
		st.Checks = append(st.Checks, c)
	}

	if kind != "" {
		var err error
		if env.Kind != kind {
			err = fmt.Errorf("%w: ждали %q, пришло %q", ErrKind, kind, env.Kind)
		}
		check(CheckKind, err, env.Kind)
	}
	check(CheckDigest, env.Open(""), Short(env.Digest))
	if prev != nil {
		var err error
		if env.Input != prev.Digest {
			err = fmt.Errorf("%w: на входе %s, а передан выход шага %d %s", ErrChain,
				shortOrNone(env.Input), prevN, Short(prev.Digest))
		}
		check(fmt.Sprintf(CheckInput, prevN), err, Short(env.Input))
	}

	var file *File
	if env.Kind == KindFile {
		var f File
		if err := json.Unmarshal(env.Data, &f); err == nil {
			file = &f
		} else if chain != nil {
			check(CheckChain, fmt.Errorf("%w: данные файла не разобрались: %v", ErrKind, err), "")
		}
	}
	if chain != nil && file != nil {
		var err error
		if !slices.Equal(file.Chain, chain) {
			err = fmt.Errorf("%w: в файле цепочка %s, а пройдена %s", ErrChain, shortChain(file.Chain), shortChain(chain))
		}
		check(CheckChain, err, shortChain(file.Chain))
	}
	return env, file, first
}

func shortOrNone(d string) string {
	if d == "" {
		return "пусто"
	}
	return Short(d)
}

func shortChain(ds []string) string {
	if len(ds) == 0 {
		return "(пусто)"
	}
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = Short(d)
	}
	return strings.Join(out, " → ")
}

// StepError — провал шага: номер, инструмент и причина. errors.Is узнаёт
// в нём и саму причину, и ошибку передачи (ErrDigest, ErrKind, ErrRef,
// ErrInput, ErrChain), если инструмент назвал её только текстом — так
// приходят ошибки по MCP.
type StepError struct {
	N    int
	Tool string
	Err  error
}

func (e *StepError) Error() string {
	return fmt.Sprintf("шаг %d (%s): %v", e.N, e.Tool, e.Err)
}

func (e *StepError) Unwrap() []error {
	errs := []error{e.Err}
	if k := transferErr(e.Err); k != nil && !errors.Is(e.Err, k) {
		errs = append(errs, k)
	}
	return errs
}

// transferErr — ошибка передачи, названная в тексте err.
func transferErr(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	for _, k := range []error{ErrDigest, ErrKind, ErrRef, ErrInput, ErrChain} {
		if strings.Contains(text, k.Error()) {
			return k
		}
	}
	return nil
}

func copyStep(s Step) Step {
	s.Checks = slices.Clone(s.Checks)
	return s
}

// marshalPlain — JSON без экранирования < > &: журнал читают люди, а
// «<конверт» им ни к чему. nil — не закодировалось.
func marshalPlain(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}
