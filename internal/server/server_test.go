package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents/agentstest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

type app struct {
	srv   *httptest.Server
	brain *agentstest.Brain
	m     *runs.Manager
}

func newApp(t *testing.T) *app {
	t.Helper()
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)
	b := &agentstest.Brain{}
	reg := features.Catalog()
	m := runs.NewManager(runs.Config{
		Agents: agents.Deps{Runner: agent.Runner{LLM: &llmtest.Fake{Fn: b.Chat}, Model: llm.DefaultModel}, Features: reg,
			Sources: agents.Local{Registry: tools.MustRegistry(tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL)...)}},
		Store: history.NewStore(store.NewDir(t.TempDir())), Registry: reg, Defaults: reg.Defaults(), Timeout: time.Minute,
	})
	static := fstest.MapFS{"index.html": {Data: []byte("<html>AnimalGuide</html>")}}
	ext := Extension{Prefix: "/api/ext/", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"ok": "расширение"})
	})}
	srv := httptest.NewServer(New(m, static, map[string]any{"model": llm.DefaultModel}, ext))
	t.Cleanup(srv.Close)
	return &app{srv: srv, brain: b, m: m}
}

func (a *app) do(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, a.srv.URL+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// stream читает поток событий хода до done и возвращает виды событий.
func (a *app) stream(t *testing.T, path string) []string {
	t.Helper()
	resp, err := http.Get(a.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("не поток: %s", resp.Header.Get("Content-Type"))
	}
	var kinds []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		line := sc.Text()
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			kinds = append(kinds, ev)
			if ev == "done" {
				break
			}
		}
	}
	return kinds
}

func TestMetaAndStatic(t *testing.T) {
	a := newApp(t)
	code, meta := a.do(t, http.MethodGet, "/api/meta", "")
	if code != 200 || meta["model"] != llm.DefaultModel || len(meta["mechanisms"].([]any)) == 0 || len(meta["topics"].([]any)) != 5 {
		t.Fatalf("meta: %d %v", code, meta)
	}
	resp, _ := http.Get(a.srv.URL + "/")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "AnimalGuide") {
		t.Fatal("фронтенд не отдаётся")
	}
	code, ext := a.do(t, http.MethodGet, "/api/ext/x", "")
	if code != 200 || ext["ok"] != "расширение" {
		t.Fatal("расширение API")
	}
}

func TestStartTurnStreamAndConversation(t *testing.T) {
	a := newApp(t)
	code, out := a.do(t, http.MethodPost, "/api/conversations", `{"text":"рысь"}`)
	if code != http.StatusAccepted {
		t.Fatalf("старт: %d %v", code, out)
	}
	kinds := a.stream(t, out["events"].(string))
	joined := strings.Join(kinds, ",")
	if !strings.HasPrefix(joined, "snapshot") || !strings.HasSuffix(joined, "done") ||
		!strings.Contains(joined, "log") || !strings.Contains(joined, "update") {
		t.Fatalf("поток: %s", joined)
	}
	id := out["conversationId"].(string)
	code, conv := a.do(t, http.MethodGet, "/api/conversations/"+id, "")
	d := conv["conversation"].(map[string]any)
	if code != 200 || len(d["turnList"].([]any)) != 1 || len(d["cards"].(map[string]any)["cards"].([]any)) != 1 {
		t.Fatalf("диалог: %d", code)
	}
	cardID := d["cards"].(map[string]any)["cards"].([]any)[0].(map[string]any)["id"].(string)

	// Раздел по клику.
	code, out = a.do(t, http.MethodPost, "/api/conversations/"+id+"/turns", `{"kind":"section","cardId":"`+cardID+`","topic":"diet"}`)
	if code != http.StatusAccepted {
		t.Fatalf("раздел: %d %v", code, out)
	}
	a.stream(t, out["events"].(string))
	code, turn := a.do(t, http.MethodGet, "/api/turns/"+out["turn"].(map[string]any)["id"].(string), "")
	if code != 200 || turn["turn"].(map[string]any)["status"] != "done" {
		t.Fatalf("ход: %v", turn)
	}

	// Выгрузка карточки.
	resp, _ := http.Get(a.srv.URL + "/api/conversations/" + id + "/export?kind=card&id=" + cardID)
	md, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(md), "## Питание") || !strings.Contains(resp.Header.Get("Content-Disposition"), "UTF-8") {
		t.Fatalf("выгрузка: %s", md)
	}
	code, raw := a.do(t, http.MethodGet, "/api/conversations/"+id+"/raw", "")
	if code != 200 || !strings.Contains(raw["json"].(string), `"schema": 2`) {
		t.Fatal("файл диалога")
	}
	code, list := a.do(t, http.MethodGet, "/api/conversations", "")
	if code != 200 || len(list["conversations"].([]any)) != 1 {
		t.Fatal("список")
	}
}

func TestBranchesFeaturesAndDelete(t *testing.T) {
	a := newApp(t)
	code, out := a.do(t, http.MethodPost, "/api/conversations", `{"empty":true,"features":"-guard"}`)
	if code != http.StatusCreated {
		t.Fatalf("пустой диалог: %d %v", code, out)
	}
	d := out["conversation"].(map[string]any)
	id := d["id"].(string)
	if d["features"].(map[string]any)["guard"] != false {
		t.Fatalf("механизмы нового диалога: %v", d["features"])
	}
	code, out = a.do(t, http.MethodPost, "/api/conversations/"+id+"/checkpoints", `{"name":"начало"}`)
	cps := out["conversation"].(map[string]any)["checkpoints"].([]any)
	if code != 200 || len(cps) != 1 {
		t.Fatalf("точка: %d", code)
	}
	cp := cps[0].(map[string]any)["id"].(string)
	code, out = a.do(t, http.MethodPost, "/api/conversations/"+id+"/branches", `{"checkpoint":"`+cp+`","name":"вторая"}`)
	tree := out["conversation"].(map[string]any)["branchTree"].([]any)
	if code != 200 || len(tree) != 2 {
		t.Fatalf("ветка: %d", code)
	}
	root := tree[0].(map[string]any)["id"].(string)
	code, out = a.do(t, http.MethodPost, "/api/conversations/"+id+"/switch", `{"branch":"`+root+`"}`)
	if code != 200 || out["conversation"].(map[string]any)["branch"] != root {
		t.Fatalf("переключение: %d", code)
	}
	code, out = a.do(t, http.MethodPost, "/api/conversations/"+id+"/features", `{"name":"facts","on":false}`)
	if code != 200 || out["conversation"].(map[string]any)["features"].(map[string]any)["facts"] != false {
		t.Fatalf("выключатель: %d %v", code, out)
	}
	// Страж выключен при создании, поэтому свод выключить можно, а включить
	// стража без свода — нет.
	if code, _ := a.do(t, http.MethodPost, "/api/conversations/"+id+"/features", `{"name":"charter","on":false}`); code != 200 {
		t.Fatalf("свод без стража: %d", code)
	}
	if code, _ := a.do(t, http.MethodPost, "/api/conversations/"+id+"/features", `{"name":"guard","on":true}`); code != 400 {
		t.Fatalf("страж без свода: %d", code)
	}
	if code, _ := a.do(t, http.MethodDelete, "/api/conversations/"+id, ""); code != 200 {
		t.Fatal("удаление")
	}
	if code, _ := a.do(t, http.MethodGet, "/api/conversations/"+id, ""); code != 404 {
		t.Fatal("удалённый диалог")
	}
}

func TestErrors(t *testing.T) {
	a := newApp(t)
	cases := []struct {
		method, path, body string
		code               int
	}{
		{http.MethodPost, "/api/conversations", `{bad`, 400},
		{http.MethodPost, "/api/conversations", `{"text":"x","features":"+gaurd"}`, 400},
		{http.MethodPost, "/api/conversations", `{"text":"  "}`, 400},
		{http.MethodPut, "/api/conversations", ``, 405},
		{http.MethodGet, "/api/conversations/", ``, 404},
		{http.MethodPost, "/api/conversations/nope/turns", `{"text":"x"}`, 404},
		{http.MethodGet, "/api/conversations/nope/raw", ``, 404},
		{http.MethodGet, "/api/conversations/nope/export?kind=card", ``, 404},
		{http.MethodPost, "/api/conversations/nope/checkpoints", `{}`, 404},
		{http.MethodPost, "/api/conversations/nope/branches", `{}`, 404},
		{http.MethodPost, "/api/conversations/nope/switch", `{}`, 404},
		{http.MethodPost, "/api/conversations/nope/features", `{"name":"guard"}`, 404},
		{http.MethodDelete, "/api/conversations/nope", ``, 404},
		{http.MethodGet, "/api/conversations/x/what", ``, 404},
		{http.MethodGet, "/api/turns/nope", ``, 404},
		{http.MethodPut, "/api/groups", ``, 405},
		{http.MethodGet, "/api/groups/nope", ``, 404},
		{http.MethodGet, "/api/groups/nope/what", ``, 404},
		{http.MethodPost, "/api/groups/nope/turns", `{"text":"x"}`, 404},
		{http.MethodPost, "/api/groups", `{"lanes":[{"name":"a","features":"+nope"}]}`, 400},
		{http.MethodPost, "/api/groups", `{"lanes":[]}`, 400},
	}
	for _, c := range cases {
		if code, _ := a.do(t, c.method, c.path, c.body); code != c.code {
			t.Errorf("%s %s: %d, ждали %d", c.method, c.path, code, c.code)
		}
	}
}

func TestBusyIsConflict(t *testing.T) {
	a := newApp(t)
	release := make(chan struct{})
	a.brain.LeadScript = func(req llm.Request, step int) llm.Response {
		<-release
		return llmtest.Text("ок")
	}
	_, out := a.do(t, http.MethodPost, "/api/conversations", `{"text":"что едят волки?"}`)
	id := out["conversationId"].(string)
	code, _ := a.do(t, http.MethodPost, "/api/conversations/"+id+"/turns", `{"text":"ещё?"}`)
	close(release)
	a.stream(t, out["events"].(string))
	if code != http.StatusConflict {
		t.Fatalf("ход во время хода: %d", code)
	}
}

func TestGroupsAPI(t *testing.T) {
	a := newApp(t)
	code, out := a.do(t, http.MethodPost, "/api/groups", `{"title":"привратник","lanes":[{"name":"с","features":""},{"name":"без","features":"-gatekeeper"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("стенд: %d %v", code, out)
	}
	group := out["group"].(string)
	code, out = a.do(t, http.MethodPost, "/api/groups/"+group+"/turns", `{"text":"рысь"}`)
	if code != http.StatusAccepted || len(out["turns"].([]any)) != 2 {
		t.Fatalf("ход стенда: %d %v", code, out)
	}
	for _, x := range out["turns"].([]any) {
		a.stream(t, x.(map[string]any)["events"].(string))
	}
	code, out = a.do(t, http.MethodGet, "/api/groups/"+group, "")
	if code != 200 || len(out["lanes"].([]any)) != 2 {
		t.Fatal("дорожки")
	}
	code, out = a.do(t, http.MethodGet, "/api/groups", "")
	if code != 200 || len(out["groups"].(map[string]any)) != 1 {
		t.Fatal("список стендов")
	}
	if code, _ := a.do(t, http.MethodPost, "/api/groups", `{"lanes":[{"name":"a"},{"name":"b","features":"-guard,-charter"}]}`); code != 400 {
		t.Fatalf("дорожки, отличающиеся двумя механизмами: %d", code)
	}
}
