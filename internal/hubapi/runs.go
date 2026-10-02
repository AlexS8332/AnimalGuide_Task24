package hubapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
)

// # Прогоны флоу
//
//	POST /api/hub/flows {"preset":"passport","species":"манул"}  → 202 {"id":"f1"}
//	GET  /api/hub/flows/{id}  → 200 FlowView: вызовы по мере хода, итог (Trace), когда готово
//	GET  /api/hub/flows       → 200 {"flows":[FlowRow…]} — последние 20, новые первыми
//
// Прогон длится минуты (десятки ходов модели), поэтому POST только
// запускает его в фоне. Прогоны живут в памяти приложения.

const (
	// flowTimeout — предел одного прогона: 13–20 вызовов и столько же
	// ходов модели.
	flowTimeout = 5 * time.Minute
	// keepRuns — сколько прогонов помнить и отдавать списком.
	keepRuns = 20
	// speciesMax — предел строки вида в символах.
	speciesMax = 200
)

// Состояния прогона.
const (
	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
)

// CallView — вызов в ответе: Pending — вызов начат, но ещё не завершён
// (onCall пришёл один раз из двух).
type CallView struct {
	flow.Call
	Pending bool `json:"pending,omitempty"`
}

// FlowView — ответ GET /api/hub/flows/{id}. Trace — только когда прогон
// кончился (state done или failed); Took у идущего — время с начала.
type FlowView struct {
	ID      string        `json:"id"`
	State   string        `json:"state"`
	Preset  string        `json:"preset"`
	Title   string        `json:"title,omitempty"`
	Species string        `json:"species"`
	Calls   []CallView    `json:"calls"`
	Trace   *flow.Trace   `json:"trace,omitempty"`
	Error   string        `json:"error,omitempty"`
	Started time.Time     `json:"started"`
	Took    time.Duration `json:"took"`
}

// FlowRow — строка списка прогонов.
type FlowRow struct {
	ID      string        `json:"id"`
	Preset  string        `json:"preset"`
	Title   string        `json:"title,omitempty"`
	Species string        `json:"species"`
	State   string        `json:"state"`
	OK      bool          `json:"ok"`
	Calls   int           `json:"calls"`
	CostUSD float64       `json:"costUsd"`
	Took    time.Duration `json:"took"`
	Started time.Time     `json:"started"`
	Error   string        `json:"error,omitempty"`
}

// flowRun — один прогон: вызовы, которые onCall дописывает по ходу, и итог.
type flowRun struct {
	id      string
	preset  flow.Preset
	species string
	state   string
	calls   []CallView
	trace   *flow.Trace
	err     string
	started time.Time
	took    time.Duration
}

type startBody struct {
	Preset  string `json:"preset"`
	Species string `json:"species"`
}

func (a *API) start(w http.ResponseWriter, r *http.Request) {
	var in startBody
	if err := readBody(r, &in); err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if a.Router == nil || a.Run == nil {
		unavailable(w, a.why(), "")
		return
	}
	p, ok := a.findPreset(strings.TrimSpace(in.Preset))
	if !ok {
		ids := []string{}
		for _, x := range a.presets() {
			ids = append(ids, x.ID)
		}
		server.WriteError(w, http.StatusBadRequest, "нет заготовки «"+in.Preset+"» — есть: "+strings.Join(ids, ", "))
		return
	}
	species := strings.TrimSpace(in.Species)
	if species == "" {
		species = p.Species
	}
	if species == "" {
		server.WriteError(w, http.StatusBadRequest, "species — вид (русское или латинское название)")
		return
	}
	if utf8.RuneCountInString(species) > speciesMax {
		server.WriteError(w, http.StatusBadRequest, "species длиннее "+itoa(speciesMax)+" символов")
		return
	}
	run, busy := a.add(p, species)
	if run == nil {
		server.WriteJSON(w, http.StatusConflict, map[string]string{"id": busy,
			"error": "флоу уже идёт (прогон " + busy + ") — дождитесь его конца: одновременно идёт один прогон"})
		return
	}
	// Прогон переживает запрос POST: окно уже получило id и опрашивает
	// состояние, а обрывать флоу из-за закрытой вкладки незачем.
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = flowTimeout
	}
	bg, stop := context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
	fn := a.Run
	go func() {
		defer stop()
		tr, err := fn(bg, p, species, func(c flow.Call) { a.call(run.id, c) })
		a.finish(run.id, tr, err)
	}()
	server.WriteJSON(w, http.StatusAccepted, map[string]string{"id": run.id})
}

// ------------------------------------------------------------ состояние

func (a *API) busyLocked() string {
	for _, r := range a.runs {
		if r.state == StateRunning {
			return r.id
		}
	}
	return ""
}

// add заводит прогон, если ни один не идёт; иначе nil и id идущего.
func (a *API) add(p flow.Preset, species string) (*flowRun, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b := a.busyLocked(); b != "" {
		return nil, b
	}
	a.seq++
	run := &flowRun{id: "f" + itoa(a.seq), preset: p, species: species, state: StateRunning, started: time.Now()}
	a.runs = append(a.runs, run)
	if len(a.runs) > keepRuns {
		a.runs = append([]*flowRun(nil), a.runs[len(a.runs)-keepRuns:]...)
	}
	return run, ""
}

func (a *API) find(id string) *flowRun {
	for _, r := range a.runs {
		if r.id == id {
			return r
		}
	}
	return nil
}

// call — onCall прогона: первый приход вызова с номером N — старт
// (Pending), второй — завершение.
func (a *API) call(id string, c flow.Call) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.find(id)
	if r == nil || r.state != StateRunning {
		return
	}
	putCall(&r.calls, CallView{Call: c, Pending: true})
}

// putCall — вызов встаёт на место своего номера; новый номер — начатый
// вызов (Pending как передан), известный — завершённый.
func putCall(calls *[]CallView, c CallView) {
	c.Args, c.Result = validJSON(c.Args), validJSON(c.Result)
	for i := range *calls {
		if c.N > 0 && (*calls)[i].N == c.N {
			c.Pending = false
			(*calls)[i] = c
			return
		}
	}
	*calls = append(*calls, c)
	sort.SliceStable(*calls, func(i, j int) bool { return (*calls)[i].N < (*calls)[j].N })
}

// finish — прогон кончился. Вызовы итога (с From от Verify) заменяют
// собранные по onCall; вызов, прерванный на ходу, — ошибка, а не вечное
// «идёт».
func (a *API) finish(id string, tr flow.Trace, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.find(id)
	if r == nil {
		return
	}
	if tr.Preset == "" {
		tr.Preset = r.preset.ID
	}
	if tr.Species == "" {
		tr.Species = r.species
	}
	if tr.Started.IsZero() {
		tr.Started = r.started
	}
	if tr.Took == 0 {
		tr.Took = time.Since(r.started)
	}
	// Вызовы итога заменяют свои номера; начатые, но не попавшие в итог
	// (прогон оборвался на них), остаются.
	tr.Calls = append([]flow.Call(nil), tr.Calls...)
	for i, c := range tr.Calls {
		tr.Calls[i].Args, tr.Calls[i].Result = validJSON(c.Args), validJSON(c.Result)
		putCall(&r.calls, CallView{Call: tr.Calls[i]})
	}
	r.state = StateDone
	if err != nil {
		r.state = StateFailed
		tr.OK = false
		if tr.Error == "" {
			tr.Error = err.Error()
		}
	}
	for i := range r.calls {
		if r.calls[i].Pending {
			r.calls[i].Pending = false
			if r.calls[i].Error == "" {
				r.calls[i].Error = "прервано: " + firstNonEmpty(tr.Error, "прогон кончился раньше вызова")
			}
		}
	}
	r.err = tr.Error
	r.took = tr.Took
	r.trace = &tr
}

func (a *API) view(id string) (FlowView, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.find(id)
	if r == nil {
		return FlowView{}, false
	}
	v := FlowView{ID: r.id, State: r.state, Preset: r.preset.ID, Title: r.preset.Title, Species: r.species,
		Calls: append([]CallView{}, r.calls...), Error: r.err, Started: r.started, Took: r.took}
	if r.state == StateRunning {
		v.Took = time.Since(r.started)
	}
	if r.trace != nil {
		tr := *r.trace
		v.Trace = &tr
	}
	return v, true
}

func (a *API) list() []FlowRow {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []FlowRow{}
	for i := len(a.runs) - 1; i >= 0 && len(out) < keepRuns; i-- {
		r := a.runs[i]
		row := FlowRow{ID: r.id, Preset: r.preset.ID, Title: r.preset.Title, Species: r.species, State: r.state,
			Calls: len(r.calls), Took: r.took, Started: r.started, Error: r.err}
		if r.state == StateRunning {
			row.Took = time.Since(r.started)
		}
		if r.trace != nil {
			row.OK = r.state == StateDone && r.trace.OK
			row.CostUSD = r.trace.CostUSD
		}
		out = append(out, row)
	}
	return out
}

// validJSON — аргументы модели бывают битым JSON (обрезанный ответ,
// лишняя кавычка). json.RawMessage с таким текстом ломает кодирование всего
// ответа — окно получило бы пустое тело. Битый текст уходит строкой JSON.
func validJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || json.Valid(raw) {
		return raw
	}
	b, _ := json.Marshal(string(raw))
	return b
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

func itoa(n int) string { return strconv.Itoa(n) }
