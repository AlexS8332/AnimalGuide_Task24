package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow"
)

// options — к каким серверам подключаться, какой флоу гнать и как показать.
type options struct {
	config  string // mcp-servers.json; пусто — встроенная конфигурация
	data    string // ${DATA}
	daemon  string // ${DAEMON}
	servers bool   // только показать серверы и маршруты
	preset  string
	species string
	model   string // пусто — llm.DefaultModel
	json    bool
	repeat  int
	timeout time.Duration
}

// errHelp — человек попросил -h: справка уже напечатана.
var errHelp = flag.ErrHelp

const usageText = `Использование:
  mcp-flow [флаги] [вид]        # длинный флоу через все MCP-серверы реестра
  mcp-flow -servers             # серверы, их статус и маршруты инструментов

Серверы — из -config (по умолчанию встроенная конфигурация: sources, daemon,
notes). Модель — DeepSeek, ключ DEEPSEEK_API_KEY в окружении или .env.
Код выхода: 0 — все проверки прошли, 1 — есть провал, 2 — конфигурация,
серверы или ключ.

Флаги:
`

// parseArgs разбирает командную строку. getenv — окружение (в тестах —
// подставное); справка и ошибки флагов пишутся в out.
func parseArgs(args []string, getenv func(string) string, out io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("mcp-flow", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprint(out, usageText)
		fs.PrintDefaults()
	}
	daemon := strings.TrimSpace(getenv("TRIVIA_SERVER"))
	if daemon == "" {
		daemon = feed.DefaultServer
	}
	fs.StringVar(&o.config, "config", "", "конфигурация серверов (mcp-servers.json); пусто — встроенная")
	fs.StringVar(&o.data, "data", ".", "каталог данных — подставляется в конфигурацию вместо ${DATA}")
	fs.StringVar(&o.daemon, "daemon", daemon, "адрес демона — вместо ${DAEMON} (по умолчанию TRIVIA_SERVER или "+feed.DefaultServer+")")
	fs.BoolVar(&o.servers, "servers", false, "подключиться, показать серверы и маршруты (со скрытыми) и выйти")
	fs.StringVar(&o.preset, "preset", "", "заготовка флоу; пусто — "+flow.Presets()[0].ID)
	fs.StringVar(&o.species, "species", "", "вид; то же, что позиционный аргумент; пусто — вид заготовки")
	fs.StringVar(&o.model, "model", getenv("DEEPSEEK_MODEL"), "модель (по умолчанию DEEPSEEK_MODEL или deepseek-v4-flash)")
	fs.BoolVar(&o.json, "json", false, "напечатать трассу (Trace) целиком в JSON")
	fs.IntVar(&o.repeat, "repeat", 1, "прогнать N раз и показать долю прохождения каждой проверки")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "предел на один прогон")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if rest := strings.TrimSpace(strings.Join(fs.Args(), " ")); rest != "" {
		if strings.TrimSpace(o.species) != "" {
			return o, errors.New("вид задан дважды: и -species, и аргументом")
		}
		o.species = rest
	}
	o.species = strings.TrimSpace(o.species)
	o.daemon = strings.TrimSpace(o.daemon)
	if o.repeat < 1 {
		return o, errors.New("-repeat должен быть не меньше 1")
	}
	if o.timeout <= 0 {
		return o, errors.New("-timeout должен быть больше нуля")
	}
	if _, err := flow.Find(o.preset); err != nil {
		return o, errors.New(strings.TrimPrefix(err.Error(), "flow: "))
	}
	return o, nil
}
