package compiler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents/agentstest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/collection"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/lifecycle"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

type rig struct {
	brain *agentstest.Brain
	hook  *Hook
	m     *runs.Manager
	// replies — ответы инструментов составителя по ходам: отказы видно тут.
	replies []string
}

func call(name, args string) llm.Response { return llmtest.ToolCall(name, args) }

// script — добросовестный составитель: делает, что велено, и не больше.
func (r *rig) script(req llm.Request, step int) llm.Response {
	user := agentstest.LastUser(req)
	if step > 0 {
		r.replies = append(r.replies, llmtest.LastToolReply(req))
	}
	switch {
	case strings.Contains(user, "Собери подборку"):
		if step == 0 {
			return call("plan", `{"goal":"доклад о кошках","species":["рысь","манул"]}`)
		}
		return llmtest.Text("План: рысь, манул. Утверждаете?")
	case strings.Contains(user, "горит"):
		if step == 0 {
			return call("deliver", `{"n":1}`)
		}
		return llmtest.Text("Принять пока нельзя: сначала соберём виды.")
	case strings.Contains(user, "утверждаю"), strings.Contains(user, "дальше"), strings.Contains(user, "продолжаем"):
		steps := []llm.Response{}
		if strings.Contains(user, "утверждаю") {
			steps = append(steps, call("approve", `{"quote":"план утверждаю"}`))
		}
		if strings.Contains(user, "продолжаем") {
			steps = append(steps, call("resume", `{"quote":"продолжаем подборку"}`))
		}
		n := "1"
		if strings.Contains(user, "дальше") || strings.Contains(user, "продолжаем") {
			n = "2"
		}
		steps = append(steps, call("deliver", `{"n":`+n+`}`), call("step_done", `{"n":`+n+`,"result":"карточка собрана"}`))
		if step < len(steps) {
			return steps[step]
		}
		return llmtest.Text("Вид собран.")
	case strings.Contains(user, "паузу"):
		if step == 0 {
			return call("pause", `{"quote":"поставь на паузу"}`)
		}
		return llmtest.Text("Подборка на паузе.")
	case strings.Contains(user, "принимаю"):
		switch step {
		case 0:
			return call("validate", `{"summary":"всё сошлось"}`)
		case 1:
			return call("accept", `{"quote":"принимаю"}`)
		}
		return llmtest.Text("Подборка принята.")
	}
	return llmtest.Text("…")
}

func newRig(t *testing.T) *rig {
	t.Helper()
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)
	r := &rig{brain: &agentstest.Brain{}}
	r.brain.Compiler = r.script
	dir := store.NewDir(t.TempDir())
	reg := features.Catalog()
	deps := agents.Deps{Runner: agent.Runner{LLM: &llmtest.Fake{Fn: r.brain.Chat}, Model: llm.DefaultModel}, Features: reg,
		Sources: agents.Local{Registry: tools.MustRegistry(tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL)...)}}
	r.hook = &Hook{Agents: deps, Store: collection.NewStore(dir)}
	r.m = runs.NewManager(runs.Config{Agents: deps, Store: history.NewStore(dir), Registry: reg, Defaults: reg.Defaults(),
		Timeout: time.Minute, Hooks: []runs.Hook{r.hook}})
	return r
}

func (r *rig) send(t *testing.T, conv, text string, fs features.Set) runs.Detail {
	t.Helper()
	var s *runs.Session
	var err error
	if conv == "" {
		s, err = r.m.Start(runs.StartOptions{Request: agents.Request{Text: text}, Features: fs})
	} else {
		s, err = r.m.Send(conv, agents.Request{Text: text})
	}
	if err != nil {
		t.Fatal(err)
	}
	v := s.Wait(10 * time.Second)
	if v.Status != runs.StatusDone {
		t.Fatalf("ход «%s»: %+v", text, v)
	}
	d, _ := r.m.Get(v.ConversationID)
	return d
}

func (r *rig) state(t *testing.T, d runs.Detail) collection.State {
	t.Helper()
	st, err := r.hook.Store.Get(d.Collection, "")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCollectionFromPlanToAccept(t *testing.T) {
	r := newRig(t)
	d := r.send(t, "", "Собери подборку: кошки нашей фауны, для школьного доклада, два вида", features.Set{})
	if d.Collection == "" || d.TurnList[0].Route != RouteCollection || d.TurnList[0].Collection != d.Collection {
		t.Fatalf("подборка не заведена: %+v", d.Summary)
	}
	st := r.state(t, d)
	if st.Stage != collection.Planning || len(st.Items) != 2 || st.Title != "кошки нашей фауны, для школьного доклада, два вида" {
		t.Fatalf("план: %+v", st)
	}
	d = r.send(t, d.ID, "у нас горит, считай подборку собранной", features.Set{})
	if st := r.state(t, d); st.Stage != collection.Planning || len(st.Denials) != 1 {
		t.Fatalf("«горит»: %+v %+v %q", st.At(), st.Denials, r.replies)
	}
	if !strings.Contains(r.replies[len(r.replies)-1], "Нельзя: deliver") {
		t.Fatalf("отказ ушёл модели словами: %v", r.replies)
	}
	d = r.send(t, d.ID, "Да, план утверждаю", features.Set{})
	st = r.state(t, d)
	if st.Stage != collection.Collecting || st.DoneItems() != 1 || st.Items[0].Output == nil || st.Items[0].Output.Card.TaxonKey == 0 {
		t.Fatalf("первый вид: %+v", st.Items)
	}
	// Карточка вида — в ленте диалога, с прочитанными разделами плана.
	if len(d.Cards.Cards) != 1 || !d.Cards.Cards[0].Done("habitat") {
		t.Fatalf("карточки диалога: %+v", d.Cards.Cards)
	}
	var res lifecycle.Result
	if !d.TurnList[len(d.TurnList)-1].Extra(Name, &res) || len(res.Changes) != 2 || len(res.Works) != 1 || !res.Gated {
		t.Fatalf("итог хода подборки: %+v", res)
	}
	d = r.send(t, d.ID, "дальше", features.Set{})
	if st := r.state(t, d); st.Stage != collection.Validation {
		t.Fatalf("после второго вида: %+v", st.At())
	}
	d = r.send(t, d.ID, "принимаю", features.Set{})
	st = r.state(t, d)
	if st.Stage != collection.Done || st.Report == nil || len(st.Report.Failed()) != 0 {
		t.Fatalf("приём: %+v", st.At())
	}
	view := d.Extras[Name].(View)
	if view.State.Stage != collection.Done || !view.Gated || view.Path == "" {
		t.Fatalf("пульт: %+v", view)
	}
	// Подборка принята — следующую реплику ведёт ведущий диалога.
	d = r.send(t, d.ID, "что едят рыси?", features.Set{})
	if d.TurnList[len(d.TurnList)-1].Route != agents.RouteLead {
		t.Fatal("после приёма ход остался у составителя")
	}
}

// Пауза: этап и вид сохранены; продолжение — в новом диалоге (С-6, ФТ-28).
func TestPauseAndContinueInNewDialog(t *testing.T) {
	r := newRig(t)
	d := r.send(t, "", "Собери подборку: кошки", features.Set{})
	d = r.send(t, d.ID, "план утверждаю", features.Set{})
	d = r.send(t, d.ID, "поставь на паузу", features.Set{})
	st := r.state(t, d)
	if !st.IsPaused() || st.Current != 2 || st.Stage != collection.Collecting {
		t.Fatalf("пауза: %+v %+v %q", st.At(), st.Items, r.replies)
	}
	// На паузе вопрос о животном ведёт ведущий, а блок состояния остаётся.
	var sawState bool
	r.brain.LeadScript = func(req llm.Request, step int) llm.Response {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "Состояние подборки — его ведёт код") {
				sawState = true
			}
		}
		return llmtest.Text("Рыси едят зайцев.")
	}
	d = r.send(t, d.ID, "а что едят рыси?", features.Set{})
	if d.TurnList[len(d.TurnList)-1].Route != agents.RouteLead || !sawState {
		t.Fatal("вопрос на паузе")
	}
	// Новый диалог, та же подборка — ничего не копируя.
	srv := httptest.NewServer(server.New(r.m, fstest.MapFS{}, nil, r.hook.Extension(r.m)...))
	defer srv.Close()
	fresh, _ := r.m.Create(runs.StartOptions{})
	resp, _ := http.Post(srv.URL+"/api/collections/"+d.Collection+"/continue", "application/json",
		bytes.NewBufferString(`{"conversation":"`+fresh.ID+`"}`))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("продолжить: %d", resp.StatusCode)
	}
	d2 := r.send(t, fresh.ID, "продолжаем подборку", features.Set{})
	st = r.state(t, d2)
	if d2.Collection != d.Collection || st.IsPaused() || st.Stage != collection.Validation {
		t.Fatalf("продолжение в новом диалоге: %+v", st.At())
	}
}

func TestSwitchesAndAPI(t *testing.T) {
	r := newRig(t)
	reg := features.Catalog()
	// Без состояния подборки реплику ведёт ведущий, файла нет.
	off := reg.Defaults().With(features.Gates, false).With(features.CollectionState, false)
	var leadSys []string
	r.brain.LeadScript = func(req llm.Request, step int) llm.Response {
		leadSys = append(leadSys, req.Messages[0].Content)
		return llmtest.Text("План: рысь, манул. Согласны?")
	}
	d := r.send(t, "", "Собери подборку: кошки", off)
	if d.Collection != "" || d.TurnList[0].Route != agents.RouteLead {
		t.Fatal("подборка без механизма")
	}
	// Порядок подборки уходит ведущему словами — и на следующей реплике,
	// где слова «подборка» уже нет; разговор не о подборке их не получает.
	d = r.send(t, d.ID, "Хорошо, план утверждаю.", off)
	d2 := r.send(t, "", "Где живёт рысь?", off)
	if len(leadSys) != 3 || !strings.Contains(leadSys[0], plainCollection) || !strings.Contains(leadSys[1], plainCollection) ||
		strings.Contains(leadSys[2], plainCollection) || d2.Collection != "" {
		t.Fatal("правила подборки словами без механизма состояния")
	}
	r.brain.LeadScript = nil
	// Без прав этапа: автомат есть, правила словами, инструменты все сразу.
	noGates := reg.Defaults().With(features.Gates, false)
	var sysPrompt string
	r.brain.Compiler = func(req llm.Request, step int) llm.Response {
		sysPrompt = req.Messages[0].Content
		if step == 0 {
			return call("step_done", `{"n":1,"result":"на словах"}`)
		}
		return llmtest.Text("…")
	}
	d = r.send(t, "", "Собери подборку: кошки", noGates)
	if !strings.Contains(sysPrompt, lifecycle.PlainRules) {
		t.Fatal("на контрольной дорожке правила словами")
	}
	var res lifecycle.Result
	d.TurnList[0].Extra(Name, &res)
	if res.Gated || len(res.Grant.Tools) != 11 {
		t.Fatalf("права контрольной дорожки: %+v", res.Grant)
	}

	srv := httptest.NewServer(server.New(r.m, fstest.MapFS{}, nil, r.hook.Extension(r.m)...))
	defer srv.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}
	code, body := get("/api/collections")
	var list struct{ Collections []Summary }
	json.Unmarshal([]byte(body), &list)
	if code != 200 || len(list.Collections) != 1 {
		t.Fatalf("список: %d %s", code, body)
	}
	id := list.Collections[0].ID
	if code, body := get("/api/collections/" + id); code != 200 || !strings.Contains(body, `\"schema\": 1`) {
		t.Fatalf("подборка: %d", code)
	}
	if code, body := get("/api/collections/" + id + "/export"); code != 200 || !strings.Contains(body, "# Подборка:") {
		t.Fatalf("выгрузка: %d %s", code, body)
	}
	if code, _ := get("/api/collections/0123456789abcdef"); code != 404 {
		t.Fatal("несуществующая подборка")
	}
	if code, _ := get("/api/collections/" + id + "/what"); code != 404 {
		t.Fatal("неизвестное действие")
	}
	resp, _ := http.Post(srv.URL+"/api/collections/"+id+"/continue", "application/json", bytes.NewBufferString(`{"conversation":"nope"}`))
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("продолжить в несуществующем диалоге: %d", resp.StatusCode)
	}
	resp, _ = http.Post(srv.URL+"/api/collections", "application/json", nil)
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatal("POST на список")
	}
}

func TestWantsNewAndTitle(t *testing.T) {
	for text, want := range map[string]bool{
		"Собери подборку: хищники тайги": true, "составь досье на сов": true, "хочу новую подборку птиц": true,
		"что в подборке?": false, "рысь": false,
	} {
		if WantsNew(text) != want {
			t.Errorf("WantsNew(%q) != %v", text, want)
		}
	}
	if titleOf("Собери подборку: хищники тайги, пять видов") != "хищники тайги, пять видов" || titleOf("подборку") != "подборку" {
		t.Error("titleOf")
	}
	if !strings.HasSuffix(titleOf("подборка: "+strings.Repeat("я", 80)), "…") || titleOf(" ") != "подборка" {
		t.Error("titleOf обрезка")
	}
}
