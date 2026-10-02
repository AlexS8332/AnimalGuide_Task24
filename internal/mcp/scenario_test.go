package mcp

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents/agentstest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// lane — одна дорожка сценария: подставная модель, журнал, итог.
type lane struct {
	fake    *llmtest.Fake
	rec     *agent.Recorder
	results []agents.Result
	deltas  []card.Delta
}

// runLane прогоняет сценарий агентов через Switch: via = local — механизм
// mcp выключен, via = mcp — включён, сервер в памяти.
func runLane(t *testing.T, r *rig, via string) *lane {
	t.Helper()
	c := r.client(t, nil)
	sw := &Switch{Local: tools.MustRegistry(r.local...), Client: c}
	b := &agentstest.Brain{}
	l := &lane{fake: &llmtest.Fake{Fn: b.Chat}, rec: &agent.Recorder{}}
	d := agents.Deps{Runner: agent.Runner{LLM: l.fake, Model: llm.DefaultModel}, Features: features.Catalog(), Sources: sw}
	fs := features.Catalog().Defaults()
	if via == tools.ViaMCP {
		fs = fs.With(features.MCP, true)
	}
	em := &agent.Safe{E: l.rec}
	for _, text := range []string{"Сравни рысь и манул", "лесной кот"} {
		res, err := agents.Run(context.Background(), d, agents.Request{Kind: agents.KindMessage, Text: text, Features: fs, Deltas: l.deltas}, em)
		if err != nil {
			t.Fatalf("%s, %q: %v", via, text, err)
		}
		for i := range res.Deltas {
			res.Deltas[i].Turn = "t"
		}
		l.deltas = append(l.deltas, res.Deltas...)
		l.results = append(l.results, res)
	}
	return l
}

// codeCall — номер вызова, который сделал код, а не модель: счётчик общий
// на процесс, в сравнении дорожек он не важен.
var codeCall = regexp.MustCompile(`code_taxon_tree_\d+`)

func normalize(s string) string { return codeCall.ReplaceAllString(s, "code_taxon_tree_N") }

// requests — запросы к модели в каноническом порядке: специалисты идут
// параллельно, и порядок их запросов не определён.
func (l *lane) requests(t *testing.T) []string {
	var out []string
	for _, req := range l.fake.Requests {
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, normalize(string(data)))
	}
	sort.Strings(out)
	return out
}

func (l *lane) calls(via string) (n int, names []string) {
	for _, ev := range l.rec.Events {
		if ev.Kind != agent.EventToolCall || !tools.IsSourceTool(ev.Tool) {
			continue
		}
		names = append(names, ev.Tool)
		if ev.Via == via {
			n++
		}
	}
	sort.Strings(names)
	return n, names
}

// Сценарии агентов на двух путях: те же карточки, тот же текст, те же
// запросы к модели побайтно — механизм не стоит ни токена. На дорожке MCP
// все вызовы источников идут через сервер, на локальной — ни одного.
func TestAgentsScenarioBothPaths(t *testing.T) {
	// Подставные источники общие: адреса статей в карточках совпадают.
	r := newRig(t, nil)
	loc := runLane(t, r, tools.ViaLocal)
	if r.dials.Load() != 0 {
		t.Fatalf("локальная дорожка подняла сервер %d раз", r.dials.Load())
	}
	rem := runLane(t, r, tools.ViaMCP)
	if r.dials.Load() != 1 {
		t.Fatalf("дорожка MCP: подключений %d", r.dials.Load())
	}
	for i := range loc.results {
		a, b := loc.results[i], rem.results[i]
		if a.Text != b.Text || a.Route != b.Route {
			t.Fatalf("ход %d: ответы разошлись\n%q\n%q", i, a.Text, b.Text)
		}
		if a.Effective.On(features.MCP) || !b.Effective.On(features.MCP) {
			t.Fatalf("ход %d: Effective %v / %v", i, a.Effective, b.Effective)
		}
	}
	sa, _ := json.Marshal(card.Fold(loc.deltas))
	sb, _ := json.Marshal(card.Fold(rem.deltas))
	if normalize(string(sa)) != normalize(string(sb)) {
		t.Fatalf("карточки разошлись:\n%s\n%s", sa, sb)
	}
	if !strings.Contains(string(sa), "Lynx lynx") || !strings.Contains(string(sa), "Otocolobus manul") {
		t.Fatalf("карточки не собрались: %s", sa)
	}
	ra, rb := loc.requests(t), rem.requests(t)
	if len(ra) != len(rb) || len(ra) == 0 {
		t.Fatalf("запросов к модели: %d и %d", len(ra), len(rb))
	}
	for i := range ra {
		if normalize(ra[i]) != normalize(rb[i]) {
			t.Fatalf("запрос к модели %d разошёлся:\n%s\n%s", i, ra[i], rb[i])
		}
	}

	nLoc, namesLoc := loc.calls(tools.ViaMCP)
	nRem, namesRem := rem.calls(tools.ViaMCP)
	if nLoc != 0 || nRem == 0 || nRem != len(namesRem) || strings.Join(namesLoc, ",") != strings.Join(namesRem, ",") {
		t.Fatalf("вызовы источников: локально %d через MCP из %v, дорожка MCP %d из %v", nLoc, namesLoc, nRem, namesRem)
	}
	// Счётчики сервера сходятся с журналом: каждый вызов дошёл до сервера.
	if got := r.srv.Stats().TotalCalls; got != nRem {
		t.Fatalf("сервер насчитал %d вызовов, журнал — %d", got, nRem)
	}
	var connects, toolsEv int
	for _, ev := range rem.rec.Events {
		switch ev.Kind {
		case EventConnect:
			connects++
		case EventTools:
			toolsEv++
		}
	}
	if connects != len(rem.results) || toolsEv != len(rem.results) {
		t.Fatalf("mcp.connect %d, mcp.tools %d на %d ходов", connects, toolsEv, len(rem.results))
	}
	// Пометка источника (ФТ-41) на обоих путях: адаптеры Untrusted.
	injected := false
	for _, req := range rem.fake.Requests {
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool && strings.Contains(m.Content, tools.TrustNote) && strings.Contains(m.Content, "Лесной кот") {
				injected = true
			}
		}
	}
	if !injected {
		t.Fatal("ответ источника через MCP ушёл модели без пометки")
	}
}
