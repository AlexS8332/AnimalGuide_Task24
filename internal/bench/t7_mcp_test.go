package bench

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// mcpRig — MCP-сервер сборки стенда в памяти. Каждое подключение — новый
// «процесс»: свой сервер, свой кэш источников, свои счётчики, как у
// настоящего перезапуска. Kill роняет текущий процесс.
type mcpRig struct {
	wiki, gbif string
	// hold — подстрока аргументов вызова, который сервер держит, пока его
	// не убьют: так убийство стенда гарантированно приходится посреди
	// вызова, а не между вызовами.
	hold string
	// fail — подключение не удаётся: сервер «не запускается».
	fail bool

	mu      sync.Mutex
	conn    sdk.Connection
	dials   int
	killed  bool
	entered chan struct{}
	release chan struct{}
}

// killable — серверный конец транспорта в памяти, который можно оборвать:
// закрытие сессии SDK ждёт ответов на все запросы, а упавший процесс не
// отвечает никому.
type killable struct {
	sdk.Transport
	m *mcpRig
}

func (k killable) Connect(ctx context.Context) (sdk.Connection, error) {
	c, err := k.Transport.Connect(ctx)
	if err == nil {
		k.m.conn = c
	}
	return c, err
}

func (m *mcpRig) dial(ctx context.Context) (sdk.Transport, *exec.Cmd, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return nil, nil, errors.New("бинарник сервера не найден")
	}
	if m.entered == nil {
		m.entered, m.release = make(chan struct{}, 1), make(chan struct{})
	}
	fetcher := tools.NewFetcher()
	ts := tools.LocalTools(fetcher, m.wiki, m.gbif)
	for i := range ts {
		ts[i] = tools.Wrap(ts[i], m.gate)
	}
	srv := mcp.NewServer(ts, mcp.ServerOptions{WikiBase: m.wiki, GBIFBase: m.gbif, Fetcher: fetcher})
	ct, st := sdk.NewInMemoryTransports()
	if _, err := srv.SDK().Connect(ctx, killable{Transport: st, m: m}, nil); err != nil {
		return nil, nil, err
	}
	m.dials++
	return ct, nil, nil
}

func (m *mcpRig) gate(ctx context.Context, args json.RawMessage, next tools.CallFunc) (string, error) {
	m.mu.Lock()
	hold := m.hold != "" && !m.killed && strings.Contains(string(args), m.hold)
	entered, release := m.entered, m.release
	m.mu.Unlock()
	if hold {
		entered <- struct{}{}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-release:
			return "", errors.New("процесс убит")
		}
	}
	return next(ctx, args)
}

// kill — процесс сервера падает: дождаться, пока удерживаемый вызов дойдёт
// до сервера, и закрыть серверную сессию.
func (m *mcpRig) kill() error {
	m.mu.Lock()
	hold, entered := m.hold, m.entered
	m.mu.Unlock()
	if hold != "" && entered != nil {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil {
		return errors.New("процесса сервера нет")
	}
	// Сначала обрыв связи, потом отпустить удерживаемый вызов: его ответ
	// уже некуда писать.
	err := m.conn.Close()
	if !m.killed {
		m.killed = true
		close(m.release)
	}
	return err
}

func (m *mcpRig) dialCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dials
}

// rigClient — клиент MCP сборки, у которого Kill роняет сервер в памяти.
type rigClient struct {
	*mcp.Client
	srv *mcpRig
}

func (c rigClient) Kill() error { return c.srv.kill() }

// shortMCP — И-7 в малом: два настоящих, выдумка и трудный случай; сервер
// убивается на трудном случае — его поиск держится до убийства.
func shortMCP() *MCP {
	return &MCP{Facts: shortFacts(), Sections: []string{"habitat", "breeding", "status"},
		Compare: [2]string{"рысь", "манул"}, KillAfter: 3}
}

func checkAll(t *testing.T, r *Result, want Status) {
	t.Helper()
	for _, c := range r.Checks {
		if c.Status != want {
			t.Errorf("%s: «%s» (%s) — %s: %s %s", r.ID, c.What, c.Lane, c.Status, c.Got, c.Note)
		}
	}
}

// И-7 на подставной модели и сервере в памяти: все проверки проходят,
// сервер убит посреди вызова, модель получила ошибку словами, следующий
// ход подключился заново.
func TestMCPTrial(t *testing.T) {
	r := newRig(t)
	r.brain.GateNo = []string{"шурундук"}
	r.hold = "полосат"
	res := r.run(t, shortMCP())
	checkAll(t, res, Pass)
	if res.Verdict() != Pass || res.Mechanism != MCPName || len(res.Lanes) != 3 {
		t.Fatalf("итог: %s %s %+v", res.Verdict(), res.Mechanism, res.Lanes)
	}
	if res.Lanes[0].Name != laneLocal || res.Lanes[1].Diff != "+mcp" || res.Lanes[2].Diff != "те же механизмы" {
		t.Fatalf("дорожки: %+v", res.Lanes)
	}
	for _, what := range []string{"настоящие опознаны", "выдумки отвергнуты", "трудные случаи отвергнуты"} {
		for _, lane := range []string{laneMCP, laneLocal} {
			find(t, res, what, lane)
		}
	}
	restart := find(t, res, "следующий ход — через перезапущенный сервер", laneMCP)
	if !strings.Contains(restart.Note, "перезапуск №1") {
		t.Fatalf("перезапуск: %+v", restart)
	}
	if c := find(t, res, "модель получила {\"error\"} «MCP-сервер недоступен»", laneMCP); !strings.Contains(c.Note, "search_wikipedia: MCP-сервер недоступен") {
		t.Fatalf("ошибка модели: %+v", c)
	}
	if c := find(t, res, "вызовы через MCP: журнал A против счётчика сервера", laneMCP); !strings.Contains(c.Got, "до убийства") || !strings.Contains(c.Got, "после перезапуска") {
		t.Fatalf("сверка со счётчиком: %+v", c)
	}
	if n := r.servers[0].dialCount(); n != 2 {
		t.Fatalf("подключений к серверу %d, ждали 2: первое и после убийства", n)
	}
	for _, m := range []struct{ what, lane, want string }{
		{"доля вызовов источников через MCP", laneMCP, "100 %"},
		{"подключений к серверу за испытание", laneMCP, "2"},
		{"совпадение наборов вызовов с B, ходов", laneRepeat, "11 из 11 (шум модели)"},
		{"тексты ошибок инструментов", laneMCP, "MCP-сервер недоступен"},
		{"тексты ошибок инструментов", laneLocal, "нет"},
		{"HTTP-запросов к источникам", laneMCP, "процессы сервера"},
		{"HTTP-запросов к источникам", laneLocal, "B и B′ вместе"},
		{"подъём после убийства", laneMCP, "подключение №2"},
		{"отношение времени к B", laneMCP, ""},
		{"накладные MCP на вызов, p50", laneMCP, "мс"},
		{"время вызова по счётчикам клиента MCP, p50 по инструментам", laneMCP, "мс"},
	} {
		got := metric(res, m.what, m.lane)
		if m.what == "доля вызовов источников через MCP" {
			got = find(t, res, m.what, m.lane).Got
		}
		if got == "" || !strings.Contains(got, m.want) {
			t.Errorf("«%s» (%s): %q, ждали %q", m.what, m.lane, got, m.want)
		}
	}
	if v := metric(res, "холодный старт сервера", laneMCP); !strings.HasSuffix(v, " с") {
		t.Errorf("холодный старт: %q", v)
	}
	killed := false
	for _, s := range res.Samples {
		if strings.HasPrefix(s.Topic, "сервер убит посреди хода") && s.Lane == laneMCP && strings.Contains(s.Note, "search_wikipedia") {
			killed = true
		}
	}
	if !killed {
		t.Fatalf("в отчёте нет ответа убитого хода: %+v", res.Samples)
	}
	// Ходы всех трёх дорожек в учёте стенда, у A — с механизмом.
	if len(res.Stats) != 3 || res.Stats[0].Brak != 0 {
		t.Fatalf("учёт дорожек: %+v", res.Stats)
	}
}

// Испытание не гоняется, если механизма нет в реестре или сборка не даёт
// клиента.
func TestMCPTrialSkips(t *testing.T) {
	r := newRig(t)
	r.noMCP = true
	if res := r.run(t, &MCP{}); !strings.Contains(res.Skipped, "не даёт клиента") || res.Verdict() != Pending {
		t.Fatalf("сборка без MCP: %+v", res)
	}
	var ms []features.Mechanism
	for _, m := range features.Catalog().All() {
		if m.Name != MCPName {
			ms = append(ms, m)
		}
	}
	reg, err := features.New(ms...)
	if err != nil {
		t.Fatal(err)
	}
	r.env.Registry = reg
	if res := r.run(t, &MCP{}); !strings.Contains(res.Skipped, "нет в реестре") {
		t.Fatalf("реестр без mcp: %+v", res)
	}
}

// Без убийства сверка со счётчиком сервера идёт одним отрезком; убийство
// на ходе без вызовов источников и за пределами сценария — «не определено».
func TestMCPTrialKillEdges(t *testing.T) {
	r := newRig(t)
	r.brain.GateNo = []string{"шурундук"}
	small := &Facts{Real: []string{"рысь"}, Fake: []string{"шурундук пятнистый"}, Hard: []string{"полосатый манул"}}

	res := r.run(t, &MCP{Facts: small})
	checkAll(t, res, Pass)
	if c := find(t, res, "вызовы через MCP: журнал A против счётчика сервера", laneMCP); strings.Contains(c.Got, "до убийства") {
		t.Fatalf("сверка без убийства: %+v", c)
	}
	for _, c := range res.Checks {
		if strings.Contains(c.What, "убит") {
			t.Fatalf("проверка убийства без убийства: %+v", c)
		}
	}
	if metric(res, "подъём после убийства", laneMCP) != "—" {
		t.Fatalf("подъём без убийства: %q", metric(res, "подъём после убийства", laneMCP))
	}

	// Выдумку привратник отвергает без инструментов: убивать некого.
	res = r.run(t, &MCP{Facts: small, KillAfter: 1})
	c := find(t, res, "ход с убитым сервером завершён", laneMCP)
	if c.Status != Pending || !strings.Contains(c.Note, "некого") {
		t.Fatalf("убийство без вызова: %+v", c)
	}
	if c := find(t, res, "вызовы через MCP: журнал A против счётчика сервера", laneMCP); c.Status != Pass {
		t.Fatalf("сверка при живом сервере: %+v", c)
	}

	res = r.run(t, &MCP{Facts: small, KillAfter: 5})
	if c := find(t, res, "следующий ход — через перезапущенный сервер", laneMCP); c.Status != Pending || !strings.Contains(c.Note, "меньше") {
		t.Fatalf("убийство за пределами сценария: %+v", c)
	}
}

// Сервер не поднялся: ход A идёт в процессе, это брак, доля через MCP —
// ноль, сверять со счётчиком нечего.
func TestMCPTrialServerDown(t *testing.T) {
	r := newRig(t)
	r.brain.GateNo = []string{"шурундук"}
	open := r.open
	r.env.Open = func(dir string, o Options) (Build, error) {
		b, err := open(dir, o)
		r.servers[len(r.servers)-1].fail = true
		return b, err
	}
	res := r.run(t, &MCP{Facts: &Facts{Real: []string{"рысь"}}})
	if res.Verdict() != Fail {
		t.Fatalf("итог при упавшем сервере: %s", res.Verdict())
	}
	if c := find(t, res, "ходов с Effective ≠ Requested", "все"); c.Status != Fail || !strings.Contains(c.Note, laneMCP) {
		t.Fatalf("брак: %+v", c)
	}
	if c := find(t, res, "доля вызовов источников через MCP", laneMCP); c.Status != Fail {
		t.Fatalf("доля через MCP: %+v", c)
	}
	if c := find(t, res, "вызовы через MCP: журнал A против счётчика сервера", laneMCP); c.Status != Pending {
		t.Fatalf("сверка без сервера: %+v", c)
	}
	if metric(res, "холодный старт сервера", laneMCP) != "—" {
		t.Fatal("холодный старт без сервера")
	}
}

func TestMCPHelpers(t *testing.T) {
	c := card.Card{TaxonKey: 5, Tree: []card.Node{{Rank: "KINGDOM", Key: 1}, {Rank: "GENUS", Key: 4}, {Rank: "SPECIES", Key: 5}}}
	if n, ok := treeNode(c); !ok || n.Key != 4 {
		t.Fatalf("узел без семейства: %+v", n)
	}
	if _, ok := treeNode(card.Card{}); ok {
		t.Fatal("узел пустого дерева")
	}
	if median(nil) != 0 || median([]float64{3, 1}) != 2 || median([]float64{5, 1, 3}) != 3 {
		t.Fatal("median")
	}
	if ratio(1, 0) != "—" || ratio(3, 2) != "1.50" || pct(1, 0) != "—" || abs(-2) != 2 || abs64(-1.5) != 1.5 {
		t.Fatal("ratio, pct, abs")
	}
	var evs []agent.Event
	for _, s := range []string{"a", "b", "c", "d", "a"} {
		evs = append(evs, agent.Event{Kind: agent.EventToolError, Tool: "search_wikipedia", Detail: s})
	}
	if got := errorTexts([]history.Turn{{Events: evs}}); !strings.HasSuffix(got, "и ещё 1") {
		t.Fatalf("тексты ошибок: %q", got)
	}
	if snapWord(serverSnap{}) != "не снят" || !strings.HasPrefix(snapWord(serverSnap{err: "x"}), "не ответил") {
		t.Fatal("snapWord")
	}
}
