package feed

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// api — обработчик раздела над подставным демоном.
func api(t *testing.T, h *Hook) http.Handler {
	t.Helper()
	ext := h.Extension()
	if len(ext) != 1 || ext[0].Prefix != "/api/facts/" {
		t.Fatalf("раздел: %+v", ext)
	}
	mux := http.NewServeMux()
	mux.Handle(ext[0].Prefix, ext[0].Handler)
	return mux
}

func do(t *testing.T, h http.Handler, method, target, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: ответ не JSON-объект: %v\n%s", method, target, err, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("%s %s: Content-Type %q", method, target, ct)
	}
	return rec.Code, out
}

// Каждый маршрут — в свой инструмент со своими аргументами; ответ демона
// отдаётся как есть.
func TestAPIRoutes(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(testToken))
	h := api(t, &Hook{Remote: newRemote(t, srv.URL, testToken)})
	cases := []struct {
		method, target, body string
		tool                 string
		args                 map[string]any
	}{
		{"GET", "/api/facts/latest", "", "facts_latest", map[string]any{}},
		{"GET", "/api/facts/latest?limit=3", "", "facts_latest", map[string]any{"limit": 3.0}},
		{"GET", "/api/facts/issue?id=42", "", "facts_get", map[string]any{"id": 42.0}},
		{"GET", "/api/facts/issue?species=Otocolobus+manul", "", "facts_get", map[string]any{"species": "Otocolobus manul"}},
		{"GET", "/api/facts/search?text=манул&since=2026-09-01&until=2026-09-25&offset=20", "", "facts_search",
			map[string]any{"text": "манул", "since": "2026-09-01", "until": "2026-09-25", "offset": 20.0}},
		{"GET", "/api/facts/search", "", "facts_search", map[string]any{}},
		{"GET", "/api/facts/summary", "", "summary_get", map[string]any{}},
		{"GET", "/api/facts/summary?id=7", "", "summary_get", map[string]any{"id": 7.0}},
		{"GET", "/api/facts/summaries", "", "summary_get", map[string]any{"list": true}},
		{"POST", "/api/facts/run", `{"job":"issue"}`, "run_now", map[string]any{"job": "issue"}},
		{"POST", "/api/facts/run", `{"job":"mdd"}`, "run_now", map[string]any{"job": "mdd"}},
		{"POST", "/api/facts/summary/build", `{"hours":48}`, "summary_build", map[string]any{"hours": 48.0}},
		{"POST", "/api/facts/summary/build", ``, "summary_build", map[string]any{}},
	}
	for _, c := range cases {
		code, out := do(t, h, c.method, c.target, c.body)
		if code != http.StatusOK {
			t.Fatalf("%s %s: %d %v", c.method, c.target, code, out)
		}
		last := d.last()
		if last.Tool != c.tool || !reflect.DeepEqual(last.Args, c.args) {
			t.Fatalf("%s %s: вызван %s %v, ждали %s %v", c.method, c.target, last.Tool, last.Args, c.tool, c.args)
		}
		if c.tool != "facts_get" && c.tool != "run_now" && !reflect.DeepEqual(out["args"], any(c.args)) {
			t.Fatalf("%s %s: ответ демона не как есть: %v", c.method, c.target, out)
		}
	}
}

func TestAPIStatus(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(testToken))
	code, out := do(t, api(t, &Hook{Remote: newRemote(t, srv.URL, testToken)}), "GET", "/api/facts/status", "")
	if code != 200 || out["conn"] != ConnOK || out["version"] != "18.0.0" || out["schedule"] == nil || out["server"] != srv.URL {
		t.Fatalf("status: %d %v", code, out)
	}
	// Демона нет — всё равно 200: «не подключён» и есть ответ.
	code, out = do(t, api(t, &Hook{Remote: newRemote(t, deadAddr(t), "")}), "GET", "/api/facts/status", "")
	if code != 200 || out["conn"] != ConnDown || !strings.Contains(out["hint"].(string), "animals-mcp -http") {
		t.Fatalf("status без демона: %d %v", code, out)
	}
	code, out = do(t, api(t, &Hook{}), "GET", "/api/facts/status", "")
	if code != 200 || out["conn"] != ConnOff {
		t.Fatalf("status выключенного: %d %v", code, out)
	}
}

// Коды ошибок: 422 — ошибка инструмента, 400 — неверный запрос, 405 — не
// тот метод, 404 — нет раздела.
func TestAPIErrors(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(testToken))
	h := api(t, &Hook{Remote: newRemote(t, srv.URL, testToken)})
	cases := []struct {
		method, target, body string
		code                 int
		want                 string
	}{
		{"GET", "/api/facts/issue?species=Рысь", "", 422, "выпусков о виде «Рысь» ещё не было"},
		{"POST", "/api/facts/run", `{"job":"summary"}`, 422, "уже идёт"},
		{"GET", "/api/facts/issue", "", 400, "id"},
		{"GET", "/api/facts/issue?id=1&species=x", "", 400, "оба"},
		{"GET", "/api/facts/issue?id=abc", "", 400, "id"},
		{"GET", "/api/facts/latest?limit=0", "", 400, "limit"},
		{"GET", "/api/facts/search?offset=-1", "", 400, "offset"},
		{"GET", "/api/facts/summary?id=x", "", 400, "id"},
		{"POST", "/api/facts/run", `{"job":"all"}`, 400, "job"},
		{"POST", "/api/facts/run", ``, 400, "job"},
		{"POST", "/api/facts/run", `{"job":`, 400, "не разобралось"},
		{"POST", "/api/facts/summary/build", `{"hours":0}`, 400, "hours"},
		{"POST", "/api/facts/summary/build", `{"hours":"сутки"}`, 400, "не разобралось"},
		{"POST", "/api/facts/latest", "", 405, "GET"},
		{"GET", "/api/facts/run", "", 405, "POST"},
		{"POST", "/api/facts/summary", `{"hours":1}`, 405, "GET"},
		{"GET", "/api/facts/summary/build", "", 405, "POST"},
		{"DELETE", "/api/facts/status", "", 405, "GET"},
		{"GET", "/api/facts/nope", "", 404, "нет такого раздела"},
	}
	for _, c := range cases {
		code, out := do(t, h, c.method, c.target, c.body)
		msg, _ := out["error"].(string)
		if code != c.code || !strings.Contains(msg, c.want) {
			t.Errorf("%s %s %s: %d %v, ждали %d «%s»", c.method, c.target, c.body, code, out, c.code, c.want)
		}
	}
	// 405 называет нужный метод и заголовком.
	req := httptest.NewRequest("GET", "/api/facts/run", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Allow") != "POST" {
		t.Fatalf("Allow: %q", rec.Header().Get("Allow"))
	}
	if n := d.count("run_now"); n != 1 { // дошёл только {"job":"summary"}
		t.Fatalf("run_now звался %d раз", n)
	}
}

// Тело больше 64 КБ — 400, до демона не доходит.
func TestAPIBodyLimit(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(""))
	h := api(t, &Hook{Remote: newRemote(t, srv.URL, "")})
	big := `{"job":"issue","pad":"` + strings.Repeat("я", 40<<10) + `"}`
	code, out := do(t, h, "POST", "/api/facts/run", big)
	if code != 400 || !strings.Contains(out["error"].(string), "64 КБ") || d.count("run_now") != 0 {
		t.Fatalf("большое тело: %d %v", code, out)
	}
	// Чуть меньше предела — проходит.
	ok := `{"job":"issue","pad":"` + strings.Repeat("a", 60<<10) + `"}`
	if code, out := do(t, h, "POST", "/api/facts/run", ok); code != 200 {
		t.Fatalf("тело под пределом: %d %v", code, out)
	}
}

// Демон недоступен или не настроен — 503 с подсказкой.
func TestAPIUnavailable(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(testToken))
	cases := []struct {
		name string
		hook *Hook
		want string
	}{
		{"не отвечает", &Hook{Remote: newRemote(t, deadAddr(t), "")}, "animals-mcp -http 127.0.0.1:"},
		{"не тот токен", &Hook{Remote: newRemote(t, srv.URL, "чужой")}, "MCP_TOKEN"},
		{"не настроен", &Hook{Remote: newRemote(t, "", "")}, "-facts-server"},
		{"нет клиента", &Hook{}, "-facts-server"},
	}
	for _, c := range cases {
		h := api(t, c.hook)
		for _, r := range [][3]string{{"GET", "/api/facts/latest", ""}, {"POST", "/api/facts/run", `{"job":"issue"}`}} {
			code, out := do(t, h, r[0], r[1], r[2])
			msg, _ := out["error"].(string)
			hint, _ := out["hint"].(string)
			if code != 503 || !strings.Contains(msg, c.want) || !strings.Contains(hint, c.want) || strings.Contains(msg, "чужой") {
				t.Errorf("%s %s %s: %d %v", c.name, r[0], r[1], code, out)
			}
		}
	}
	if d.count("run_now") != 0 {
		t.Fatal("вызов с чужим токеном дошёл до демона")
	}
}
