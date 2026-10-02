package flow

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow/flowtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/notes"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

// Пути заготовки на настоящих ответах инструментов: Википедия и GBIF —
// настоящие инструменты над подставными HTTP-серверами, MDD — над
// хранилищем в памяти. Главный риск заготовки — путь, которого нет в
// реальном ответе («results.*.title» против «hits.*.title»): такая
// зависимость провалила бы каждый живой прогон, а тест с выдуманными
// ответами этого бы не заметил.
func TestPassportPathsOnRealReplies(t *testing.T) {
	r := flowtest.NewRouter()
	defer r.Close()
	ctx := context.Background()
	bound, _ := r.Tools(ctx)
	byName := map[string]tools.Tool{}
	for _, b := range bound {
		byName[b.Route.Tool] = b.Tool
	}
	call := func(tool, args string) any {
		t.Helper()
		out, err := byName[tool].Call(ctx, json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s %s: %v", tool, args, err)
		}
		return decode(json.RawMessage(out))
	}
	values := func(res any, path string) []string {
		var out []string
		for _, f := range extract(res, path) {
			s, _ := scalar(f.val)
			out = append(out, s)
		}
		return out
	}
	nb := call(notes.ToolOpen, `{"title":"Манул","species":"Otocolobus manul"}`)
	replies := map[string]any{
		"wiki_search": call("search_wikipedia", `{"query":"манул"}`),
		"mdd_search":  call("mdd_search", `{"text":"manul"}`),
		"mdd_get":     call("mdd_get", `{"id":1006010}`),
		"match":       call("match_taxon", `{"scientific_name":"Otocolobus manul"}`),
		"nb_open":     nb,
	}
	want := map[string]string{
		"results.*.title":         "Манул",
		"species.*.id":            strconv.Itoa(mddtest.Manul),
		"sci_name":                "Otocolobus manul",
		"usage_key":               "2435270",
		"id|sci_name|common_name": "1006010",
	}
	p, _ := Find("passport")
	for _, f := range p.Spec.Flows {
		got := values(replies[f.From], f.Path)
		if len(got) == 0 {
			t.Errorf("%s: в ответе нет пути %s", f.From, f.Path)
			continue
		}
		if w, ok := want[f.Path]; ok && !slices.Contains(got, w) {
			t.Errorf("%s.%s = %v, нет %s", f.From, f.Path, got, w)
		}
	}
	if got := values(replies["mdd_get"], "id|sci_name|common_name"); !slices.Equal(got, []string{"1006010", "Otocolobus manul", "Pallas's Cat"}) {
		t.Errorf("альтернативы пути: %v", got)
	}
	if got := values(replies["match"], "usage_key"); len(got) != 1 || got[0] != "2435270" {
		t.Errorf("usage_key: %v (ключ манула %d)", got, toolstest.KeyManul)
	}

	// Аргументы, в которые приходят значения и на которые стоят условия, —
	// настоящие имена из схем инструментов: опечатка в Arg дала бы «нет
	// аргумента» в каждом прогоне.
	props := func(tool string) map[string]any {
		var s struct {
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(byName[tool].Spec().Parameters, &s)
		return s.Properties
	}
	stepTool := map[string]string{}
	for _, st := range p.Spec.Steps {
		stepTool[st.ID] = st.Tool
		if byName[st.Tool] == nil {
			t.Errorf("шаг %s: инструмента %s у реестра нет", st.ID, st.Tool)
		}
		for arg := range st.Args {
			if _, ok := props(st.Tool)[arg]; !ok {
				t.Errorf("шаг %s: у %s нет аргумента %s", st.ID, st.Tool, arg)
			}
		}
	}
	for _, f := range p.Spec.Flows {
		if _, ok := props(stepTool[f.To])[f.Arg]; !ok {
			t.Errorf("поток %s → %s: у %s нет аргумента %s", f.From, f.To, stepTool[f.To], f.Arg)
		}
	}
	// cites — поле контракта notes, а не подставного блокнота.
	field, _ := reflect.TypeOf(notes.AddArgs{}).FieldByName("Cites")
	if tag := strings.Split(field.Tag.Get("json"), ",")[0]; tag != p.Spec.CiteArg {
		t.Errorf("CiteArg %q, в notes.AddArgs — %q", p.Spec.CiteArg, tag)
	}
}

func TestExtractAndMatch(t *testing.T) {
	res := decode(json.RawMessage(`{"species":[{"id":1006010,"sci_name":"Otocolobus manul"},{"id":1006007}],"n":2435270,"tags":["a","b"]}`))
	got := extract(res, "species.*.id")
	if len(got) != 2 || got[0].path != "species.0.id" || got[1].path != "species.1.id" {
		t.Fatalf("species.*.id: %+v", got)
	}
	if s, _ := scalar(got[0].val); s != "1006010" {
		t.Errorf("число: %q", s)
	}
	if got := extract(res, "tags"); len(got) != 2 || got[1].path != "tags.1" {
		t.Errorf("массив на конце: %+v", got)
	}
	if got := extract(res, "species.1.sci_name|n"); len(got) != 1 || got[0].path != "n" {
		t.Errorf("альтернативы: %+v", got)
	}
	for _, c := range []struct {
		arg  any
		val  any
		mode string
		ok   bool
	}{
		{json.Number("1006010"), json.Number("1006010"), FlowEq, true},
		{"1006010", json.Number("1006010"), FlowEq, true}, // строка с числом = число
		{json.Number("1006010.0"), json.Number("1006010"), FlowEq, true},
		{json.Number("1006011"), json.Number("1006010"), FlowEq, false},
		{"otocolobus MANUL", "Otocolobus manul", FlowEqFold, true},
		{"otocolobus MANUL", "Otocolobus manul", FlowEq, false},
		{" Манул ", "Манул", FlowEq, true},
		{"", "", FlowEq, false},
		{"паспорт манула", "манул", FlowContains, true},
		{[]any{"x", "Манул"}, "манул", FlowContains, true},
		{[]any{"x"}, "манул", FlowContains, false},
	} {
		if got := flowMatch(c.arg, c.val, c.mode); got != c.ok {
			t.Errorf("flowMatch(%v, %v, %s) = %v", c.arg, c.val, c.mode, got)
		}
	}
}

func TestPresets(t *testing.T) {
	p, err := Find("")
	if err != nil || p.ID != "passport" || p.Species != "манул" {
		t.Fatalf("по умолчанию: %+v %v", p, err)
	}
	if _, err := Find("nope"); err == nil || !strings.Contains(err.Error(), "passport") {
		t.Errorf("неизвестная: %v", err)
	}
	if task := p.TaskFor("снежный барс"); !strings.Contains(task, "«снежный барс»") || strings.Contains(task, "%s") {
		t.Errorf("задача: %s", task)
	}
	for _, w := range []string{"Таксономия", "Описание", "Интересные факты", "cites", FinishTool} {
		if !strings.Contains(p.Task, w) {
			t.Errorf("в задаче нет %q", w)
		}
	}
	s := p.SpecFor("Lynx (lynx)")
	if re := s.Steps[0].Args["query"].Regexp; re != `(?i)Lynx \(lynx\)` {
		t.Errorf("вид в условии: %q", re)
	}
	if p.Spec.Steps[0].Args["query"].Regexp != "(?i)"+SpeciesMark {
		t.Error("SpecFor поменял исходную заготовку")
	}
	ids := map[string]bool{}
	for _, st := range p.Spec.Steps {
		ids[st.ID] = true
	}
	for _, f := range p.Spec.Flows {
		if !ids[f.From] || !ids[f.To] {
			t.Errorf("поток ссылается на неизвестный шаг: %+v", f)
		}
	}
	for _, o := range p.Spec.Before {
		if !ids[o.A] || !ids[o.B] {
			t.Errorf("порядок ссылается на неизвестный шаг: %+v", o)
		}
	}
}
