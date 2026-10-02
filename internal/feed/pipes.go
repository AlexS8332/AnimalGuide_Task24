package feed

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
)

// # REST конвейера (префикс /api/pipeline)
//
// Цепочка search → summarize → save_to_file идёт у демона, а ведёт её
// приложение: код (pipeline.Run) или модель (pipeline.RunAgent). Прогон
// длится до пары минут, поэтому POST только запускает его в фоне, а окно
// опрашивает состояние — шаги видны по мере выполнения.
//
//	POST /api/pipeline/runs {"query":"манул","random":false,"format":"md","pass":"inline","mode":"code"}
//	     → 202 {"id":"…"}
//	GET  /api/pipeline/runs/{id}   (или /api/pipeline/runs?id=…)
//	     → 200 {"id","mode","done","trace":Trace}
//	GET  /api/pipeline/runs         → 200 {"runs":[RunRow…]} — последние 10, новые первыми
//
// Коды: 400 — неверный запрос; 404 — нет такого прогона; 405 — не тот
// метод; 409 — уже идёт другой прогон (одновременно — один: summarize
// платный, и два прогона подряд — это две покупки); 503 — демон не
// подключён или у него нет инструментов конвейера (текст с подсказкой, как
// у /api/facts), у исполнителя-модели нет ключа модели.
//
// Прогоны живут в памяти приложения, последние 20: это след для окна, а
// сам результат — файл у демона.

const (
	// PipelinePrefix — раздел API конвейера.
	PipelinePrefix = "/api/pipeline"
	// pipeTimeout — предел одного прогона: summarize — это редактор и
	// проверяющий, до пары минут; модель-исполнитель добавляет свои ходы.
	pipeTimeout = 3 * time.Minute
	// pipeKeep — сколько прогонов помнить; pipeList — сколько отдавать списком.
	pipeKeep = 20
	pipeList = 10
	// pipeQueryMax — предел строки вида в символах.
	pipeQueryMax = 200
)

// Исполнитель-код и исполнитель-модель; в тестах подменяются.
type (
	codeRunner  func(ctx context.Context, c pipeline.Caller, req pipeline.Request, onStep func(pipeline.Step)) (pipeline.Trace, error)
	agentRunner func(ctx context.Context, cfg pipeline.AgentConfig, req pipeline.Request, onStep func(pipeline.Step)) (pipeline.Trace, error)
)

// Pipelines — REST конвейера: запуск прогонов в фоне и их состояние.
// Демон — тот же, что у «Интересных фактов» (Remote), модель для
// исполнителя-модели — та же, что у приложения.
type Pipelines struct {
	Remote *Remote
	LLM    llm.Chatter
	Model  string

	run     codeRunner  // nil — pipeline.Run
	agent   agentRunner // nil — pipeline.RunAgent
	timeout time.Duration

	mu   sync.Mutex
	seq  int
	runs []*pipeRun // старые первыми
}

// pipeRun — один прогон: запрос и след, который onStep дописывает по ходу.
type pipeRun struct {
	id    string
	done  bool
	trace pipeline.Trace
}

// RunRow — строка списка прогонов.
type RunRow struct {
	ID      string        `json:"id"`
	Query   string        `json:"query,omitempty"`
	Random  bool          `json:"random,omitempty"`
	Mode    string        `json:"mode"`
	Format  string        `json:"format,omitempty"`
	Pass    string        `json:"pass,omitempty"`
	Done    bool          `json:"done"`
	OK      bool          `json:"ok"`
	Error   string        `json:"error,omitempty"`
	File    string        `json:"file,omitempty"`
	CostUSD float64       `json:"cost_usd"`
	Took    time.Duration `json:"took"`
	Started time.Time     `json:"started"`
}

// RunView — ответ GET /api/pipeline/runs/{id}.
type RunView struct {
	ID    string         `json:"id"`
	Mode  string         `json:"mode"`
	Done  bool           `json:"done"`
	Trace pipeline.Trace `json:"trace"`
}

// Extension — раздел /api/pipeline/ для сервера приложения.
func (p *Pipelines) Extension() []server.Extension {
	return []server.Extension{{Prefix: PipelinePrefix + "/", Handler: http.HandlerFunc(p.handle)}}
}

func (p *Pipelines) handle(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, PipelinePrefix), "/")
	id := ""
	switch {
	case path == "runs":
		id = strings.TrimSpace(r.URL.Query().Get("id"))
	case strings.HasPrefix(path, "runs/"):
		id = strings.TrimPrefix(path, "runs/")
	default:
		server.WriteError(w, http.StatusNotFound, "нет такого раздела: "+PipelinePrefix+"/"+path)
		return
	}
	if id != "" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		v, ok := p.view(id)
		if !ok {
			server.WriteError(w, http.StatusNotFound, "прогона "+id+" нет — прогоны живут в памяти приложения, последние "+strconv.Itoa(pipeKeep))
			return
		}
		server.WriteJSON(w, http.StatusOK, v)
		return
	}
	switch r.Method {
	case http.MethodGet:
		server.WriteJSON(w, http.StatusOK, map[string]any{"runs": p.list()})
	case http.MethodPost:
		p.start(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		server.WriteError(w, http.StatusMethodNotAllowed, "нужен GET или POST")
	}
}

// startBody — тело POST.
type startBody struct {
	Query  string `json:"query"`
	Random bool   `json:"random"`
	Format string `json:"format"`
	Pass   string `json:"pass"`
	Mode   string `json:"mode"`
}

// parseStart — запрос и исполнитель из тела; пустые поля — умолчания.
func parseStart(r *http.Request) (pipeline.Request, string, error) {
	var in startBody
	if err := readBody(r, &in); err != nil {
		return pipeline.Request{}, "", err
	}
	req := pipeline.Request{Query: strings.TrimSpace(in.Query), Random: in.Random,
		Format: strings.TrimSpace(in.Format), Pass: strings.TrimSpace(in.Pass)}
	if req.Random {
		req.Query = "" // случайный вид — строка вида не нужна
	} else if req.Query == "" {
		return req, "", badRequest("query — вид (русское или латинское название, mdd-id), или random: true")
	}
	if utf8.RuneCountInString(req.Query) > pipeQueryMax {
		return req, "", badRequest("query длиннее " + strconv.Itoa(pipeQueryMax) + " символов")
	}
	switch req.Format {
	case "":
		req.Format = pipeline.FormatMarkdown
	case pipeline.FormatMarkdown, pipeline.FormatJSON:
	default:
		return req, "", badRequest("format — md или json, а не «" + req.Format + "»")
	}
	switch req.Pass {
	case "":
		req.Pass = pipeline.PassInline
	case pipeline.PassInline, pipeline.PassRef:
	default:
		return req, "", badRequest("pass — inline или ref, а не «" + req.Pass + "»")
	}
	mode := strings.TrimSpace(in.Mode)
	switch mode {
	case "":
		mode = pipeline.ModeCode
	case pipeline.ModeCode, pipeline.ModeAgent:
	default:
		return req, "", badRequest("mode — code или agent, а не «" + mode + "»")
	}
	return req, mode, nil
}

func (p *Pipelines) start(w http.ResponseWriter, r *http.Request) {
	req, mode, err := parseStart(r)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			server.WriteError(w, http.StatusBadRequest, "тело запроса больше 64 КБ")
			return
		}
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if busy := p.busy(); busy != "" {
		server.WriteError(w, http.StatusConflict, "конвейер уже идёт (прогон "+busy+") — дождитесь его конца: одновременно идёт один прогон")
		return
	}
	if mode == pipeline.ModeAgent && p.LLM == nil {
		server.WriteError(w, http.StatusServiceUnavailable, "исполнитель-модель недоступен: у приложения нет модели")
		return
	}
	// Демон проверяем до 202: окно сразу покажет «не подключён» с
	// подсказкой, а не прогон, упавший на первом шаге. Список инструментов
	// заодно говорит, что демон — нужной версии.
	rm := p.remote()
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	list, err := rm.Tools(ctx, pipeline.ToolNames...)
	cancel()
	if err != nil {
		rm.Forget()
		hint := rm.Hint(err)
		if hint == "" {
			hint = "запусти демон animals-mcp версии 19 или новее — в нём есть search, summarize и save_to_file"
		}
		msg := err.Error()
		if !strings.Contains(msg, hint) {
			msg += " — " + hint
		}
		server.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msg, "hint": hint})
		return
	}
	run, busy := p.add(req, mode)
	if run == nil {
		server.WriteError(w, http.StatusConflict, "конвейер уже идёт (прогон "+busy+") — дождитесь его конца: одновременно идёт один прогон")
		return
	}
	// Прогон переживает запрос POST: окно уже получило id и опрашивает
	// состояние, а прерывать платный шаг из-за закрытой вкладки незачем.
	timeout := p.timeout
	if timeout <= 0 {
		timeout = pipeTimeout
	}
	bg, stop := context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
	go func() {
		defer stop()
		onStep := func(s pipeline.Step) { p.step(run.id, s) }
		var tr pipeline.Trace
		var err error
		if mode == pipeline.ModeAgent {
			fn := p.agent
			if fn == nil {
				fn = pipeline.RunAgent
			}
			tr, err = fn(bg, pipeline.AgentConfig{LLM: p.LLM, Model: p.Model, Tools: list}, req, onStep)
		} else {
			fn := p.run
			if fn == nil {
				fn = pipeline.Run
			}
			tr, err = fn(bg, rm, req, onStep)
		}
		p.finish(run.id, tr, err)
		// summarize тратит деньги: состояние демона (расход) устарело.
		rm.Forget()
	}()
	server.WriteJSON(w, http.StatusAccepted, map[string]string{"id": run.id})
}

func (p *Pipelines) remote() *Remote {
	if p.Remote == nil {
		return NewRemote("", "", nil)
	}
	return p.Remote
}

// ------------------------------------------------------------ состояние

// busy — id идущего прогона или "".
func (p *Pipelines) busy() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.busyLocked()
}

func (p *Pipelines) busyLocked() string {
	for _, r := range p.runs {
		if !r.done {
			return r.id
		}
	}
	return ""
}

// add заводит прогон, если ни один не идёт; иначе nil и id идущего.
// Проверка повторяется под замком: между busy и add мог успеть соседний POST.
func (p *Pipelines) add(req pipeline.Request, mode string) (*pipeRun, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b := p.busyLocked(); b != "" {
		return nil, b
	}
	p.seq++
	steps := make([]pipeline.Step, len(pipeline.ToolNames))
	for i, name := range pipeline.ToolNames {
		steps[i] = pipeline.Step{N: i + 1, Tool: name, Status: pipeline.StepPending}
	}
	run := &pipeRun{id: "p" + strconv.Itoa(p.seq),
		trace: pipeline.Trace{Request: req, Mode: mode, Steps: steps, Started: time.Now()}}
	p.runs = append(p.runs, run)
	if len(p.runs) > pipeKeep {
		p.runs = append([]*pipeRun(nil), p.runs[len(p.runs)-pipeKeep:]...)
	}
	return run, ""
}

func (p *Pipelines) find(id string) *pipeRun {
	for _, r := range p.runs {
		if r.id == id {
			return r
		}
	}
	return nil
}

// step — onStep исполнителя: шаг встаёт на место своего номера.
func (p *Pipelines) step(id string, s pipeline.Step) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.find(id)
	if r == nil || r.done {
		return
	}
	r.trace.Steps = putStep(r.trace.Steps, s)
}

func putStep(steps []pipeline.Step, s pipeline.Step) []pipeline.Step {
	for i := range steps {
		if s.N > 0 && steps[i].N == s.N {
			steps[i] = s
			return steps
		}
	}
	steps = append(steps, s)
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].N < steps[j].N })
	return steps
}

// finish — прогон закончился: след исполнителя заменяет собранный по
// onStep. Ошибка без следа (исполнитель не дошёл до шагов) — в Trace.Error.
func (p *Pipelines) finish(id string, tr pipeline.Trace, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.find(id)
	if r == nil {
		return
	}
	cur := r.trace
	if len(tr.Steps) == 0 {
		tr.Steps = cur.Steps
	}
	if tr.Mode == "" {
		tr.Mode = cur.Mode
	}
	if tr.Request == (pipeline.Request{}) {
		tr.Request = cur.Request
	}
	if tr.Started.IsZero() {
		tr.Started = cur.Started
	}
	if tr.Finished.IsZero() {
		tr.Finished = time.Now()
	}
	if tr.Took == 0 {
		tr.Took = tr.Finished.Sub(tr.Started)
	}
	if err != nil {
		tr.OK = false
		if tr.Error == "" {
			tr.Error = err.Error()
		}
	}
	// Шаги, до которых дело не дошло, так и остаются «ожидает», а шаг,
	// прерванный на ходу, — «ошибка»: окно не должно крутить его вечно.
	for i := range tr.Steps {
		if tr.Steps[i].Status == pipeline.StepRunning {
			tr.Steps[i].Status = pipeline.StepFailed
			if tr.Steps[i].Error == "" {
				tr.Steps[i].Error = "прервано: " + tr.Error
			}
		}
	}
	r.trace, r.done = tr, true
}

func (p *Pipelines) view(id string) (RunView, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.find(id)
	if r == nil {
		return RunView{}, false
	}
	tr := r.trace
	tr.Steps = append([]pipeline.Step(nil), tr.Steps...)
	if !r.done {
		tr.Took = time.Since(tr.Started)
	}
	return RunView{ID: r.id, Mode: tr.Mode, Done: r.done, Trace: tr}, true
}

func (p *Pipelines) list() []RunRow {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []RunRow{}
	for i := len(p.runs) - 1; i >= 0 && len(out) < pipeList; i-- {
		r := p.runs[i]
		tr := r.trace
		row := RunRow{ID: r.id, Query: tr.Request.Query, Random: tr.Request.Random, Mode: tr.Mode,
			Format: tr.Request.Format, Pass: tr.Request.Pass, Done: r.done, OK: r.done && tr.OK,
			Error: tr.Error, CostUSD: tr.CostUSD, Took: tr.Took, Started: tr.Started}
		if !r.done {
			row.Took = time.Since(tr.Started)
		}
		if tr.File != nil {
			row.File = tr.File.Path
		}
		out = append(out, row)
	}
	return out
}
