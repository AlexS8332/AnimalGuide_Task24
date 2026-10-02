package flow_test

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow/flowtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/hub"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
)

// evidenceFor — счётчики серверов, в точности подтверждающие трассу: у
// мутаций, которые портят не свидетельство, оно не должно мешать.
func evidenceFor(calls []flow.Call) *flow.Evidence {
	ev := &flow.Evidence{Before: hub.Snapshot{}, After: hub.Snapshot{}}
	for i, s := range []string{"sources", "daemon", "notes"} {
		ev.Before[s] = mcp.Info{Server: s, PID: 100 + i, Calls: map[string]int{"server_info": 5}}
		ev.After[s] = mcp.Info{Server: s, PID: 100 + i, Calls: map[string]int{"server_info": 6}}
	}
	for _, c := range calls {
		if c.Server != "" {
			ev.After[c.Server].Calls[c.Tool]++
		}
	}
	return ev
}

// renumber — N по порядку после перестановок.
func renumber(calls []flow.Call) []flow.Call {
	for i := range calls {
		calls[i].N = i + 1
	}
	return calls
}

func clone(calls []flow.Call) []flow.Call {
	out := make([]flow.Call, len(calls))
	for i, c := range calls {
		c.Args = append(json.RawMessage(nil), c.Args...)
		c.Result = append(json.RawMessage(nil), c.Result...)
		c.From = nil
		out[i] = c
	}
	return out
}

func index(calls []flow.Call, tool string) int {
	return slices.IndexFunc(calls, func(c flow.Call) bool { return c.Tool == tool })
}

// Мутационные тесты Verify: трасса хорошего прогона, испорченная по одному
// месту. Каждая порча обязана дать свой провал — по имени проверки; порча,
// которая флоу не ломает (лишние вызовы), — только предупреждения.
func TestVerifyMutations(t *testing.T) {
	good := runFlow(t, &flowtest.Brain{}, nil)
	if !good.tr.OK {
		t.Fatalf("исходная трасса не прошла:\n%s", dump(good.tr.Verdict))
	}
	routes, _ := good.router.Routes(context.Background())
	spec := flow.Presets()[0].SpecFor("")
	base := good.tr.Calls

	if v := flow.Verify(spec, clone(base), routes, evidenceFor(base)); !v.OK {
		t.Fatalf("исходная трасса с подставным свидетельством:\n%s", dump(v))
	}

	for _, m := range []struct {
		name  string
		mut   func([]flow.Call) ([]flow.Call, *flow.Evidence)
		fails []string // ровно эти провалы; nil — провалов нет
		warns []string // среди предупреждений есть эти
		loose bool     // провал может тянуть за собой соседние
	}{
		{
			name: "вызов на другом сервере",
			mut: func(cs []flow.Call) ([]flow.Call, *flow.Evidence) {
				cs[index(cs, "mdd_get")].Server = "sources"
				return cs, evidenceFor(cs)
			},
			fails: []string{flow.CheckRoute},
		},
		{
			name: "удалён mdd_get",
			mut: func(cs []flow.Call) ([]flow.Call, *flow.Evidence) {
				cs = slices.Delete(cs, index(cs, "mdd_get"), index(cs, "mdd_get")+1)
				return renumber(cs), evidenceFor(cs)
			},
			fails: []string{flow.CheckChoice("mdd_get")},
			loose: true, // за ним — данные match_taxon и facts_get, ссылка cites на mdd_get
		},
		{
			name: "добавлен run_now",
			mut: func(cs []flow.Call) ([]flow.Call, *flow.Evidence) {
				cs = slices.Insert(cs, 3, flow.Call{Turn: 3, Tool: "run_now", Args: json.RawMessage(`{}`),
					Error: "инструмента run_now нет"})
				return renumber(cs), evidenceFor(cs)
			},
			fails: []string{flow.CheckForbidden},
			warns: []string{flow.CheckUnknown},
		},
		{
			name: "20 лишних вызовов",
			mut: func(cs []flow.Call) ([]flow.Call, *flow.Evidence) {
				extra := cs[index(cs, "search_wikipedia")]
				for range 20 {
					cs = append(cs, extra)
				}
				return renumber(cs), evidenceFor(cs)
			},
			fails: nil,
			warns: []string{flow.CheckCalls, flow.CheckRepeats, flow.CheckChoice("search_wikipedia")},
		},
		{
			name: "cites без демона",
			mut: func(cs []flow.Call) ([]flow.Call, *flow.Evidence) {
				for i := range cs {
					if cs[i].Tool == "nb_add" {
						var a map[string]any
						_ = json.Unmarshal(cs[i].Args, &a)
						a["cites"] = []string{"read_wikipedia", "taxon_tree"}
						cs[i].Args, _ = json.Marshal(a)
					}
				}
				return cs, evidenceFor(cs)
			},
			fails: []string{flow.CheckCites},
		},
		{
			name: "сервер насчитал меньше трассы",
			mut: func(cs []flow.Call) ([]flow.Call, *flow.Evidence) {
				ev := evidenceFor(cs)
				ev.After["daemon"].Calls["mdd_get"]--
				return cs, ev
			},
			fails: []string{flow.CheckEvidence},
		},
		{
			name: "nb_add после nb_close",
			mut: func(cs []flow.Call) ([]flow.Call, *flow.Evidence) {
				i := index(cs, "nb_add")
				moved := cs[i]
				cs = slices.Delete(cs, i, i+1)
				cs = append(cs, moved)
				return renumber(cs), evidenceFor(cs)
			},
			fails: []string{flow.CheckOrder("nb_add", "nb_close")},
		},
	} {
		t.Run(m.name, func(t *testing.T) {
			calls, ev := m.mut(clone(base))
			v := flow.Verify(spec, calls, routes, ev)
			got := fails(v)
			sort.Strings(got)
			want := append([]string(nil), m.fails...)
			sort.Strings(want)
			switch {
			case m.loose:
				for _, w := range want {
					if !slices.Contains(got, w) {
						t.Errorf("нет провала %q; провалы: %q\n%s", w, got, dump(v))
					}
				}
			case !slices.Equal(got, want):
				t.Errorf("провалы %q, ждали %q\n%s", got, want, dump(v))
			}
			if v.OK != (len(want) == 0) {
				t.Errorf("OK = %v", v.OK)
			}
			for _, w := range m.warns {
				if level(v, w) != flow.LevelWarn {
					t.Errorf("%s: %q, ждали warn\n%s", w, level(v, w), dump(v))
				}
			}
		})
	}
}

// Шаговые мелочи Verify: условие на аргументы, AllowError, чужие вызовы на
// сервере, отсутствие снимка.
func TestVerifyDetails(t *testing.T) {
	good := runFlow(t, &flowtest.Brain{}, nil)
	routes, _ := good.router.Routes(context.Background())
	p := flow.Presets()[0]

	// Искали не тот вид — шаг поиска не засчитан, и пояснение говорит почему.
	v := flow.Verify(p.SpecFor("рысь"), clone(good.tr.Calls), routes, nil)
	if level(v, flow.CheckChoice("search_wikipedia")) != flow.LevelFail {
		t.Errorf("другой вид:\n%s", dump(v))
	}
	if level(v, flow.CheckEvidence) != flow.LevelWarn {
		t.Error("без снимка — предупреждение")
	}

	// facts_get ошибкой засчитывается (AllowError), но попадает в ошибки.
	cs := clone(good.tr.Calls)
	i := index(cs, "facts_get")
	cs[i].OK, cs[i].Result, cs[i].Error = false, nil, "выпусков о виде ещё не было"
	for k := range cs {
		if cs[k].Tool == "nb_add" {
			cs[k].Args = json.RawMessage(`{"notebook_id":` + string(mustField(cs[k].Args, "notebook_id")) +
				`,"heading":"x","text":"y","cites":["read_wikipedia","mdd_get"]}`)
		}
	}
	v = flow.Verify(p.SpecFor(""), cs, routes, evidenceFor(cs))
	if !v.OK || level(v, flow.CheckRepeats) != flow.LevelWarn {
		t.Errorf("facts_get ошибкой:\n%s", dump(v))
	}

	// Ссылка на facts_get, ответивший «выпуска не было»: ответ засчитан
	// шагу (AllowError) — это сведение, сослаться можно. Так делает живая
	// модель в разделе «Интересные факты». А ссылка на упавший инструмент,
	// которому ошибка не разрешена, — провал.
	for k := range cs {
		if cs[k].Tool == "nb_add" {
			cs[k].Args = json.RawMessage(`{"notebook_id":` + string(mustField(cs[k].Args, "notebook_id")) +
				`,"heading":"x","text":"y","cites":["read_wikipedia","facts_get"]}`)
		}
	}
	v = flow.Verify(p.SpecFor(""), cs, routes, evidenceFor(cs))
	if level(v, flow.CheckCites) != flow.LevelOK {
		t.Errorf("ссылка на facts_get с разрешённой ошибкой:\n%s", dump(v))
	}
	j := index(cs, "match_taxon")
	cs[j].OK, cs[j].Result, cs[j].Error = false, nil, "GBIF не ответил"
	for k := range cs {
		if cs[k].Tool == "nb_add" {
			cs[k].Args = json.RawMessage(`{"notebook_id":` + string(mustField(cs[k].Args, "notebook_id")) +
				`,"heading":"x","text":"y","cites":["match_taxon","mdd_get"]}`)
		}
	}
	v = flow.Verify(p.SpecFor(""), cs, routes, evidenceFor(cs))
	if level(v, flow.CheckCites) != flow.LevelFail {
		t.Errorf("ссылка на упавший match_taxon:\n%s", dump(v))
	}

	// Демон обслужил и других клиентов — предупреждение, не провал.
	cs = clone(good.tr.Calls)
	ev := evidenceFor(cs)
	ev.After["daemon"].Calls["facts_latest"] += 3
	v = flow.Verify(p.SpecFor(""), cs, routes, ev)
	if !v.OK || level(v, flow.CheckEvidence) != flow.LevelWarn {
		t.Errorf("чужие вызовы:\n%s", dump(v))
	}

	// Маршрут реестра разошёлся со спецификацией.
	moved := slices.Clone(routes)
	for k := range moved {
		if moved[k].Tool == "facts_get" {
			moved[k].Server = "notes"
		}
	}
	v = flow.Verify(p.SpecFor(""), clone(good.tr.Calls), moved, evidenceFor(good.tr.Calls))
	if level(v, flow.CheckRoute) != flow.LevelFail {
		t.Errorf("маршрут реестра:\n%s", dump(v))
	}

	// Мало серверов: без блокнота и демона.
	var only []flow.Call
	for _, c := range good.tr.Calls {
		if c.Server == "sources" {
			only = append(only, c)
		}
	}
	v = flow.Verify(p.SpecFor(""), renumber(only), routes, nil)
	if level(v, flow.CheckServers) != flow.LevelFail {
		t.Errorf("один сервер:\n%s", dump(v))
	}
}

func mustField(args json.RawMessage, key string) json.RawMessage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(args, &m)
	return m[key]
}
