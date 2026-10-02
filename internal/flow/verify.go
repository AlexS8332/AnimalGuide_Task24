package flow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/hub"
)

// Имена проверок, не привязанных к шагу. Имена шаговых проверок собираются
// из имён инструментов («выбор: mdd_get», «данные: mdd_search → mdd_get.id»,
// «порядок: nb_add → nb_close»): они стабильны между прогонами, и -repeat
// считает по ним долю прохождения.
const (
	CheckRoute     = "маршрут"
	CheckForbidden = "запрещённые инструменты"
	CheckServers   = "покрытие серверов"
	CheckCites     = "ссылки на источники"
	CheckEvidence  = "серверы подтвердили"
	CheckPID       = "серверы не перезапускались"
	CheckCalls     = "число вызовов"
	CheckRepeats   = "ошибки и повторы"
	CheckUnknown   = "неизвестные инструменты"
)

// infoTool — служебный инструмент серверов: его вызывает Snapshot, а не
// модель, поэтому в сверке счётчиков он не участвует.
const infoTool = "server_info"

// CheckChoice — имя проверки обязательного шага.
func CheckChoice(tool string) string { return "выбор: " + tool }

// CheckData — имя проверки зависимости данных.
func CheckData(fromTool, toTool, arg string) string {
	return fmt.Sprintf("данные: %s → %s.%s", fromTool, toTool, arg)
}

// CheckOrder — имя проверки частичного порядка.
func CheckOrder(a, b string) string { return fmt.Sprintf("порядок: %s → %s", a, b) }

// Сколько примеров показывать в пояснении: проверка на двадцать вызовов не
// должна превращаться в простыню, а первого нарушения хватает, чтобы понять.
const noteExamples = 3

// Verify сверяет вызовы со спецификацией; routes — маршруты реестра на
// момент прогона (сервер каждого инструмента), ev — счётчики серверов.
//
// Провал — то, что делает флоу недоказанным: нет обязательного шага, вызов
// ушёл не на тот сервер, значение пришло не из ответа более раннего вызова,
// нарушен частичный порядок, вызван запрещённый инструмент, серверов меньше
// нужного, ссылки cites ведут в никуда, сервер насчитал вызовов меньше, чем
// трасса. Предупреждение — то, что флоу не ломает, но заслуживает взгляда:
// лишние и повторные вызовы, ошибки, выдуманные имена, чужие вызовы на
// сервере, перезапуск сервера.
//
// Verify заполняет Call.From у вызовов, чьи аргументы пришли из ответов
// других (по совпавшим Flow): это стрелки «← №k» в журнале.
func Verify(s Spec, calls []Call, routes []hub.Route, ev *Evidence) Verdict {
	v := &verifier{spec: s, calls: calls, v: Verdict{OK: true, Match: map[string][]int{}}}
	v.prepare()
	v.steps()
	v.route(routes)
	v.flows()
	v.before()
	v.forbidden()
	v.servers()
	v.cites()
	v.evidence(calls, routes, ev)
	v.hygiene()
	return v.v
}

type verifier struct {
	spec  Spec
	calls []Call
	v     Verdict

	args    []map[string]any // разобранные аргументы вызова i
	results []any            // разобранный ответ вызова i
	byStep  map[string][]int // индексы засчитанных вызовов шага
	stepOf  map[string]StepSpec
}

func (v *verifier) add(name string, lvl Level, note string) {
	if lvl == LevelFail {
		v.v.OK = false
	}
	v.v.Checks = append(v.v.Checks, Check{Name: name, Level: lvl, Note: note})
}

// addList — проверка по списку нарушений: пусто — ok с пояснением okNote.
func (v *verifier) addList(name string, lvl Level, bad []string, okNote string) {
	if len(bad) == 0 {
		v.add(name, LevelOK, okNote)
		return
	}
	v.add(name, lvl, examples(bad))
}

func (v *verifier) prepare() {
	v.args = make([]map[string]any, len(v.calls))
	v.results = make([]any, len(v.calls))
	for i := range v.calls {
		v.calls[i].From = nil
		if m, ok := decode(v.calls[i].Args).(map[string]any); ok {
			v.args[i] = m
		}
		if v.calls[i].OK {
			v.results[i] = decode(v.calls[i].Result)
		}
	}
	v.byStep = map[string][]int{}
	v.stepOf = map[string]StepSpec{}
	for _, st := range v.spec.Steps {
		v.stepOf[st.ID] = st
	}
}

// counted — засчитывается ли вызов шагу: успешный, а у шага с AllowError —
// и ответ-ошибка (выпуска о виде может не быть).
func counted(c Call, st StepSpec) bool {
	return c.OK || (st.AllowError && c.Error != "")
}

// steps — обязательные шаги: сколько вызовов подошло по инструменту и
// условиям на аргументы.
func (v *verifier) steps() {
	for _, st := range v.spec.Steps {
		var n, tried int
		var mismatch []string
		for i, c := range v.calls {
			if c.Tool != st.Tool {
				continue
			}
			tried++
			if why := argsMismatch(st.Args, v.args[i]); why != "" {
				mismatch = append(mismatch, fmt.Sprintf("№%d: %s", c.N, why))
				continue
			}
			if !counted(c, st) {
				continue
			}
			n++
			v.byStep[st.ID] = append(v.byStep[st.ID], i)
			v.v.Match[st.ID] = append(v.v.Match[st.ID], c.N)
		}
		name := CheckChoice(st.Tool)
		switch {
		case n < st.Min:
			note := fmt.Sprintf("нужно засчитанных вызовов ≥ %d, есть %d (всего вызовов %d)", st.Min, n, tried)
			if len(mismatch) > 0 {
				note += "; не подошли по аргументам: " + examples(mismatch)
			}
			v.add(name, LevelFail, note)
		case st.Max > 0 && n > st.Max:
			v.add(name, LevelWarn, fmt.Sprintf("вызовов %d при пределе %d: %s", n, st.Max, v.numbers(v.byStep[st.ID])))
		default:
			v.add(name, LevelOK, v.numbers(v.byStep[st.ID]))
		}
	}
}

// route — каждый вызов ушёл на сервер своего маршрута, и маршрут реестра
// совпадает с тем, что ждёт спецификация.
func (v *verifier) route(routes []hub.Route) {
	want := map[string]string{}
	for _, r := range routes {
		if !r.Hidden {
			want[r.Tool] = r.Server
		}
	}
	var bad []string
	for _, st := range v.spec.Steps {
		if got, ok := want[st.Tool]; ok && got != st.Server {
			bad = append(bad, fmt.Sprintf("%s по спецификации — на %s, а реестр ведёт на %s", st.Tool, st.Server, got))
		}
		if _, ok := want[st.Tool]; !ok && len(routes) > 0 {
			bad = append(bad, fmt.Sprintf("%s нет среди инструментов реестра", st.Tool))
		}
	}
	stepServer := map[string]string{}
	for _, st := range v.spec.Steps {
		stepServer[st.Tool] = st.Server
	}
	for _, c := range v.calls {
		if c.Server == "" {
			continue // выдуманное имя — отдельная проверка
		}
		exp, ok := want[c.Tool]
		if !ok {
			exp = stepServer[c.Tool]
		}
		if exp != "" && c.Server != exp {
			bad = append(bad, fmt.Sprintf("№%d %s ушёл на %s, маршрут — %s", c.N, c.Tool, c.Server, exp))
		}
	}
	v.addList(CheckRoute, LevelFail, bad, "все вызовы — на серверы своих маршрутов")
}

// flows — зависимости данных: значение аргумента каждого засчитанного
// вызова шага To есть в ответе более раннего успешного вызова шага From.
func (v *verifier) flows() {
	for _, f := range v.spec.Flows {
		from, to := v.stepOf[f.From], v.stepOf[f.To]
		name := CheckData(from.Tool, to.Tool, f.Arg)
		targets := v.byStep[f.To]
		if len(targets) == 0 {
			v.add(name, LevelWarn, fmt.Sprintf("вызовов %s нет — проверять нечего", to.Tool))
			continue
		}
		var bad, good []string
		for _, ti := range targets {
			c := &v.calls[ti]
			av, ok := lookup(v.args[ti], f.Arg)
			if !ok {
				bad = append(bad, fmt.Sprintf("№%d %s без аргумента %s", c.N, c.Tool, f.Arg))
				continue
			}
			hit := false
			var seen []string
			srcs := v.byStep[f.From]
			for k := len(srcs) - 1; k >= 0 && !hit; k-- {
				pi := srcs[k]
				p := v.calls[pi]
				if !p.OK || p.N >= c.N {
					continue
				}
				seen = append(seen, fmt.Sprintf("№%d", p.N))
				for _, fv := range extract(v.results[pi], f.Path) {
					if flowMatch(av, fv.val, f.Mode) {
						hit = true
						if !slices.Contains(c.From, p.N) {
							c.From = append(c.From, p.N)
						}
						good = append(good, fmt.Sprintf("№%d %s.%s = %s ← №%d %s", c.N, c.Tool, f.Arg, show(av), p.N, fv.path))
						break
					}
				}
			}
			if hit {
				continue
			}
			if len(seen) == 0 {
				bad = append(bad, fmt.Sprintf("№%d %s.%s = %s — успешного %s раньше не было", c.N, c.Tool, f.Arg, show(av), from.Tool))
			} else {
				bad = append(bad, fmt.Sprintf("№%d %s.%s = %s — нет в %s ответов %s (%s)", c.N, c.Tool, f.Arg, show(av), f.Path, from.Tool, strings.Join(seen, ", ")))
			}
		}
		if len(bad) > 0 {
			v.add(name, LevelFail, examples(bad))
			continue
		}
		v.add(name, LevelOK, examples(good))
	}
}

// before — частичный порядок: ни один вызов шага A не идёт после первого
// засчитанного вызова шага B. Считаются и неудачные попытки A: nb_add после
// nb_close сервер отвергнет, но это ошибка порядка у модели, а не у сервера.
func (v *verifier) before() {
	for _, o := range v.spec.Before {
		a, b := v.stepOf[o.A], v.stepOf[o.B]
		name := CheckOrder(a.Tool, b.Tool)
		first := -1
		for _, i := range v.byStep[o.B] {
			if v.calls[i].OK && (first < 0 || v.calls[i].N < v.calls[first].N) {
				first = i
			}
		}
		if first < 0 {
			v.add(name, LevelOK, fmt.Sprintf("успешного %s нет — проверять нечего", b.Tool))
			continue
		}
		fb := v.calls[first]
		var bad []string
		for _, c := range v.calls {
			if c.Tool == a.Tool && c.N > fb.N {
				bad = append(bad, fmt.Sprintf("№%d %s — после №%d %s", c.N, a.Tool, fb.N, b.Tool))
			}
		}
		v.addList(name, LevelFail, bad, fmt.Sprintf("все %s раньше №%d %s", a.Tool, fb.N, b.Tool))
	}
}

func (v *verifier) forbidden() {
	var bad []string
	for _, c := range v.calls {
		if forbidden(v.spec.Forbidden, c.Tool) {
			bad = append(bad, fmt.Sprintf("№%d %s", c.N, c.Tool))
		}
	}
	v.addList(CheckForbidden, LevelFail, bad, "не вызывались")
}

// forbidden — подпадает ли имя под запрет (шаблон с «*» в конце).
func forbidden(patterns []string, tool string) bool {
	for _, p := range patterns {
		if pre, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(tool, pre) {
				return true
			}
		} else if p == tool {
			return true
		}
	}
	return false
}

func (v *verifier) servers() {
	var names []string
	for _, c := range v.calls {
		if c.OK && c.Server != "" && !slices.Contains(names, c.Server) {
			names = append(names, c.Server)
		}
	}
	note := fmt.Sprintf("%d из нужных %d: %s", len(names), v.spec.MinServers, strings.Join(names, ", "))
	if len(names) < v.spec.MinServers {
		v.add(CheckServers, LevelFail, note)
		return
	}
	v.add(CheckServers, LevelOK, note)
}

// cites — ссылки на источники: каждый названный инструмент засчитанно
// вызван раньше раздела, а среди их серверов есть все CiteServers.
// Засчитанный — как у шагов (counted): успешный, а у шага с AllowError и
// ответ-ошибка. «Выпуска о виде ещё не было» от facts_get — тоже сведение
// из источника, и сослаться на него честно; вживую модель так и делает.
func (v *verifier) cites() {
	if v.spec.CiteTool == "" {
		return
	}
	var bad []string
	covered := map[string]bool{}
	sections := 0
	for i, c := range v.calls {
		if c.Tool != v.spec.CiteTool || !c.OK {
			continue
		}
		sections++
		raw, _ := lookup(v.args[i], v.spec.CiteArg)
		names := strList(raw)
		if len(names) == 0 {
			bad = append(bad, fmt.Sprintf("№%d %s без %s", c.N, c.Tool, v.spec.CiteArg))
			continue
		}
		for _, name := range names {
			server := ""
			for _, p := range v.calls[:i] {
				if p.Tool == name && p.Server != "" && v.citable(p) {
					server = p.Server
				}
			}
			if server == "" {
				bad = append(bad, fmt.Sprintf("№%d %s ссылается на %s — засчитанного вызова раньше не было", c.N, c.Tool, name))
				continue
			}
			covered[server] = true
		}
	}
	if sections == 0 {
		v.add(CheckCites, LevelOK, fmt.Sprintf("успешных %s нет — проверять нечего", v.spec.CiteTool))
		return
	}
	var missing []string
	for _, s := range v.spec.CiteServers {
		if !covered[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		bad = append(bad, "среди ссылок нет серверов: "+strings.Join(missing, ", "))
	}
	got := make([]string, 0, len(covered))
	for s := range covered {
		got = append(got, s)
	}
	sort.Strings(got)
	v.addList(CheckCites, LevelFail, bad, fmt.Sprintf("разделов %d, ссылки ведут на серверы: %s", sections, strings.Join(got, ", ")))
}

// evidence — счётчики самих серверов. Сервер насчитал меньше, чем трасса
// приписывает ему, — вызов до него не дошёл (или ушёл не туда): провал.
// Больше — демон обслуживает и других клиентов (приложение, свои задания):
// предупреждение. Сменившийся PID обесценивает разницу: сервер перезапущен
// во время флоу, и его счётчики начались заново.
func (v *verifier) evidence(calls []Call, routes []hub.Route, ev *Evidence) {
	if ev == nil {
		v.add(CheckEvidence, LevelWarn, "счётчики серверов не снимались")
		v.add(CheckPID, LevelWarn, "счётчики серверов не снимались")
		return
	}
	var fails, warns, restarted []string
	for _, d := range Deltas(calls, routes, ev) {
		before, hadBefore := ev.Before[d.Server]
		_, hasAfter := ev.After[d.Server]
		restart := hadBefore && hasAfter && before.PID != 0 && d.PID != 0 && before.PID != d.PID
		if restart {
			restarted = append(restarted, fmt.Sprintf("%s: PID %d → %d", d.Server, before.PID, d.PID))
		}
		if !hasAfter {
			if sum(d.Traced) > 0 {
				warns = append(warns, fmt.Sprintf("%s: снимка после флоу нет, вызовов в трассе %d", d.Server, sum(d.Traced)))
			}
			continue
		}
		for _, tool := range keys(d.Served, d.Traced) {
			served, traced := d.Served[tool], d.Traced[tool]
			switch {
			case served < traced && !restart:
				fails = append(fails, fmt.Sprintf("%s.%s: сервер насчитал %d, в трассе %d", d.Server, tool, served, traced))
			case served < traced:
				warns = append(warns, fmt.Sprintf("%s.%s: сервер насчитал %d, в трассе %d (сервер перезапущен)", d.Server, tool, served, traced))
			case served > traced:
				warns = append(warns, fmt.Sprintf("%s.%s: сервер насчитал %d, в трассе %d — чужие вызовы", d.Server, tool, served, traced))
			}
		}
	}
	switch {
	case len(fails) > 0:
		v.add(CheckEvidence, LevelFail, examples(append(fails, warns...)))
	case len(warns) > 0:
		v.add(CheckEvidence, LevelWarn, examples(warns))
	default:
		v.add(CheckEvidence, LevelOK, "счётчики серверов совпали с трассой")
	}
	v.addList(CheckPID, LevelWarn, restarted, "PID до и после совпали")
}

// hygiene — предупреждения: лишние вызовы, ошибки и повторы, выдуманные
// имена. Флоу они не ломают, но цена и надёжность видны по ним.
func (v *verifier) hygiene() {
	if v.spec.MaxCalls > 0 {
		note := fmt.Sprintf("%d при пределе %d", len(v.calls), v.spec.MaxCalls)
		if len(v.calls) > v.spec.MaxCalls {
			v.add(CheckCalls, LevelWarn, note)
		} else {
			v.add(CheckCalls, LevelOK, note)
		}
	}
	var errs, reps []string
	seen := map[string]int{}
	for i, c := range v.calls {
		if !c.OK && c.Error != "" {
			errs = append(errs, fmt.Sprintf("№%d %s", c.N, c.Tool))
		}
		key := c.Tool + "\x00" + canon(v.args[i], c.Args)
		if n, ok := seen[key]; ok {
			reps = append(reps, fmt.Sprintf("№%d повторяет №%d %s", c.N, n, c.Tool))
		} else {
			seen[key] = c.N
		}
	}
	if len(errs)+len(reps) == 0 {
		v.add(CheckRepeats, LevelOK, "ошибок и повторов нет")
	} else {
		var parts []string
		if len(errs) > 0 {
			parts = append(parts, fmt.Sprintf("ошибок %d: %s", len(errs), examples(errs)))
		}
		if len(reps) > 0 {
			parts = append(parts, fmt.Sprintf("повторов %d: %s", len(reps), examples(reps)))
		}
		v.add(CheckRepeats, LevelWarn, strings.Join(parts, "; "))
	}
	var unknown []string
	for _, c := range v.calls {
		if c.Server == "" {
			unknown = append(unknown, fmt.Sprintf("№%d %s", c.N, c.Tool))
		}
	}
	v.addList(CheckUnknown, LevelWarn, unknown, "все имена — из реестра")
}

// Deltas — сколько вызовов каждого инструмента насчитал каждый сервер за
// флоу (server_info после минус до) и сколько их в трассе. Серверы — в
// порядке маршрутов, затем прочие по алфавиту; server_info не учитывается.
// ev == nil — только трасса.
func Deltas(calls []Call, routes []hub.Route, ev *Evidence) []ServerDelta {
	var order []string
	addServer := func(s string) {
		if s != "" && !slices.Contains(order, s) {
			order = append(order, s)
		}
	}
	for _, r := range routes {
		addServer(r.Server)
	}
	var extra []string
	for _, c := range calls {
		if c.Server != "" && !slices.Contains(order, c.Server) && !slices.Contains(extra, c.Server) {
			extra = append(extra, c.Server)
		}
	}
	if ev != nil {
		for s := range ev.After {
			if !slices.Contains(order, s) && !slices.Contains(extra, s) {
				extra = append(extra, s)
			}
		}
	}
	sort.Strings(extra)
	order = append(order, extra...)

	out := make([]ServerDelta, 0, len(order))
	for _, s := range order {
		d := ServerDelta{Server: s, Traced: map[string]int{}}
		for _, c := range calls {
			if c.Server == s {
				d.Traced[c.Tool]++
			}
		}
		if ev != nil {
			if after, ok := ev.After[s]; ok {
				d.PID = after.PID
				d.Served = map[string]int{}
				before := ev.Before[s]
				for tool, n := range after.Calls {
					if tool == infoTool {
						continue
					}
					if delta := n - before.Calls[tool]; delta != 0 {
						d.Served[tool] = delta
					}
				}
			}
		}
		out = append(out, d)
	}
	return out
}

// ------------------------------------------------------------ значения

// decode — JSON в дерево с числами json.Number: usage_key 2435270 не должен
// превращаться в 2.43527e+06.
func decode(raw json.RawMessage) any {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	return v
}

// lookup — аргумент по имени (допускается путь через точку).
func lookup(args map[string]any, name string) (any, bool) {
	if args == nil {
		return nil, false
	}
	if v, ok := args[name]; ok {
		return v, v != nil
	}
	found := extract(args, name)
	if len(found) == 0 {
		return nil, false
	}
	return found[0].val, true
}

type found struct {
	path string // конкретный путь: «species.0.id»
	val  any
}

// extract — значения по пути: сегменты через точку, индекс массива числом,
// «*» — любой элемент массива (или значение объекта); альтернативы пути —
// через «|». Массив на конце пути раскрывается поэлементно.
func extract(root any, path string) []found {
	var out []found
	for _, alt := range strings.Split(path, "|") {
		alt = strings.TrimSpace(alt)
		if alt == "" {
			continue
		}
		walk(root, strings.Split(alt, "."), "", &out)
	}
	return out
}

func walk(v any, segs []string, at string, out *[]found) {
	join := func(seg string) string {
		if at == "" {
			return seg
		}
		return at + "." + seg
	}
	if len(segs) == 0 {
		if arr, ok := v.([]any); ok {
			for i, e := range arr {
				if _, ok := scalar(e); ok {
					*out = append(*out, found{join(strconv.Itoa(i)), e})
				}
			}
			return
		}
		if _, ok := scalar(v); ok {
			*out = append(*out, found{at, v})
		}
		return
	}
	seg, rest := segs[0], segs[1:]
	switch x := v.(type) {
	case map[string]any:
		if seg == "*" {
			for _, k := range sortedKeys(x) {
				walk(x[k], rest, join(k), out)
			}
			return
		}
		if e, ok := x[seg]; ok {
			walk(e, rest, join(seg), out)
		}
	case []any:
		if seg == "*" {
			for i, e := range x {
				walk(e, rest, join(strconv.Itoa(i)), out)
			}
			return
		}
		if i, err := strconv.Atoi(seg); err == nil && i >= 0 && i < len(x) {
			walk(x[i], rest, join(seg), out)
		}
	}
}

// scalar — строковое представление скаляра: строка без пробелов по краям,
// число в каноническом виде (1006010, а не 1.00601e+06), логическое.
func scalar(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x), true
	case json.Number:
		if f, err := strconv.ParseFloat(x.String(), 64); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64), true
		}
		return x.String(), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

// sameValue — равенство двух скаляров: числа как числа («1006010» из
// строки аргумента равно 1006010 из ответа), строки — точно или без учёта
// регистра.
func sameValue(a, b string, fold bool) bool {
	if a == "" || b == "" {
		return false
	}
	fa, ea := strconv.ParseFloat(a, 64)
	fb, eb := strconv.ParseFloat(b, 64)
	if ea == nil && eb == nil {
		return fa == fb
	}
	if fold {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// flowMatch — пришло ли значение из ответа в аргумент по режиму Flow.
func flowMatch(arg, val any, mode string) bool {
	v, ok := scalar(val)
	if !ok {
		return false
	}
	if mode == FlowContains {
		if arr, ok := arg.([]any); ok {
			for _, e := range arr {
				if s, ok := scalar(e); ok && sameValue(s, v, true) {
					return true
				}
			}
			return false
		}
		s, ok := scalar(arg)
		return ok && v != "" && strings.Contains(strings.ToLower(s), strings.ToLower(v))
	}
	s, ok := scalar(arg)
	return ok && sameValue(s, v, mode == FlowEqFold)
}

// argsMismatch — почему аргументы не подходят под условия шага; пусто —
// подходят.
func argsMismatch(conds map[string]Match, args map[string]any) string {
	for _, name := range sortedKeys(conds) {
		m := conds[name]
		if m.Eq == "" && m.Regexp == "" {
			continue
		}
		raw, ok := lookup(args, name)
		val, isScalar := scalar(raw)
		if !ok || !isScalar {
			return fmt.Sprintf("нет аргумента %s", name)
		}
		if m.Eq != "" && val != strings.TrimSpace(m.Eq) {
			return fmt.Sprintf("%s = %q, ожидалось %q", name, val, m.Eq)
		}
		if m.Regexp != "" {
			re, err := regexp.Compile(m.Regexp)
			if err != nil {
				return fmt.Sprintf("условие на %s не разобралось: %v", name, err)
			}
			if !re.MatchString(val) {
				return fmt.Sprintf("%s = %q не подходит под %s", name, val, m.Regexp)
			}
		}
	}
	return ""
}

// strList — список строк из аргумента: массив или одна строка.
func strList(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case string:
		for _, s := range strings.Split(x, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// show — значение для пояснения: строки в кавычках, числа как есть.
func show(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(strings.TrimSpace(s))
	}
	if s, ok := scalar(v); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// canon — аргументы в каноническом виде для поиска повторов.
func canon(m map[string]any, raw json.RawMessage) string {
	if m == nil {
		return string(bytes.TrimSpace(raw))
	}
	b, err := json.Marshal(m)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

func (v *verifier) numbers(idx []int) string {
	if len(idx) == 0 {
		return "вызовов нет"
	}
	parts := make([]string, len(idx))
	for i, k := range idx {
		parts[i] = fmt.Sprintf("№%d", v.calls[k].N)
	}
	return strings.Join(parts, ", ")
}

// examples — первые noteExamples пунктов и «и ещё N».
func examples(items []string) string {
	if len(items) <= noteExamples {
		return strings.Join(items, "; ")
	}
	return strings.Join(items[:noteExamples], "; ") + fmt.Sprintf("; и ещё %d", len(items)-noteExamples)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// keys — объединение ключей двух счётчиков по алфавиту.
func keys(a, b map[string]int) []string {
	set := map[string]int{}
	for k := range a {
		set[k] = 0
	}
	for k := range b {
		set[k] = 0
	}
	return sortedKeys(set)
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// citable — можно ли сослаться на вызов: он засчитан хотя бы одному шагу
// с тем же инструментом, а у инструментов вне спецификации — успешен.
func (v *verifier) citable(c Call) bool {
	if c.OK {
		return true
	}
	for _, st := range v.spec.Steps {
		if st.Tool == c.Tool && counted(c, st) {
			return true
		}
	}
	return false
}
