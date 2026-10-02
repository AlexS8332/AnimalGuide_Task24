package persona

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents/agentstest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/extract"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/memory"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/profile"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

type rig struct {
	brain    *agentstest.Brain
	fake     *llmtest.Fake
	hook     *Hook
	m        *runs.Manager
	leadReqs []llm.Request
}

func newRig(t *testing.T) *rig {
	t.Helper()
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)
	r := &rig{brain: &agentstest.Brain{}}
	r.fake = &llmtest.Fake{Fn: r.brain.Chat}
	r.brain.LeadScript = func(req llm.Request, step int) llm.Response {
		r.leadReqs = append(r.leadReqs, req)
		return llmtest.Text("Ты угадал: рыси едят зайцев.")
	}
	dir := store.NewDir(t.TempDir())
	r.hook = &Hook{Memory: memory.NewStore(dir), Profiles: profile.NewStore(dir),
		Extractor: extract.Extractor{LLM: r.fake, Model: llm.DefaultModel}}
	reg := features.Catalog()
	r.m = runs.NewManager(runs.Config{
		Agents: agents.Deps{Runner: agent.Runner{LLM: r.fake, Model: llm.DefaultModel}, Features: reg,
			Sources: agents.Local{Registry: tools.MustRegistry(tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL)...)}},
		Store: history.NewStore(dir), Registry: reg, Defaults: reg.Defaults(), Timeout: time.Minute,
		Hooks: []runs.Hook{r.hook},
	})
	return r
}

func (r *rig) turn(t *testing.T, conv string, text string, fs features.Set) (runs.View, runs.Detail) {
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
	d, _ := r.m.Get(v.ConversationID)
	return v, d
}

const prefs = `{"profile":{"set":[{"field":"length","value":"short","scope":"always","quote":"пиши мне всегда коротко"},
 {"field":"address","value":"ty","scope":"always","quote":"давай на ты"},
 {"field":"form","value":"table","scope":"once","quote":"сделай таблицей"}]},
 "memory":{"set":[{"layer":"long","key":"интерес","value":"хищники тайги"},{"layer":"long","key":"ареал","value":"тайга"}]},
 "facts":{"set":[{"key":"тема","value":"чем питаются рыси"}]}}`

func TestExtractFillsProfileMemoryAndFacts(t *testing.T) {
	r := newRig(t)
	r.brain.Extract = func(llm.Request) (string, error) { return prefs, nil }
	v, d := r.turn(t, "", "Мне интересны хищники тайги. Пиши мне всегда коротко, давай на ты, и сделай таблицей: чем питаются рыси?", features.Set{})
	if v.Status != runs.StatusDone {
		t.Fatalf("ход: %+v", v)
	}
	p, _ := r.hook.Profiles.Get("me", "")
	if p.Val(profile.FieldLength) != "short" || p.Val(profile.FieldAddress) != "ty" || p.Val(profile.FieldForm) != "" {
		t.Fatalf("анкета: разовая форма не должна попасть в файл: %+v", p.Values)
	}
	long, _ := r.hook.Memory.Card(memory.LayerLong, "me", "")
	if v, _ := long.Get("интерес"); v != "хищники тайги" {
		t.Fatalf("долговременная: %+v", long.Entries)
	}
	if _, ok := long.Get("ареал"); ok {
		t.Fatal("зоологический ключ записан в память")
	}
	if !d.Facts.Has("тема") {
		t.Fatal("карточка фактов ветки")
	}
	// Профиль ушёл модели этим же ходом, с разовой правкой, и первым после свода.
	req := r.leadReqs[0]
	var profBlock string
	for _, m := range req.Messages {
		if m.Role == llm.RoleSystem && strings.HasPrefix(m.Content, "Как разговаривать") {
			profBlock = m.Content
		}
	}
	if !strings.Contains(profBlock, "до четырёх предложений") || !strings.Contains(profBlock, "таблицу") {
		t.Fatalf("блок профиля хода:\n%s", profBlock)
	}
	turn := d.TurnList[0]
	var checks []profile.Check
	if !turn.Extra("checks", &checks) || len(checks) == 0 {
		t.Fatal("проверки соблюдения профиля не записаны")
	}
	var changes profile.Changes
	if !turn.Extra("profile", &changes) || len(changes) != 3 {
		t.Fatalf("чипы профиля: %+v", changes)
	}
	if d.Meter.Calls != 1 || r.brain.Calls("extract") != 1 {
		t.Fatalf("расход извлекателя: %+v", d.Meter)
	}
	view := d.Extras[Name].(View)
	if view.Owner != "me" || view.Profile.Val(profile.FieldLength) != "short" || view.Paths["profile"] == "" {
		t.Fatalf("пульт: %+v", view)
	}
}

// Живучесть хода при неудачном извлечении (НТ-5).
func TestTurnSurvivesBrokenExtractor(t *testing.T) {
	r := newRig(t)
	r.brain.Extract = func(llm.Request) (string, error) { return "", errors.New("модель недоступна") }
	v, d := r.turn(t, "", "Мне для урока: что едят рыси?", features.Set{})
	if v.Status != runs.StatusDone || v.Reply == "" {
		t.Fatalf("ход упал вместе с извлекателем: %+v", v)
	}
	found := false
	for _, e := range d.TurnList[0].Events {
		if strings.Contains(e.Title, "не обновлены") {
			found = true
		}
	}
	if !found || d.Meter.Calls != 1 {
		t.Fatal("неудача извлекателя не видна в журнале или не посчитана")
	}
	r.brain.Extract = func(llm.Request) (string, error) { return "мусор", nil }
	v, _ = r.turn(t, v.ConversationID, "А мне ещё: где живут?", features.Set{})
	if v.Status != runs.StatusDone {
		t.Fatal("мусорный ответ извлекателя уронил ход")
	}
}

func TestSwitchesMakeZeroCost(t *testing.T) {
	r := newRig(t)
	reg := features.Catalog()
	off := reg.Defaults().With(features.Extract, false).With(features.Profile, false).With(features.MemoryLong, false)
	pr, _ := profile.PresetOf("child")
	r.hook.Profiles.Save(pr.Build("me", ""))
	v, d := r.turn(t, "", "что едят рыси?", off)
	if v.Status != runs.StatusDone || r.brain.Calls("extract") != 0 {
		t.Fatal("выключенный извлекатель делал запрос")
	}
	for _, m := range r.leadReqs[0].Messages {
		if strings.HasPrefix(m.Content, "Как разговаривать") || strings.HasPrefix(m.Content, "Известное о человеке") {
			t.Fatal("выключенный механизм добавил блок")
		}
	}
	var checks []profile.Check
	if d.TurnList[0].Extra("checks", &checks) {
		t.Fatal("проверки профиля при выключенном профиле")
	}
	noted := false
	for _, e := range d.TurnList[0].Events {
		if e.Mechanism == string(features.Extract) && strings.Contains(e.Title, "выключен") {
			noted = true
		}
	}
	if !noted {
		t.Fatal("выключенный извлекатель — молчаливая дыра")
	}
}

// Вопрос о животном без слов о человеке, форме и подборке — извлекатель не
// зовётся, а в журнале видно почему; реплика о себе — зовётся.
func TestPlainQuestionSkipsExtractor(t *testing.T) {
	r := newRig(t)
	v, d := r.turn(t, "", "Что едят рыси?", features.Set{})
	if v.Status != runs.StatusDone || r.brain.Calls("extract") != 0 || d.Meter.Calls != 0 {
		t.Fatalf("извлекатель на вопросе о животном: %d запросов", r.brain.Calls("extract"))
	}
	noted := false
	for _, e := range d.TurnList[0].Events {
		if e.Mechanism == string(features.Extract) && strings.Contains(e.Title, "извлекать нечего") {
			noted = true
		}
	}
	if !noted {
		t.Fatal("пропуск извлекателя не виден в журнале")
	}
	r.turn(t, v.ConversationID, "Меня зовут Алекс, что едят рыси?", features.Set{})
	if r.brain.Calls("extract") != 1 {
		t.Fatalf("реплика о себе без извлекателя: %d", r.brain.Calls("extract"))
	}
}

func TestBareNameSkipsExtractorAndRecordsRead(t *testing.T) {
	r := newRig(t)
	_, d := r.turn(t, "", "рысь", features.Set{})
	if r.brain.Calls("extract") != 0 {
		t.Fatal("извлекатель на реплике из одного названия")
	}
	long, _ := r.hook.Memory.Card(memory.LayerLong, "me", "")
	if v, _ := long.Get(memory.KeyRead); v != "Обыкновенная рысь" {
		t.Fatalf("уже читал: %q", v)
	}
	var read []string
	if !d.TurnList[0].Extra("read", &read) || len(read) != 1 {
		t.Fatal("итог «уже читал» в ходе")
	}
	// Проверка профиля на карточке — по описанию, которое написала модель.
	pr, _ := profile.PresetOf("child")
	r.hook.Profiles.Save(pr.Build("me", ""))
	_, d = r.turn(t, d.ID, "манул", features.Set{})
	var checks []profile.Check
	if !d.TurnList[1].Extra("checks", &checks) {
		t.Fatal("карточка без проверки профиля")
	}
}

func TestWorkMemoryFollowsCollection(t *testing.T) {
	r := newRig(t)
	r.brain.Extract = func(req llm.Request) (string, error) {
		return `{"memory":{"set":[{"layer":"work","key":"для чего","value":"школьный доклад"}]}}`, nil
	}
	collection := runs.Hook(hookFunc{before: func(tr *runs.Turn) {
		tr.Collection, tr.CollectionTitle = "c1", "хищники тайги"
	}})
	r.m.AddHook(collection)
	// Хук подборки стоит после — значит, в этом ходе persona адрес ещё не
	// видит; ход записывает подборку в диалог, и следующий ход её видит.
	v, _ := r.turn(t, "", "собери подборку для школьного доклада?", features.Set{})
	_, d := r.turn(t, v.ConversationID, "это для школьного доклада?", features.Set{})
	if d.Collection != "c1" {
		t.Fatalf("подборка диалога: %q", d.Collection)
	}
	work, _ := r.hook.Memory.Card(memory.LayerWork, "c1", "")
	if v, _ := work.Get("для чего"); v != "школьный доклад" {
		t.Fatalf("рабочая память подборки: %+v", work.Entries)
	}
	if d.Extras[Name].(View).Work.ID != "c1" {
		t.Fatal("рабочая память на пульте")
	}
}

type hookFunc struct{ before func(*runs.Turn) }

func (h hookFunc) Name() string                                 { return "test" }
func (h hookFunc) Before(_ context.Context, t *runs.Turn) error { h.before(t); return nil }
func (h hookFunc) After(context.Context, *runs.Turn) error      { return nil }

func TestReserved(t *testing.T) {
	for key, want := range map[string]string{"длина": "анкета профиля", "латынь": "карточка животного", "уже читал": "код приложения", "интерес": ""} {
		if got := Reserved(key); got != want {
			t.Errorf("Reserved(%q) = %q", key, got)
		}
	}
}

func TestAPI(t *testing.T) {
	r := newRig(t)
	srv := httptest.NewServer(server.New(r.m, fstest.MapFS{}, Meta(), r.hook.Extension()...))
	defer srv.Close()
	do := func(method, path, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewBufferString(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := do("POST", "/api/people/me/profile", `{"op":"preset","preset":"expert"}`); code != 200 {
		t.Fatalf("заготовка: %d", code)
	}
	code, out := do("POST", "/api/people/me/profile", `{"op":"set","field":"length","value":"коротко"}`)
	p := out["person"].(map[string]any)
	if code != 200 || !strings.Contains(p["summary"].(string), "коротко") {
		t.Fatalf("правка поля: %d %v", code, out)
	}
	for _, body := range []string{`{"op":"set","field":"length","value":"средне"}`, `{"op":"set","field":"цвет"}`,
		`{"op":"clear","field":"цвет"}`, `{"op":"preset","preset":"nope"}`, `{"op":"what"}`} {
		if code, _ := do("POST", "/api/people/me/profile", body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
	for _, body := range []string{`{"op":"clear","field":"length"}`, `{"op":"limit","text":"не пугай"}`, `{"op":"unlimit","text":"не пугай"}`, `{"op":"reset"}`} {
		if code, _ := do("POST", "/api/people/me/profile", body); code != 200 {
			t.Errorf("%s: %d", body, code)
		}
	}
	if code, _ := do("POST", "/api/people/me/memory", `{"op":"put","key":"имя","value":"Саша"}`); code != 200 {
		t.Fatal("запись памяти")
	}
	if code, _ := do("POST", "/api/people/me/memory", `{"op":"forget","key":"нет"}`); code != 400 {
		t.Fatal("удаление отсутствующей записи")
	}
	if code, _ := do("POST", "/api/people/me/memory", `{"op":"x"}`); code != 400 {
		t.Fatal("неизвестное действие с памятью")
	}
	if code, _ := do("POST", "/api/people/me/bookmark", `{"name":"Манул"}`); code != 200 {
		t.Fatal("закладка")
	}
	code, out = do("GET", "/api/people/me", "")
	long := out["person"].(map[string]any)["long"].(map[string]any)
	if code != 200 || len(long["entries"].([]any)) != 2 {
		t.Fatalf("человек: %v", out)
	}
	code, out = do("GET", "/api/people", "")
	if code != 200 || len(out["people"].([]any)) != 1 {
		t.Fatalf("картотека: %v", out)
	}
	if code, _ := do("POST", "/api/memory/work/c1", `{"op":"put","key":"виды","value":"рысь"}`); code != 200 {
		t.Fatal("рабочая руками")
	}
	if code, _ := do("POST", "/api/memory/work/c1", `{"op":"forget","key":"виды"}`); code != 200 {
		t.Fatal("удаление рабочей")
	}
	if code, _ := do("POST", "/api/memory/work/c1", `{"op":"x"}`); code != 400 {
		t.Fatal("неизвестное действие")
	}
	code, out = do("GET", "/api/memory", "")
	if code != 200 || len(out["long"].([]any)) != 1 || len(out["work"].([]any)) != 1 {
		t.Fatalf("память целиком: %v", out)
	}
	if code, out = do("GET", "/api/memory/long/me", ""); code != 200 || !strings.Contains(out["json"].(string), "Саша") {
		t.Fatal("файл слоя")
	}
	if code, _ := do("GET", "/api/memory/short/me", ""); code != 404 {
		t.Fatal("несуществующий слой")
	}
	for _, c := range [][3]string{{"GET", "/api/people/..%2Fx", ""}, {"PUT", "/api/people/me", "{}"}, {"POST", "/api/people/me/what", "{}"},
		{"POST", "/api/people", ""}, {"POST", "/api/memory", ""}} {
		if code, _ := do(c[0], c[1], c[2]); code < 400 {
			t.Errorf("%v: %d", c, code)
		}
	}
	if len(Meta()["profileFields"].([]profile.Field)) != 7 {
		t.Fatal("Meta")
	}
}
