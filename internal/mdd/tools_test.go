package mdd_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

func loadedTools(t *testing.T) map[string]tools.Tool {
	t.Helper()
	st := mdd.NewMemory()
	if err := st.Replace(context.Background(), mddtest.Sample()); err != nil {
		t.Fatal(err)
	}
	return byName(mdd.Tools(st))
}

func byName(ts []tools.Tool) map[string]tools.Tool {
	m := map[string]tools.Tool{}
	for _, tl := range ts {
		m[tl.Spec().Name] = tl
	}
	return m
}

// call вызывает инструмент и разбирает ответ.
func call(t *testing.T, tl tools.Tool, args string, out any) string {
	t.Helper()
	res, err := tl.Call(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s(%s): %v", tl.Spec().Name, args, err)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(res), out); err != nil {
			t.Fatalf("%s: ответ не JSON: %v\n%s", tl.Spec().Name, err, res)
		}
	}
	return res
}

func callErr(t *testing.T, tl tools.Tool, args string) string {
	t.Helper()
	res, err := tl.Call(context.Background(), json.RawMessage(args))
	if err == nil {
		t.Fatalf("%s(%s): ждали ошибку, получено %s", tl.Spec().Name, args, res)
	}
	return err.Error()
}

func TestToolSpecs(t *testing.T) {
	ts := mdd.Tools(mdd.NewMemory())
	if got := tools.Names(ts); !reflect.DeepEqual(got, mdd.ToolNames) {
		t.Fatalf("имена %v, ждали %v", got, mdd.ToolNames)
	}
	if _, err := tools.NewRegistry(ts...); err != nil {
		t.Fatal(err)
	}
	for _, tl := range ts {
		s := tl.Spec()
		if !s.Untrusted || s.Via != tools.ViaLocal {
			t.Errorf("%s: Untrusted=%v Via=%q", s.Name, s.Untrusted, s.Via)
		}
		if !strings.Contains(s.Description, "млекопитающих") || !strings.Contains(s.Description, "русских нет") {
			t.Errorf("%s: описание не объясняет источник: %s", s.Name, s.Description)
		}
		var schema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Additional *bool                      `json:"additionalProperties"`
		}
		if err := json.Unmarshal(s.Parameters, &schema); err != nil {
			t.Fatalf("%s: схема не JSON: %v", s.Name, err)
		}
		if schema.Type != "object" || len(schema.Properties) == 0 || schema.Additional == nil || *schema.Additional {
			t.Errorf("%s: схема %s", s.Name, s.Parameters)
		}
	}
}

type card struct {
	mdd.Species
	URL    string `json:"url"`
	Source string `json:"source"`
}

func TestGet(t *testing.T) {
	ts := loadedTools(t)
	get := ts[mdd.ToolGet]
	want := mddtest.Sample().Species[7]
	if want.ID != mddtest.Manul {
		t.Fatalf("в наборе не манул: %d", want.ID)
	}
	want.Phylosort = 0 // в ответ не идёт

	for _, args := range []string{`{"id":1006010}`, `{"name":"Otocolobus manul"}`, `{"name":" otocolobus_manul "}`,
		`{"name":"Pallas's Cat"}`, `{"name":"MANUL"}`} {
		var c card
		call(t, get, args, &c)
		if !reflect.DeepEqual(c.Species, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", args, c.Species, want)
		}
		if c.URL != "https://www.mammaldiversity.org/taxon/1006010/" || c.Source != "MDD v2.5 (2026-07-28)" {
			t.Errorf("%s: url %q, source %q", args, c.URL, c.Source)
		}
	}

	// Длинные тексты обрезаются с пометкой.
	var p card
	call(t, get, `{"id":1000001}`, &p)
	if !strings.HasSuffix(p.TaxonomyNotes, "…[обрезано]") || len([]rune(p.TaxonomyNotes)) > 620 {
		t.Errorf("заметки не обрезаны: %d рун", len([]rune(p.TaxonomyNotes)))
	}
	if strings.HasSuffix(p.TypeLocality, "[обрезано]") || !strings.HasPrefix(p.TypeLocality, `"In Australasia."`) {
		t.Errorf("типовое местонахождение: %q", p.TypeLocality)
	}
}

func TestGetErrors(t *testing.T) {
	get := loadedTools(t)[mdd.ToolGet]
	cases := map[string]string{
		`{}`:                           "нужен один аргумент",
		``:                             "нужен один аргумент",
		`{"name":"  "}`:                "нужен один аргумент",
		`{"id":1006010,"name":"Lion"}`: "не оба",
		`{"id":0}`:                     "положительным",
		`{"id":-5}`:                    "положительным",
		`{"id":"1006010"}`:             "аргументы не разобрались",
		`{"sci_name":"Lynx lynx"}`:     "аргументы не разобрались",
		`{"name":"Манул"}`:             "mdd_search",
		`{"name":"Pallas"}`:            "не найден",
		`{"id":42}`:                    "mdd-id 42",
	}
	for args, want := range cases {
		if msg := callErr(t, get, args); !strings.Contains(msg, want) {
			t.Errorf("%s: %q, ждали «%s»", args, msg, want)
		}
	}
	if msg := callErr(t, get, `{"name":"Манул"}`); !strings.Contains(msg, "русских названий в MDD нет") {
		t.Errorf("совет про русское название: %q", msg)
	}
}

type searchOut struct {
	Source   string `json:"source"`
	Total    int    `json:"total"`
	Returned int    `json:"returned"`
	Offset   int    `json:"offset"`
	Species  []struct {
		ID         int    `json:"id"`
		SciName    string `json:"sci_name"`
		CommonName string `json:"common_name"`
		Family     string `json:"family"`
		IUCN       string `json:"iucn"`
		URL        string `json:"url"`
	} `json:"species"`
	NextOffset int    `json:"next_offset"`
	Hint       string `json:"hint"`
}

func (s searchOut) ids() []int {
	out := []int{}
	for _, r := range s.Species {
		out = append(out, r.ID)
	}
	return out
}

func TestSearch(t *testing.T) {
	search := loadedTools(t)[mdd.ToolSearch]
	n := len(mddtest.SampleOrder)

	var all searchOut
	call(t, search, `{}`, &all)
	if all.Total != n || all.Returned != n || !reflect.DeepEqual(all.ids(), mddtest.SampleOrder) || all.Hint != "" || all.NextOffset != 0 {
		t.Errorf("пустой запрос: %+v", all)
	}
	if all.Source != "MDD v2.5 (2026-07-28)" {
		t.Errorf("source %q", all.Source)
	}
	var none searchOut
	call(t, search, ``, &none)
	if none.Total != n {
		t.Errorf("без аргументов: total %d", none.Total)
	}

	var page searchOut
	call(t, search, `{"limit":3}`, &page)
	if page.Total != n || page.Returned != 3 || page.NextOffset != 3 || !strings.Contains(page.Hint, "offset=3") {
		t.Errorf("первая страница: %+v", page)
	}
	var last searchOut
	call(t, search, `{"limit":3,"offset":9}`, &last)
	if last.Returned != 2 || last.NextOffset != 0 || last.Hint != "" || last.Offset != 9 {
		t.Errorf("последняя страница: %+v", last)
	}
	var beyond searchOut
	call(t, search, `{"offset":50}`, &beyond)
	if beyond.Returned != 0 || beyond.Total != n || !strings.Contains(beyond.Hint, "за пределами") || beyond.Species == nil {
		t.Errorf("за пределами: %+v", beyond)
	}

	var manul searchOut
	call(t, search, `{"text":"manul"}`, &manul)
	if manul.Total != 1 || manul.Species[0].SciName != "Otocolobus manul" || manul.Species[0].CommonName != "Pallas's Cat" ||
		manul.Species[0].Family != "Felidae" || manul.Species[0].IUCN != "LC" ||
		manul.Species[0].URL != "https://www.mammaldiversity.org/taxon/1006010/" {
		t.Errorf("манул: %+v", manul)
	}

	cases := map[string][]int{
		`{"country":"azerbaijan"}`:           {mddtest.RedFox, mddtest.Lynx, mddtest.Manul},
		`{"family":"Felidae","iucn":["vu"]}`: {mddtest.Lion, mddtest.SnowLeopard},
		`{"iucn":["CR","EN"]}`:               {mddtest.BlackRhino},
		`{"extinct":true}`:                   {mddtest.Thylacine},
		`{"domestic":true}`:                  {mddtest.DomesticCat},
		`{"order":"carnivora","domestic":false,"realm":"Palearctic","genus":"panthera"}`: {mddtest.SnowLeopard},
		`{"text":"  snow_leopard "}`: {mddtest.SnowLeopard},
	}
	for args, want := range cases {
		var out searchOut
		call(t, search, args, &out)
		if !reflect.DeepEqual(out.ids(), want) || out.Total != len(want) {
			t.Errorf("%s: %v, ждали %v", args, out.ids(), want)
		}
	}

	var empty searchOut
	call(t, search, `{"text":"манул"}`, &empty)
	if empty.Total != 0 || empty.Species == nil || !strings.Contains(empty.Hint, "русских нет") {
		t.Errorf("пустая выдача: %+v", empty)
	}

	var capped searchOut
	call(t, search, `{"limit":500}`, &capped)
	if capped.Returned != n {
		t.Errorf("limit 500: %d", capped.Returned)
	}

	for args, want := range map[string]string{
		`{"iucn":["XX"]}`:   "неизвестный статус МСОП",
		`{"limit":-1}`:      "отрицательными",
		`{"offset":-1}`:     "отрицательными",
		`{"iucn":"VU"}`:     "аргументы не разобрались",
		`{"class":"Aves"}`:  "аргументы не разобрались",
		`{"extinct":"yes"}`: "аргументы не разобрались",
	} {
		if msg := callErr(t, search, args); !strings.Contains(msg, want) {
			t.Errorf("%s: %q, ждали «%s»", args, msg, want)
		}
	}
}

type changesOut struct {
	Source      string       `json:"source"`
	Version     string       `json:"version"`
	PrevVersion string       `json:"prev_version"`
	Category    string       `json:"category"`
	Returned    int          `json:"returned"`
	Changes     []mdd.Change `json:"changes"`
	Hint        string       `json:"hint"`
}

func TestChanges(t *testing.T) {
	changes := loadedTools(t)[mdd.ToolChanges]

	var all changesOut
	call(t, changes, `{}`, &all)
	if all.Version != "v2.5" || all.PrevVersion != "v2.4" || all.Returned != 5 || len(all.Changes) != 5 || all.Hint != "" {
		t.Errorf("все: %+v", all)
	}
	for _, c := range all.Changes {
		if len([]rune(c.Reference)) > 320 {
			t.Errorf("ссылка не обрезана: %d рун", len([]rune(c.Reference)))
		}
	}

	var novo changesOut
	call(t, changes, `{"category":" De Novo "}`, &novo)
	if novo.Category != "de novo" || novo.Returned != 2 {
		t.Errorf("de novo: %+v", novo)
	}
	for _, c := range novo.Changes {
		if c.Category != "de novo" || c.OldName != "" {
			t.Errorf("de novo: %+v", c)
		}
	}

	var two changesOut
	call(t, changes, `{"limit":2}`, &two)
	if two.Returned != 2 || !strings.Contains(two.Hint, "limit") {
		t.Errorf("limit 2: %+v", two)
	}

	var unknown changesOut
	call(t, changes, `{"category":"merge"}`, &unknown)
	if unknown.Returned != 0 || unknown.Changes == nil || !strings.Contains(unknown.Hint, "Категории MDD") {
		t.Errorf("нет категории: %+v", unknown)
	}

	for args, want := range map[string]string{
		`{"limit":-3}`:       "отрицательным",
		`{"category":5}`:     "аргументы не разобрались",
		`{"version":"v2.4"}`: "аргументы не разобрались",
	} {
		if msg := callErr(t, changes, args); !strings.Contains(msg, want) {
			t.Errorf("%s: %q, ждали «%s»", args, msg, want)
		}
	}
}

func TestToolsEmptyStore(t *testing.T) {
	ts := byName(mdd.Tools(mdd.NewMemory()))
	for name, args := range map[string]string{
		mdd.ToolGet:     `{"name":"Otocolobus manul"}`,
		mdd.ToolSearch:  `{}`,
		mdd.ToolChanges: `{}`,
	} {
		if msg := callErr(t, ts[name], args); !strings.Contains(msg, "ещё не загружен") {
			t.Errorf("%s на пустой базе: %q", name, msg)
		}
	}
	// Неверные аргументы важнее пустой базы: модель узнаёт об ошибке сразу.
	if msg := callErr(t, ts[mdd.ToolGet], `{}`); !strings.Contains(msg, "нужен один аргумент") {
		t.Errorf("аргументы на пустой базе: %q", msg)
	}
}

// TestToolsOverMCP — те же объекты, зарегистрированные в MCP-сервере
// проекта, отвечают через клиентскую сессию SDK побайтно так же, как в
// процессе; ошибки приходят результатом с IsError и тем же текстом.
func TestToolsOverMCP(t *testing.T) {
	ctx := context.Background()
	st := mdd.NewMemory()
	if err := st.Replace(ctx, mddtest.Sample()); err != nil {
		t.Fatal(err)
	}
	local := mdd.Tools(st)
	srv := mcp.NewServer(local, mcp.ServerOptions{})

	ct, stt := sdk.NewInMemoryTransports()
	ss, err := srv.SDK().Connect(ctx, stt, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cl := sdk.NewClient(&sdk.Implementation{Name: "mdd-test", Version: "0"}, nil)
	cs, err := cl.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	remote := map[string]*sdk.Tool{}
	for _, tl := range list.Tools {
		remote[tl.Name] = tl
	}
	for _, tl := range local {
		s := tl.Spec()
		r, ok := remote[s.Name]
		if !ok {
			t.Fatalf("%s не зарегистрирован в MCP", s.Name)
		}
		schema, _ := json.Marshal(r.InputSchema)
		if r.Description != s.Description || string(tools.Canon(schema)) != string(tools.Canon(s.Parameters)) {
			t.Errorf("%s: описание или схема разошлись:\n%s\n%s", s.Name, tools.Canon(schema), tools.Canon(s.Parameters))
		}
	}

	calls := []struct {
		name, args string
		fail       bool
	}{
		{mdd.ToolGet, `{"id":1006010}`, false},
		{mdd.ToolGet, `{"name":"Pallas's Cat"}`, false},
		{mdd.ToolGet, `{"id":1000001}`, false},
		{mdd.ToolSearch, `{"family":"Felidae","limit":2}`, false},
		{mdd.ToolSearch, `{"iucn":["VU","CR"],"country":"India"}`, false},
		{mdd.ToolChanges, `{"category":"de novo"}`, false},
		{mdd.ToolGet, `{"name":"Манул"}`, true},
		{mdd.ToolSearch, `{"iucn":["XX"]}`, true},
	}
	byname := byName(local)
	for _, c := range calls {
		want, lerr := byname[c.name].Call(ctx, json.RawMessage(c.args))
		if (lerr != nil) != c.fail {
			t.Fatalf("%s(%s) в процессе: %v", c.name, c.args, lerr)
		}
		if lerr != nil {
			want = lerr.Error()
		}
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: c.name, Arguments: json.RawMessage(c.args)})
		if err != nil {
			t.Fatalf("%s(%s) через MCP: %v", c.name, c.args, err)
		}
		if res.IsError != c.fail || len(res.Content) != 1 {
			t.Fatalf("%s(%s): IsError=%v, %d блоков", c.name, c.args, res.IsError, len(res.Content))
		}
		got := res.Content[0].(*sdk.TextContent).Text
		if got != want {
			t.Errorf("%s(%s) разошлись:\n  mcp %s\nlocal %s", c.name, c.args, got, want)
		}
	}
}
