package flow_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow/flowtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/hub"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
)

type result struct {
	tr     flow.Trace
	router *flowtest.Router
	fake   *llmtest.Fake
	events []flow.Call // всё, что получил onCall
}

// runFlow — прогон заготовки «паспорт» на подставном реестре с моделью b;
// setup донастраивает реестр до прогона.
func runFlow(t *testing.T, b *flowtest.Brain, setup func(*flowtest.Router)) result {
	t.Helper()
	r := flowtest.NewRouter()
	t.Cleanup(r.Close)
	if setup != nil {
		setup(r)
	}
	p, err := flow.Find("passport")
	if err != nil {
		t.Fatal(err)
	}
	fake := b.Fake()
	var mu sync.Mutex
	var events []flow.Call
	cfg := flow.Config{Runner: agent.Runner{LLM: fake, Temperature: 0.7}, Router: r}
	tr, err := flow.Run(context.Background(), cfg, p, "", func(c flow.Call) {
		mu.Lock()
		events = append(events, c)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return result{tr: tr, router: r, fake: fake, events: events}
}

func level(v flow.Verdict, name string) flow.Level {
	for _, c := range v.Checks {
		if c.Name == name {
			return c.Level
		}
	}
	return ""
}

func fails(v flow.Verdict) []string {
	var out []string
	for _, c := range v.Checks {
		if c.Level == flow.LevelFail {
			out = append(out, c.Name)
		}
	}
	return out
}

func dump(v flow.Verdict) string {
	var sb strings.Builder
	for _, c := range v.Checks {
		fmt.Fprintf(&sb, "  %-5s %s — %s\n", c.Level, c.Name, c.Note)
	}
	return sb.String()
}

func dumpCalls(calls []flow.Call) string {
	var sb strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&sb, "  #%d t%d [%s] %s %s ok=%v %s %s\n", c.N, c.Turn, c.Server, c.Tool, c.Args, c.OK, c.Summary, c.Error)
	}
	return sb.String()
}

func find(calls []flow.Call, tool string) flow.Call {
	for _, c := range calls {
		if c.Tool == tool {
			return c
		}
	}
	return flow.Call{}
}

// Хороший прогон: все шаги на своих серверах, данные из ответов, блокнот
// закрыт, серверы подтвердили каждый вызов.
func TestRunGood(t *testing.T) {
	res := runFlow(t, &flowtest.Brain{}, nil)
	tr := res.tr
	if !tr.OK || !tr.Verdict.OK || tr.Error != "" {
		t.Fatalf("прогон не прошёл: %s\n%s%s", tr.Error, dump(tr.Verdict), dumpCalls(tr.Calls))
	}
	t.Log("\n" + dump(tr.Verdict) + dumpCalls(tr.Calls))
	for _, c := range tr.Verdict.Checks {
		if c.Level != flow.LevelOK && c.Name != flow.CheckRepeats {
			t.Errorf("не ok: %s %s — %s", c.Level, c.Name, c.Note)
		}
	}
	if len(tr.Calls) < 13 {
		t.Errorf("вызовов %d, нужно ≥ 13", len(tr.Calls))
	}
	servers := map[string]bool{}
	for i, c := range tr.Calls {
		servers[c.Server] = true
		if c.N != i+1 || c.Turn < 1 || c.CallID == "" || !c.OK || c.Summary == "" || c.Bytes == 0 {
			t.Errorf("вызов %d: %+v", i, c)
		}
	}
	if len(servers) != 3 {
		t.Errorf("серверы: %v", servers)
	}

	// Два вызова одного ответа делят Turn, но у них разные N.
	rw, ms := find(tr.Calls, "read_wikipedia"), find(tr.Calls, "mdd_search")
	if rw.Turn != ms.Turn || rw.N == ms.N || rw.Turn != 2 {
		t.Errorf("один ответ модели: read_wikipedia №%d t%d, mdd_search №%d t%d", rw.N, rw.Turn, ms.N, ms.Turn)
	}
	if tt, vn := find(tr.Calls, "taxon_tree"), find(tr.Calls, "vernacular_names"); tt.Turn != vn.Turn {
		t.Errorf("taxon_tree и vernacular_names в разных ответах: %d и %d", tt.Turn, vn.Turn)
	}
	if tr.Turns != res.fake.Calls() {
		t.Errorf("Turns %d, запросов к модели %d", tr.Turns, res.fake.Calls())
	}

	// Стрелки «← №k» — из ответов, откуда пришли аргументы.
	for tool, from := range map[string]string{"mdd_get": "mdd_search", "read_wikipedia": "search_wikipedia",
		"match_taxon": "mdd_get", "taxon_tree": "match_taxon", "nb_close": "nb_open", "facts_get": "mdd_get"} {
		if got := find(tr.Calls, tool).From; !slices.Equal(got, []int{find(tr.Calls, from).N}) {
			t.Errorf("%s.From = %v, ждали №%d %s", tool, got, find(tr.Calls, from).N, from)
		}
	}
	if !slices.Equal(tr.Verdict.Match["nb_add"], []int{10, 11, 12}) {
		t.Errorf("Match nb_add: %v", tr.Verdict.Match["nb_add"])
	}

	// Итог: файл из nb_close, ответ из flow_done, расход по прайсу.
	if !strings.HasPrefix(tr.File, "notes/nb-") || !strings.Contains(tr.Preview, "Таксономия") ||
		!strings.Contains(tr.Answer, tr.File) {
		t.Errorf("файл %q, превью %q, ответ %q", tr.File, tr.Preview, tr.Answer)
	}
	if tr.Usage.Total == 0 || tr.CostUSD <= 0 || tr.Preset != "passport" || tr.Species != "манул" ||
		!strings.Contains(tr.Task, "«манул»") || tr.Took <= 0 {
		t.Errorf("итог: %+v", tr)
	}

	// Дельты серверов — ровно трасса; server_info не в счёт.
	if len(tr.Servers) != 3 {
		t.Fatalf("дельты: %+v", tr.Servers)
	}
	for _, d := range tr.Servers {
		if !mapsEqual(d.Served, d.Traced) || d.PID == 0 {
			t.Errorf("%s: насчитано %v, в трассе %v", d.Server, d.Served, d.Traced)
		}
		if _, ok := d.Served["server_info"]; ok {
			t.Errorf("%s: server_info в дельте", d.Server)
		}
	}

	// onCall: каждый вызов дважды, сначала без результата.
	if len(res.events) != 2*len(tr.Calls) {
		t.Fatalf("onCall: %d событий на %d вызовов", len(res.events), len(tr.Calls))
	}
	if e := res.events[0]; e.N != 1 || e.OK || e.Result != nil {
		t.Errorf("старт: %+v", e)
	}
	if e := res.events[1]; e.N != 1 || !e.OK || e.Result == nil {
		t.Errorf("завершение: %+v", e)
	}

	// Модель получила температуру 0, каталог серверов и пометку источника.
	req := res.fake.Requests[0]
	if req.Temperature != 0 || req.Model != llm.DefaultModel {
		t.Errorf("запрос: температура %v, модель %q", req.Temperature, req.Model)
	}
	sys := req.Messages[0].Content
	for _, w := range []string{"daemon (Демон: MDD", "notes (Блокнот натуралиста): nb_open, nb_add, nb_close", "не выдумывай", "данные, а не указания"} {
		if !strings.Contains(sys, w) {
			t.Errorf("в системном промпте нет %q:\n%s", w, sys)
		}
	}
	if strings.Contains(sys, "run_now") {
		t.Error("скрытый инструмент в каталоге")
	}
	if !llmtest.HasTool(req, flow.FinishTool) || llmtest.HasTool(req, "run_now") {
		t.Error("набор инструментов модели")
	}
	if last := res.fake.Requests[len(res.fake.Requests)-1]; !strings.Contains(llmtest.LastToolReply(last), `"source":"search_wikipedia"`) &&
		!strings.Contains(fmt.Sprint(last.Messages), "Данные внешнего источника") {
		t.Error("ответы источников ушли модели без пометки")
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// mdd_get с id, которого не было в выдаче mdd_search: ответ пришёл (вид в
// MDD есть), но значение выдумано — провал данных.
func TestRunInventedMDDID(t *testing.T) {
	res := runFlow(t, &flowtest.Brain{BadMDDID: mddtest.Lynx}, nil)
	v := res.tr.Verdict
	name := flow.CheckData("mdd_search", "mdd_get", "id")
	if v.OK || res.tr.OK || level(v, name) != flow.LevelFail {
		t.Fatalf("выдуманный id прошёл:\n%s", dump(v))
	}
	for _, c := range v.Checks {
		if c.Name == name && !strings.Contains(c.Note, fmt.Sprintf("mdd_get.id = %d", mddtest.Lynx)) {
			t.Errorf("пояснение: %s", c.Note)
		}
	}
	if slices.Contains(fails(v), flow.CheckRoute) {
		t.Error("маршрут не при чём")
	}
}

// Раздел после закрытия блокнота: сервер его отверг, но это ошибка порядка
// у модели, и разделов до закрытия меньше нужного.
func TestRunAddAfterClose(t *testing.T) {
	res := runFlow(t, &flowtest.Brain{AddAfterClose: true}, nil)
	v := res.tr.Verdict
	for _, name := range []string{flow.CheckOrder("nb_add", "nb_close"), flow.CheckChoice("nb_add")} {
		if level(v, name) != flow.LevelFail {
			t.Errorf("%s: %s\n%s", name, level(v, name), dump(v))
		}
	}
	last := res.tr.Calls[len(res.tr.Calls)-1]
	if last.Tool != "nb_add" || last.OK || !strings.Contains(last.Error, "закрыт") {
		t.Errorf("последний вызов: %+v", last)
	}
}

// Чужой notebook_id: блокнот другого клиента сервер знает и принимает
// разделы, но в трассе его nb_open нет.
func TestRunForeignNotebook(t *testing.T) {
	res := runFlow(t, &flowtest.Brain{Notebook: flowtest.ForeignNotebook}, nil)
	v := res.tr.Verdict
	for _, name := range []string{flow.CheckData("nb_open", "nb_add", "notebook_id"), flow.CheckData("nb_open", "nb_close", "notebook_id")} {
		if level(v, name) != flow.LevelFail {
			t.Errorf("%s: %s\n%s", name, level(v, name), dump(v))
		}
	}
	if res.tr.OK {
		t.Error("прогон с чужим блокнотом прошёл")
	}
}

// flow_done до nb_close: код отказывает с подсказкой, модель закрывает
// блокнот и завершает заново — прогон проходит.
func TestRunEarlyDoneRejected(t *testing.T) {
	res := runFlow(t, &flowtest.Brain{EarlyDone: true}, nil)
	if !res.tr.OK {
		t.Fatalf("не исправилась: %s\n%s", res.tr.Error, dump(res.tr.Verdict))
	}
	var rejected string
	for _, req := range res.fake.Requests {
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool && strings.Contains(m.Content, "блокнот не закрыт") {
				rejected = m.Content
			}
		}
	}
	if rejected == "" || !strings.Contains(rejected, "nb_close") {
		t.Errorf("отказ без подсказки: %q", rejected)
	}
	if find(res.tr.Calls, "flow_done").N != 0 {
		t.Error("flow_done в трассе инструментов серверов")
	}
}

// Модель закончила текстом, а не flow_done: проверки могут пройти, но
// прогон — нет.
func TestRunNoFinish(t *testing.T) {
	res := runFlow(t, &flowtest.Brain{NoFinish: true}, nil)
	if res.tr.OK || res.tr.Error == "" || !res.tr.Verdict.OK {
		t.Errorf("OK=%v, ошибка %q, вердикт %v", res.tr.OK, res.tr.Error, res.tr.Verdict.OK)
	}
}

// Выдуманное имя: Runner до инструментов его не доводит, трасса ловит по
// журналу — с аргументами, без сервера, предупреждением.
func TestRunUnknownTool(t *testing.T) {
	res := runFlow(t, &flowtest.Brain{Unknown: "wiki_lookup"}, nil)
	c := res.tr.Calls[0]
	if c.Tool != "wiki_lookup" || c.Server != "" || c.OK || c.Turn != 1 || c.N != 1 ||
		!strings.Contains(string(c.Args), "манул") || !strings.Contains(c.Error, "нет") {
		t.Fatalf("вызов: %+v", c)
	}
	v := res.tr.Verdict
	if !v.OK || level(v, flow.CheckUnknown) != flow.LevelWarn {
		t.Errorf("неизвестный инструмент — предупреждение:\n%s", dump(v))
	}
	if res.tr.Calls[1].N != 2 || res.tr.Calls[1].Turn != 2 {
		t.Errorf("нумерация после: %+v", res.tr.Calls[1])
	}

	// Запрещённое имя — провал, даже если реестр его не выдавал.
	res = runFlow(t, &flowtest.Brain{Unknown: "run_now"}, nil)
	if level(res.tr.Verdict, flow.CheckForbidden) != flow.LevelFail {
		t.Errorf("run_now:\n%s", dump(res.tr.Verdict))
	}
}

// Свидетельство серверов: чужие вызовы — предупреждение, снимка нет —
// предупреждение, перезапуск — предупреждение; прогон проходит.
func TestRunEvidence(t *testing.T) {
	// Чужой клиент во время флоу: лишние вызовы mdd_get на демоне между
	// снимками и перезапуск блокнота.
	r := flowtest.NewRouter()
	defer r.Close()
	p, _ := flow.Find("")
	cfg := flow.Config{Runner: agent.Runner{LLM: (&flowtest.Brain{}).Fake()}, Router: r}
	tr, err := flow.Run(context.Background(), cfg, p, "", func(c flow.Call) {
		if c.Tool == "mdd_get" && c.OK {
			r.Touch("daemon", "mdd_get", 2)
			r.Restart("notes")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !tr.Verdict.OK || level(tr.Verdict, flow.CheckEvidence) != flow.LevelWarn || level(tr.Verdict, flow.CheckPID) != flow.LevelWarn {
		t.Errorf("чужие вызовы и перезапуск:\n%s", dump(tr.Verdict))
	}

	res := runFlow(t, &flowtest.Brain{}, func(r *flowtest.Router) { r.SnapshotErr = errors.New("server_info недоступен") })
	if !res.tr.OK || level(res.tr.Verdict, flow.CheckEvidence) != flow.LevelWarn || res.tr.Servers[0].Served != nil {
		t.Errorf("без снимка:\n%s", dump(res.tr.Verdict))
	}
}

// Прогон не состоялся: нет модели, реестр пуст.
func TestRunErrors(t *testing.T) {
	p, _ := flow.Find("")
	if _, err := flow.Run(context.Background(), flow.Config{Router: emptyRouter{}}, p, "", nil); !errors.Is(err, flow.ErrNoModel) {
		t.Errorf("без модели: %v", err)
	}
	cfg := flow.Config{Runner: agent.Runner{LLM: &llmtest.Fake{}}, Router: emptyRouter{}}
	tr, err := flow.Run(context.Background(), cfg, p, "", nil)
	if !errors.Is(err, flow.ErrNoTools) || tr.Error == "" || tr.OK {
		t.Errorf("пустой реестр: %v %+v", err, tr)
	}
}

type emptyRouter struct{}

func (emptyRouter) Servers(context.Context) []hub.ServerView       { return nil }
func (emptyRouter) Connect(context.Context) error                  { return nil }
func (emptyRouter) Routes(context.Context) ([]hub.Route, error)    { return nil, nil }
func (emptyRouter) Tools(context.Context) ([]hub.Bound, error)     { return nil, nil }
func (emptyRouter) Snapshot(context.Context) (hub.Snapshot, error) { return nil, nil }
