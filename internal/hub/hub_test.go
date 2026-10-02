package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/notes"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

// world — три настоящих mcp.Server в процессе, как в жизни: источники
// (с MDD — у настоящего stdio-сервера они тоже есть), демон по HTTP (те же
// источники и MDD, факты и платный run_now) и блокнот в памяти. Сеть не
// нужна: Википедия и GBIF подставные, HTTP — httptest на loopback.
type world struct {
	sources, daemon, notes *mcp.Server
	daemonHTTP             *httptest.Server
	notesDir               string
	cfg                    Config
	dial                   map[string]mcp.Dialer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)
	store := mdd.NewMemory()
	if err := store.Replace(ctx, mddtest.Sample()); err != nil {
		t.Fatal(err)
	}

	w := &world{notesDir: t.TempDir()}
	src := tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL)
	src = append(src, mdd.Tools(store)...)
	w.sources = mcp.NewServer(src, mcp.ServerOptions{WikiBase: wiki.URL, GBIFBase: gbif.URL})

	dm := tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL)
	dm = append(dm, mdd.Tools(store)...)
	dm = append(dm, fakeTool("facts_get", false), fakeTool("facts_latest", false), fakeTool("run_now", true))
	w.daemon = mcp.NewServer(dm, mcp.ServerOptions{Name: "animals-daemon", Title: "Демон"})
	w.daemonHTTP = httptest.NewServer(w.daemon.HTTPHandler(mcp.HTTPOptions{}))
	t.Cleanup(w.daemonHTTP.Close)

	w.notes = mcp.NewServer(notes.Tools(w.notesDir), mcp.ServerOptions{Name: notes.ServerName, Title: "Блокнот"})

	w.cfg = Config{Servers: []ServerConfig{
		{Name: "sources", Title: "Источники", Transport: TransportStdio, Expect: mcp.ServerName,
			Tools: []string{"search_wikipedia", "read_wikipedia", "match_taxon", "taxon_tree", "taxon_children", "vernacular_names"}},
		{Name: "daemon", Title: "Демон", Transport: TransportHTTP, URL: w.daemonHTTP.URL, TokenEnv: "HUB_TEST_TOKEN",
			Expect: "animals-daemon", Tools: []string{"mdd_*", "facts_latest", "facts_get", "facts_search", "summary_get"}},
		{Name: "notes", Title: "Блокнот", Transport: TransportStdio, Expect: notes.ServerName, Tools: []string{"nb_*"}},
	}}
	w.dial = map[string]mcp.Dialer{
		"sources": memDial(w.sources),
		"daemon":  mcp.HTTPDialer(w.daemonHTTP.URL, "", nil),
		"notes":   memDial(w.notes),
	}
	return w
}

func (w *world) open(t *testing.T) *Hub {
	t.Helper()
	h, err := OpenWith(w.cfg, nil, w.dial)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// memDial — подключение к серверу в памяти: каждое подключение — новая
// серверная сессия того же сервера (счётчики общие).
func memDial(srv *mcp.Server) mcp.Dialer {
	return func(ctx context.Context) (sdk.Transport, *exec.Cmd, error) {
		ct, st := sdk.NewInMemoryTransports()
		if _, err := srv.SDK().Connect(ctx, st, nil); err != nil {
			return nil, nil, err
		}
		return ct, nil, nil
	}
}

func fakeTool(name string, write bool) tools.Tool {
	return tools.Func{
		S: tools.Spec{Name: name, Description: "подставной " + name,
			Parameters: json.RawMessage(`{"type":"object","properties":{"species":{"type":"string"}}}`), Write: write},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			return `{"tool":"` + name + `"}`, nil
		},
	}
}

func views(h *Hub) map[string]ServerView {
	out := map[string]ServerView{}
	for _, v := range h.Servers(context.Background()) {
		out[v.Name] = v
	}
	return out
}

func bound(t *testing.T, h *Hub, name string) Bound {
	t.Helper()
	bs, err := h.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		if b.Route.Tool == name {
			return b
		}
	}
	t.Fatalf("инструмента %s модели не выдано", name)
	return Bound{}
}

func call(t *testing.T, h *Hub, name string, args any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return bound(t, h, name).Tool.Call(context.Background(), raw)
}

func TestRoutesAndTools(t *testing.T) {
	w := newWorld(t)
	h := w.open(t)
	ctx := context.Background()

	for _, v := range h.Servers(ctx) {
		if v.Status != StatusIdle {
			t.Errorf("%s до подключения: %s", v.Name, v.Status)
		}
	}
	if err := h.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	vs := views(h)
	for name, want := range map[string]string{"sources": mcp.ServerName, "daemon": "animals-daemon", "notes": notes.ServerName} {
		v := vs[name]
		if v.Status != StatusOK || v.Reported != want || v.Version != mcp.Version || v.PID != os.Getpid() {
			t.Errorf("%s: %+v", name, v)
		}
	}
	if v := vs["sources"]; v.Tools != 6 || v.Hidden != 4 { // mdd_* ×3 и server_info
		t.Errorf("sources: выдано %d, скрыто %d", v.Tools, v.Hidden)
	}
	if v := vs["daemon"]; v.Tools != 5 || v.Hidden != 8 || v.Addr != w.daemonHTTP.URL {
		t.Errorf("daemon: выдано %d, скрыто %d, адрес %q", v.Tools, v.Hidden, v.Addr)
	}

	routes, err := h.Routes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Route{}
	for _, r := range routes {
		got[r.Server+"/"+r.Tool] = r
	}
	for key, want := range map[string]Route{
		"daemon/match_taxon":      {Tool: "match_taxon", Server: "daemon", Hidden: true, Reason: "дубль → sources"},
		"daemon/read_wikipedia":   {Tool: "read_wikipedia", Server: "daemon", Hidden: true, Reason: "дубль → sources"},
		"sources/mdd_get":         {Tool: "mdd_get", Server: "sources", Hidden: true, Reason: "дубль → daemon"},
		"daemon/run_now":          {Tool: "run_now", Server: "daemon", Hidden: true, Reason: "не разрешён"},
		"notes/server_info":       {Tool: "server_info", Server: "notes", Hidden: true, Reason: "служебный"},
		"sources/match_taxon":     {Tool: "match_taxon", Server: "sources"},
		"daemon/mdd_get":          {Tool: "mdd_get", Server: "daemon"},
		"daemon/facts_get":        {Tool: "facts_get", Server: "daemon"},
		"notes/nb_open":           {Tool: "nb_open", Server: "notes"},
		"sources/taxon_children":  {Tool: "taxon_children", Server: "sources"},
		"daemon/vernacular_names": {Tool: "vernacular_names", Server: "daemon", Hidden: true, Reason: "дубль → sources"},
	} {
		if got[key] != want {
			t.Errorf("маршрут %s: %+v, ждали %+v", key, got[key], want)
		}
	}

	bs, err := h.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	order := map[string]int{"sources": 0, "daemon": 1, "notes": 2}
	seen := map[string]bool{}
	last := 0
	for _, b := range bs {
		s := b.Tool.Spec()
		if b.Route.Hidden || s.Name != b.Route.Tool || s.Via != tools.ViaMCP || !s.Untrusted || s.Description == "" {
			t.Errorf("выдан %+v / %+v", b.Route, s)
		}
		if seen[s.Name] {
			t.Errorf("%s выдан дважды", s.Name)
		}
		seen[s.Name] = true
		if order[b.Route.Server] < last {
			t.Errorf("порядок серверов нарушен на %s", s.Name)
		}
		last = order[b.Route.Server]
	}
	if len(bs) != 6+5+3 {
		t.Errorf("выдано %d инструментов: %v", len(bs), boundNames(bs))
	}
	// Схема — как у сервера, в каноническом виде; Write — по пометке сервера.
	nb := bound(t, h, notes.ToolAdd).Tool.Spec()
	if !nb.Write || !strings.Contains(string(nb.Parameters), `"notebook_id"`) {
		t.Errorf("nb_add: %+v", nb)
	}
	if mt := bound(t, h, "match_taxon").Tool.Spec(); mt.Write || string(mt.Parameters) != string(tools.Canon(mt.Parameters)) {
		t.Errorf("match_taxon: %+v", mt)
	}
}

func boundNames(bs []Bound) []string {
	var out []string
	for _, b := range bs {
		out = append(out, b.Route.Server+"/"+b.Route.Tool)
	}
	return out
}

// Вызов match_taxon уходит на sources, а не на демон, у которого такой же
// инструмент есть: это видно по server_info обоих серверов.
func TestCallRouting(t *testing.T) {
	w := newWorld(t)
	h := w.open(t)
	ctx := context.Background()
	before, err := h.Snapshot(ctx) // сам подключается
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 3 {
		t.Fatalf("снимок: %v", before)
	}
	if _, err := call(t, h, "match_taxon", map[string]string{"scientific_name": "Otocolobus manul"}); err != nil {
		t.Fatal(err)
	}
	out, err := call(t, h, "mdd_get", map[string]string{"name": "Otocolobus manul"})
	if err != nil || !strings.Contains(out, "Otocolobus") {
		t.Fatalf("mdd_get: %v %s", err, out)
	}
	after, err := h.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	delta := func(server, tool string) int { return after[server].Calls[tool] - before[server].Calls[tool] }
	if delta("sources", "match_taxon") != 1 || delta("daemon", "match_taxon") != 0 {
		t.Errorf("match_taxon: sources +%d, daemon +%d", delta("sources", "match_taxon"), delta("daemon", "match_taxon"))
	}
	if delta("daemon", "mdd_get") != 1 || delta("sources", "mdd_get") != 0 {
		t.Errorf("mdd_get: daemon +%d, sources +%d", delta("daemon", "mdd_get"), delta("sources", "mdd_get"))
	}
	if after["notes"].Server != notes.ServerName {
		t.Errorf("снимок блокнота: %+v", after["notes"])
	}
	vs := views(h)
	if vs["sources"].Calls != 1 || vs["daemon"].Calls != 1 || vs["notes"].Calls != 0 {
		t.Errorf("счётчики реестра: sources %d, daemon %d, notes %d", vs["sources"].Calls, vs["daemon"].Calls, vs["notes"].Calls)
	}
}

// Блокнот через реестр: полный цикл и ошибка инструмента как есть.
func TestNotesThroughHub(t *testing.T) {
	w := newWorld(t)
	h := w.open(t)
	_, err := call(t, h, notes.ToolAdd, notes.AddArgs{NotebookID: "nb-000000000000", Heading: "x", Text: "y"})
	var te *feed.ToolError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "сначала nb_open") || strings.Contains(err.Error(), "MCP-сервер") {
		t.Errorf("ошибка инструмента переписана: %v", err)
	}
	out, err := call(t, h, notes.ToolOpen, notes.OpenArgs{Title: "Манул", Species: "Otocolobus manul"})
	if err != nil {
		t.Fatal(err)
	}
	var op notes.OpenResult
	json.Unmarshal([]byte(out), &op)
	if _, err := call(t, h, notes.ToolAdd, notes.AddArgs{NotebookID: op.NotebookID, Heading: "Питание",
		Text: "Пищухи.", Cites: []string{"read_wikipedia"}}); err != nil {
		t.Fatal(err)
	}
	out, err = call(t, h, notes.ToolClose, notes.CloseArgs{NotebookID: op.NotebookID})
	if err != nil {
		t.Fatal(err)
	}
	var cl notes.CloseResult
	json.Unmarshal([]byte(out), &cl)
	if filepath.Dir(cl.Path) != w.notesDir {
		t.Errorf("файл %q не в %q", cl.Path, w.notesDir)
	}
}

// Ответил не тот сервер: статус mismatch, его инструменты модели не
// выдаются и в маршрутах их нет.
func TestMismatch(t *testing.T) {
	w := newWorld(t)
	w.dial["notes"] = memDial(w.sources)
	h := w.open(t)
	if err := h.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := views(h)["notes"]
	if v.Status != StatusMismatch || v.Reported != mcp.ServerName || v.Tools != 0 ||
		!strings.Contains(v.Reason, notes.ServerName) || v.Hint == "" {
		t.Errorf("notes: %+v", v)
	}
	routes, _ := h.Routes(context.Background())
	for _, r := range routes {
		if r.Server == "notes" {
			t.Errorf("маршрут у чужого сервера: %+v", r)
		}
	}
	bs, _ := h.Tools(context.Background())
	for _, b := range bs {
		if strings.HasPrefix(b.Route.Tool, "nb_") {
			t.Errorf("выдан %v", b.Route)
		}
	}
}

// Демон не запущен: реестр жив, у демона down с подсказкой, инструменты
// остальных выдаются.
func TestDaemonDown(t *testing.T) {
	w := newWorld(t)
	w.daemonHTTP.Close()
	h := w.open(t)
	if err := h.Connect(context.Background()); err != nil {
		t.Fatalf("частичный набор — не ошибка: %v", err)
	}
	vs := views(h)
	d := vs["daemon"]
	host := strings.TrimPrefix(w.daemonHTTP.URL, "http://")
	if d.Status != StatusDown || !strings.HasPrefix(d.Reason, "MCP-сервер daemon недоступен: ") ||
		strings.Contains(d.Reason, "Интересных фактов") || d.Hint != "запусти animals-mcp -http "+host+
		"; если задан HUB_TEST_TOKEN, у сервера он должен быть тот же" {
		t.Errorf("daemon: %+v", d)
	}
	if vs["sources"].Status != StatusOK || vs["notes"].Status != StatusOK {
		t.Errorf("остальные: %+v", vs)
	}
	bs, err := h.Tools(context.Background())
	if err != nil || len(bs) != 6+3 {
		t.Errorf("частичный набор: %v %v", err, boundNames(bs))
	}
}

// Демон упал между вызовами: ошибка словами реестра, статус down.
func TestCallAfterDown(t *testing.T) {
	w := newWorld(t)
	h := w.open(t)
	b := bound(t, h, "mdd_get")
	w.daemonHTTP.Close()
	_, err := b.Tool.Call(context.Background(), json.RawMessage(`{"name":"Otocolobus manul"}`))
	if err == nil || !strings.HasPrefix(err.Error(), "MCP-сервер daemon недоступен: ") || !errors.Is(err, feed.ErrDown) {
		t.Fatalf("ошибка: %v", err)
	}
	if v := views(h)["daemon"]; v.Status != StatusDown || v.Calls != 1 {
		t.Errorf("daemon: %+v", v)
	}
}

// Токен не тот: denied и подсказка про переменную.
func TestDenied(t *testing.T) {
	w := newWorld(t)
	secured := httptest.NewServer(w.daemon.HTTPHandler(mcp.HTTPOptions{Token: "secret"}))
	defer secured.Close()
	w.dial["daemon"] = mcp.HTTPDialer(secured.URL, "wrong", nil)
	t.Setenv("HUB_TEST_TOKEN", "")
	h := w.open(t)
	h.Connect(context.Background())
	d := views(h)["daemon"]
	if d.Status != StatusDenied || !strings.Contains(d.Reason, "401") || !strings.Contains(d.Hint, "HUB_TEST_TOKEN") {
		t.Errorf("daemon: %+v", d)
	}
}

func TestAllDown(t *testing.T) {
	w := newWorld(t)
	fail := func(ctx context.Context) (sdk.Transport, *exec.Cmd, error) {
		return nil, nil, errors.New("нет процесса")
	}
	w.daemonHTTP.Close()
	w.dial["sources"], w.dial["notes"] = fail, fail
	h := w.open(t)
	err := h.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ни один") || !strings.Contains(err.Error(), "нет процесса") {
		t.Errorf("Connect: %v", err)
	}
	if v := views(h)["notes"]; v.Status != StatusDown || v.Hint != "пересобери animals-mcp: go build -o . ./cmd/animals-mcp" {
		t.Errorf("notes: %+v", v)
	}
	if _, err := h.Tools(context.Background()); err != nil {
		t.Errorf("Tools после Connect: %v", err) // Connect уже был — пустой набор, а не ошибка
	}
}

func TestClose(t *testing.T) {
	w := newWorld(t)
	h := w.open(t)
	b := bound(t, h, "match_taxon")
	h.Close()
	h.Close() // повторно — без паники
	if _, err := b.Tool.Call(context.Background(), json.RawMessage(`{"scientific_name":"Lynx lynx"}`)); err == nil {
		t.Error("вызов после Close прошёл")
	}
	if err := h.Connect(context.Background()); err == nil {
		t.Error("Connect после Close прошёл")
	}
}

// Настоящий подпроцесс animals-mcp -role notes через Launcher: процесс
// поднимается, отвечает своим именем и после Close не остаётся висеть.
func TestRealNotesProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("сборка animals-mcp")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go не в PATH")
	}
	bin := filepath.Join(t.TempDir(), mcp.BinaryName+exe())
	build := exec.Command(gobin, "build", "-o", bin, "./cmd/animals-mcp")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка: %v\n%s", err, out)
	}
	data := t.TempDir()
	cfg := Config{Servers: []ServerConfig{{Name: "notes", Transport: TransportStdio, Command: bin,
		Args: []string{"-role", "notes", "-data", data}, Expect: notes.ServerName, Tools: []string{"nb_*"}}}}
	h, err := Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := views(h)["notes"]
	if v.Status != StatusOK || v.PID == 0 || v.PID == os.Getpid() || v.Tools != 3 {
		t.Fatalf("notes: %+v", v)
	}
	out, err := call(t, h, notes.ToolOpen, notes.OpenArgs{Title: "Манул", Species: "Otocolobus manul"})
	if err != nil || !strings.Contains(out, "nb-") {
		t.Fatalf("nb_open: %v %s", err, out)
	}
	h.Close()
	if alive(v.PID) {
		t.Errorf("процесс %d жив после Close", v.PID)
	}
}
