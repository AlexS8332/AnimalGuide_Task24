package flowtest

import (
	"encoding/json"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/notes"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Brain — реактивная модель флоу «паспорт вида»: по истории запроса видит,
// какие инструменты уже отвечали, и следующий вызов строит из их ответов.
// Два независимых вызова она делает одним ответом (read_wikipedia и
// mdd_search, taxon_tree и vernacular_names) — так проверяется, что
// вызовы одного ответа делят Turn.
//
// Нулевое значение — хороший прогон о мануле. Поля включают поломки.
type Brain struct {
	Species string // query для search_wikipedia; пусто — «манул»
	MDDText string // text для mdd_search; пусто — «manul»
	Section string // раздел read_wikipedia; пусто — «Описание»

	// BadMDDID — mdd_get с этим id вместо id из выдачи mdd_search: модель
	// «вспомнила» номер (он есть в MDD, но не пришёл из ответа).
	BadMDDID int
	// Notebook — nb_add и nb_close с этим notebook_id вместо полученного из
	// nb_open (например, ForeignNotebook).
	Notebook string
	// AddAfterClose — один раздел, закрытие, затем ещё раздел.
	AddAfterClose bool
	// EarlyDone — flow_done до nb_close (код отказывает, модель исправляется).
	EarlyDone bool
	// Unknown — первым ответом вызвать инструмент с этим именем.
	Unknown string
	// NoFinish — не вызывать flow_done, а ответить текстом.
	NoFinish bool
}

// Fake — llmtest.Fake, которую ведёт Brain.
func (b *Brain) Fake() *llmtest.Fake { return &llmtest.Fake{Fn: b.Fn} }

// seen — вызов из истории и ответ на него.
type seen struct {
	name  string
	reply string // без пометки источника
	ok    bool
}

// history — вызовы инструментов по порядку ответов на них.
func history(req llm.Request) []seen {
	names := map[string]string{}
	var out []seen
	for _, m := range req.Messages {
		for _, c := range m.ToolCalls {
			names[c.ID] = c.Function.Name
		}
		if m.Role != llm.RoleTool {
			continue
		}
		content, _ := tools.Unwrap(m.Content)
		ok := true
		var e map[string]any
		if json.Unmarshal([]byte(content), &e) == nil && len(e) == 1 {
			if _, isErr := e["error"]; isErr {
				ok = false
			}
		}
		out = append(out, seen{name: names[m.ToolCallID], reply: content, ok: ok})
	}
	return out
}

func (b *Brain) Fn(req llm.Request) (llm.Response, error) {
	h := history(req)
	count := func(name string) int {
		n := 0
		for _, s := range h {
			if s.name == name {
				n++
			}
		}
		return n
	}
	last := func(name string) map[string]any {
		for i := len(h) - 1; i >= 0; i-- {
			if h[i].name == name && h[i].ok {
				dec := json.NewDecoder(strings.NewReader(h[i].reply))
				dec.UseNumber()
				var m map[string]any
				if dec.Decode(&m) == nil {
					return m
				}
			}
		}
		return map[string]any{}
	}
	call := func(name string, args map[string]any) llmtest.Call {
		data, _ := json.Marshal(args)
		return llmtest.Call{Name: name, Args: string(data)}
	}
	one := func(name string, args map[string]any) (llm.Response, error) {
		return llmtest.ToolCalls(call(name, args)), nil
	}

	species := or(b.Species, "манул")
	switch {
	case b.Unknown != "" && count(b.Unknown) == 0:
		return one(b.Unknown, map[string]any{"query": species})
	case count("search_wikipedia") == 0:
		return one("search_wikipedia", map[string]any{"query": species})
	case count("read_wikipedia") == 0:
		title := species
		if rs, ok := last("search_wikipedia")["results"].([]any); ok && len(rs) > 0 {
			if r, ok := rs[0].(map[string]any); ok {
				title, _ = r["title"].(string)
			}
		}
		return llmtest.ToolCalls(
			call("read_wikipedia", map[string]any{"title": title, "section": or(b.Section, "Описание")}),
			call("mdd_search", map[string]any{"text": or(b.MDDText, "manul")}),
		), nil
	case count("mdd_get") == 0:
		var id any = 0
		if rows, ok := last("mdd_search")["species"].([]any); ok && len(rows) > 0 {
			if row, ok := rows[0].(map[string]any); ok {
				id = row["id"]
			}
		}
		if b.BadMDDID != 0 {
			id = b.BadMDDID
		}
		return one("mdd_get", map[string]any{"id": id})
	}
	card := last("mdd_get")
	sci, _ := card["sci_name"].(string)
	switch {
	case count("match_taxon") == 0:
		return one("match_taxon", map[string]any{"scientific_name": sci})
	case count("taxon_tree") == 0:
		key := last("match_taxon")["usage_key"]
		return llmtest.ToolCalls(
			call("taxon_tree", map[string]any{"usage_key": key}),
			call("vernacular_names", map[string]any{"usage_key": key}),
		), nil
	case count("facts_get") == 0:
		return one("facts_get", map[string]any{"species": sci})
	case count(notes.ToolOpen) == 0:
		return one(notes.ToolOpen, map[string]any{"title": "Манул — паспорт вида", "species": sci})
	}

	nb, _ := last(notes.ToolOpen)["notebook_id"].(string)
	if b.Notebook != "" {
		nb = b.Notebook
	}
	factsOK := len(last("facts_get")) > 0
	sections := []map[string]any{
		{"heading": "Таксономия", "text": "Отряд Carnivora, семейство Felidae, род Otocolobus.",
			"cites": []string{"mdd_get", "match_taxon", "taxon_tree", "vernacular_names"}},
		{"heading": "Описание", "text": "Размером с домашнюю кошку, длина тела 52–65 см.",
			"cites": []string{"read_wikipedia"}},
		{"heading": "Интересные факты", "text": "У манула круглые зрачки.",
			"cites": []string{"facts_get"}},
	}
	if !factsOK {
		sections[2]["cites"] = []string{"read_wikipedia"}
	}
	add := func(i int) (llm.Response, error) {
		args := map[string]any{"notebook_id": nb}
		for k, v := range sections[i%len(sections)] {
			args[k] = v
		}
		return one(notes.ToolAdd, args)
	}
	adds, closes := count(notes.ToolAdd), count(notes.ToolClose)
	before := len(sections)
	if b.AddAfterClose {
		before = 1
	}
	switch {
	case closes == 0 && adds < before:
		return add(adds)
	case b.EarlyDone && count("flow_done") == 0:
		return one("flow_done", map[string]any{"answer": "готово"})
	case closes == 0:
		return one(notes.ToolClose, map[string]any{"notebook_id": nb})
	case b.AddAfterClose && adds < 2:
		return add(adds)
	case b.NoFinish:
		return llmtest.Text("Блокнот готов."), nil
	}
	path, _ := last(notes.ToolClose)["path"].(string)
	return one("flow_done", map[string]any{"answer": "Блокнот о мануле: " + path + ", разделы «Таксономия», «Описание», «Интересные факты»."})
}

func or(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
