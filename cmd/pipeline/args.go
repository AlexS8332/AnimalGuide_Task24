package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
)

// options — что запустить и как показать.
type options struct {
	url     string
	token   string
	req     pipeline.Request
	agent   bool   // цепочку ведёт модель (RunAgent), а не код (Run)
	model   string // модель исполнителя; пусто — llm.DefaultModel
	json    bool   // печатать Trace целиком
	timeout time.Duration
}

// errHelp — человек попросил -h: справка уже напечатана.
var errHelp = flag.ErrHelp

const usageText = `Использование:
  pipeline [флаги] <вид>        # search → summarize → save_to_file у демона
  pipeline -random [флаги]      # то же для случайного вида

Демон — animals-mcp -http 127.0.0.1:8766; токен — -token или MCP_TOKEN.
С -agent цепочку ведёт модель (нужен DEEPSEEK_API_KEY).

Флаги:
`

// parseArgs разбирает командную строку. getenv — окружение (в тестах —
// подставное); справка и ошибки флагов пишутся в out.
func parseArgs(args []string, getenv func(string) string, out io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("pipeline", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprint(out, usageText)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.url, "url", feed.DefaultServer, "адрес демона animals-mcp -http")
	fs.StringVar(&o.token, "token", getenv("MCP_TOKEN"), "токен демона (по умолчанию из MCP_TOKEN)")
	fs.BoolVar(&o.req.Random, "random", false, "случайный вид из MDD вместо названия")
	fs.StringVar(&o.req.Format, "format", pipeline.FormatMarkdown, "формат файла: md или json")
	fs.StringVar(&o.req.Pass, "pass", pipeline.PassInline, "передача между шагами: inline (конверт целиком) или ref (только digest)")
	fs.BoolVar(&o.agent, "agent", false, "цепочку ведёт модель: сама зовёт три инструмента, передавая данные по ref")
	fs.StringVar(&o.model, "model", getenv("DEEPSEEK_MODEL"), "модель для -agent (по умолчанию DEEPSEEK_MODEL или deepseek-v4-flash)")
	fs.BoolVar(&o.json, "json", false, "напечатать след цепочки (Trace) целиком в JSON")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "предел на всю цепочку")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.req.Query = strings.Join(fs.Args(), " ")
	o.url = strings.TrimSpace(o.url)
	if o.url == "" {
		return o, errors.New("не задан адрес демона (-url)")
	}
	if o.timeout <= 0 {
		return o, errors.New("-timeout должен быть больше нуля")
	}
	if o.agent && o.req.Pass == pipeline.PassInline && passSet(fs) {
		return o, errors.New("-agent передаёт данные только по ref: -pass inline с ним не сочетается")
	}
	req, err := o.req.Normalize()
	if err != nil {
		return o, errors.New(strings.TrimPrefix(err.Error(), pipeline.ErrRequest.Error()+": "))
	}
	if o.agent {
		req.Pass = pipeline.PassRef
	}
	o.req = req
	return o, nil
}

// passSet — задан ли -pass явно.
func passSet(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "pass" {
			set = true
		}
	})
	return set
}
