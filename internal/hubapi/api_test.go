package hubapi

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

	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/hub"
)

// fakeRouter — подставной реестр: три сервера, до Connect все idle.
type fakeRouter struct {
	mu        sync.Mutex
	connected bool
	connects  int
	routesN   int
	fail      bool // Connect не поднимает ни одного
}

func (f *fakeRouter) Servers(context.Context) []hub.ServerView {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := hub.StatusIdle
	if f.connected {
		st = hub.StatusOK
	}
	return []hub.ServerView{
		{Name: "sources", Title: "Источники", Color: "#2f6b4f", Transport: hub.TransportStdio, Addr: "animals-mcp", Status: st, Tools: 6},
		{Name: "daemon", Title: "Демон", Color: "#2c5a85", Transport: hub.TransportHTTP, Addr: "http://127.0.0.1:8766/mcp", Status: st, Tools: 5, Hidden: 8},
		{Name: "notes", Title: "Блокнот", Color: "#a5701a", Transport: hub.TransportStdio, Addr: "animals-mcp -role notes", Status: hub.StatusDown, Reason: "не поднялся"},
	}
}

func (f *fakeRouter) Connect(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects++
	if f.fail {
		return errors.New("ни один сервер не поднялся")
	}
	f.connected = true
	return nil
}

func (f *fakeRouter) Routes(context.Context) ([]hub.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routesN++
	return []hub.Route{
		{Tool: "search_wikipedia", Server: "sources"},
		{Tool: "mdd_get", Server: "daemon"},
		{Tool: "search_wikipedia", Server: "daemon", Hidden: true, Reason: "дубль → sources"},
	}, nil
}

func (f *fakeRouter) Tools(context.Context) ([]hub.Bound, error)     { return nil, nil }
func (f *fakeRouter) Snapshot(context.Context) (hub.Snapshot, error) { return hub.Snapshot{}, nil }

// gate — прогон, который отдаёт вызовы по команде теста: каждое значение
// из steps — разрешение на следующий шаг (старт вызова или его конец).
type gate struct {
	steps   chan struct{}
	n       int // вызовов в прогоне
	fail    bool
	mu      sync.Mutex
	species []string
}

func newGate(n int) *gate { return &gate{steps: make(chan struct{}), n: n} }

func (g *gate) run(ctx context.Context, p flow.Preset, species string, onCall func(flow.Call)) (flow.Trace, error) {
	g.mu.Lock()
	g.species = append(g.species, species)
	g.mu.Unlock()
	tr := flow.Trace{Preset: p.ID, Species: species, Started: time.Now()}
	wait := func() error {
		select {
		case <-g.steps:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for i := 1; i <= g.n; i++ {
		c := flow.Call{N: i, Turn: i, Server: "sources", Tool: fmt.Sprintf("tool_%d", i), Args: json.RawMessage(`{"q":"манул"}`)}
		onCall(c)
		if err := wait(); err != nil {
			return tr, err
		}
		if g.fail && i == 2 {
			return tr, errors.New("модель не ответила")
		}
		c.OK, c.Result, c.Took = true, json.RawMessage(`{"ok":true}`), time.Millisecond
		if i > 1 {
			c.From = []int{i - 1}
		}
		onCall(c)
		if err := wait(); err != nil {
			return tr, err
		}
		tr.Calls = append(tr.Calls, c)
	}
	tr.OK = true
	tr.CostUSD = 0.004
	tr.Answer = "паспорт манула в блокноте"
	tr.Verdict = flow.Verdict{OK: true, Checks: []flow.Check{{Name: "маршрут", Level: flow.LevelOK}}}
	return tr, nil
}

func (g *gate) next(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case g.steps <- struct{}{}:
		case <-time.After(10 * time.Second):
			t.Fatal("прогон не ждёт следующего шага")
		}
	}
}

var testPresets = []flow.Preset{
	{ID: "passport", Title: "Паспорт вида в блокнот", Species: "манул"},
	{ID: "short", Title: "Коротко", Species: "рысь"},
}

func newAPI(t *testing.T, a *API) http.Handler {
	t.Helper()
	ext := a.Extension()
	if len(ext) != 1 || ext[0].Prefix != "/api/hub/" {
		t.Fatalf("раздел: %+v", ext)
	}
	mux := http.NewServeMux()
	mux.Handle(ext[0].Prefix, ext[0].Handler)
	return mux
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.Bytes()
}

// poll — GET прогона, пока cond не выполнится.
func poll(t *testing.T, h http.Handler, id string, cond func(FlowView) bool) FlowView {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for {
		code, _, raw := do(t, h, "GET", "/api/hub/flows/"+id, "")
		if code != 200 {
			t.Fatalf("GET %s: %d %s", id, code, raw)
		}
		var v FlowView
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		if cond(v) {
			return v
		}
		if time.Now().After(end) {
			t.Fatalf("не дождались: %s", raw)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServersAndConnect(t *testing.T) {
	r := &fakeRouter{}
	h := newAPI(t, &API{Router: r, Run: newGate(1).run, Presets: testPresets})

	code, out, raw := do(t, h, "GET", "/api/hub/servers", "")
	if code != 200 || len(out["servers"].([]any)) != 3 {
		t.Fatalf("servers: %d %s", code, raw)
	}
	if len(out["routes"].([]any)) != 0 || r.routesN != 0 {
		t.Fatalf("до подключения маршруты не спрашиваем (Routes подключил бы реестр): %s, routes=%d", raw, r.routesN)
	}
	if out["canRun"] != true {
		t.Fatalf("canRun: %s", raw)
	}

	code, out, raw = do(t, h, "POST", "/api/hub/connect", "")
	if code != 200 || r.connects != 1 {
		t.Fatalf("connect: %d %s", code, raw)
	}
	routes := out["routes"].([]any)
	if len(routes) != 3 || routes[2].(map[string]any)["hidden"] != true || routes[2].(map[string]any)["reason"] != "дубль → sources" {
		t.Fatalf("маршруты: %s", raw)
	}
	if s := out["servers"].([]any)[0].(map[string]any); s["status"] != "ok" || s["color"] != "#2f6b4f" {
		t.Fatalf("сервер: %v", s)
	}

	code, out, _ = do(t, h, "GET", "/api/hub/servers", "")
	if code != 200 || len(out["routes"].([]any)) != 3 {
		t.Fatalf("после подключения GET отдаёт маршруты: %v", out)
	}
	if code, _, _ := do(t, h, "GET", "/api/hub/connect", ""); code != 405 {
		t.Fatalf("GET connect: %d", code)
	}
}

func TestConnectError(t *testing.T) {
	r := &fakeRouter{fail: true}
	h := newAPI(t, &API{Router: r})
	code, out, raw := do(t, h, "POST", "/api/hub/connect", "")
	if code != 200 || !strings.Contains(fmt.Sprint(out["connectError"]), "ни один") || len(out["servers"].([]any)) != 3 {
		t.Fatalf("connect: %d %s", code, raw)
	}
}

func TestPresets(t *testing.T) {
	h := newAPI(t, &API{Presets: testPresets})
	code, _, raw := do(t, h, "GET", "/api/hub/presets", "")
	var list []PresetView
	json.Unmarshal(raw, &list)
	if code != 200 || len(list) != 2 || list[0] != (PresetView{ID: "passport", Title: "Паспорт вида в блокнот", Species: "манул"}) {
		t.Fatalf("presets: %d %s", code, raw)
	}
	if strings.Contains(string(raw), `"spec":`) || strings.Contains(string(raw), `"task":`) {
		t.Fatalf("в списке лишнее: %s", raw)
	}
}

func TestDisabled(t *testing.T) {
	h := newAPI(t, &API{Presets: testPresets, Why: "нет DEEPSEEK_API_KEY"})
	code, out, raw := do(t, h, "GET", "/api/hub/servers", "")
	if code != 200 || len(out["servers"].([]any)) != 0 || out["why"] != "нет DEEPSEEK_API_KEY" || out["canRun"] != false {
		t.Fatalf("servers без реестра: %d %s", code, raw)
	}
	code, out, raw = do(t, h, "POST", "/api/hub/connect", "")
	if code != 503 || out["why"] != "нет DEEPSEEK_API_KEY" {
		t.Fatalf("connect без реестра: %d %s", code, raw)
	}
	code, out, raw = do(t, h, "POST", "/api/hub/flows", `{"preset":"passport"}`)
	if code != 503 || !strings.Contains(fmt.Sprint(out["error"]), "нет DEEPSEEK_API_KEY") || out["hint"] == "" {
		t.Fatalf("flows без модели: %d %s", code, raw)
	}
	// Реестр есть, модели нет.
	h = newAPI(t, &API{Router: &fakeRouter{}, Presets: testPresets})
	code, out, raw = do(t, h, "POST", "/api/hub/flows", `{}`)
	if code != 503 || !strings.Contains(fmt.Sprint(out["why"]), "нет модели") {
		t.Fatalf("flows без Run: %d %s", code, raw)
	}
}

func TestBadRequests(t *testing.T) {
	g := newGate(1)
	h := newAPI(t, &API{Router: &fakeRouter{}, Run: g.run, Presets: testPresets})
	for _, c := range []struct{ body, want string }{
		{`{"preset":"нет-такой"}`, "нет заготовки"},
		{`{"preset":`, "не разобралось"},
		{`{"species":"` + strings.Repeat("я", 201) + `"}`, "длиннее"},
	} {
		code, out, raw := do(t, h, "POST", "/api/hub/flows", c.body)
		if code != 400 || !strings.Contains(fmt.Sprint(out["error"]), c.want) {
			t.Errorf("%s: %d %s", c.body, code, raw)
		}
	}
	if code, _, _ := do(t, h, "GET", "/api/hub/flows/f9", ""); code != 404 {
		t.Errorf("нет прогона: %d", code)
	}
	if code, _, _ := do(t, h, "GET", "/api/hub/nope", ""); code != 404 {
		t.Errorf("нет раздела: %d", code)
	}
	if code, _, _ := do(t, h, "DELETE", "/api/hub/flows", ""); code != 405 {
		t.Errorf("DELETE: %d", code)
	}
}

func TestFlowLifecycle(t *testing.T) {
	g := newGate(3)
	a := &API{Router: &fakeRouter{}, Run: g.run, Presets: testPresets}
	h := newAPI(t, a)

	code, out, raw := do(t, h, "POST", "/api/hub/flows", `{"preset":"passport","species":"  "}`)
	if code != 202 || out["id"] != "f1" {
		t.Fatalf("старт: %d %s", code, raw)
	}
	// Первый вызов начат, но не кончился: он в списке и помечен pending.
	v := poll(t, h, "f1", func(v FlowView) bool { return len(v.Calls) == 1 })
	if v.State != StateRunning || !v.Calls[0].Pending || v.Calls[0].Tool != "tool_1" || v.Trace != nil || v.Species != "манул" {
		t.Fatalf("по ходу: %+v", v)
	}
	if g.species[0] != "манул" {
		t.Fatalf("пустой вид — вид заготовки: %v", g.species)
	}

	// Второй POST, пока идёт первый, — 409 с id идущего.
	code, out, raw = do(t, h, "POST", "/api/hub/flows", `{"preset":"short"}`)
	if code != 409 || out["id"] != "f1" {
		t.Fatalf("409: %d %s", code, raw)
	}

	g.next(t, 1) // tool_1 завершён
	v = poll(t, h, "f1", func(v FlowView) bool { return len(v.Calls) == 1 && !v.Calls[0].Pending })
	if !v.Calls[0].OK || string(v.Calls[0].Result) != `{"ok":true}` {
		t.Fatalf("завершение вызова: %+v", v.Calls[0])
	}
	g.next(t, 1) // tool_2 начат
	poll(t, h, "f1", func(v FlowView) bool { return len(v.Calls) == 2 && v.Calls[1].Pending })

	// Список: идущий прогон.
	_, out, raw = do(t, h, "GET", "/api/hub/flows", "")
	rows := out["flows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["state"] != "running" || rows[0].(map[string]any)["calls"] != float64(2) {
		t.Fatalf("список по ходу: %s", raw)
	}

	g.next(t, 4) // tool_2 конец, tool_3 старт и конец, выход
	v = poll(t, h, "f1", func(v FlowView) bool { return v.State != StateRunning })
	if v.State != StateDone || v.Trace == nil || !v.Trace.OK || len(v.Calls) != 3 || v.Calls[2].From[0] != 2 {
		t.Fatalf("итог: %+v", v)
	}
	if v.Trace.Answer != "паспорт манула в блокноте" || len(v.Trace.Verdict.Checks) != 1 {
		t.Fatalf("трасса: %+v", v.Trace)
	}

	// Второй прогон — уже можно; в списке новые первыми.
	code, out, raw = do(t, h, "POST", "/api/hub/flows", `{"preset":"short","species":"рысь"}`)
	if code != 202 || out["id"] != "f2" {
		t.Fatalf("второй старт: %d %s", code, raw)
	}
	_, out, raw = do(t, h, "GET", "/api/hub/flows", "")
	rows = out["flows"].([]any)
	first, second := rows[0].(map[string]any), rows[1].(map[string]any)
	if len(rows) != 2 || first["id"] != "f2" || second["id"] != "f1" || second["ok"] != true ||
		second["costUsd"] != 0.004 || second["preset"] != "passport" || second["species"] != "манул" {
		t.Fatalf("список: %s", raw)
	}
	g.next(t, 6)
	poll(t, h, "f2", func(v FlowView) bool { return v.State == StateDone })
}

func TestFlowFailed(t *testing.T) {
	g := newGate(3)
	g.fail = true
	h := newAPI(t, &API{Router: &fakeRouter{}, Run: g.run, Presets: testPresets})
	do(t, h, "POST", "/api/hub/flows", `{}`)
	g.next(t, 3) // tool_1 конец, tool_2 старт, обрыв
	v := poll(t, h, "f1", func(v FlowView) bool { return v.State != StateRunning })
	if v.State != StateFailed || v.Error != "модель не ответила" || v.Trace == nil || v.Trace.OK {
		t.Fatalf("провал: %+v", v)
	}
	if len(v.Calls) != 2 || v.Calls[1].Pending || !strings.Contains(v.Calls[1].Error, "прервано") {
		t.Fatalf("прерванный вызов: %+v", v.Calls)
	}
	_, out, _ := do(t, h, "GET", "/api/hub/flows", "")
	if row := out["flows"].([]any)[0].(map[string]any); row["state"] != "failed" || row["ok"] != false {
		t.Fatalf("строка: %v", row)
	}
}

func TestFlowTimeout(t *testing.T) {
	g := newGate(2)
	h := newAPI(t, &API{Router: &fakeRouter{}, Run: g.run, Presets: testPresets, Timeout: 50 * time.Millisecond})
	do(t, h, "POST", "/api/hub/flows", `{}`)
	v := poll(t, h, "f1", func(v FlowView) bool { return v.State != StateRunning })
	if v.State != StateFailed || !strings.Contains(v.Error, "deadline") {
		t.Fatalf("таймаут: %+v", v)
	}
}

func TestKeepRuns(t *testing.T) {
	a := &API{Router: &fakeRouter{}, Presets: testPresets}
	a.Run = func(ctx context.Context, p flow.Preset, species string, onCall func(flow.Call)) (flow.Trace, error) {
		return flow.Trace{OK: true}, nil
	}
	h := newAPI(t, a)
	for i := 1; i <= keepRuns+3; i++ {
		code, out, raw := do(t, h, "POST", "/api/hub/flows", `{}`)
		if code != 202 {
			t.Fatalf("старт %d: %d %s", i, code, raw)
		}
		poll(t, h, out["id"].(string), func(v FlowView) bool { return v.State == StateDone })
	}
	_, out, _ := do(t, h, "GET", "/api/hub/flows", "")
	rows := out["flows"].([]any)
	if len(rows) != keepRuns || rows[0].(map[string]any)["id"] != fmt.Sprintf("f%d", keepRuns+3) {
		t.Fatalf("список: %d, первый %v", len(rows), rows[0])
	}
	if code, _, _ := do(t, h, "GET", "/api/hub/flows/f1", ""); code != 404 {
		t.Fatalf("старый прогон не забыт: %d", code)
	}
}

// Битые аргументы модели не должны ломать ответ целиком: они уходят
// строкой JSON.
func TestBrokenArgs(t *testing.T) {
	a := &API{Router: &fakeRouter{}, Presets: testPresets}
	a.Run = func(ctx context.Context, p flow.Preset, species string, onCall func(flow.Call)) (flow.Trace, error) {
		c := flow.Call{N: 1, Tool: "nb_add", Args: json.RawMessage(`{"text":"<img onerror="x">"}`)}
		onCall(c)
		c.OK, c.Result = true, json.RawMessage(`{oops`)
		onCall(c)
		return flow.Trace{Calls: []flow.Call{c}, OK: true}, nil
	}
	h := newAPI(t, a)
	do(t, h, "POST", "/api/hub/flows", `{}`)
	v := poll(t, h, "f1", func(v FlowView) bool { return v.State == StateDone })
	var args string
	if err := json.Unmarshal(v.Calls[0].Args, &args); err != nil || !strings.Contains(args, `onerror="x"`) {
		t.Fatalf("аргументы: %s (%v)", v.Calls[0].Args, err)
	}
	if string(v.Trace.Calls[0].Result) != `"{oops"` {
		t.Fatalf("ответ в трассе: %s", v.Trace.Calls[0].Result)
	}
}
