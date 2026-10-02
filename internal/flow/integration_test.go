package flow_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow/flowtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/hub"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/notes"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

// TestFlowThroughRealServers — длинный флоу целиком по настоящему MCP:
// встроенная конфигурация реестра, три настоящих mcp.Server (источники и
// блокнот — через транспорт в памяти вместо stdio-процесса, демон — по
// Streamable HTTP), настоящий hub и flow.Run с реактивной моделью. Здесь
// сходятся все части: маршрут каждого вызова подтверждают счётчики самих
// серверов, дубли источников у демона не вызываются ни разу, файл блокнота
// лежит на диске с тем sha256, что в ответе nb_close.
func TestFlowThroughRealServers(t *testing.T) {
	ctx := context.Background()
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)
	store := mdd.NewMemory()
	if err := store.Replace(ctx, mddtest.Sample()); err != nil {
		t.Fatal(err)
	}

	// Источники — как у stdio-сервера: Википедия, GBIF и MDD (MDD у него
	// скрыт конфигурацией: владелец mdd_* — демон).
	src := append(tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL), mdd.Tools(store)...)
	sources := mcp.NewServer(src, mcp.ServerOptions{WikiBase: wiki.URL, GBIFBase: gbif.URL})

	// Демон — те же источники и MDD плюс facts_get; выпуск о мануле берётся
	// у подставного реестра флоу, чтобы ответ был той же формы.
	facts := borrow(t, "facts_get")
	dm := append(tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL), mdd.Tools(store)...)
	daemon := mcp.NewServer(append(dm, facts), mcp.ServerOptions{Name: "animals-daemon"})
	daemonHTTP := httptest.NewServer(daemon.HTTPHandler(mcp.HTTPOptions{}))
	t.Cleanup(daemonHTTP.Close)

	data := t.TempDir()
	nb := mcp.NewServer(notes.Tools(data+"/notes"), mcp.ServerOptions{Name: notes.ServerName})

	cfg, err := hub.LoadConfig("", hub.Defaults{DataDir: data, DaemonURL: daemonHTTP.URL})
	if err != nil {
		t.Fatal(err)
	}
	h, err := hub.OpenWith(cfg, nil, map[string]mcp.Dialer{"sources": memDial(sources), "notes": memDial(nb)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)

	p, err := flow.Find("passport")
	if err != nil {
		t.Fatal(err)
	}
	brain := &flowtest.Brain{}
	runner := agent.Runner{LLM: brain.Fake(), Model: "deepseek-v4-flash"}
	tr, err := flow.Run(ctx, flow.Config{Runner: runner, Router: h}, p, "манул", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range tr.Verdict.Checks {
		if c.Level == flow.LevelFail {
			t.Errorf("провал %q: %s", c.Name, c.Note)
		}
	}
	if !tr.OK {
		t.Fatalf("флоу не прошёл: %s", tr.Error)
	}

	// Выбор и маршрут: 13+ вызовов, три сервера, у каждого вызова — сервер
	// из встроенной конфигурации.
	owner := map[string]string{}
	for _, s := range cfg.Servers {
		for _, name := range s.Tools {
			owner[strings.TrimSuffix(name, "*")] = s.Name
		}
	}
	used := map[string]bool{}
	for _, c := range tr.Calls {
		want := owner[c.Tool]
		if want == "" && strings.HasPrefix(c.Tool, "mdd_") {
			want = owner["mdd_"]
		}
		if want == "" && strings.HasPrefix(c.Tool, "nb_") {
			want = owner["nb_"]
		}
		if c.Server != want {
			t.Errorf("№%d %s ушёл на %q, а по конфигурации — %q", c.N, c.Tool, c.Server, want)
		}
		used[c.Server] = true
	}
	if len(tr.Calls) < 13 || len(used) != 3 {
		t.Fatalf("вызовов %d, серверов %d — ждали ≥13 и 3", len(tr.Calls), len(used))
	}

	// Свидетельство серверов: сам сервер насчитал столько же вызовов,
	// сколько их в трассе; дубли источников у демона не вызывались.
	for _, d := range tr.Servers {
		for tool, n := range d.Traced {
			if d.Served[tool] != n {
				t.Errorf("%s/%s: сервер насчитал %d, в трассе %d", d.Server, tool, d.Served[tool], n)
			}
		}
	}
	info := daemon.Stats()
	for _, name := range tools.SourceTools {
		if info.Calls[name] != 0 {
			t.Errorf("дубль %s вызван у демона %d раз — маршрут должен вести на sources", name, info.Calls[name])
		}
	}
	if sources.Stats().Calls["match_taxon"] == 0 || daemon.Stats().Calls["mdd_get"] == 0 {
		t.Error("match_taxon должен дойти до sources, mdd_get — до демона")
	}

	// Итог — файл блокнота на диске с тем же sha256.
	body, err := os.ReadFile(tr.File)
	if err != nil {
		t.Fatalf("файл блокнота: %v", err)
	}
	var close notes.CloseResult
	for _, c := range tr.Calls {
		if c.Tool == notes.ToolClose && c.OK {
			if err := json.Unmarshal(c.Result, &close); err != nil {
				t.Fatal(err)
			}
		}
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != close.SHA256 {
		t.Errorf("sha256 файла %x, в ответе nb_close %s", sum, close.SHA256)
	}
	if !strings.Contains(string(body), "Источники:") || close.Sections < 2 {
		t.Errorf("блокнот без источников или разделов: %d разделов\n%s", close.Sections, body)
	}
}

// borrow — инструмент подставного реестра флоу по имени.
func borrow(t *testing.T, name string) tools.Tool {
	t.Helper()
	r := flowtest.NewRouter()
	t.Cleanup(r.Close)
	bs, err := r.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		if b.Route.Tool == name {
			s := b.Tool.Spec()
			s.Via, s.Untrusted = "", false
			return tools.Func{S: s, Fn: b.Tool.Call}
		}
	}
	t.Fatalf("у подставного реестра нет %s", name)
	return nil
}

// memDial — подключение к серверу в памяти вместо stdio-процесса.
func memDial(srv *mcp.Server) mcp.Dialer {
	return func(ctx context.Context) (sdk.Transport, *exec.Cmd, error) {
		ct, st := sdk.NewInMemoryTransports()
		if _, err := srv.SDK().Connect(ctx, st, nil); err != nil {
			return nil, nil, err
		}
		return ct, nil, nil
	}
}
