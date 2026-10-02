// AnimalGuide — справочник по животным, с которым разговаривают: локальный
// сервер с веб-интерфейсом. Пользователь спрашивает про животное обычными
// словами и получает карточку, собранную только из источников (русская
// Википедия и GBIF), раскрывает её разделы, ходит по дереву классификации,
// сравнивает животных и собирает подборки.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/charter"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/invariants"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/persona"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tokens"
)

// Фронтенд лежит в бинарнике: после `go build` приложение запускается одним
// файлом, без каталога с ассетами рядом.
//
//go:embed web
var webFiles embed.FS

const (
	// Слушаем только петлевой интерфейс: приложение локальное.
	defaultAddr = "127.0.0.1:8770"
	// Общий срок одного хода: подборка — это десятки запросов.
	turnTimeout = 10 * time.Minute
	// defaultContextLimit — свой лимит контекста: у DeepSeek в отказе API
	// стоит 1048576 (1 Mi), изменить его нельзя — max_tokens ограничивает
	// только ответ. Поэтому лимит живёт здесь и ловит переполнение до
	// отправки.
	defaultContextLimit = 1_048_576
)

// options — флаги запуска.
type options struct {
	addr, data, featureSpec, overflow string
	mcpServer                         string
	open                              bool
	window, keep, limit               int
	// report — опыт -report вместо сервера: испытания и отчёт в markdown.
	report            bool
	trials, reportOut string
	// factsServer — адрес демона «Интересных фактов»; factsSet — задан ли
	// он флагом явно (тогда и пустая строка — «выключено»).
	factsServer string
	factsSet    bool
	// mcpConfig — конфигурация реестра MCP-серверов (окно «MCP-серверы»,
	// длинный флоу); пусто — MCP_CONFIG, иначе встроенная.
	mcpConfig string
	// kb — путь к базе знаний (kb.db); пусто — KB_DB, иначе <data>/kb.db.
	kb string
	// embedURL — адрес эмбеддера; пусто — EMBED_BASE_URL, иначе сайдкар по умолчанию.
	embedURL string
}

// facts — адрес демона фактов: флаг, иначе TRIVIA_SERVER, иначе адрес по
// умолчанию. Окружение читается здесь, а не в умолчании флага: .env-файлы
// подхватываются уже после разбора флагов.
func (o options) facts() string {
	if o.factsSet {
		return o.factsServer
	}
	if v := strings.TrimSpace(os.Getenv("TRIVIA_SERVER")); v != "" {
		return v
	}
	return feed.DefaultServer
}

// hubConfig — путь к конфигурации реестра серверов: флаг, иначе MCP_CONFIG,
// иначе пусто (встроенная).
func (o options) hubConfig() string {
	if o.mcpConfig != "" {
		return o.mcpConfig
	}
	return strings.TrimSpace(os.Getenv("MCP_CONFIG"))
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.addr, "addr", defaultAddr, "адрес, на котором слушать")
	flag.BoolVar(&o.open, "open", true, "открыть браузер при старте")
	flag.StringVar(&o.data, "data", ".", "каталог данных: history, memory, profiles, collections, invariants")
	flag.StringVar(&o.featureSpec, "features", "", "механизмы новых диалогов поверх умолчаний: «+mcp,-guard», «none,charter», «all»")
	flag.IntVar(&o.window, "window", history.DefaultWindow, "сколько последних сообщений уходит модели дословно (механизм window)")
	flag.IntVar(&o.keep, "keep-tools", history.DefaultKeepToolRunes, "до скольких символов сокращать ответы инструментов прошлых ходов (механизм compact)")
	flag.IntVar(&o.limit, "context-limit", defaultContextLimit, "свой лимит контекста в токенах; 0 — не проверять")
	flag.StringVar(&o.mcpServer, "mcp-server", "", "бинарник MCP-сервера источников (механизм mcp); пусто — рядом с приложением, в PATH или сборка из исходников")
	flag.StringVar(&o.overflow, "on-overflow", agent.OverflowFail, "что делать при переполнении: fail — не отправлять, trim — выбрасывать старые ходы, off — отправить как есть")
	flag.BoolVar(&o.report, "report", false, "прогнать испытания на живой модели и записать отчёт вместо запуска сервера")
	flag.StringVar(&o.trials, "trials", "all", "какие испытания гонять с -report: «all», «1,6», «И-2»")
	flag.StringVar(&o.reportOut, "report-out", filepath.Join("examples", "report.md"), "куда записать отчёт -report")
	flag.StringVar(&o.factsServer, "facts-server", feed.DefaultServer, "адрес демона «Интересных фактов» (animals-mcp -http, механизм trivia и раздел интерфейса); по умолчанию TRIVIA_SERVER, иначе этот; пусто — выключить. Токен — MCP_TOKEN")
	flag.StringVar(&o.mcpConfig, "mcp-config", "", "конфигурация реестра MCP-серверов (см. mcp-servers.example.json); пусто — MCP_CONFIG, иначе встроенная: sources, daemon, notes")
	flag.StringVar(&o.kb, "kb", "", "база знаний (собирается командой go run ./cmd/kb index); пусто — KB_DB, иначе <data>/kb.db")
	flag.StringVar(&o.embedURL, "embedder", "", "адрес эмбеддера (OpenAI-совместимый /v1/embeddings: сайдкар embedder/, Ollama, облако); пусто — EMBED_BASE_URL, иначе http://127.0.0.1:8777")
	flag.Parse()
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "facts-server" {
			o.factsSet = true
		}
	})
	return o
}

func main() {
	o := parseFlags()
	enableUTF8Console()
	loadEnvFiles()

	switch o.overflow {
	case agent.OverflowFail, agent.OverflowTrim, agent.OverflowOff:
	default:
		fail(fmt.Errorf("неизвестный режим -on-overflow=%q; допустимы fail, trim, off", o.overflow))
	}
	registry := features.Catalog()
	defaults, err := registry.Parse(o.featureSpec, registry.Defaults())
	if err != nil {
		fail(err)
	}
	if err := registry.Validate(defaults); err != nil {
		fail(err)
	}

	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		fail(fmt.Errorf("не задан DEEPSEEK_API_KEY — задай переменную окружения или впиши ключ в .env.local (см. .env.example)"))
	}
	model := strings.TrimSpace(os.Getenv("DEEPSEEK_MODEL"))
	if model == "" {
		model = llm.DefaultModel
	}

	runner := agent.Runner{
		LLM: llm.NewClient(apiKey, os.Getenv("DEEPSEEK_BASE_URL")), Model: model, Temperature: 0,
		ContextLimit: o.limit, OnOverflow: o.overflow, Calibration: &tokens.Calibration{},
	}
	if o.report {
		if err := runReport(o, registry, defaults, runner, model); err != nil {
			fail(err)
		}
		return
	}
	a, err := wire(o, registry, defaults, runner, o.data, "")
	if err != nil {
		fail(err)
	}
	manager, people, compile, guide, local := a.Manager, a.People, a.Compile, a.Guide, a.Local
	loaded, problems := manager.Load()
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "предупреждение: файл диалога пропущен: "+p.Error())
	}

	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		fail(fmt.Errorf("встроенный фронтенд не читается: %w", err))
	}
	meta := map[string]any{"model": model, "window": o.window, "contextLimit": o.limit}
	for _, m := range []map[string]any{persona.Meta(), charter.Meta()} {
		for k, v := range m {
			meta[k] = v
		}
	}
	exts := append(people.Extension(), compile.Extension(manager)...)
	exts = append(exts, guide.Extension()...)
	exts = append(exts, a.Sources.Extension()...)
	exts = append(exts, a.Feed.Extension()...)
	exts = append(exts, a.Pipes.Extension()...)
	exts = append(exts, a.Hub.Extension()...)
	exts = append(exts, a.KB.Extension()...)
	handler := server.New(manager, static, meta, exts...)

	listener, err := net.Listen("tcp", o.addr)
	if err != nil {
		fail(fmt.Errorf("не удалось занять %s: %w", o.addr, err))
	}
	url := "http://" + listener.Addr().String()
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	fmt.Println("AnimalGuide — справочник по животным, с которым разговаривают")
	fmt.Println("  интерфейс:  " + url)
	fmt.Println("  модель:     " + model)
	fmt.Println("  источники:  " + strings.Join(local.Names(), ", "))
	fmt.Printf("  диалоги:    %s (загружено: %d)\n", manager.DisplayDir(), loaded)
	fmt.Println("  свод:       " + guide.Store.DisplayPath(invariants.GuideID))
	fmt.Println("  механизмы:  " + defaults.String())
	fmt.Println("  факты:      " + factsLine(a.Feed.Remote))
	fmt.Println("  база:       " + kbLine(a.KB))
	if defaults.On(features.MCP) {
		fmt.Println("  MCP:        включён для новых диалогов; сервер запустится при первом ходе")
	}
	fmt.Println("  остановить: Ctrl+C")

	errs := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			errs <- err
		}
	}()
	if o.open {
		openBrowser(url)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	select {
	case err := <-errs:
		fail(err)
	case <-signals:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	a.Close()
	fmt.Println("Остановлено. Диалоги остались в " + manager.DisplayDir())
}

// factsLine — строка о демоне фактов для стартового вывода. Проверка
// короткая: старт не ждёт демон дольше двух секунд, а приложение работает
// и без него.
func factsLine(r *feed.Remote) string {
	if !r.Configured() {
		return "выключены (-facts-server \"\")"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st := r.Status(ctx)
	switch st.Conn {
	case feed.ConnOK:
		line := r.Server() + " (подключён"
		if st.Version != "" {
			line += ", версия " + st.Version
		}
		return line + ")"
	case feed.ConnDenied:
		return r.Server() + " (отверг токен — " + st.Hint + ")"
	}
	why := "не отвечает — " + st.Hint
	if ctx.Err() != nil {
		why = "не ответил за 2 с — " + st.Hint
	}
	return r.Server() + " (" + why + ")"
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "Ошибка: "+err.Error())
	os.Exit(1)
}
