package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/daemon"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// daemonTools — инструменты демона для HTTP-сервера (facts_*, summary_*,
// schedule_status, run_now). Переменная, а не прямой вызов: тест
// подменяет её фейковыми инструментами.
var daemonTools = daemon.Tools

// pipelineTools — конвейер search → summarize → save_to_file над демоном:
// досье о виде, проверенные факты по нему и файл в <data>/exports. Тоже
// переменная: тест подставляет фейковые источники, чтобы пройти цепочку
// по HTTP без сети.
var pipelineTools = func(d *daemon.Daemon) []tools.Tool { return pipeline.Tools(d.PipelineDeps()) }

// checkListen проверяет адрес HTTP-сервера. Наружу без токена слушать
// нельзя: run_now и summary_build тратят деньги на модель, а без токена их
// вызовет любой, кто достучится до порта. На loopback без токена можно —
// туда ходят только процессы этой машины (от страниц браузера защищает
// проверка Origin в самом обработчике).
func checkListen(addr, token string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("адрес %q: нужен вид хост:порт, например 127.0.0.1:8766", addr)
	}
	if port == "" {
		return fmt.Errorf("адрес %q: не указан порт", addr)
	}
	if token != "" || isLoopback(host) {
		return nil
	}
	return fmt.Errorf("адрес %q доступен не только с этой машины, а токен не задан: "+
		"задайте MCP_TOKEN (или -token) либо слушайте 127.0.0.1", addr)
}

// isLoopback — только этой машины ли адрес. Пустой хост («:8766») — все
// интерфейсы. Имя, кроме localhost, не резолвим: что оно значит в момент
// запуска и через час — разные вещи, поэтому такое имя требует токена.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// shutdownTimeout — сколько ждать начатые HTTP-вызовы при остановке.
// Выпуск фактов (run_now issue) — это несколько секунд модели; дольше ждать
// незачем: остановку просил человек.
const shutdownTimeout = 30 * time.Second

// serveUntil обслуживает HTTP на уже открытом ln и крутит планировщик
// run, пока не отменят ctx (Ctrl+C) или один из них не упадёт. Остановка —
// по порядку: сначала HTTP (новые вызовы больше не принимаются, начатые
// доживают до таймаута), затем планировщик. Обратный порядок оборвал бы
// начатый по HTTP run_now: ручной запуск отменяется вместе с контекстом
// планировщика, и клиент получил бы ошибку за уже оплаченный вызов.
func serveUntil(ctx context.Context, srv *http.Server, ln net.Listener, run func(context.Context) error) error {
	httpErr := make(chan error, 1)
	go func() { httpErr <- srv.Serve(ln) }()

	// Контекст планировщика не наследует отмену ctx: его гасим сами, после HTTP.
	schedCtx, stopSched := context.WithCancel(context.WithoutCancel(ctx))
	defer stopSched()
	runErr := make(chan error, 1)
	go func() { runErr <- run(schedCtx) }()

	var errs []error
	httpDone, runDone := false, false
	select {
	case <-ctx.Done():
	case err := <-httpErr:
		httpDone = true
		errs = append(errs, fmt.Errorf("HTTP-сервер: %w", err))
	case err := <-runErr:
		runDone = true
		if err != nil {
			errs = append(errs, fmt.Errorf("демон: %w", err))
		} else {
			errs = append(errs, errors.New("демон: планировщик остановился сам"))
		}
	}

	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		// Не дождались — рвём соединения: вызовы получат отмену контекста.
		srv.Close()
		errs = append(errs, fmt.Errorf("HTTP-сервер не остановился за %s: %w", shutdownTimeout, err))
	}
	if !httpDone {
		if err := <-httpErr; !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, fmt.Errorf("HTTP-сервер: %w", err))
		}
	}
	stopSched()
	if !runDone {
		if err := <-runErr; err != nil && !errors.Is(err, context.Canceled) {
			errs = append(errs, fmt.Errorf("демон: %w", err))
		}
	}
	return errors.Join(errs...)
}
