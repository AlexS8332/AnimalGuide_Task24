// Package flowtest — подставной реестр серверов и реактивная модель для
// тестов флоу без сети (ИП-11).
//
// Router отдаёт настоящие инструменты там, где их можно поднять без сети:
// Википедия и GBIF — поверх подставных HTTP-серверов toolstest, MDD — над
// хранилищем в памяти с набором mddtest. Так пути заготовки (results.*.title,
// species.*.id, usage_key…) сверяются с настоящими формами ответов, а не с
// тем, как их себе представляет тест. Блокнот и facts_get — подставные, но
// по контракту notes и форме ответа демона.
//
// Brain — модель, которая строит аргументы из ответов инструментов в
// истории запроса, как это делала бы настоящая: id из выдачи поиска,
// usage_key из сверки, notebook_id из nb_open. Поломки (выдуманный id,
// чужой блокнот, раздел после закрытия) включаются полями.
package flowtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/hub"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/notes"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

// Имена серверов — как во встроенной конфигурации реестра.
const (
	Sources = "sources"
	Daemon  = "daemon"
	Notes   = "notes"
)

// ForeignNotebook — блокнот, который открыл другой клиент сервера блокнота:
// сервер его знает и принимает в него разделы, но в трассе флоу его nb_open
// нет.
const ForeignNotebook = "nb-f0f0f0f0f0f0"

// FactsIssueID — номер выпуска о мануле у подставного демона.
const FactsIssueID = 42

// Router — подставной реестр: три сервера, маршруты со скрытыми, счётчики
// вызовов как у server_info. Безопасен для одновременных вызовов.
type Router struct {
	Wiki *toolstest.Wiki
	GBIF *toolstest.GBIF
	// NoFacts — у демона нет выпуска о виде: facts_get отвечает ошибкой.
	NoFacts bool
	// SnapshotErr — Snapshot отвечает этой ошибкой.
	SnapshotErr error

	mu     sync.Mutex
	served map[string]map[string]int
	pids   map[string]int
	books  map[string]*notebook
	opened int
	bound  []hub.Bound
	routes []hub.Route
}

// NewRouter поднимает подставные источники и собирает реестр; Close —
// обязателен.
func NewRouter() *Router {
	r := &Router{
		Wiki:   toolstest.NewWiki(),
		GBIF:   toolstest.NewGBIF(),
		served: map[string]map[string]int{},
		pids:   map[string]int{Sources: 4101, Daemon: 4102, Notes: 4103},
		books:  map[string]*notebook{ForeignNotebook: {id: ForeignNotebook, title: "Чужой блокнот"}},
	}
	store := mdd.NewMemory()
	if err := store.Replace(context.Background(), mddtest.Sample()); err != nil {
		panic(err)
	}
	f := tools.NewFetcher()
	for _, t := range tools.LocalTools(f, r.Wiki.URL, r.GBIF.URL) {
		r.bind(Sources, t)
	}
	for _, t := range mdd.Tools(store) {
		r.bind(Daemon, t)
	}
	r.bind(Daemon, r.factsGet())
	for _, t := range r.notebookTools() {
		r.bind(Notes, t)
	}
	for _, name := range tools.SourceTools {
		r.routes = append(r.routes, hub.Route{Tool: name, Server: Daemon, Hidden: true, Reason: "дубль → " + Sources})
	}
	for _, name := range []string{"run_now", "summary_build", "search", "summarize", "save_to_file"} {
		r.routes = append(r.routes, hub.Route{Tool: name, Server: Daemon, Hidden: true, Reason: "не разрешён"})
	}
	return r
}

// Close гасит подставные источники.
func (r *Router) Close() {
	r.Wiki.Close()
	r.GBIF.Close()
}

// bind выдаёт инструмент модели через сервер: описание как из tools/list
// (Via — MCP, недоверенный ответ), вызов — со счётчиком сервера.
func (r *Router) bind(server string, t tools.Tool) {
	s := t.Spec()
	s.Via, s.Untrusted = tools.ViaMCP, true
	name := s.Name
	route := hub.Route{Tool: name, Server: server}
	r.routes = append(r.routes, route)
	r.bound = append(r.bound, hub.Bound{Route: route, Tool: tools.Func{S: s, Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
		r.Touch(server, name, 1)
		return t.Call(ctx, args)
	}}})
}

// Touch добавляет серверу вызовы, которых нет в трассе: так выглядят
// другие клиенты демона.
func (r *Router) Touch(server, tool string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.served[server] == nil {
		r.served[server] = map[string]int{}
	}
	r.served[server][tool] += n
}

// Restart меняет PID сервера — будто его перезапустили.
func (r *Router) Restart(server string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pids[server] += 1000
}

func (r *Router) Servers(context.Context) []hub.ServerView {
	r.mu.Lock()
	defer r.mu.Unlock()
	views := []hub.ServerView{
		{Name: Sources, Title: "Источники: Википедия и GBIF", Transport: hub.TransportStdio, Addr: "animals-mcp", Reported: mcp.ServerName},
		{Name: Daemon, Title: "Демон: MDD и «Интересные факты»", Transport: hub.TransportHTTP, Addr: "http://127.0.0.1:8766", Reported: "animals-daemon"},
		{Name: Notes, Title: "Блокнот натуралиста", Transport: hub.TransportStdio, Addr: "animals-mcp -role notes", Reported: notes.ServerName},
	}
	for i := range views {
		v := &views[i]
		v.Status, v.PID = hub.StatusOK, r.pids[v.Name]
		for _, rt := range r.routes {
			if rt.Server != v.Name {
				continue
			}
			if rt.Hidden {
				v.Hidden++
			} else {
				v.Tools++
			}
		}
		for _, n := range r.served[v.Name] {
			v.Calls += n
		}
	}
	return views
}

func (r *Router) Connect(context.Context) error { return nil }

func (r *Router) Routes(context.Context) ([]hub.Route, error) {
	return append([]hub.Route(nil), r.routes...), nil
}

func (r *Router) Tools(context.Context) ([]hub.Bound, error) {
	return append([]hub.Bound(nil), r.bound...), nil
}

// Snapshot — server_info каждого сервера. Сам вызов server_info тоже
// считается сервером, как у настоящего: Verify обязан его не учитывать.
func (r *Router) Snapshot(context.Context) (hub.Snapshot, error) {
	if r.SnapshotErr != nil {
		return nil, r.SnapshotErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := hub.Snapshot{}
	for _, s := range []string{Sources, Daemon, Notes} {
		if r.served[s] == nil {
			r.served[s] = map[string]int{}
		}
		r.served[s]["server_info"]++
		calls := map[string]int{}
		total := 0
		for k, v := range r.served[s] {
			calls[k] = v
			if k != "server_info" {
				total += v
			}
		}
		out[s] = mcp.Info{Server: s, PID: r.pids[s], Calls: calls, TotalCalls: total}
	}
	return out, nil
}

// Served — сколько раз сервер обслужил инструмент (без server_info).
func (r *Router) Served(server, tool string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.served[server][tool]
}

// ------------------------------------------------------------ демон

// factsGet — facts_get демона: выпуск о мануле (по mdd-id, латинскому,
// английскому или русскому названию) либо та же ошибка, что у настоящего,
// когда выпуска нет.
func (r *Router) factsGet() tools.Tool {
	return tools.Func{
		S: tools.Spec{
			Name:        "facts_get",
			Description: "«Интересные факты» — выпуски демона. Один выпуск целиком: по id выпуска или последний выпуск о виде.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer","minimum":1},` +
				`"species":{"type":"string","description":"Вид: mdd-id, латинское, английское или русское название"}},"additionalProperties":false}`),
		},
		Fn: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				ID      *int64          `json:"id"`
				Species json.RawMessage `json:"species"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", fmt.Errorf("аргументы не разобрались: %w", err)
			}
			species := strings.Trim(strings.TrimSpace(string(in.Species)), `"`)
			if in.ID == nil && species == "" {
				return "", errors.New("нужен один аргумент: id (номер выпуска) или species (mdd-id или название вида)")
			}
			manul := false
			for _, s := range []string{strconv.Itoa(mddtest.Manul), "Otocolobus manul", "Pallas's Cat", "манул"} {
				manul = manul || strings.EqualFold(species, s)
			}
			if in.ID != nil {
				manul = *in.ID == FactsIssueID
			}
			if r.NoFacts || !manul {
				return "", fmt.Errorf("выпусков о виде «%s» ещё не было: демон выбирает виды случайно. "+
					"facts_search ищет по тексту выпусков, facts_latest — последние выпуски", species)
			}
			return tools.Result(map[string]any{
				"id": FactsIssueID, "created_at": "24.09.2026 14:00", "species_id": mddtest.Manul,
				"sci_name": "Otocolobus manul", "name_ru": "Манул", "iucn": "LC", "status": "ok",
				"title": "Манул: кот с круглыми зрачками",
				"facts": []map[string]any{
					{"text": "У манула круглые зрачки, а не вертикальные, как у домашней кошки.",
						"sources": []map[string]string{{"id": "S1", "title": "Манул — Википедия"}}},
					{"text": "Манул почти не умеет быстро бегать и спасается, затаившись среди камней.",
						"sources": []map[string]string{{"id": "S2", "title": "MDD"}}},
				},
				"observations": map[string]any{"total": 1200, "recent": 14},
				"cost_usd":     0.0021, "took": "38s",
			})
		},
	}
}

// ------------------------------------------------------------ блокнот

type section struct {
	heading, text string
	cites         []string
}

type notebook struct {
	id, title, species string
	sections           []section
	closed             bool
}

// notebookTools — nb_open, nb_add, nb_close по контракту notes: случайный
// (здесь — детерминированный) notebook_id, разделы со ссылками, закрытие в
// файл. Раздел в закрытый или неизвестный блокнот — ошибка.
func (r *Router) notebookTools() []tools.Tool {
	schema := func(s string) json.RawMessage { return json.RawMessage(s) }
	open := tools.Func{
		S: tools.Spec{Name: notes.ToolOpen, Description: "Открыть блокнот натуралиста о виде. Возвращает notebook_id.",
			Parameters: schema(`{"type":"object","properties":{"title":{"type":"string"},"species":{"type":"string"}},"required":["title","species"]}`)},
		Fn: func(_ context.Context, args json.RawMessage) (string, error) {
			var in notes.OpenArgs
			if err := tools.ParseArgs(args, &in); err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Title) == "" {
				return "", errors.New("title пуст")
			}
			r.mu.Lock()
			r.opened++
			sum := sha256.Sum256([]byte("notebook-" + strconv.Itoa(r.opened)))
			id := "nb-" + hex.EncodeToString(sum[:6])
			r.books[id] = &notebook{id: id, title: in.Title, species: in.Species}
			r.mu.Unlock()
			return tools.Result(notes.OpenResult{NotebookID: id, Path: "notes/" + id + ".md"})
		},
	}
	add := tools.Func{
		S: tools.Spec{Name: notes.ToolAdd, Description: "Добавить раздел в открытый блокнот.",
			Parameters: schema(`{"type":"object","properties":{"notebook_id":{"type":"string"},"heading":{"type":"string"},"text":{"type":"string"},"cites":{"type":"array","items":{"type":"string"}}},"required":["notebook_id","heading","text","cites"]}`)},
		Fn: func(_ context.Context, args json.RawMessage) (string, error) {
			var in notes.AddArgs
			if err := tools.ParseArgs(args, &in); err != nil {
				return "", err
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			nb, err := r.book(in.NotebookID)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Heading) == "" || strings.TrimSpace(in.Text) == "" {
				return "", errors.New("heading и text обязательны")
			}
			nb.sections = append(nb.sections, section{in.Heading, in.Text, in.Cites})
			return tools.Result(notes.AddResult{NotebookID: nb.id, Section: len(nb.sections), Sections: len(nb.sections)})
		},
	}
	closeTool := tools.Func{
		S: tools.Spec{Name: notes.ToolClose, Description: "Закрыть блокнот и записать файл.",
			Parameters: schema(`{"type":"object","properties":{"notebook_id":{"type":"string"},"format":{"type":"string"}},"required":["notebook_id"]}`)},
		Fn: func(_ context.Context, args json.RawMessage) (string, error) {
			var in notes.CloseArgs
			if err := tools.ParseArgs(args, &in); err != nil {
				return "", err
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			nb, err := r.book(in.NotebookID)
			if err != nil {
				return "", err
			}
			if len(nb.sections) == 0 {
				return "", errors.New("в блокноте нет разделов: добавь хотя бы один через nb_add")
			}
			nb.closed = true
			var sb strings.Builder
			fmt.Fprintf(&sb, "# %s\n\n", nb.title)
			set := map[string]bool{}
			for _, s := range nb.sections {
				fmt.Fprintf(&sb, "## %s\n\n%s\n\nИсточники: %s\n\n", s.heading, s.text, strings.Join(s.cites, ", "))
				for _, c := range s.cites {
					set[c] = true
				}
			}
			cites := make([]string, 0, len(set))
			for c := range set {
				cites = append(cites, c)
			}
			sort.Strings(cites)
			text := sb.String()
			sum := sha256.Sum256([]byte(text))
			return tools.Result(notes.CloseResult{NotebookID: nb.id, Path: "notes/" + nb.id + ".md",
				SHA256: hex.EncodeToString(sum[:]), Bytes: len(text), Sections: len(nb.sections),
				Cites: cites, Preview: tools.Truncate(text, 200)})
		},
	}
	return []tools.Tool{open, add, closeTool}
}

// book — открытый блокнот по id; вызывается под r.mu.
func (r *Router) book(id string) (*notebook, error) {
	nb, ok := r.books[strings.TrimSpace(id)]
	if !ok {
		return nil, fmt.Errorf("блокнота %q нет: notebook_id берётся из ответа nb_open", id)
	}
	if nb.closed {
		return nil, fmt.Errorf("блокнот %s уже закрыт: разделы добавляются до nb_close", id)
	}
	return nb, nil
}
