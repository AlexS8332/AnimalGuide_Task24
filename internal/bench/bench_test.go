package bench

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents/agentstest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/collection"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/compiler"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/extract"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/memory"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/persona"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/profile"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tokens"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

// rig — приложение на подставной модели и подставных источниках, собранное
// так же, как его собирает main: агенты, составитель подборки и человек
// вокруг хода. Модель отвечает с правдоподобным usage: оценка запроса ×1,05
// и 70 % из кэша, — иначе калибровке и кэшу нечего было бы мерить.
type rig struct {
	brain *agentstest.Brain
	fake  *llmtest.Fake
	env   *Env
	wiki  *toolstest.Wiki
	gbif  *toolstest.GBIF
	// charter — подставной механизм свода: блок из текста, поправка по
	// словам человека, события стража. Настоящий свод живёт в своём пакете,
	// а стенд должен уметь мерить его и без него.
	charter *charterHook
	opens   int
	t       *testing.T
	// servers — MCP-серверы сборок, по одному на Open; hold — подстрока
	// аргументов вызова, который сервер держит до своего убийства; noMCP —
	// сборка не отдаёт стенду клиента MCP.
	servers []*mcpRig
	hold    string
	noMCP   bool
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{brain: &agentstest.Brain{}, wiki: toolstest.NewWiki(), gbif: toolstest.NewGBIF(),
		charter: &charterHook{text: map[string]string{}}, t: t}
	t.Cleanup(r.wiki.Close)
	t.Cleanup(r.gbif.Close)
	r.fake = &llmtest.Fake{Fn: r.chat}
	reg := features.Catalog()
	r.env = &Env{Registry: reg, Base: reg.Defaults(), Root: t.TempDir(), Model: llm.DefaultModel,
		Timeout: 20 * time.Second, Legacy: "../../testdata/legacy", Open: r.open}
	return r
}

func (r *rig) chat(req llm.Request) (llm.Response, error) {
	resp, err := r.brain.Chat(req)
	if err != nil {
		return resp, err
	}
	resp = plain(req, resp)
	est := tokens.OfMessages(req.Tools, req.Messages)
	prompt := int(float64(est)*1.05 + 0.5)
	resp.Usage = llm.Usage{Prompt: prompt, Completion: 12, Total: prompt + 12, CacheHit: prompt * 7 / 10, CacheMiss: prompt - prompt*7/10}
	return resp, nil
}

// plain — контрольная дорожка без трекера: завершающих инструментов у
// агента нет, и добросовестная модель пишет результат текстом.
func plain(req llm.Request, resp llm.Response) llm.Response {
	if len(resp.Message.ToolCalls) != 1 {
		return resp
	}
	c := resp.Message.ToolCalls[0].Function
	if llmtest.HasTool(req, c.Name) {
		return resp
	}
	switch c.Name {
	case "submit_card", "submit_section":
		return llmtest.Text(c.Arguments)
	case "report_not_found":
		var in struct{ Reason string }
		json.Unmarshal([]byte(c.Arguments), &in)
		return llmtest.Text(`{"not_found":"` + in.Reason + `"}`)
	}
	return resp
}

func (r *rig) open(dir string, o Options) (Build, error) {
	r.opens++
	wiki := r.wiki.URL
	if o.WikiBase != "" {
		wiki = o.WikiBase
	}
	reg := r.env.Registry
	d := store.NewDir(dir)
	// Путь до источников — как у приложения: переключатель по механизмам
	// диалога, сервер MCP в памяти над теми же подставными источниками.
	fetcher := tools.NewFetcher()
	local := tools.LocalTools(fetcher, wiki, r.gbif.URL)
	srv := &mcpRig{wiki: wiki, gbif: r.gbif.URL, hold: r.hold}
	client := mcp.NewClient(mcp.Options{Dial: srv.dial, Want: tools.Fingerprint(local), Logger: slog.New(slog.DiscardHandler)})
	r.t.Cleanup(func() { client.Close() })
	r.servers = append(r.servers, srv)
	deps := agents.Deps{Runner: agent.Runner{LLM: r.fake, Model: llm.DefaultModel, Calibration: &tokens.Calibration{}},
		Features: reg, Sources: &mcp.Switch{Local: tools.MustRegistry(local...), Client: client}}
	people := &persona.Hook{Memory: memory.NewStore(d), Profiles: profile.NewStore(d),
		Extractor: extract.Extractor{LLM: r.fake, Model: llm.DefaultModel}}
	compile := &compiler.Hook{Agents: deps, Store: collection.NewStore(d)}
	hooks := []runs.Hook{compile, people, &charterDir{h: r.charter, dir: dir}}
	// База знаний — как у приложения: стенд задаёт путь (И-10), хук rag
	// ищет по ней hash-эмбеддером, которым база собрана в тесте.
	if o.KB != "" {
		st, err := kb.Open(context.Background(), o.KB)
		if err != nil {
			return Build{}, err
		}
		r.t.Cleanup(func() { st.Close() })
		hooks = append(hooks, &rag.Hook{Searcher: &kb.Searcher{Store: st, Embedder: embed.Hash{}}})
	}
	m := runs.NewManager(runs.Config{Agents: deps, Store: history.NewStore(d), Registry: reg, Defaults: reg.Defaults(),
		Timeout: time.Minute, Hooks: hooks})
	b := Build{Manager: m, HTTP: fetcher.Requests}
	if !r.noMCP {
		b.MCP = rigClient{Client: client, srv: srv}
	}
	return b, nil
}

// charterHook — подставной свод: у каждого каталога свой текст.
type charterHook struct {
	mu   sync.Mutex
	text map[string]string
}

type charterDir struct {
	h   *charterHook
	dir string
}

const charterBase = "Свод справочника: И-1…И-6."

func (c *charterDir) Name() string { return "charter" }

func (c *charterDir) Before(_ context.Context, t *runs.Turn) error {
	if !t.Features.On(features.Charter) {
		return nil
	}
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	text, ok := c.h.text[c.dir]
	if !ok {
		text = charterBase
	}
	t.AddBlock(features.Block{Feature: features.Charter, Title: "свод", Text: text})
	if strings.Contains(t.Request.Text, "принимаю эту поправку") {
		c.h.text[c.dir] = text + " Поправка 1: не называть животных милыми."
	}
	return nil
}

func (c *charterDir) After(_ context.Context, t *runs.Turn) error {
	if t.Features.On(features.Guard) {
		t.Em.Log(agent.Event{Agent: "guard", Kind: agent.EventMechanism, Mechanism: string(features.Guard), Title: "страж: нарушений нет"})
	}
	return nil
}

// oneLead — ведущий отвечает одной фразой на всё.
func (r *rig) oneLead(text string) {
	r.brain.LeadScript = func(llm.Request, int) llm.Response { return llmtest.Text(text) }
}

func (r *rig) run(t *testing.T, tr Trial) *Result {
	t.Helper()
	res := RunOne(context.Background(), r.env, tr)
	if res.Err != "" {
		t.Fatalf("%s: стенд сломался: %s", tr.ID(), res.Err)
	}
	return res
}

func find(t *testing.T, r *Result, what, lane string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.What == what && (lane == "" || c.Lane == lane) {
			return c
		}
	}
	t.Fatalf("%s: проверки «%s» (%s) нет среди %d", r.ID, what, lane, len(r.Checks))
	return Check{}
}

func metric(r *Result, what, lane string) string {
	for _, m := range r.Metrics {
		if m.What == what && m.Lane == lane {
			return m.Value
		}
	}
	return ""
}

func mustPass(t *testing.T, r *Result, whats ...string) {
	t.Helper()
	for _, w := range whats {
		if c := find(t, r, w, ""); c.Status != Pass {
			t.Errorf("%s: «%s» — %s: %s %s", r.ID, w, c.Status, c.Got, c.Note)
		}
	}
}

// --- стенд ---

func TestCheckLanesExactlyOne(t *testing.T) {
	reg := features.Catalog()
	base := reg.Defaults()
	lane := func(name string, fs features.Set) Lane { return Lane{Name: name, Features: fs} }
	cases := []struct {
		name  string
		lanes []Lane
		want  string
	}{
		{"ноль механизмов", []Lane{lane("a", base), lane("b", base)}, "не отличается"},
		{"два механизма", []Lane{lane("a", base), lane("b", base.With(features.Window, false).With(features.Compact, false))}, "window, compact"},
		{"разные механизмы у разных дорожек", []Lane{lane("a", base), lane("b", base.With(features.Window, false)), lane("c", base.With(features.Facts, false))}, "разные механизмы"},
		{"та же, но другая", []Lane{lane("a", base), {Name: "b", Features: base.With(features.Facts, false), Same: true}}, "помечена как та же"},
		{"одна дорожка", []Lane{lane("a", base)}, "сравнивать не с чем"},
		{"только повтор", []Lane{lane("a", base), {Name: "a′", Features: base, Same: true}}, "ни одна дорожка"},
	}
	for _, c := range cases {
		_, err := CheckLanes(reg, c.lanes)
		if !errors.Is(err, ErrLanes) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	for _, bad := range [][]Lane{
		{lane("a", base), lane("a", base.With(features.Window, false))},
		{lane("", base), lane("b", base.With(features.Window, false))},
		{lane("a", base), lane("b", base.With(features.Charter, false))},
	} {
		if _, err := CheckLanes(reg, bad); err == nil {
			t.Errorf("дорожки %v приняты", bad)
		}
	}
	n, err := CheckLanes(reg, []Lane{lane("a", base), {Name: "a′", Features: base, Same: true}, lane("b", base.With(features.Facts, false))})
	if err != nil || n != features.Facts {
		t.Fatalf("повтор и одна разница: %v %v", n, err)
	}
}

// Стенд отказывается запускаться при разнице в 0 и 2 механизма — до того,
// как заведёт хоть один диалог.
func TestStandRefusesBeforeCreatingDialogs(t *testing.T) {
	r := newRig(t)
	s, err := r.env.NewStand("отказ", Options{})
	if err != nil {
		t.Fatal(err)
	}
	base := r.env.Base
	for _, lanes := range [][]Lane{
		{{Name: "a", Features: base}, {Name: "b", Features: base}},
		{{Name: "a", Features: base}, {Name: "b", Features: base.With(features.Window, false).With(features.Facts, false)}},
	} {
		if _, err := s.Group("x", lanes); !errors.Is(err, ErrLanes) {
			t.Fatalf("стенд запустился: %v", err)
		}
	}
	if n := len(s.Manager().List()); n != 0 {
		t.Fatalf("отказавший стенд завёл диалогов: %d", n)
	}
	if _, err := s.Solo("x", Lane{Name: "a", Features: base.With(features.Charter, false)}); err == nil {
		t.Fatal("несогласованный набор одиночного диалога")
	}
}

func TestGroupSendRestartContinue(t *testing.T) {
	r := newRig(t)
	r.oneLead("Ответ ведущего.")
	s, err := r.env.NewStand("стенд", Options{})
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.Group("пара", []Lane{{Name: "основная", Features: r.env.Base}, {Name: "без окна", Features: r.env.Base.With(features.Window, false)}})
	if err != nil || g.Mechanism != features.Window {
		t.Fatalf("стенд: %v %v", g, err)
	}
	ctx := context.Background()
	steps, err := g.Send(ctx, agents.Request{Text: "Как дела у рысей?"})
	if err != nil || len(steps) != 2 || !steps[0].OK() || steps[1].Lane != "без окна" {
		t.Fatalf("ход стенда: %+v %v", steps, err)
	}
	if err := s.Restart(); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Send(ctx, agents.Request{Text: "А зимой?"}); err != nil {
		t.Fatalf("стенд после перезапуска: %v", err)
	}
	d := g.Dialog("без окна")
	if d == nil || g.Dialog("нет такой") != nil {
		t.Fatal("Dialog по имени")
	}
	old := d.ID
	if err := d.Continue("новый", ""); err != nil || d.ID == old {
		t.Fatalf("новый диалог: %v", err)
	}
	steps, err = g.Send(ctx, agents.Request{Text: "И ещё?"})
	if err != nil || len(steps) != 2 {
		t.Fatalf("стенд с перешедшей дорожкой: %v", err)
	}
	if det, ok := d.Detail(); !ok || det.Lane != "без окна" || len(det.TurnList) != 1 {
		t.Fatalf("новый диалог дорожки: %+v", det.Summary)
	}
	if _, err := g.Send(ctx, agents.Request{Text: "  "}); err == nil {
		t.Fatal("пустой вопрос")
	}
	d.ID = "nope"
	if err := d.Continue("x", ""); err == nil {
		t.Fatal("продолжение несуществующего диалога")
	}
	if _, err := d.Ask(ctx, "x?"); err == nil {
		t.Fatal("ход в несуществующем диалоге")
	}
	order, turns := s.rec.snapshot()
	if len(order) != 2 || len(turns["основная"]) != 3 {
		t.Fatalf("учёт дорожек: %v %d", order, len(turns["основная"]))
	}
	if s.Dir() == "" || r.opens != 2 {
		t.Fatalf("каталог и сборки менеджера: %q %d", s.Dir(), r.opens)
	}
}

func TestStandErrors(t *testing.T) {
	r := newRig(t)
	env := *r.env
	env.Open = nil
	if _, err := env.NewStand("x", Options{}); err == nil {
		t.Fatal("стенд без сборки менеджера")
	}
	env.Open = func(string, Options) (Build, error) { return Build{}, errors.New("сборка") }
	if _, err := env.NewStand("x", Options{}); err == nil {
		t.Fatal("ошибка сборки")
	}
	env.Open = func(string, Options) (Build, error) { return Build{}, nil }
	if _, err := env.NewStand("x", Options{}); err == nil {
		t.Fatal("сборка без менеджера")
	}
	res := RunOne(context.Background(), &env, NewMemory())
	if res.Err == "" || res.Verdict() != Fail {
		t.Fatal("ошибка стенда должна попасть в итог испытания")
	}
	s, _ := r.env.NewStand("setup", Options{})
	bad := func(*Stand, string) error { return errors.New("анкета") }
	if _, err := s.Group("x", []Lane{{Name: "a", Features: r.env.Base, Setup: bad}, {Name: "b", Features: r.env.Base.With(features.Window, false)}}); err == nil {
		t.Fatal("ошибка подготовки дорожки")
	}
	if _, err := s.Solo("x", Lane{Name: "a", Features: r.env.Base, Setup: bad}); err == nil {
		t.Fatal("ошибка подготовки одиночной дорожки")
	}
	if err := withPreset("nope")(s, "x"); err == nil {
		t.Fatal("неизвестная заготовка")
	}
	// Ход, который не успел закончиться, — ошибка стенда, а не проверка.
	release := make(chan struct{})
	r.fake.Fn = func(llm.Request) (llm.Response, error) { <-release; return llmtest.Text("ок"), nil }
	r.env.Timeout = 50 * time.Millisecond
	d, _ := s.Solo("медленно", Lane{Name: "a", Features: r.env.Base})
	_, err := d.Ask(context.Background(), "Как дела?")
	close(release)
	if err == nil || !strings.Contains(err.Error(), "не закончился") {
		t.Fatalf("медленный ход: %v", err)
	}
	// Дождаться записи хода: каталог теста удаляется после него.
	for i := 0; i < 200; i++ {
		if det, _ := d.Detail(); !det.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSlugs(t *testing.T) {
	if slug("И-1") != "i-1" || slug("!!") != "trial" || laneSlug("очень длинное имя дорожки стенда") != "ochen-dlinnoe-imya-doroz" {
		t.Fatalf("slug: %q %q %q", slug("И-1"), slug("!!"), laneSlug("очень длинное имя дорожки стенда"))
	}
	if clip("абв где", 3) != "абв…" || orDash("") != "—" {
		t.Fatal("clip, orDash")
	}
}

func TestSelect(t *testing.T) {
	all := Trials()
	if len(all) != 11 {
		t.Fatalf("испытаний %d", len(all))
	}
	got, err := Select(all, "И-1, 6,i6")
	if err != nil || len(got) != 2 || got[0].ID() != "И-1" || got[1].ID() != "И-6" {
		t.Fatalf("выбор: %v", err)
	}
	if got, _ := Select(all, "all"); len(got) != 11 {
		t.Fatal("all")
	}
	if got, _ := Select(all, ""); len(got) != 11 {
		t.Fatal("пусто — все")
	}
	if _, err := Select(all, "И-12"); err == nil {
		t.Fatal("неизвестное испытание")
	}
	if _, err := Select(all, " , "); err == nil {
		t.Fatal("ничего не выбрано")
	}
	for _, tr := range all {
		if tr.Title() == "" {
			t.Errorf("%s без названия", tr.ID())
		}
	}
}
