// Команда animals-mcp — MCP-сервер источников справочника: шесть
// инструментов русской Википедии и GBIF, те же, что приложение вызывает в
// процессе, три инструмента справочного слоя MDD (mdd_get, mdd_search,
// mdd_changes) и служебный server_info со счётчиками вызовов.
//
// Справочник MDD лежит в SQLite (<data>/trivia.db). Если база пуста, сервер
// скачивает архив MDD в фоне: инструменты mdd_* до конца загрузки честно
// отвечают, что справочник ещё не загружен. Флаг -mdd-update загружает
// (или обновляет по ETag) справочник и завершает работу.
//
// Сервер говорит по протоколу MCP через стандартный ввод-вывод: клиент
// запускает его дочерним процессом, пишет запросы в stdin и читает ответы
// из stdout. Приложение запускает его само, когда в диалоге включён
// механизм mcp; вручную его удобно смотреть клиентом mcp-list:
//
//	go run ./cmd/mcp-list
//
// stdout занят протоколом, поэтому всё человекочитаемое уходит в stderr:
// ошибки — всегда, журнал вызовов — с флагом -v.
//
// С флагом -http сервер — это демон (планировщик выпусков и сводок), который
// ещё и открыт по Streamable HTTP: источники, MDD и инструменты демона для
// нескольких клиентов сразу, а также конвейер из трёх инструментов: search
// (досье о виде из MDD, Википедии и GBIF), summarize (3–5 проверенных
// фактов по досье, платно) и save_to_file (файл Markdown или JSON в
// <data>/exports). Шаги передают друг другу конверт с отпечатком sha256
// данных — целиком (input) или по ref. Токен — MCP_TOKEN или -token; без
// него сервер слушает только loopback, наружу не стартует. Проверка живости — GET
// /healthz.
//
// С флагом -role notes stdio-сервер — другой: «блокнот натуралиста»
// (animals-notes) с тремя инструментами nb_open, nb_add и nb_close. Агент
// открывает блокнот о виде, добавляет разделы со ссылками на
// инструменты-источники и закрывает его — блокнот становится файлом Markdown
// или JSON в <data>/notes. Ни сети, ни базы, ни модели этому серверу не
// нужно; блокноты до закрытия живут в памяти процесса.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"
	_ "time/tzdata" // пояса для Daily и лимита суток: на Windows базы зон может не быть

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/daemon"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

func main() {
	verbose := flag.Bool("v", false, "писать журнал вызовов инструментов в stderr")
	wikiBase := flag.String("wiki-base", os.Getenv("WIKIPEDIA_BASE_URL"), "адрес API Википедии; пусто — ru.wikipedia.org (переменная WIKIPEDIA_BASE_URL)")
	gbifBase := flag.String("gbif-base", os.Getenv("GBIF_BASE_URL"), "адрес API GBIF; пусто — api.gbif.org (переменная GBIF_BASE_URL)")
	dataDir := flag.String("data", ".", "каталог данных сервера: в нём база trivia.db")
	mddURL := flag.String("mdd-url", os.Getenv("MDD_URL"), "адрес архива MDD; пусто — репозиторий MDD на GitHub (переменная MDD_URL)")
	mddSync := flag.Bool("mdd-sync", true, "скачать справочник MDD в фоне, если база пуста")
	mddUpdate := flag.Bool("mdd-update", false, "загрузить или обновить справочник MDD и выйти")
	role := flag.String("role", "sources", "роль stdio-сервера: sources — источники и MDD, notes — блокнот натуралиста (nb_open, nb_add, nb_close)")
	var dc daemonFlags
	flag.BoolVar(&dc.on, "daemon", false, "режим 24/7: выпуск фактов по расписанию, проверка релиза MDD, суточная сводка")
	flag.StringVar(&dc.run, "run", "", "выполнить одно задание демона (issue, summary, mdd) и выйти")
	flag.DurationVar(&dc.every, "every", daemon.DefaultEvery, "как часто собирать выпуск (демон)")
	flag.StringVar(&dc.summaryAt, "summary-at", daemon.DefaultSummaryAt, "время суточной сводки, ЧЧ:ММ")
	flag.StringVar(&dc.mddAt, "mdd-at", daemon.DefaultMDDAt, "время проверки релиза MDD, ЧЧ:ММ")
	flag.Float64Var(&dc.budget, "budget", daemon.DefaultBudget, "лимит расходов на модель в сутки, $; отрицательный — без лимита")
	flag.StringVar(&dc.http, "http", "", "адрес MCP-сервера по HTTP, например 127.0.0.1:8766 (включает режим демона)")
	flag.StringVar(&dc.token, "token", "", "Bearer-токен HTTP-сервера; обязателен, если адрес не loopback (переменная MCP_TOKEN)")
	flag.Parse()
	if dc.token == "" {
		// Не значением по умолчанию: -h печатает умолчания, и токен ушёл бы
		// на экран.
		dc.token = os.Getenv("MCP_TOKEN")
	}

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	switch *role {
	case "sources":
	case "notes":
		os.Exit(runNotes(*dataDir, logger))
	default:
		fmt.Fprintf(os.Stderr, "неизвестная роль %q: sources или notes\n", *role)
		os.Exit(2)
	}

	if dc.on || dc.run != "" || dc.http != "" {
		os.Exit(runDaemon(dc, *dataDir, *mddURL, sources{wiki: *wikiBase, gbif: *gbifBase}, logger))
	}

	// Кэш источников у сервера свой: это второй кэш рядом с кэшем
	// приложения, и он — часть цены механизма.
	fetcher := tools.NewFetcher()
	ts := tools.LocalTools(fetcher, *wikiBase, *gbifBase)

	// Ctrl+C закрывает соединение так же, как закрытый клиентом stdin.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	conn, err := db.Open(ctx, filepath.Join(*dataDir, "trivia.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "база данных:", err)
		os.Exit(1)
	}
	defer conn.Close()
	ref, err := mdd.NewSQLite(ctx, conn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "справочник MDD:", err)
		os.Exit(1)
	}
	sync := func() error {
		start := time.Now()
		res, err := mdd.Sync(ctx, ref, mdd.SyncOptions{URL: *mddURL})
		if err != nil {
			return err
		}
		logger.Info("справочник MDD", "version", res.Release.Version, "species", res.Release.Species,
			"updated", res.Updated, "took", time.Since(start).Round(time.Millisecond))
		return nil
	}
	if *mddUpdate {
		// Итог нужен человеку и без -v: уровень журнала здесь не важен.
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
		if err := sync(); err != nil {
			fmt.Fprintln(os.Stderr, "обновление MDD:", err)
			os.Exit(1)
		}
		return
	}
	if _, err := ref.Release(ctx); errors.Is(err, mdd.ErrNotFound) && *mddSync {
		// В фоне: stdio-сервер должен ответить на initialize сразу, а
		// архив — это 15 МБ и несколько секунд.
		go func() {
			if err := sync(); err != nil && ctx.Err() == nil {
				logger.Warn("справочник MDD не загружен", "err", err)
			}
		}()
	}
	ts = append(ts, mdd.Tools(ref)...)
	srv := mcp.NewServer(ts, mcp.ServerOptions{WikiBase: *wikiBase, GBIFBase: *gbifBase, Fetcher: fetcher, Logger: logger})

	logger.Info("сервер запущен", "transport", "stdio", "version", mcp.Version, "tools", len(ts))
	if err := srv.Run(ctx, &sdk.StdioTransport{}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "сервер остановлен с ошибкой:", err)
		os.Exit(1)
	}
}
