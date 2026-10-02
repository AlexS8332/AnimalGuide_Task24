package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tokens"
)

// Trial — испытание: сценарий, дорожки, жёсткие проверки и отчётные числа.
// Новое упражнение добавляет сюда реализацию, а не свой отчёт.
type Trial interface {
	// ID — номер по ТЗ: «И-1».
	ID() string
	Title() string
	// Run гоняет сценарий на стенде и заполняет проверки. Ошибка — это
	// поломка стенда (ход не записан, диалог пропал), а не проваленная
	// проверка: проваленная проверка — результат.
	Run(ctx context.Context, s *Stand, r *Result) error
}

// Status — исход проверки.
type Status string

const (
	Pass Status = "pass"
	Fail Status = "fail"
	// Pending — проверку на этом прогоне не определить: механизм, по
	// следам которого она считается, ещё не подключён или сценарий до неё
	// не дошёл. Не засчитывается ни в пользу, ни против.
	Pending Status = "pending"
)

// Check — жёсткая проверка с порогом из ТЗ.
type Check struct {
	What   string `json:"what"`
	Want   string `json:"want"`
	Lane   string `json:"lane,omitempty"`
	Got    string `json:"got"`
	Status Status `json:"status"`
	Note   string `json:"note,omitempty"`
}

// Metric — отчётное число: без порога, для сравнения дорожек.
type Metric struct {
	What  string `json:"what"`
	Lane  string `json:"lane,omitempty"`
	Value string `json:"value"`
}

// Sample — что получил человек: вопрос и ответ дорожки.
type Sample struct {
	Topic string `json:"topic"`
	Lane  string `json:"lane"`
	User  string `json:"user"`
	Reply string `json:"reply"`
	Note  string `json:"note,omitempty"`
}

// LaneInfo — дорожка в отчёте.
type LaneInfo struct {
	Name     string   `json:"name"`
	Note     string   `json:"note"`
	Diff     string   `json:"diff"`
	Features []string `json:"features"`
}

// Result — итог испытания.
type Result struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Goal — что испытание доказывает, одной фразой.
	Goal string `json:"goal"`
	// Mechanism — механизм, который сравнивали дорожки.
	Mechanism features.Name `json:"mechanism,omitempty"`
	Lanes     []LaneInfo    `json:"lanes"`
	Checks    []Check       `json:"checks"`
	Metrics   []Metric      `json:"metrics"`
	Samples   []Sample      `json:"samples,omitempty"`
	Notes     []string      `json:"notes,omitempty"`
	Stats     []LaneStats   `json:"stats"`
	// Skipped — испытание не гонялось (механизма ещё нет в реестре).
	Skipped string        `json:"skipped,omitempty"`
	Err     string        `json:"error,omitempty"`
	Elapsed time.Duration `json:"elapsed"`
}

// Verdict — итог испытания: провал, если провалена хоть одна проверка;
// «не определено», если есть неопределённые; иначе принято.
func (r *Result) Verdict() Status {
	if r.Err != "" {
		return Fail
	}
	if r.Skipped != "" {
		return Pending
	}
	out := Pass
	for _, c := range r.Checks {
		switch c.Status {
		case Fail:
			return Fail
		case Pending:
			out = Pending
		}
	}
	if len(r.Checks) == 0 {
		return Pending
	}
	return out
}

// Count — сколько проверок с таким исходом.
func (r *Result) Count(s Status) int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == s {
			n++
		}
	}
	return n
}

// check добавляет проверку.
func (r *Result) check(c Check) { r.Checks = append(r.Checks, c) }

// atLeast — проверка «не меньше need из total».
func (r *Result) atLeast(what, lane string, ok, total, need int) {
	st := Pass
	if ok < need {
		st = Fail
	}
	r.check(Check{What: what, Want: fmt.Sprintf("%d из %d", need, total), Lane: lane,
		Got: fmt.Sprintf("%d из %d", ok, total), Status: st})
}

// zero — проверка «ноль случаев».
func (r *Result) zero(what, lane string, n int, examples []string) {
	c := Check{What: what, Want: "0", Lane: lane, Got: fmt.Sprint(n), Status: Pass}
	if n > 0 {
		c.Status = Fail
		c.Note = strings.Join(firstN(examples, 3), "; ")
	}
	r.check(c)
}

// yes — проверка «да/нет».
func (r *Result) yes(what, lane string, ok bool, note string) {
	c := Check{What: what, Want: "да", Lane: lane, Got: "да", Status: Pass, Note: note}
	if !ok {
		c.Got, c.Status = "нет", Fail
	}
	r.check(c)
}

// pending — проверку не определить.
func (r *Result) pending(what, want, lane, why string) {
	r.check(Check{What: what, Want: want, Lane: lane, Got: "—", Status: Pending, Note: why})
}

// metric добавляет отчётное число.
func (r *Result) metric(what, lane string, format string, args ...any) {
	r.Metrics = append(r.Metrics, Metric{What: what, Lane: lane, Value: fmt.Sprintf(format, args...)})
}

// sample добавляет ответ в отчёт.
func (r *Result) sample(topic string, st Step, note string) {
	r.Samples = append(r.Samples, Sample{Topic: topic, Lane: st.Lane, User: st.Turn.User, Reply: replyOf(st), Note: note})
}

func (r *Result) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// describeLanes — дорожки в отчёт.
func (r *Result) describeLanes(reg *features.Registry, lanes []Lane) {
	if len(lanes) == 0 {
		return
	}
	base := reg.Complete(lanes[0].Features)
	r.Lanes = r.Lanes[:0]
	for i, l := range lanes {
		fs := reg.Complete(l.Features)
		info := LaneInfo{Name: l.Name, Note: l.Note, Diff: "основная"}
		for _, n := range fs.Names() {
			info.Features = append(info.Features, string(n))
		}
		if i > 0 {
			var parts []string
			for _, n := range reg.Diff(base, fs) {
				sign := "−"
				if fs.On(n) {
					sign = "+"
				}
				parts = append(parts, sign+string(n))
			}
			info.Diff = strings.Join(parts, ", ")
			if info.Diff == "" {
				info.Diff = "те же механизмы"
			}
		}
		r.Lanes = append(r.Lanes, info)
	}
}

func firstN(list []string, n int) []string {
	if len(list) > n {
		return list[:n]
	}
	return list
}

// replyOf — что увидел человек: ответ хода, у неудачного — ошибка.
func replyOf(st Step) string {
	if st.Turn.Status != history.TurnDone {
		return "(ход не завершён: " + st.Turn.Error + ")"
	}
	return st.Turn.Reply
}

// LaneStats — цена и разбивка запроса одной дорожки по всем её ходам
// испытания.
type LaneStats struct {
	Lane      string        `json:"lane"`
	Turns     int           `json:"turns"`
	Failed    int           `json:"failed"`
	LLMCalls  int           `json:"llmCalls"`
	ToolCalls int           `json:"toolCalls"`
	Usage     llm.Usage     `json:"usage"`
	Cost      llm.Cost      `json:"cost"`
	Seconds   float64       `json:"seconds"`
	Blocks    []BlockTokens `json:"blocks"`
	// Средняя разбивка запроса на ход (по самому тяжёлому прогону хода).
	System   int `json:"system"`
	Tools    int `json:"tools"`
	History  int `json:"history"`
	User     int `json:"user"`
	Constant int `json:"constant"`
	// ConstantMax — самая большая постоянная часть за испытание.
	ConstantMax int `json:"constantMax"`
	// Brak — ходов, прошедших не с теми механизмами, что просили.
	Brak int `json:"brak"`
	// Calibration — оценка против usage по запросам к модели.
	Calibration CalibrationStats `json:"calibration"`
}

// BlockTokens — средняя доля механизма в запросе на ход.
type BlockTokens struct {
	Feature features.Name `json:"feature"`
	Tokens  int           `json:"tokens"`
	// Turns — в скольких ходах блок был.
	Turns int `json:"turns"`
}

// CacheShare — доля токенов запроса, взятых из кэша.
func (s LaneStats) CacheShare() float64 {
	if s.Usage.Prompt == 0 {
		return 0
	}
	return float64(s.Usage.CacheHit) / float64(s.Usage.Prompt)
}

// PerTurn — запросов к модели на ход.
func (s LaneStats) PerTurn() float64 {
	if s.Turns == 0 {
		return 0
	}
	return float64(s.LLMCalls) / float64(s.Turns)
}

// CalibrationStats — сверка оценщика токенов с usage (ФТ-32).
type CalibrationStats struct {
	Pairs int `json:"pairs"`
	// RawPct — средняя ошибка оценки без поправки, в процентах от факта.
	RawPct float64 `json:"rawPct"`
	// Factor — поправочный множитель по этим парам.
	Factor float64 `json:"factor"`
	// ResidualPct — средняя абсолютная ошибка после поправки: её и держит
	// допуск 5 %.
	ResidualPct float64 `json:"residualPct"`
}

// calibrate — калибровка по парам «оценка — факт» из журнала ходов.
func calibrate(turns []history.Turn) CalibrationStats {
	var cal tokens.Calibration
	type pair struct{ est, act int }
	var pairs []pair
	for _, t := range turns {
		for _, e := range t.Events {
			if e.Kind != agent.EventLLMReply || e.Tokens == nil || e.Tokens.Actual <= 0 || e.Tokens.Estimated <= 0 {
				continue
			}
			cal.Observe(e.Tokens.Estimated, e.Tokens.Actual)
			pairs = append(pairs, pair{e.Tokens.Estimated, e.Tokens.Actual})
		}
	}
	out := CalibrationStats{Pairs: cal.Pairs(), RawPct: cal.ErrorPct(), Factor: cal.Factor()}
	if len(pairs) == 0 {
		return out
	}
	var sum float64
	for _, p := range pairs {
		sum += math.Abs(float64(p.est)*out.Factor-float64(p.act)) / float64(p.act) * 100
	}
	out.ResidualPct = sum / float64(len(pairs))
	return out
}

// statsOf — цена и разбивка дорожки.
func statsOf(reg *features.Registry, lane string, turns []history.Turn) LaneStats {
	s := LaneStats{Lane: lane, Turns: len(turns)}
	blocks := map[features.Name]*BlockTokens{}
	measured := 0
	for _, t := range turns {
		if t.Status != history.TurnDone {
			s.Failed++
		}
		s.LLMCalls += t.Totals.LLMCalls
		s.ToolCalls += t.Totals.ToolCalls
		s.Usage = s.Usage.Add(t.Totals.Usage)
		s.Cost = s.Cost.Add(t.Totals.Cost)
		s.Seconds += t.Totals.Seconds
		if brak(reg, t) {
			s.Brak++
		}
		est := t.Context.Estimate
		if est.Total == 0 {
			continue
		}
		measured++
		s.System += est.System
		s.Tools += est.Tools
		s.History += est.History
		s.User += est.User
		s.Constant += est.Constant
		if est.Constant > s.ConstantMax {
			s.ConstantMax = est.Constant
		}
		for n, v := range est.Blocks {
			if v == 0 {
				continue
			}
			b := blocks[n]
			if b == nil {
				b = &BlockTokens{Feature: n}
				blocks[n] = b
			}
			b.Tokens += v
			b.Turns++
		}
	}
	if measured > 0 {
		s.System /= measured
		s.Tools /= measured
		s.History /= measured
		s.User /= measured
		s.Constant /= measured
	}
	for _, b := range blocks {
		b.Tokens /= measured
		s.Blocks = append(s.Blocks, *b)
	}
	place := func(n features.Name) int {
		if m, ok := reg.Get(n); ok {
			return int(m.Place)
		}
		return math.MaxInt32
	}
	sort.Slice(s.Blocks, func(i, j int) bool {
		if place(s.Blocks[i].Feature) != place(s.Blocks[j].Feature) {
			return place(s.Blocks[i].Feature) < place(s.Blocks[j].Feature)
		}
		return s.Blocks[i].Feature < s.Blocks[j].Feature
	})
	s.Calibration = calibrate(turns)
	return s
}

// brak — ход прошёл не с теми механизмами, что просили.
func brak(reg *features.Registry, t history.Turn) bool {
	if t.Effective.Empty() {
		return false
	}
	return len(reg.Diff(t.Requested, t.Effective)) > 0
}

// Run гоняет испытания по очереди; у каждого свой каталог. Поломка одного
// испытания не останавливает остальные: отчёт покажет её строкой.
func Run(ctx context.Context, env *Env, trials []Trial) []*Result {
	var out []*Result
	for _, t := range trials {
		out = append(out, RunOne(ctx, env, t))
	}
	return out
}

// RunOne — одно испытание.
func RunOne(ctx context.Context, env *Env, t Trial) *Result {
	started := time.Now()
	r := &Result{ID: t.ID(), Title: t.Title()}
	env.logf("%s. %s", t.ID(), t.Title())
	s, err := env.NewStand(t.ID(), Options{})
	if err != nil {
		r.Err = err.Error()
		return r
	}
	if err := t.Run(ctx, s, r); err != nil {
		r.Err = err.Error()
	}
	order, turns := s.rec.snapshot()
	for _, lane := range order {
		r.Stats = append(r.Stats, statsOf(env.Registry, lane, turns[lane]))
	}
	r.Elapsed = time.Since(started)
	env.logf("%s: %s, проверок %d из %d", t.ID(), verdictWord(r.Verdict()), r.Count(Pass), len(r.Checks))
	return r
}

func verdictWord(s Status) string {
	switch s {
	case Pass:
		return "принято"
	case Fail:
		return "не принято"
	}
	return "не определено"
}

// promptsOf — что получала модель в ходе: события «промпт» по порядку.
func promptsOf(t history.Turn) []agent.Prompt {
	var out []agent.Prompt
	for _, e := range t.Events {
		if e.Kind != agent.EventPrompt {
			continue
		}
		var p agent.Prompt
		if json.Unmarshal([]byte(e.Detail), &p) == nil {
			out = append(out, p)
		}
	}
	return out
}

// blockText — текст блока механизма в первом запросе хода, где он есть.
func blockText(t history.Turn, n features.Name) (string, bool) {
	for _, p := range promptsOf(t) {
		for _, b := range p.Blocks {
			if b.Feature == string(n) && strings.TrimSpace(b.Text) != "" {
				return b.Text, true
			}
		}
	}
	return "", false
}

// toolCalls — сколько вызовов инструмента в ходе (без завершающих).
func toolCalls(t history.Turn, name string) int {
	n := 0
	for _, e := range t.Events {
		if e.Kind == agent.EventToolCall && e.Tool == name && !e.Final {
			n++
		}
	}
	return n
}

// mechanismEvents — события обвязки механизма в ходе.
func mechanismEvents(t history.Turn, n features.Name) int {
	c := 0
	for _, e := range t.Events {
		if e.Mechanism == string(n) {
			c++
		}
	}
	return c
}
