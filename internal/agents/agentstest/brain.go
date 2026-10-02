// Package agentstest — подставная модель, которая ведёт себя как
// добросовестные агенты справочника: отвечает по тому, кто спрашивает
// (системный промпт) и что уже вернули инструменты. Порядок запросов при
// параллельных специалистах не определён, поэтому сценарий смотрит на
// содержимое запроса, а не на номер.
package agentstest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Brain — подставная модель, которая ведёт себя как добросовестный агент:
// отвечает по тому, кто спрашивает (системный промпт) и что уже вернули
// инструменты. Отклонения от добросовестности включаются полями.
type Brain struct {
	mu sync.Mutex
	// GateNo — названия, на которые привратник отвечает НЕТ.
	GateNo []string
	// FakeLatin — идентификатор сначала сдаёт эту латынь, не сверив её.
	FakeLatin string
	// LeadScript — ответы ведущего по шагам (сколько ответов инструментов
	// уже в запросе).
	LeadScript func(req llm.Request, step int) llm.Response
	// TextOnly — идентификатор отвечает текстом вместо инструмента.
	TextOnly bool
	// Extract — ответ извлекателя памяти и профиля; пусто — «правок нет».
	Extract func(req llm.Request) (string, error)
	// Compiler — ответы составителя подборки по шагам.
	Compiler func(req llm.Request, step int) llm.Response
	// Judge — ответ судьи свода; пусто — «нарушений нет» по каждому
	// правилу, которое ему показали.
	Judge func(req llm.Request) string
	calls map[string]int
}

func (b *Brain) count(who string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.calls == nil {
		b.calls = map[string]int{}
	}
	b.calls[who]++
}

// Calls — сколько запросов получил агент (gatekeeper, identifier,
// section, comparer, lead).
func (b *Brain) Calls(who string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[who]
}

// LastUser — последняя реплика пользователя в запросе.
func LastUser(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == llm.RoleUser {
			return req.Messages[i].Content
		}
	}
	return ""
}

// TurnSteps — сколько ответов инструментов пришло после последней реплики
// пользователя: шаг текущего хода, без истории окна.
func TurnSteps(req llm.Request) int {
	n := 0
	for i := len(req.Messages) - 1; i >= 0 && req.Messages[i].Role != llm.RoleUser; i-- {
		if req.Messages[i].Role == llm.RoleTool {
			n++
		}
	}
	return n
}

// ToolReplies — ответы инструментов запроса с их именами, без пометки
// источника.
func ToolReplies(req llm.Request) []struct{ Name, Out string } {
	names := map[string]string{}
	var out []struct{ Name, Out string }
	for _, m := range req.Messages {
		for _, c := range m.ToolCalls {
			names[c.ID] = c.Function.Name
		}
		if m.Role == llm.RoleTool {
			data, _ := tools.Unwrap(m.Content)
			out = append(out, struct{ Name, Out string }{names[m.ToolCallID], data})
		}
	}
	return out
}

// LastReply — последний ответ инструмента с этим именем.
func LastReply(req llm.Request, name string) string {
	rs := ToolReplies(req)
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Name == name {
			return rs[i].Out
		}
	}
	return ""
}

var judgedRe = regexp.MustCompile(`(?m)^\[([^\]]+)\] `)

// Verdicts — ответ судьи: по записи на каждое правило, показанное ему в
// запросе; нарушены — те, что в broken.
func Verdicts(req llm.Request, broken map[string]string) string {
	type verdict struct {
		Invariant string `json:"invariant"`
		Violates  bool   `json:"violates"`
		Why       string `json:"why"`
	}
	var out struct {
		Verdicts []verdict `json:"verdicts"`
	}
	out.Verdicts = []verdict{}
	for _, m := range judgedRe.FindAllStringSubmatch(LastUser(req), -1) {
		why, bad := broken[m[1]]
		if !bad {
			why = "ответ правило не нарушает"
		}
		out.Verdicts = append(out.Verdicts, verdict{Invariant: m[1], Violates: bad, Why: why})
	}
	return Args(out)
}

var latinRe = regexp.MustCompile(`лат\. ([A-Z][a-z]+ [a-z]+)`)
var quotedRe = regexp.MustCompile(`«([^»]+)»`)

// Args — аргументы вызова в JSON.
func Args(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// Chat — ответ на запрос; годится как llmtest.Fake.Fn.
func (b *Brain) Chat(req llm.Request) (llm.Response, error) {
	sys := req.Messages[0].Content
	switch {
	case strings.HasPrefix(sys, "Ты — зоолог-систематик"):
		b.count("gatekeeper")
		name := strings.ToLower(LastUser(req))
		for _, no := range b.GateNo {
			if strings.Contains(name, no) {
				return llmtest.Text("- | НЕТ"), nil
			}
		}
		return llmtest.Text("Lynx lynx | ДА"), nil
	case strings.Contains(sys, "агент-идентификатор"):
		b.count("identifier")
		return b.identifier(req), nil
	case strings.Contains(sys, "агент-специалист по разделу"):
		b.count("section")
		return b.section(req), nil
	case strings.Contains(sys, "агент сравнения"):
		b.count("comparer")
		return b.comparer(req), nil
	case strings.HasPrefix(sys, "Ты ведёшь подборку справочника"):
		b.count("compiler")
		if b.Compiler == nil {
			return llmtest.Text("Подборка идёт."), nil
		}
		return b.Compiler(req, TurnSteps(req)), nil
	case strings.HasPrefix(sys, "Ты — судья свода"):
		b.count("judge")
		if b.Judge != nil {
			return llmtest.Text(b.Judge(req)), nil
		}
		return llmtest.Text(Verdicts(req, nil)), nil
	case strings.HasPrefix(sys, "Ты ведёшь память и профиль"):
		b.count("extract")
		if b.Extract == nil {
			return llmtest.Text(`{"profile":{"set":[]},"memory":{"set":[]},"facts":{"set":[]}}`), nil
		}
		text, err := b.Extract(req)
		if err != nil {
			return llm.Response{}, err
		}
		return llmtest.Text(text), nil
	case strings.Contains(sys, "ведёшь разговор справочника"):
		b.count("lead")
		if b.LeadScript != nil {
			return b.LeadScript(req, len(ToolReplies(req))), nil
		}
		return llmtest.Text("Ответ ведущего."), nil
	}
	return llm.Response{}, fmt.Errorf("неизвестный агент: %.60s", sys)
}

func (b *Brain) identifier(req llm.Request) llm.Response {
	query := quotedRe.FindStringSubmatch(LastUser(req))
	q := ""
	if query != nil {
		q = query[1]
	}
	if b.TextOnly {
		return llmtest.Text(`{"name_ru":"Рысь","latin":"Lynx rufus","wiki_title":"Рысь","summary":"по памяти"}`)
	}
	search := LastReply(req, "search_wikipedia")
	if search == "" {
		return llmtest.ToolCall("search_wikipedia", Args(map[string]string{"query": q}))
	}
	var hits struct {
		Results []struct{ Title string } `json:"results"`
	}
	json.Unmarshal([]byte(search), &hits)
	title := ""
	for _, h := range hits.Results {
		// Добросовестно: берём статью о том же животном, похожее — нет.
		if strings.Contains(strings.ToLower(h.Title), strings.ToLower(lastWord(q))) && !strings.Contains(strings.ToLower(q), "полосат") {
			title = h.Title
		}
	}
	if title == "" {
		return llmtest.ToolCall("report_not_found", `{"reason":"статьи именно об этом животном нет"}`)
	}
	read := LastReply(req, "read_wikipedia")
	if read == "" {
		return llmtest.ToolCall("read_wikipedia", Args(map[string]string{"title": title}))
	}
	var art struct {
		Title string `json:"title"`
		Intro string `json:"intro"`
	}
	json.Unmarshal([]byte(read), &art)
	m := latinRe.FindStringSubmatch(art.Intro)
	if m == nil {
		return llmtest.ToolCall("report_not_found", `{"reason":"латынь не найдена"}`)
	}
	latin := m[1]
	if b.FakeLatin != "" && !refused(req) {
		return llmtest.ToolCall("submit_card", Args(map[string]any{"name_ru": q, "latin": b.FakeLatin, "wiki_title": art.Title, "summary": "по памяти"}))
	}
	if LastReply(req, "match_taxon") == "" {
		return llmtest.ToolCall("match_taxon", Args(map[string]string{"scientific_name": latin}))
	}
	return llmtest.ToolCall("submit_card", Args(map[string]any{
		"name_ru": art.Title, "latin": latin, "wiki_title": art.Title, "summary": firstSentence(art.Intro),
		"tree_ru": []map[string]string{{"name": "Felidae", "name_ru": "Кошачьи"}, {"name": "Mammalia", "name_ru": "Млекопитающие"}},
	}))
}

// refused — получала ли модель в этом прогоне отказ завершающего
// инструмента: после отказа добросовестная модель делает, что велено.
func refused(req llm.Request) bool {
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool && strings.Contains(m.Content, "Не принято") {
			return true
		}
	}
	return false
}

func lastWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return s
	}
	w := f[len(f)-1]
	if r := []rune(w); len(r) > 4 {
		return string(r[:len(r)-1])
	}
	return w
}

func firstSentence(s string) string {
	if i := strings.Index(s, "."); i > 0 {
		return s[:i+1]
	}
	return s
}

func (b *Brain) section(req llm.Request) llm.Response {
	user := LastUser(req)
	title := quotedRe.FindStringSubmatch(user)[1]
	topic := ""
	if i := strings.LastIndex(user, "Тема: "); i >= 0 {
		topic = strings.TrimSuffix(strings.TrimSpace(user[i+len("Тема: "):]), ".")
	}
	// Кандидаты по теме, как в промпте.
	want := map[string]string{"Питание": "питание", "Ареал": "распространение", "Статус охраны": "охран",
		"Размножение": "размножение", "Образ жизни": "образ жизни"}[topic]
	read := LastReply(req, "read_wikipedia")
	if read == "" {
		return llmtest.ToolCall("read_wikipedia", Args(map[string]string{"title": title, "section": want}))
	}
	var sec struct {
		Found   bool   `json:"found"`
		Section string `json:"section"`
		Text    string `json:"text"`
	}
	json.Unmarshal([]byte(read), &sec)
	if !sec.Found {
		return llmtest.ToolCall("submit_section", Args(map[string]any{"found": false, "section": "", "text": "в статье нет раздела о теме «" + topic + "»"}))
	}
	return llmtest.ToolCall("submit_section", Args(map[string]any{"found": true, "section": sec.Section, "text": "Пересказ: " + sec.Text}))
}

func (b *Brain) comparer(req llm.Request) llm.Response {
	titles := quotedRe.FindAllStringSubmatch(LastUser(req), -1)
	a, c := titles[0][1], titles[1][1]
	reads := 0
	for _, r := range ToolReplies(req) {
		if r.Name == "read_wikipedia" {
			reads++
		}
	}
	switch reads {
	case 0:
		return llmtest.ToolCalls(
			llmtest.Call{Name: "read_wikipedia", Args: Args(map[string]string{"title": a, "section": "Распространение"})},
			llmtest.Call{Name: "read_wikipedia", Args: Args(map[string]string{"title": c, "section": "Распространение"})})
	}
	return llmtest.ToolCall("submit_comparison", Args(map[string]any{"rows": []map[string]string{
		{"aspect": "Ареал", "a": "леса Евразии", "a_section": "Распространение", "b": "степи Азии", "b_section": "Распространение"},
		{"aspect": "Размеры", "a": "крупная", "a_section": "Внешний вид", "b": "сведений нет", "b_section": ""},
	}}))
}
