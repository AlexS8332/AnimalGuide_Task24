// Команда mcp-flow — длинный флоу агента через несколько MCP-серверов:
// реестр подключается к серверам из конфигурации (источники, демон,
// блокнот), модель получает инструменты всех серверов без префиксов и сама
// ведёт флоу, а код после неё проверяет выбор инструментов, маршрут каждого
// вызова, зависимости данных, порядок и свидетельство самих серверов
// (server_info до и после).
//
//	mcp-flow                         # «Паспорт вида в блокнот» о мануле
//	mcp-flow снежный барс            # о другом виде
//	mcp-flow -servers                # серверы, статусы, маршруты со скрытыми
//	mcp-flow -repeat 5               # пять прогонов, доля прохождения проверок
//	mcp-flow -json                   # трасса целиком в JSON
//
// Код выхода 0 — все проверки прошли; 1 — есть провал; 2 — конфигурация,
// серверы или ключ модели.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/hub"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

// Коды выхода.
const (
	exitOK     = 0
	exitFailed = 1
	exitSetup  = 2
)

func main() {
	enableUTF8Console()
	loadEnvFiles()
	o, err := parseArgs(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, errHelp) {
		os.Exit(exitOK)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		fmt.Fprintln(os.Stderr, "Справка: mcp-flow -h")
		os.Exit(exitSetup)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, o, os.Stdout, os.Stderr))
}

// openRouter — реестр серверов по опциям. Тесты подменяют его подставным:
// настоящие серверы — это процессы и HTTP-демон.
var openRouter = hubOpen

// hubOpen — настоящий реестр: конфигурация (файл или встроенная) с
// подстановками ${DATA} и ${DAEMON}.
func hubOpen(o options) (hub.Router, func(), error) {
	cfg, err := hub.LoadConfig(o.config, hub.Defaults{DataDir: o.data, DaemonURL: o.daemon})
	if err != nil {
		return nil, nil, err
	}
	h, err := hub.Open(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		return nil, nil, err
	}
	return h, h.Close, nil
}

// newModel — модель DeepSeek с ключом из окружения. Тесты подменяют её
// реактивной подставной: живые вызовы модели в тестах запрещены.
var newModel = func(o options) (llm.Chatter, error) {
	key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if key == "" {
		return nil, errors.New("нужен DEEPSEEK_API_KEY — в окружении или в .env/.env.local")
	}
	return llm.NewClient(key, os.Getenv("DEEPSEEK_BASE_URL")), nil
}

// run подключается к серверам, гонит флоу и возвращает код выхода. Журнал
// и итог — в out; ход прогона по мере вызовов — в errOut (с -json out
// остаётся чистым JSON).
func run(ctx context.Context, o options, out, errOut io.Writer) int {
	router, closeRouter, err := openRouter(o)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка: реестр серверов:", err)
		return exitSetup
	}
	defer closeRouter()

	cerr := router.Connect(ctx)
	if o.servers {
		printServers(ctx, out, router)
		if cerr != nil {
			fmt.Fprintln(errOut, "ошибка:", cerr)
			return exitSetup
		}
		return exitOK
	}
	if cerr != nil {
		fmt.Fprintln(errOut, "ошибка: ни один сервер не подключился:", cerr)
		printServers(ctx, errOut, router)
		return exitSetup
	}
	chatter, err := newModel(o)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitSetup
	}
	p, err := flow.Find(o.preset)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitSetup
	}
	cfg := flow.Config{Runner: agent.Runner{LLM: chatter, Model: o.model}, Router: router}
	if !o.json {
		model := o.model
		if model == "" {
			model = llm.DefaultModel
		}
		species := o.species
		if species == "" {
			species = p.Species
		}
		fmt.Fprintf(out, "Флоу «%s» — вид «%s», модель %s", p.Title, species, model)
		if o.repeat > 1 {
			fmt.Fprintf(out, ", прогонов %d", o.repeat)
		}
		fmt.Fprintln(out)
		downServers(ctx, out, router)
	}

	var traces []flow.Trace
	code := exitOK
	for i := range o.repeat {
		if o.repeat > 1 && !o.json {
			fmt.Fprintf(out, "\n━━ прогон %d из %d\n", i+1, o.repeat)
		}
		rctx, cancel := context.WithTimeout(ctx, o.timeout)
		tr, err := flow.Run(rctx, cfg, p, o.species, progress(errOut))
		cancel()
		if err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitSetup
		}
		traces = append(traces, tr)
		if !tr.OK {
			code = exitFailed
		}
		if !o.json {
			printTrace(out, tr)
		}
		if ctx.Err() != nil {
			break // Ctrl+C: остальные прогоны не нужны
		}
	}
	if o.json {
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if len(traces) == 1 {
			_ = enc.Encode(traces[0])
		} else {
			_ = enc.Encode(traces)
		}
	} else if len(traces) > 1 {
		printRates(out, traces)
	}
	return code
}
