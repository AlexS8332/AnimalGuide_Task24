package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
)

// pipeDaemon — подставной демон с инструментами конвейера (ответы не
// важны: исполнитель в тестах подменён, демон нужен для проверки
// подключения и списка инструментов).
func pipeDaemon(t *testing.T) (*fakeDaemon, string) {
	t.Helper()
	d := &fakeDaemon{}
	echo := func(args map[string]any) (any, error) { return map[string]any{"args": args}, nil }
	list := append(d.tools(),
		d.tool(pipeline.ToolSearch, "Досье о виде.", echo),
		d.tool(pipeline.ToolSummarize, "Факты по досье.", echo),
		d.tool(pipeline.ToolSaveFile, "Сохранить выпуск в файл.", echo))
	h := mcp.NewServer(list, mcp.ServerOptions{Version: "19.0.0"}).HTTPHandler(mcp.HTTPOptions{Token: testToken})
	return d, serve(t, "", h).URL
}

func pipeAPI(t *testing.T, p *Pipelines) http.Handler {
	t.Helper()
	ext := p.Extension()
	if len(ext) != 1 || ext[0].Prefix != "/api/pipeline/" {
		t.Fatalf("раздел: %+v", ext)
	}
	mux := http.NewServeMux()
	mux.Handle(ext[0].Prefix, ext[0].Handler)
	return mux
}

// gate — исполнитель, который идёт по шагам по команде теста: каждое
// значение из steps — разрешение на следующий шаг.
type gate struct {
	mu    sync.Mutex
	calls []pipeline.Request
	cfg   pipeline.AgentConfig
	mode  string
	steps chan struct{}
	fail  bool
}

func newGate() *gate { return &gate{steps: make(chan struct{})} }

func (g *gate) exec(ctx context.Context, mode string, req pipeline.Request, onStep func(pipeline.Step)) (pipeline.Trace, error) {
	g.mu.Lock()
	g.calls = append(g.calls, req)
	g.mode = mode
	g.mu.Unlock()
	tr := pipeline.Trace{Request: req, Mode: mode, Started: time.Now()}
	prev := ""
	for i, tool := range pipeline.ToolNames {
		s := pipeline.Step{N: i + 1, Tool: tool, Status: pipeline.StepRunning, Input: prev, Started: time.Now()}
		onStep(s)
		select {
		case <-g.steps:
		case <-ctx.Done():
			return tr, ctx.Err()
		}
		if g.fail && i == 1 {
			s.Status, s.Error = pipeline.StepFailed, "summarize: модель не ответила"
			onStep(s)
			tr.Steps = append(tr.Steps, s)
			tr.Error = s.Error
			return tr, errors.New(s.Error)
		}
		s.Status, s.Digest, s.Summary, s.Took = pipeline.StepOK, fmt.Sprintf("sha256:%012d", i+1), tool+" готов", time.Second
		s.Checks = []pipeline.Check{{Name: "отпечаток ответа", OK: true}}
		onStep(s)
		tr.Steps = append(tr.Steps, s)
		prev = s.Digest
	}
	tr.OK = true
	tr.File = &pipeline.File{Path: "exports/manul.md", Format: req.Format, Bytes: 2100, SHA256: "abc"}
	tr.CostUSD = 0.002
	tr.Finished = time.Now()
	return tr, nil
}

func (g *gate) code(ctx context.Context, c pipeline.Caller, req pipeline.Request, onStep func(pipeline.Step)) (pipeline.Trace, error) {
	if c == nil {
		return pipeline.Trace{}, errors.New("нет Caller")
	}
	return g.exec(ctx, pipeline.ModeCode, req, onStep)
}

func (g *gate) agent(ctx context.Context, cfg pipeline.AgentConfig, req pipeline.Request, onStep func(pipeline.Step)) (pipeline.Trace, error) {
	g.mu.Lock()
	g.cfg = cfg
	g.mu.Unlock()
	return g.exec(ctx, pipeline.ModeAgent, req, onStep)
}

// next — разрешить n шагов.
func (g *gate) next(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case g.steps <- struct{}{}:
		case <-time.After(10 * time.Second):
			t.Fatal("исполнитель не ждёт следующего шага")
		}
	}
}

func newPipes(t *testing.T, g *gate) (*Pipelines, *fakeDaemon) {
	t.Helper()
	d, url := pipeDaemon(t)
	p := &Pipelines{Remote: newRemote(t, url, testToken), LLM: &llmtest.Fake{}, Model: "m-test", run: g.code, agent: g.agent}
	return p, d
}

func getRun(t *testing.T, h http.Handler, id string) RunView {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/pipeline/runs/"+id, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET %s: %d %s", id, rec.Code, rec.Body.String())
	}
	var v RunView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// waitRun — опрос, пока cond не станет истинным.
func waitRun(t *testing.T, h http.Handler, id string, cond func(RunView) bool) RunView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v := getRun(t, h, id)
		if cond(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("прогон %s не дошёл до нужного состояния: %+v", id, v)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func stepStatus(v RunView) []string {
	out := make([]string, len(v.Trace.Steps))
	for i, s := range v.Trace.Steps {
		out[i] = s.Tool + ":" + s.Status
	}
	return out
}

// Запуск → 202 и id; опрос видит шаги по мере выполнения (onStep);
// конец — след исполнителя, файл, список прогонов.
func TestPipelineRun(t *testing.T) {
	g := newGate()
	p, _ := newPipes(t, g)
	h := pipeAPI(t, p)
	code, out := do(t, h, "POST", "/api/pipeline/runs", `{"query":" манул ","format":"json","pass":"ref","mode":"code"}`)
	if code != http.StatusAccepted || out["id"] == "" {
		t.Fatalf("запуск: %d %v", code, out)
	}
	id := out["id"].(string)

	// Ещё ни одного шага: три карточки «ожидает» сразу.
	v := getRun(t, h, id)
	if v.Done || len(v.Trace.Steps) != 3 || v.Trace.Mode != pipeline.ModeCode {
		t.Fatalf("начало: %+v", v)
	}
	v = waitRun(t, h, id, func(v RunView) bool { return v.Trace.Steps[0].Status == pipeline.StepRunning })
	if got := strings.Join(stepStatus(v), " "); got != "search:running summarize:pending save_to_file:pending" || v.Done {
		t.Fatalf("первый шаг идёт: %s", got)
	}
	g.next(t, 1)
	v = waitRun(t, h, id, func(v RunView) bool { return v.Trace.Steps[1].Status == pipeline.StepRunning })
	if s := v.Trace.Steps[0]; s.Status != pipeline.StepOK || s.Summary != "search готов" || len(s.Checks) != 1 || v.Trace.Steps[1].Input != s.Digest {
		t.Fatalf("после search: %+v", v.Trace.Steps)
	}
	if v.Trace.Took <= 0 {
		t.Fatalf("время идущего прогона: %v", v.Trace.Took)
	}
	g.next(t, 2)
	v = waitRun(t, h, id, func(v RunView) bool { return v.Done })
	if !v.Trace.OK || v.Trace.File == nil || v.Trace.File.Path != "exports/manul.md" || v.Trace.CostUSD != 0.002 || v.Trace.Error != "" {
		t.Fatalf("итог: %+v", v.Trace)
	}
	if got := strings.Join(stepStatus(v), " "); got != "search:ok summarize:ok save_to_file:ok" {
		t.Fatalf("шаги в итоге: %s", got)
	}
	want := pipeline.Request{Query: "манул", Format: "json", Pass: "ref"}
	if len(g.calls) != 1 || g.calls[0] != want || v.Trace.Request != want {
		t.Fatalf("запрос исполнителю: %+v, в следе %+v", g.calls, v.Trace.Request)
	}
	// Тот же прогон и через ?id=.
	if code, out := do(t, h, "GET", "/api/pipeline/runs?id="+id, ""); code != 200 || out["done"] != true || out["id"] != id {
		t.Fatalf("?id=: %d %v", code, out)
	}

	code, out = do(t, h, "GET", "/api/pipeline/runs", "")
	rows, _ := out["runs"].([]any)
	if code != 200 || len(rows) != 1 {
		t.Fatalf("список: %d %v", code, out)
	}
	row := rows[0].(map[string]any)
	if row["id"] != id || row["query"] != "манул" || row["ok"] != true || row["file"] != "exports/manul.md" || row["mode"] != "code" {
		t.Fatalf("строка списка: %v", row)
	}
}

// Исполнитель-модель: инструменты — три инструмента конвейера от демона,
// модель — приложения; умолчания формата и передачи.
func TestPipelineAgent(t *testing.T) {
	g := newGate()
	p, _ := newPipes(t, g)
	h := pipeAPI(t, p)
	code, out := do(t, h, "POST", "/api/pipeline/runs", `{"random":true,"query":"забыть","mode":"agent"}`)
	if code != 202 {
		t.Fatalf("запуск: %d %v", code, out)
	}
	g.next(t, 3)
	v := waitRun(t, h, out["id"].(string), func(v RunView) bool { return v.Done })
	if v.Mode != pipeline.ModeAgent || !v.Trace.OK {
		t.Fatalf("итог: %+v", v)
	}
	if want := (pipeline.Request{Random: true, Format: "md", Pass: "inline"}); g.calls[0] != want {
		t.Fatalf("запрос: %+v", g.calls[0])
	}
	var names []string
	for _, tl := range g.cfg.Tools {
		names = append(names, tl.Spec().Name)
	}
	if strings.Join(names, ",") != "search,summarize,save_to_file" || g.cfg.Model != "m-test" || g.cfg.LLM == nil {
		t.Fatalf("конфиг модели: %v %q %v", names, g.cfg.Model, g.cfg.LLM)
	}
	// Без модели исполнитель-модель недоступен.
	p.LLM = nil
	if code, out := do(t, h, "POST", "/api/pipeline/runs", `{"query":"манул","mode":"agent"}`); code != 503 || !strings.Contains(out["error"].(string), "модел") {
		t.Fatalf("без модели: %d %v", code, out)
	}
}

// Одновременно — один прогон: второй POST — 409, после конца первого —
// снова можно.
func TestPipelineBusy(t *testing.T) {
	g := newGate()
	p, _ := newPipes(t, g)
	h := pipeAPI(t, p)
	_, out := do(t, h, "POST", "/api/pipeline/runs", `{"query":"манул"}`)
	id := out["id"].(string)
	code, out := do(t, h, "POST", "/api/pipeline/runs", `{"query":"рысь"}`)
	if code != http.StatusConflict || !strings.Contains(out["error"].(string), "уже идёт") || !strings.Contains(out["error"].(string), id) {
		t.Fatalf("второй прогон: %d %v", code, out)
	}
	g.next(t, 3)
	waitRun(t, h, id, func(v RunView) bool { return v.Done })
	code, out = do(t, h, "POST", "/api/pipeline/runs", `{"query":"рысь"}`)
	if code != 202 || out["id"] == id {
		t.Fatalf("после конца: %d %v", code, out)
	}
	g.next(t, 3)
}

// Ошибка шага: прогон кончается с ok=false, текст ошибки — в следе и
// у шага; шаг 3 так и остался «ожидает».
func TestPipelineFailed(t *testing.T) {
	g := newGate()
	g.fail = true
	p, _ := newPipes(t, g)
	h := pipeAPI(t, p)
	_, out := do(t, h, "POST", "/api/pipeline/runs", `{"query":"манул"}`)
	g.next(t, 2)
	v := waitRun(t, h, out["id"].(string), func(v RunView) bool { return v.Done })
	if v.Trace.OK || !strings.Contains(v.Trace.Error, "модель не ответила") || v.Trace.Steps[1].Status != pipeline.StepFailed {
		t.Fatalf("сбой: %+v", v.Trace)
	}
	_, list := do(t, h, "GET", "/api/pipeline/runs", "")
	row := list["runs"].([]any)[0].(map[string]any)
	if row["ok"] != false || !strings.Contains(row["error"].(string), "модель не ответила") {
		t.Fatalf("строка: %v", row)
	}
}

// Прогон, упёршийся в срок: идущий шаг — «ошибка», а не вечное «идёт»;
// контекст прогона не зависит от запроса POST.
func TestPipelineTimeout(t *testing.T) {
	g := newGate()
	p, _ := newPipes(t, g)
	p.timeout = 150 * time.Millisecond
	h := pipeAPI(t, p)
	req := httptest.NewRequest("POST", "/api/pipeline/runs", strings.NewReader(`{"query":"манул"}`))
	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	cancel() // запрос POST закончился — прогон идёт дальше
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	time.Sleep(50 * time.Millisecond)
	if v := getRun(t, h, out["id"]); v.Done {
		t.Fatalf("прогон умер вместе с запросом: %+v", v.Trace)
	}
	v := waitRun(t, h, out["id"], func(v RunView) bool { return v.Done })
	if v.Trace.OK || v.Trace.Steps[0].Status != pipeline.StepFailed || !strings.Contains(v.Trace.Error, "deadline") {
		t.Fatalf("по сроку: %+v", v.Trace)
	}
}

// Список — последние 10, новые первыми; в памяти — не больше 20.
func TestPipelineList(t *testing.T) {
	g := newGate()
	p, _ := newPipes(t, g)
	h := pipeAPI(t, p)
	var ids []string
	for i := range 23 {
		code, out := do(t, h, "POST", "/api/pipeline/runs", fmt.Sprintf(`{"query":"вид %d"}`, i))
		if code != 202 {
			t.Fatalf("прогон %d: %d %v", i, code, out)
		}
		id := out["id"].(string)
		ids = append(ids, id)
		g.next(t, 3)
		waitRun(t, h, id, func(v RunView) bool { return v.Done })
	}
	_, out := do(t, h, "GET", "/api/pipeline/runs", "")
	rows := out["runs"].([]any)
	if len(rows) != 10 || rows[0].(map[string]any)["id"] != ids[22] || rows[9].(map[string]any)["id"] != ids[13] {
		t.Fatalf("список: %v", rows)
	}
	if len(p.runs) != 20 {
		t.Fatalf("в памяти %d прогонов", len(p.runs))
	}
	// Старые вытеснены: 404.
	if code, out := do(t, h, "GET", "/api/pipeline/runs/"+ids[0], ""); code != 404 || !strings.Contains(out["error"].(string), "нет") {
		t.Fatalf("вытесненный: %d %v", code, out)
	}
}

// Неверный запрос — 400, не тот метод — 405, нет раздела — 404; до
// исполнителя ничего не доходит.
func TestPipelineBadRequest(t *testing.T) {
	g := newGate()
	p, _ := newPipes(t, g)
	h := pipeAPI(t, p)
	cases := []struct {
		method, target, body string
		code                 int
		want                 string
	}{
		{"POST", "/api/pipeline/runs", `{}`, 400, "query"},
		{"POST", "/api/pipeline/runs", ``, 400, "query"},
		{"POST", "/api/pipeline/runs", `{"query":"   "}`, 400, "query"},
		{"POST", "/api/pipeline/runs", `{"query":"` + strings.Repeat("я", 201) + `"}`, 400, "длиннее"},
		{"POST", "/api/pipeline/runs", `{"query":"манул","format":"pdf"}`, 400, "format"},
		{"POST", "/api/pipeline/runs", `{"query":"манул","pass":"mail"}`, 400, "pass"},
		{"POST", "/api/pipeline/runs", `{"query":"манул","mode":"human"}`, 400, "mode"},
		{"POST", "/api/pipeline/runs", `{"query":`, 400, "не разобралось"},
		{"POST", "/api/pipeline/runs", `{"query":"манул","pad":"` + strings.Repeat("a", 70<<10) + `"}`, 400, "64 КБ"},
		{"DELETE", "/api/pipeline/runs", ``, 405, "GET или POST"},
		{"POST", "/api/pipeline/runs/p1", `{}`, 405, "GET"},
		{"GET", "/api/pipeline/runs/p404", ``, 404, "нет"},
		{"GET", "/api/pipeline/nope", ``, 404, "нет такого раздела"},
	}
	for _, c := range cases {
		code, out := do(t, h, c.method, c.target, c.body)
		msg, _ := out["error"].(string)
		if code != c.code || !strings.Contains(msg, c.want) {
			t.Errorf("%s %s %.40s: %d %v, ждали %d «%s»", c.method, c.target, c.body, code, out, c.code, c.want)
		}
	}
	if len(g.calls) != 0 {
		t.Fatalf("исполнитель звался: %v", g.calls)
	}
}

// Демон недоступен, не настроен, чужой токен или без инструментов
// конвейера — 503 с подсказкой, прогон не заводится.
func TestPipelineUnavailable(t *testing.T) {
	old := &fakeDaemon{} // демон v18: инструментов конвейера нет
	oldSrv := serve(t, "", old.handler(testToken))
	_, url := pipeDaemon(t)
	cases := []struct {
		name   string
		remote *Remote
		want   string
	}{
		{"не отвечает", newRemote(t, deadAddr(t), ""), "animals-mcp -http 127.0.0.1:"},
		{"не тот токен", newRemote(t, url, "чужой"), "MCP_TOKEN"},
		{"не настроен", newRemote(t, "", ""), "-facts-server"},
		{"нет клиента", nil, "-facts-server"},
		{"старый демон", newRemote(t, oldSrv.URL, testToken), "версии 19"},
	}
	for _, c := range cases {
		g := newGate()
		p := &Pipelines{Remote: c.remote, LLM: &llmtest.Fake{}, run: g.code, agent: g.agent}
		h := pipeAPI(t, p)
		for _, mode := range []string{"code", "agent"} {
			code, out := do(t, h, "POST", "/api/pipeline/runs", `{"query":"манул","mode":"`+mode+`"}`)
			msg, _ := out["error"].(string)
			hint, _ := out["hint"].(string)
			if code != 503 || !strings.Contains(msg, c.want) || !strings.Contains(hint, c.want) || strings.Contains(msg, "чужой") {
				t.Errorf("%s %s: %d %v", c.name, mode, code, out)
			}
		}
		if len(p.runs) != 0 || len(g.calls) != 0 {
			t.Errorf("%s: прогон заведён", c.name)
		}
		// Список пустой, но отвечает.
		if code, out := do(t, h, "GET", "/api/pipeline/runs", ""); code != 200 || len(out["runs"].([]any)) != 0 {
			t.Errorf("%s: список %d %v", c.name, code, out)
		}
	}
}
