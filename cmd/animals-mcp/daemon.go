package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/daemon"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// daemonFlags — флаги режима демона.
type daemonFlags struct {
	on        bool
	run       string
	every     time.Duration
	summaryAt string
	mddAt     string
	budget    float64
	// http — адрес MCP-сервера по Streamable HTTP рядом с планировщиком;
	// token — Bearer-токен для него.
	http  string
	token string
}

// check — согласованность флагов до открытия базы и ключа: ошибку в
// адресе человек должен увидеть сразу, а не после загрузки справочника.
func (f *daemonFlags) check() error {
	if f.http == "" {
		return nil
	}
	if f.run != "" {
		return errors.New("-http и -run несовместимы: -run выполняет одно задание и выходит")
	}
	f.on = true // -http — это демон, открытый по HTTP
	return checkListen(f.http, f.token)
}

// sources — адреса источников для инструментов HTTP-сервера.
type sources struct{ wiki, gbif string }

// runDaemon — режим 24/7 (-daemon, -http) или одно задание (-run).
// Возвращает код выхода. Ключ модели — только из окружения: сервер не ищет
// .env сам, его запускают из консоли, службы или планировщика Windows, где
// переменная задаётся явно.
func runDaemon(f daemonFlags, dataDir, mddURL string, src sources, logger *slog.Logger) int {
	if err := f.check(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "демону нужен ключ DeepSeek в переменной DEEPSEEK_API_KEY")
		return 2
	}
	// Журнал демона нужен человеку всегда, не только с -v.
	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// os.Interrupt — Ctrl+C и Ctrl+Break в консоли (на Windows тоже).
	// SIGTERM на Windows приходит при закрытии окна консоли, выходе из
	// системы и её выключении: у процесса есть несколько секунд, чтобы
	// закрыть базу. Stop-Process и taskkill /F убивают процесс без сигнала.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := daemon.Open(ctx, daemon.Config{
		DataDir: dataDir, Every: f.every, SummaryAt: f.summaryAt, MDDAt: f.mddAt, Budget: f.budget,
		LLM: llm.NewClient(key, os.Getenv("DEEPSEEK_BASE_URL")), Model: os.Getenv("DEEPSEEK_MODEL"),
		MDDURL: mddURL, Log: logger,
		OnRun: func(r schedule.Run) {
			logger.Info("запуск", "job", r.Job, "trigger", r.Trigger, "status", r.Status,
				"cost_usd", fmt.Sprintf("%.4f", r.CostUSD), "ref", r.Ref, "detail", r.Detail, "error", r.Error,
				"took", r.Finished.Sub(r.Started).Round(time.Millisecond))
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "демон:", err)
		return 1
	}
	defer d.Close()

	if f.run != "" {
		r, err := d.Sched.RunNow(ctx, f.run)
		if err != nil {
			fmt.Fprintln(os.Stderr, "задание:", err)
			return 1
		}
		if r.Status != schedule.RunOK {
			return 1
		}
		return 0
	}
	st, err := d.Sched.Status(ctx)
	if err == nil {
		for _, j := range st.Jobs {
			logger.Info("задание", "job", j.Name, "every", j.Every, "daily", j.Daily, "next", j.Next.Format("2006-01-02 15:04"))
		}
		logger.Info("демон запущен", "budget_usd", st.Budget, "spent_today_usd", fmt.Sprintf("%.4f", st.Spent), "location", st.Location)
	}

	if f.http == "" {
		err = d.Run(ctx)
	} else {
		err = serveDaemon(ctx, d, f, src, logger)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "демон:", err)
		return 1
	}
	logger.Info("демон остановлен")
	return 0
}

// serveDaemon — демон с MCP-сервером по HTTP. Порт открывается до старта
// планировщика: занятый порт — ошибка запуска, а не тихий демон без
// сервера. Сервер отвечает и во время первой загрузки MDD — инструменты
// mdd_* до её конца честно говорят, что справочник не загружен.
func serveDaemon(ctx context.Context, d *daemon.Daemon, f daemonFlags, src sources, logger *slog.Logger) error {
	srv, n := daemonServer(d, src, logger)

	ln, err := net.Listen("tcp", f.http)
	if err != nil {
		return fmt.Errorf("HTTP-сервер: %w", err)
	}
	hs := &http.Server{Handler: srv.HTTPHandler(mcp.HTTPOptions{Token: f.token}),
		ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn)}
	logger.Info("MCP-сервер слушает", "url", "http://"+ln.Addr().String()+mcp.MCPPath,
		"health", "http://"+ln.Addr().String()+mcp.HealthPath, "token", f.token != "", "tools", n, "version", mcp.Version)
	return serveUntil(ctx, hs, ln, d.Run)
}

// daemonState — краткое состояние демона для server_info: лимит, расход
// за сутки и ближайшие запуски. Полный снимок отдаёт schedule_status.
func daemonState(ctx context.Context, d *daemon.Daemon) any {
	st, err := d.Status(ctx)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	type job struct {
		Name    string    `json:"name"`
		Running bool      `json:"running"`
		Next    time.Time `json:"next,omitzero"`
		Last    string    `json:"last_status,omitempty"`
	}
	out := struct {
		Mode     string  `json:"mode"`
		Budget   float64 `json:"budget_usd"`
		Spent    float64 `json:"spent_today_usd"`
		Location string  `json:"location"`
		Jobs     []job   `json:"jobs"`
	}{Mode: "daemon", Budget: st.Budget, Spent: st.Spent, Location: st.Location}
	for _, j := range st.Jobs {
		x := job{Name: j.Name, Running: j.Running, Next: j.Next}
		if j.Last != nil {
			x.Last = j.Last.Status
		}
		out.Jobs = append(out.Jobs, x)
	}
	return out
}

// DaemonName — имя демона в initialize и server_info: реестр серверов
// отличает по нему демон от stdio-сервера источников.
const DaemonName = "animals-daemon"

// daemonServer — MCP-сервер демона: источники (как по stdio), справочник
// MDD, инструменты демона и конвейер search → summarize → save_to_file.
// Возвращает и число инструментов — для журнала.
func daemonServer(d *daemon.Daemon, src sources, logger *slog.Logger) (*mcp.Server, int) {
	// Кэш источников свой, как у stdio-сервера.
	fetcher := tools.NewFetcher()
	ts := tools.LocalTools(fetcher, src.wiki, src.gbif)
	ts = append(ts, mdd.Tools(d.MDD)...)
	ts = append(ts, daemonTools(d)...)
	ts = append(ts, pipelineTools(d)...)
	return mcp.NewServer(ts, mcp.ServerOptions{WikiBase: src.wiki, GBIFBase: src.gbif, Fetcher: fetcher, Logger: logger,
		Name: DaemonName, Title: "Демон «Интересных фактов»",
		State: func(ctx context.Context) any { return daemonState(ctx, d) }}), len(ts)
}
