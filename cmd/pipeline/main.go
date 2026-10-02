// Команда pipeline — клиент конвейера демона: вызывает по MCP три его
// инструмента по цепочке search → summarize → save_to_file и проверяет
// передачу данных между ними (конверты, отпечатки, вход = выход
// предыдущего шага, цепочка в файле).
//
// Цепочку ведёт код (по умолчанию) или модель (-agent): модели даются
// только эти три инструмента, и она сама вызывает их, передавая данные по
// ref; код затем проверяет её цепочку так же строго.
//
//	pipeline манул                          # демон на 127.0.0.1:8766, токен из MCP_TOKEN
//	pipeline -pass ref -format json манул   # передача по digest, файл JSON
//	pipeline -random                        # случайный вид
//	pipeline -agent манул                   # цепочку ведёт модель (DEEPSEEK_API_KEY)
//	pipeline -json манул                    # след цепочки целиком в JSON
//
// Код выхода 0 — только если цепочка сошлась; 1 — не сошлась; 2 — ошибка
// в аргументах.
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

	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
)

func main() {
	enableUTF8Console()
	loadEnvFiles()
	o, err := parseArgs(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, errHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		fmt.Fprintln(os.Stderr, "Справка: pipeline -h")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, o, os.Stdout))
}

// run запускает цепочку против демона и возвращает код выхода.
func run(ctx context.Context, o options, out io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	remote := feed.NewRemote(o.url, o.token, slog.New(slog.DiscardHandler))
	defer remote.Close()

	j := &journal{w: out, agent: o.agent, shown: map[int]int{}}
	onStep := j.step
	if o.json {
		onStep = nil
	} else {
		fmt.Fprintf(out, "Конвейер: %s — демон %s\n", describe(o), remote.Server())
	}

	var (
		tr  pipeline.Trace
		err error
	)
	if o.agent {
		tr, err = runAgent(ctx, o, remote, onStep)
	} else {
		tr, err = pipeline.Run(ctx, remote, o.req, onStep)
	}
	if o.json {
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(tr)
	} else {
		j.summary(tr, remote.Hint(err))
	}
	if err != nil || !tr.OK {
		return 1
	}
	return 0
}

// runAgent — исполнитель-модель: ключ из окружения (или .env), инструменты
// — описания от демона.
func runAgent(ctx context.Context, o options, remote *feed.Remote, onStep func(pipeline.Step)) (pipeline.Trace, error) {
	fail := func(err error) (pipeline.Trace, error) {
		return pipeline.Trace{Request: o.req, Mode: pipeline.ModeAgent, Error: err.Error()}, err
	}
	key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if key == "" {
		return fail(errors.New("для -agent нужен DEEPSEEK_API_KEY — в окружении или в .env/.env.local"))
	}
	ts, err := remote.Tools(ctx, pipeline.ToolNames...)
	if err != nil {
		return fail(err)
	}
	cfg := pipeline.AgentConfig{
		LLM:   llm.NewClient(key, os.Getenv("DEEPSEEK_BASE_URL")),
		Model: o.model,
		Tools: ts,
	}
	return pipeline.RunAgent(ctx, cfg, o.req, onStep)
}

// describe — что запущено, одной строкой.
func describe(o options) string {
	what := "вид «" + o.req.Query + "»"
	if o.req.Random {
		what = "случайный вид"
	}
	who := "исполнитель — код"
	if o.agent {
		model := o.model
		if model == "" {
			model = llm.DefaultModel
		}
		who = "исполнитель — модель " + model
	}
	return fmt.Sprintf("%s, %s, передача %s, формат %s", what, who, o.req.Pass, o.req.Format)
}
