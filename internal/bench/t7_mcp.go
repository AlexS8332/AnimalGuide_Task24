package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// MCPName — имя механизма MCP в реестре.
const MCPName = features.MCP

// Дорожки И-7. Основная — B: с ней стенд сверяет остальные, и повтор B′
// обязан совпадать с ней механизмами.
const (
	laneLocal  = "B: в процессе"
	laneMCP    = "A: через MCP"
	laneRepeat = "B′: повтор B"
)

// unavailableText — с чего начинается ошибка, которую модель получает,
// когда сервер упал посреди вызова.
const unavailableText = "MCP-сервер недоступен"

// MCP — И-7: тот же набор инструментов источников через MCP-сервер.
// Дорожки: A — умолчания + mcp, B — умолчания, B′ — повтор B (Lane.Same)
// для замера шума модели. Сценарий — запросы И-1, три раздела первой
// карточки, узел её дерева и сравнение; на дорожке A сервер убивается
// посреди хода.
//
// Жёсткие проверки:
//
//   - описания инструментов в каждом запросе к модели, A против B —
//     совпадают (отпечаток Fingerprint в событии «промпт»);
//   - постоянная часть запроса, A − B — 0 токенов;
//   - доля вызовов источников через MCP (Event.Via) — A 100 %, B 0 %, и
//     журнал A сходится со счётчиком самого сервера (server_info);
//   - пороги И-1 — на обеих дорожках;
//   - ходов с Effective ≠ Requested — 0;
//   - сервер убит посреди хода — ход завершён, модель получила ошибку
//     «MCP-сервер недоступен», следующий ход идёт через перезапущенный
//     сервер.
//
// Отчётные числа: отношение времени A/B, доля кэша, холодный старт,
// накладные на вызов, число HTTP-запросов к источникам, тексты ошибок,
// совпадение последовательностей вызовов A/B против B/B′.
type MCP struct {
	// Facts — запросы И-1 и раздел, который читается у каждой карточки;
	// nil — сценарий И-1 целиком.
	Facts *Facts
	// Sections — разделы первой карточки, которые читаются после запросов.
	Sections []string
	// Compare — пара для сравнения; пусто — без сравнения.
	Compare [2]string
	// KillAfter — после скольких запросов И-1 сервер убивается: в
	// следующем запросе, на первом вызове источника дорожки A. 0 — не
	// убивать.
	KillAfter int
}

// NewMCP — сценарий ТЗ: 15 запросов И-1, три раздела, узел дерева,
// сравнение; сервер убивается в шестом запросе.
func NewMCP() *MCP {
	return &MCP{Facts: NewFacts(), Sections: []string{"habitat", "breeding", "status"},
		Compare: [2]string{"рысь", "манул"}, KillAfter: 5}
}

func (*MCP) ID() string    { return "И-7" }
func (*MCP) Title() string { return "MCP: тот же путь до источников" }

// mcpLanes — B, A и B′.
func mcpLanes(base features.Set) []Lane {
	local := base.With(MCPName, false)
	return []Lane{
		{Name: laneLocal, Note: "умолчания приложения: инструменты источников в процессе", Features: local},
		{Name: laneMCP, Note: "те же инструменты через MCP-сервер, отдельный процесс; сервер убивается посреди хода",
			Features: local.With(MCPName, true)},
		{Name: laneRepeat, Note: "B ещё раз: шум модели при тех же механизмах", Features: local, Same: true},
	}
}

func (m *MCP) Run(ctx context.Context, s *Stand, r *Result) error {
	r.Goal = "тот же набор инструментов через MCP-сервер: ответы, цена и журнал совпадают с локальным путём, падение сервера модель переживает"
	if _, ok := s.env.Registry.Get(MCPName); !ok {
		r.Skipped = "механизма «mcp» нет в реестре"
		return nil
	}
	cl := s.MCP()
	if cl == nil {
		r.Skipped = "сборка стенда не даёт клиента MCP"
		return nil
	}
	f := m.Facts
	if f == nil {
		f = NewFacts()
	}
	lanes := mcpLanes(s.env.Base)
	r.describeLanes(s.env.Registry, lanes)
	run := &mcpRun{m: m, f: f, s: s, r: r, cl: cl, log: &laneLog{}, tally: map[string]*laneTally{}, since: map[int]time.Time{}}
	for _, l := range lanes {
		run.tally[l.Name] = &laneTally{}
	}
	run.httpStart = s.HTTP()
	if err := run.scenario(ctx, lanes); err != nil {
		return err
	}
	run.snapshot(ctx, "в конце")
	run.judge()
	return nil
}

// mcpRun — один прогон И-7: ходы дорожек по ключам, снимки счётчиков
// сервера и что случилось при убийстве.
type mcpRun struct {
	m     *MCP
	f     *Facts
	s     *Stand
	r     *Result
	cl    MCPClient
	log   *laneLog
	tally map[string]*laneTally
	kill  killer
	// killedKey — ключ хода A, в котором сервер убивали.
	killedKey string
	snaps     []serverSnap
	// since — когда началось подключение с номером N (Conn.Since).
	since     map[int]time.Time
	httpStart int64
}

// scenario — запросы И-1, разделы, узел дерева и сравнение.
func (x *mcpRun) scenario(ctx context.Context, lanes []Lane) error {
	var first *Group
	var firstCard *card.Card
	for i, q := range x.f.queries() {
		g, err := x.s.Group("И-7: "+q.name, lanes)
		if err != nil {
			return err
		}
		x.r.Mechanism = g.Mechanism
		req := agents.Request{Kind: agents.KindOpen, Name: q.name, Text: q.name}
		killing := x.m.KillAfter > 0 && i == x.m.KillAfter
		var steps []Step
		if killing {
			x.kill.cl = x.cl
			steps, err = g.SendWatch(ctx, req, x.kill.watch)
			x.kill.wait()
			x.killedKey = "open:" + q.name
		} else {
			steps, err = g.Send(ctx, req)
		}
		if err != nil {
			return err
		}
		if killing {
			x.snapshot(ctx, "после убитого хода")
		}
		for j, st := range steps {
			x.log.add(st.Lane, "open:"+q.name, st.Turn)
			c, sec, err := x.f.score(ctx, x.r, g.Dialogs[j], q, st, x.tally[st.Lane])
			if err != nil {
				return err
			}
			if sec != nil {
				x.log.add(st.Lane, "section:"+q.name, sec.Turn)
			}
			if c != nil && first == nil && st.Lane == laneMCP {
				first, firstCard = g, c
			}
			if killing && st.Lane == laneMCP {
				x.r.sample("сервер убит посреди хода: "+q.name, st, "Kill на вызове "+orDash(x.kill.tool))
			}
		}
		if x.m.KillAfter > 0 && i == x.m.KillAfter-1 {
			x.snapshot(ctx, "перед убийством")
		}
	}
	send := func(g *Group, key string, req agents.Request) error {
		steps, err := g.Send(ctx, req)
		if err != nil {
			return err
		}
		for _, st := range steps {
			x.log.add(st.Lane, key, st.Turn)
		}
		return nil
	}
	if first != nil {
		for _, topic := range x.m.Sections {
			if err := send(first, "extra:section:"+topic, agents.Request{Kind: agents.KindSection, CardID: firstCard.ID, Topic: topic}); err != nil {
				return err
			}
		}
		if n, ok := treeNode(*firstCard); ok {
			if err := send(first, "extra:node", agents.Request{Kind: agents.KindNode, CardID: firstCard.ID, NodeKey: n.Key, NodeName: n.Name}); err != nil {
				return err
			}
		}
	} else if len(x.m.Sections) > 0 {
		x.r.note("на дорожке A не собралось ни одной карточки: разделы и узел дерева не читались")
	}
	if x.m.Compare[0] != "" && x.m.Compare[1] != "" {
		g, err := x.s.Group("И-7: сравнение", lanes)
		if err != nil {
			return err
		}
		if err := send(g, "compare", agents.Request{Kind: agents.KindCompare, A: x.m.Compare[0], B: x.m.Compare[1]}); err != nil {
			return err
		}
	}
	return nil
}

// treeNode — узел дерева карточки для клика: семейство, иначе ближайший
// к виду узел выше него.
func treeNode(c card.Card) (card.Node, bool) {
	for _, n := range c.Tree {
		if strings.EqualFold(n.Rank, "FAMILY") && n.Key > 0 {
			return n, true
		}
	}
	for i := len(c.Tree) - 1; i >= 0; i-- {
		if n := c.Tree[i]; n.Key > 0 && n.Key != c.TaxonKey {
			return n, true
		}
	}
	return card.Node{}, false
}

// killer убивает сервер на первом вызове источника через MCP в ходе
// дорожки A: вызов идёт, когда процесс умирает.
type killer struct {
	cl   MCPClient
	done chan struct{}
	// Заполняются горутиной до закрытия done.
	fired bool
	tool  string
	err   error
}

func (k *killer) watch(lane string, sess *runs.Session) {
	if lane != laneMCP || k.done != nil {
		return
	}
	k.done = make(chan struct{})
	go func() {
		defer close(k.done)
		snap, ch, stop := sess.Subscribe()
		defer stop()
		for _, ev := range snap.Events {
			if k.try(ev) {
				return
			}
		}
		for msg := range ch {
			if msg.Event != "log" {
				continue
			}
			var ev agent.Event
			if json.Unmarshal([]byte(msg.Data), &ev) == nil && k.try(ev) {
				return
			}
		}
	}()
}

func (k *killer) try(ev agent.Event) bool {
	if !mcpSourceCall(ev) {
		return false
	}
	k.fired, k.tool = true, ev.Tool
	k.err = k.cl.Kill()
	return true
}

func (k *killer) wait() {
	if k.done != nil {
		<-k.done
	}
}

// sourceCall — вызов инструмента источника моделью (не завершающий).
func sourceCall(ev agent.Event) bool {
	return ev.Kind == agent.EventToolCall && !ev.Final && tools.IsSourceTool(ev.Tool)
}

func mcpSourceCall(ev agent.Event) bool { return sourceCall(ev) && ev.Via == tools.ViaMCP }

// serverSnap — счётчики сервера в какой-то момент прогона.
type serverSnap struct {
	when string
	// n — номер подключения клиента (1 — первый процесс, дальше
	// перезапуски); 0 — клиент не подключался.
	n int
	// info — ответ server_info; nil — сервер не отвечает (убит и ещё не
	// поднят).
	info *mcp.Info
	err  string
}

// snapshot — снять счётчики клиента и сервера. Процесс ради этого не
// поднимается: ServerInfo ходит только к живому серверу.
func (x *mcpRun) snapshot(ctx context.Context, when string) {
	st := x.cl.State()
	sn := serverSnap{when: when}
	if st.Conn != nil {
		sn.n = st.Conn.N
		x.since[st.Conn.N] = st.Conn.Since
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := x.cl.ServerInfo(c)
	if err != nil {
		sn.err = err.Error()
	} else {
		sn.info = info
	}
	x.snaps = append(x.snaps, sn)
}

func (x *mcpRun) snap(when string) (serverSnap, bool) {
	for _, s := range x.snaps {
		if s.when == when {
			return s, true
		}
	}
	return serverSnap{}, false
}

// judge — проверки и отчётные числа по записанным ходам.
func (x *mcpRun) judge() {
	r := x.r
	x.checkPrompts()
	x.checkShare()
	x.checkServerCount()
	x.f.report(r, laneMCP, x.tally[laneMCP], true)
	x.f.report(r, laneLocal, x.tally[laneLocal], true)
	x.f.report(r, laneRepeat, x.tally[laneRepeat], false)

	brakN := 0
	var brakEx []string
	for _, lane := range []string{laneMCP, laneLocal, laneRepeat} {
		for _, t := range x.log.list(lane) {
			if brak(x.s.env.Registry, t) {
				brakN++
				brakEx = append(brakEx, lane+": "+clip(t.User, 40))
			}
		}
	}
	r.zero("ходов с Effective ≠ Requested", "все", brakN, brakEx)
	x.checkKill()
	x.metrics()
}

// checkPrompts — описания инструментов и постоянная часть запроса, A
// против B, ход за ходом.
func (x *mcpRun) checkPrompts() {
	r := x.r
	compared, differ := 0, 0
	var differEx []string
	turns, worst := 0, 0
	worstEx := ""
	for _, p := range x.log.pairs(laneMCP, laneLocal) {
		a, b := fingerprints(p.a), fingerprints(p.b)
		for ag, fa := range a {
			fb, ok := b[ag]
			if !ok {
				continue
			}
			compared++
			if strings.Join(fa, ",") != strings.Join(fb, ",") {
				differ++
				differEx = append(differEx, fmt.Sprintf("%s, %s: %v против %v", p.key, ag, fa, fb))
			}
		}
		// Убитый ход в сравнение цены не входит: путь модели в нём
		// изменило падение сервера, а не механизм.
		ea, eb := p.a.Context.Estimate, p.b.Context.Estimate
		if ea.Total == 0 || eb.Total == 0 || p.key == x.killedKey {
			continue
		}
		turns++
		if d := abs(ea.Constant - eb.Constant); d > worst {
			worst = d
			worstEx = fmt.Sprintf("%s: A %d, B %d", p.key, ea.Constant, eb.Constant)
		}
	}
	sort.Strings(differEx)
	c := Check{What: "описания инструментов в запросах к модели, A против B", Want: "совпадают побайтно",
		Got: fmt.Sprintf("сравнений %d, расхождений %d", compared, differ), Status: statusIf(differ == 0),
		Note: strings.Join(firstN(differEx, 3), "; ")}
	if compared == 0 {
		c.Status, c.Note = Pending, "в журналах нет пар запросов с отпечатком"
	}
	r.check(c)
	c = Check{What: "постоянная часть запроса, A − B", Want: "0 токенов",
		Got: fmt.Sprintf("наибольшая разница %d по %d парам ходов", worst, turns), Status: statusIf(worst == 0), Note: worstEx}
	if turns == 0 {
		c.Status, c.Note = Pending, "нет пар ходов с оценкой запроса"
	}
	r.check(c)
}

// fingerprints — отпечатки описаний инструментов в запросах хода по
// агентам, без повторов.
func fingerprints(t history.Turn) map[string][]string {
	set := map[string]map[string]bool{}
	for _, p := range promptsOf(t) {
		if p.Fingerprint == "" {
			continue
		}
		if set[p.Agent] == nil {
			set[p.Agent] = map[string]bool{}
		}
		set[p.Agent][p.Fingerprint] = true
	}
	out := map[string][]string{}
	for ag, fs := range set {
		for f := range fs {
			out[ag] = append(out[ag], f)
		}
		sort.Strings(out[ag])
	}
	return out
}

// calls — вызовы источников дорожки и сколько из них шли через MCP.
func calls(turns []history.Turn) (total, viaMCP int) {
	for _, t := range turns {
		for _, e := range t.Events {
			if sourceCall(e) {
				total++
				if e.Via == tools.ViaMCP {
					viaMCP++
				}
			}
		}
	}
	return total, viaMCP
}

func (x *mcpRun) checkShare() {
	for _, lane := range []string{laneMCP, laneLocal, laneRepeat} {
		total, via := calls(x.log.list(lane))
		c := Check{What: "доля вызовов источников через MCP", Lane: lane, Got: fmt.Sprintf("%s (%d из %d)", pct(via, total), via, total)}
		if lane == laneMCP {
			c.Want, c.Status = "100 %", statusIf(total > 0 && via == total)
		} else {
			c.Want, c.Status = "0 %", statusIf(via == 0)
		}
		x.r.check(c)
	}
}

// checkServerCount сверяет вызовы через MCP по журналу A со счётчиком
// самого сервера. Счётчик живёт вместе с процессом, поэтому сверка идёт
// отрезками: до убийства — первый процесс, после — перезапущенный. Убитый
// ход в сверку не входит: его вызовы до падения счётчик унёс с собой.
func (x *mcpRun) checkServerCount() {
	turns := x.log.list(laneMCP)
	keys := x.log.keys[laneMCP]
	cut := -1
	for i, k := range keys {
		if k == x.killedKey {
			cut = i
		}
	}
	count := func(from, to int) int {
		_, via := calls(turns[from:to])
		return via
	}
	end, _ := x.snap("в конце")
	c := Check{What: "вызовы через MCP: журнал A против счётчика сервера", Lane: laneMCP, Want: "совпадают"}
	var parts []string
	ok, known := true, true
	if x.killedKey == "" || cut < 0 {
		j := count(0, len(turns))
		if end.info == nil || end.n != 1 {
			known = false
			parts = append(parts, fmt.Sprintf("журнал %d, сервер %s", j, snapWord(end)))
		} else {
			ok = j == end.info.TotalCalls
			parts = append(parts, fmt.Sprintf("журнал %d, сервер %d", j, end.info.TotalCalls))
		}
	} else {
		pre, _ := x.snap("перед убийством")
		mid, _ := x.snap("после убитого хода")
		j := count(0, cut)
		if pre.info == nil || pre.n != 1 {
			known = false
			parts = append(parts, fmt.Sprintf("до убийства: журнал %d, сервер %s", j, snapWord(pre)))
		} else {
			ok = ok && j == pre.info.TotalCalls
			parts = append(parts, fmt.Sprintf("до убийства: журнал %d, сервер %d", j, pre.info.TotalCalls))
		}
		j = count(cut+1, len(turns))
		server := -1
		switch {
		case end.info == nil:
		case mid.info != nil && mid.n == end.n:
			server = end.info.TotalCalls - mid.info.TotalCalls
		case mid.info == nil && end.n == mid.n+1:
			server = end.info.TotalCalls
		}
		if server < 0 {
			known = false
			parts = append(parts, fmt.Sprintf("после перезапуска: журнал %d, сервер не сверить (%s)", j, snapWord(end)))
		} else {
			ok = ok && j == server
			parts = append(parts, fmt.Sprintf("после перезапуска: журнал %d, сервер %d", j, server))
		}
		c.Note = "убитый ход не сверяется: счётчик его вызовов умер с процессом"
	}
	c.Got = strings.Join(parts, "; ")
	c.Status = statusIf(ok)
	if !known && ok {
		c.Status = Pending
	}
	x.r.check(c)
}

func snapWord(s serverSnap) string {
	if s.info == nil {
		if s.err != "" {
			return "не ответил: " + clip(s.err, 80)
		}
		return "не снят"
	}
	return fmt.Sprintf("%d (подключение №%d)", s.info.TotalCalls, s.n)
}

// checkKill — ход, в котором убили сервер, и ход за ним.
func (x *mcpRun) checkKill() {
	if x.m.KillAfter <= 0 {
		return
	}
	r := x.r
	const (
		whatDone    = "ход с убитым сервером завершён"
		whatError   = "модель получила {\"error\"} «" + unavailableText + "»"
		whatRestart = "следующий ход — через перезапущенный сервер"
	)
	if x.killedKey == "" {
		why := fmt.Sprintf("в сценарии меньше %d запросов — убивать не в чем", x.m.KillAfter+1)
		r.pending(whatDone, "да", laneMCP, why)
		r.pending(whatError, "да", laneMCP, why)
		r.pending(whatRestart, "да", laneMCP, why)
		return
	}
	if !x.kill.fired {
		why := "в ходе не было вызова источника через MCP — убивать было некого"
		r.pending(whatDone, "да", laneMCP, why)
		r.pending(whatError, "да", laneMCP, why)
		r.pending(whatRestart, "да", laneMCP, why)
		return
	}
	if x.kill.err != nil {
		r.note("Kill: %v", x.kill.err)
	}
	keys, turns := x.log.keys[laneMCP], x.log.list(laneMCP)
	idx := -1
	for i, k := range keys {
		if k == x.killedKey {
			idx = i
		}
	}
	killed := turns[idx]
	r.yes(whatDone, laneMCP, killed.Status == history.TurnDone, clip(killed.Error, 200))

	var errs []string
	got := false
	for _, e := range killed.Events {
		if e.Kind == agent.EventToolError && e.Via == tools.ViaMCP {
			errs = append(errs, e.Tool+": "+clip(e.Detail, 160))
			if strings.Contains(e.Detail, unavailableText) {
				got = true
			}
		}
	}
	note := strings.Join(firstN(errs, 2), "; ")
	if !got && note == "" {
		note = "вызов " + x.kill.tool + " успел закончиться до падения сервера"
	}
	r.yes(whatError, laneMCP, got, note)

	mid, _ := x.snap("после убитого хода")
	if idx+1 >= len(turns) {
		r.pending(whatRestart, "да", laneMCP, "за убитым ходом других ходов не было")
		return
	}
	next := turns[idx+1]
	title := ""
	for _, e := range next.Events {
		if e.Kind == mcp.EventConnect {
			title = e.Title
		}
	}
	restarted := strings.Contains(title, "перезапуск №")
	switch {
	case restarted:
		note = "mcp.connect: " + title
	case mid.info != nil && mid.n > 1:
		// Модель повторила вызов в том же ходе — сервер поднят тем же
		// ходом, следующий идёт через уже живое соединение.
		restarted = true
		note = fmt.Sprintf("сервер поднят ещё в убитом ходе (подключение №%d); mcp.connect следующего хода: %s", mid.n, orDash(title))
	default:
		note = "mcp.connect следующего хода: " + orDash(title)
	}
	r.yes(whatRestart, laneMCP, restarted && next.Status == history.TurnDone && next.Effective.On(MCPName), note)
}

// metrics — отчётные числа.
func (x *mcpRun) metrics() {
	r := x.r
	reg := x.s.env.Registry
	lanes := []string{laneMCP, laneLocal, laneRepeat}
	secs := map[string]float64{}
	share := map[string]float64{}
	for _, lane := range lanes {
		turns := x.log.list(lane)
		for _, t := range turns {
			secs[lane] += t.Totals.Seconds
		}
		share[lane] = statsOf(reg, lane, turns).CacheShare()
		total, _ := calls(turns)
		r.metric("вызовов источников", lane, "%d", total)
		r.metric("время ходов", lane, "%.0f с", secs[lane])
		r.metric("доля кэша", lane, "%.0f %%", share[lane]*100)
	}
	r.metric("отношение времени к B", laneMCP, "%s", ratio(secs[laneMCP], secs[laneLocal]))
	r.metric("отношение времени к B", laneRepeat, "%s", ratio(secs[laneRepeat], secs[laneLocal]))
	r.metric("|A − B| доли кэша", laneMCP, "%.1f п.п.", abs64(share[laneMCP]-share[laneLocal])*100)
	r.metric("|A − B| доли кэша", laneRepeat, "%.1f п.п. (B′ − B, шум)", abs64(share[laneRepeat]-share[laneLocal])*100)

	cold, restart := x.startTimes()
	r.metric("холодный старт сервера", laneMCP, "%s", cold)
	r.metric("подъём после убийства", laneMCP, "%s", restart)

	// Время вызова: по журналу — у всех дорожек одинаково, по счётчикам
	// клиента — только MCP, средним по каждому инструменту.
	p50 := map[string]float64{}
	for _, lane := range lanes {
		ms := callMillis(x.log.list(lane))
		p50[lane] = median(ms)
		r.metric("время вызова источника, p50", lane, "%.0f мс (вызовов %d)", p50[lane], len(ms))
	}
	r.metric("накладные MCP на вызов, p50", laneMCP, "%+.0f мс (A − B по журналу)", p50[laneMCP]-p50[laneLocal])
	var avg []float64
	for _, t := range x.cl.State().Tools {
		if t.Calls > 0 {
			avg = append(avg, float64(t.Millis)/float64(t.Calls))
		}
	}
	if len(avg) > 0 {
		r.metric("время вызова по счётчикам клиента MCP, p50 по инструментам", laneMCP, "%.0f мс", median(avg))
	}

	if n, ok := x.serverHTTP(); ok {
		r.metric("HTTP-запросов к источникам", laneMCP, "≥ %d (процессы сервера, server_info; второй кэш)", n)
	}
	if x.httpStart >= 0 {
		r.metric("HTTP-запросов к источникам", laneLocal, "%d (процесс приложения, B и B′ вместе — кэш общий)", x.s.HTTP()-x.httpStart)
	}

	for _, lane := range lanes {
		r.metric("тексты ошибок инструментов", lane, "%s", errorTexts(x.log.list(lane)))
	}
	ab, n := x.log.sameCalls(laneMCP, laneLocal)
	bb, m := x.log.sameCalls(laneRepeat, laneLocal)
	r.metric("совпадение наборов вызовов с B, ходов", laneMCP, "%d из %d", ab, n)
	r.metric("совпадение наборов вызовов с B, ходов", laneRepeat, "%d из %d (шум модели)", bb, m)
	if st := x.cl.State(); st.Conn != nil {
		r.metric("подключений к серверу за испытание", laneMCP, "%d", st.Conn.N)
	}
}

// startTimes — холодный старт и подъём после убийства: от начала
// подключения (Conn.Since) до события mcp.connect в журнале хода.
func (x *mcpRun) startTimes() (cold, restart string) {
	cold, restart = "—", "—"
	for _, t := range x.log.list(laneMCP) {
		for _, e := range t.Events {
			if e.Kind != mcp.EventConnect {
				continue
			}
			var d struct {
				N     int  `json:"n"`
				Fresh bool `json:"fresh"`
			}
			if data, err := json.Marshal(e.Data); err == nil {
				json.Unmarshal(data, &d)
			}
			since, ok := x.since[d.N]
			if !d.Fresh || !ok || e.Time.IsZero() {
				continue
			}
			v := fmt.Sprintf("%.2f с", e.Time.Sub(since).Seconds())
			if d.N == 1 {
				cold = v
			} else if restart == "—" {
				restart = v + fmt.Sprintf(" (подключение №%d)", d.N)
			}
		}
	}
	return cold, restart
}

// serverHTTP — запросы к источникам из процессов сервера: последний снимок
// каждого процесса. Запросы убитого процесса после последнего снимка
// потеряны, поэтому число — нижняя граница.
func (x *mcpRun) serverHTTP() (int64, bool) {
	last := map[int]int64{}
	for _, s := range x.snaps {
		if s.info != nil {
			last[s.n] = s.info.HTTPRequests
		}
	}
	var sum int64
	for _, v := range last {
		sum += v
	}
	return sum, len(last) > 0
}

// callMillis — время вызовов источников по журналу, в миллисекундах.
func callMillis(turns []history.Turn) []float64 {
	var out []float64
	for _, t := range turns {
		for _, e := range t.Events {
			if e.Kind == agent.EventToolResult && !e.Final && tools.IsSourceTool(e.Tool) {
				out = append(out, e.Seconds*1000)
			}
		}
	}
	return out
}

// errorTexts — тексты ошибок инструментов источников, без повторов.
func errorTexts(turns []history.Turn) string {
	seen := map[string]bool{}
	var out []string
	for _, t := range turns {
		for _, e := range t.Events {
			if e.Kind != agent.EventToolError || e.Final {
				continue
			}
			s := e.Tool + ": " + clip(e.Detail, 120)
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		return "нет"
	}
	text := strings.Join(firstN(out, 3), "; ")
	if len(out) > 3 {
		text += fmt.Sprintf("; и ещё %d", len(out)-3)
	}
	return text
}

// laneLog — ходы дорожек по ключам сценария: пары ходов разных дорожек
// ищутся по ключу, а не по номеру — дорожка, не собравшая карточку, не
// читает её раздел, и номера разъезжаются.
type laneLog struct {
	mu    sync.Mutex
	keys  map[string][]string
	turns map[string]map[string]history.Turn
}

func (l *laneLog) add(lane, key string, t history.Turn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.keys == nil {
		l.keys, l.turns = map[string][]string{}, map[string]map[string]history.Turn{}
	}
	if l.turns[lane] == nil {
		l.turns[lane] = map[string]history.Turn{}
	}
	if _, ok := l.turns[lane][key]; !ok {
		l.keys[lane] = append(l.keys[lane], key)
	}
	l.turns[lane][key] = t
}

// list — ходы дорожки по порядку.
func (l *laneLog) list(lane string) []history.Turn {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []history.Turn
	for _, k := range l.keys[lane] {
		out = append(out, l.turns[lane][k])
	}
	return out
}

type turnPair struct {
	key  string
	a, b history.Turn
}

// pairs — ходы дорожки a и те же ходы дорожки b.
func (l *laneLog) pairs(a, b string) []turnPair {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []turnPair
	for _, k := range l.keys[a] {
		if tb, ok := l.turns[b][k]; ok {
			out = append(out, turnPair{key: k, a: l.turns[a][k], b: tb})
		}
	}
	return out
}

// sameCalls — в скольких парах ходов набор вызовов источников совпал.
// Специалисты идут параллельно, поэтому сравнивается набор, а не порядок.
func (l *laneLog) sameCalls(a, b string) (same, total int) {
	for _, p := range l.pairs(a, b) {
		total++
		if callSet(p.a) == callSet(p.b) {
			same++
		}
	}
	return same, total
}

func callSet(t history.Turn) string {
	var names []string
	for _, e := range t.Events {
		if sourceCall(e) {
			names = append(names, e.Tool)
		}
	}
	sort.Strings(names)
	return strings.Join(names, "\n")
}

func pct(n, total int) string {
	if total == 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f %%", float64(n)/float64(total)*100)
}

func ratio(a, b float64) string {
	if b <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f", a/b)
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func abs64(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
