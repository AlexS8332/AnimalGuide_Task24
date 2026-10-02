package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Интерактивный режим: соединение с сервером остаётся открытым, а
// команды набирает человек. Полезнее всего он на чужом сервере —
// посмотрел список инструментов, тут же попробовал вызвать.

// session — состояние разговора с сервером.
type replSession struct {
	client *mcp.ClientSession
	tools  []*mcp.Tool
	full   bool
	// timeout — предел на один вызов, а не на весь сеанс: между
	// командами человек думает сколько угодно, и общий дедлайн
	// превратил бы все вызовы после раздумий в «context deadline
	// exceeded».
	timeout time.Duration
	out     io.Writer
}

func repl(ctx context.Context, client *mcp.ClientSession, tools []*mcp.Tool, full bool, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	r := &replSession{client: client, tools: tools, full: full, timeout: timeout, out: os.Stdout}

	fmt.Println()
	section("Ручной режим")
	fmt.Println("Команды: tools, schema <инструмент>, call <инструмент> [аргументы],")
	fmt.Println("info, full on|off, help, quit. Имя инструмента можно писать без call.")
	fmt.Println("Аргументы — JSON-объект или пары ключ=значение: search_wikipedia query=рысь")

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for {
		fmt.Print("\n> ")
		if !in.Scan() {
			// Конец ввода (Ctrl+D, Ctrl+Z) — обычное завершение.
			fmt.Println()
			return in.Err()
		}

		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		done, err := r.exec(ctx, line)
		if err != nil {
			// Ошибка команды не должна ронять сессию: человек
			// ошибается в аргументах чаще, чем сервер падает.
			fmt.Fprintln(os.Stderr, "  ошибка:", err)
			continue
		}
		if done {
			return nil
		}
	}
}

// exec выполняет одну команду. Возвращает true, если пора выходить.
func (r *replSession) exec(ctx context.Context, line string) (bool, error) {
	command, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)

	switch command {
	case "quit", "exit", "q":
		return true, nil

	case "help", "?":
		r.help()
		return false, nil

	case "tools":
		r.printTools(rest)
		return false, nil

	case "schema":
		if rest == "" {
			return false, fmt.Errorf("укажите инструмент: schema read_wikipedia")
		}
		name, flag, _ := strings.Cut(rest, " ")
		tool, ok := r.tool(name)
		if !ok {
			return false, r.unknown(name)
		}
		printTool(r.index(name), tool, strings.TrimSpace(flag) == "json")
		return false, nil

	case "info":
		return false, r.info(ctx)

	case "full":
		switch rest {
		case "on":
			r.full = true
		case "off":
			r.full = false
		case "":
		default:
			return false, fmt.Errorf("full on или full off")
		}
		fmt.Printf("  результаты вызовов: %s\n", map[bool]string{true: "целиком", false: "обрезанные"}[r.full])
		return false, nil

	case "call":
		if rest == "" {
			return false, fmt.Errorf("укажите инструмент: call server_info")
		}
		name, args, _ := strings.Cut(rest, " ")
		return false, r.call(ctx, name, args)

	default:
		// Имя инструмента можно писать без call: так короче, а спутать
		// его с командой нельзя — набор команд закрытый.
		if _, ok := r.tool(command); ok {
			return false, r.call(ctx, command, rest)
		}
		return false, r.unknown(command)
	}
}

func (r *replSession) call(ctx context.Context, name, rawArgs string) error {
	if _, ok := r.tool(name); !ok {
		return r.unknown(name)
	}
	args, err := parseArgs(rawArgs)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	if err := callTool(ctx, r.client, name, args, r.full); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%s не ответил за %s (предел задаёт флаг -timeout)", name, r.timeout)
		}
		return err
	}
	return nil
}

func (r *replSession) tool(name string) (*mcp.Tool, bool) {
	for _, t := range r.tools {
		if t.Name == name {
			return t, true
		}
	}
	return nil, false
}

func (r *replSession) index(name string) int {
	for i, t := range r.tools {
		if t.Name == name {
			return i + 1
		}
	}
	return 0
}

// unknown — не просто «нет такого», а подсказка с похожими именами:
// инструментов полтора десятка, и опечатка стоит дешевле поиска.
func (r *replSession) unknown(name string) error {
	var similar []string
	for _, t := range r.tools {
		if strings.Contains(t.Name, name) || strings.Contains(name, t.Name) {
			similar = append(similar, t.Name)
		}
	}
	if len(similar) > 0 {
		return fmt.Errorf("нет команды или инструмента «%s»; похожие: %s", name, strings.Join(similar, ", "))
	}
	return fmt.Errorf("нет команды или инструмента «%s»; список даёт tools", name)
}

func (r *replSession) printTools(filter string) {
	filter = strings.ToLower(strings.TrimSpace(filter))

	width := 0
	for _, t := range r.tools {
		width = max(width, len(t.Name))
	}
	shown := 0
	for _, t := range r.tools {
		if filter != "" && !strings.Contains(strings.ToLower(t.Name+" "+t.Title), filter) {
			continue
		}
		shown++
		title := t.Title
		if !readOnly(t) {
			title += "  " + writeMark
		}
		fmt.Printf("  %-*s  %s\n", width, t.Name, title)
	}
	if shown == 0 {
		fmt.Printf("  по «%s» ничего не нашлось\n", filter)
		return
	}
	fmt.Println("\n  подробности: schema <инструмент>, вызов: <инструмент> ключ=значение")
}

func (r *replSession) info(ctx context.Context) error {
	init := r.client.InitializeResult()
	field("имя", fmt.Sprintf("%s %s", init.ServerInfo.Name, init.ServerInfo.Version))
	field("протокол", init.ProtocolVersion)
	field("умеет", capabilities(init.Capabilities))
	if init.Instructions != "" {
		field("инструкция", init.Instructions)
	}
	// Сведения о себе сервер тоже отдаёт инструментом — спросим и его,
	// если он есть: там номер процесса и счётчики вызовов.
	if _, ok := r.tool("server_info"); ok {
		return r.call(ctx, "server_info", "")
	}
	return nil
}

func (r *replSession) help() {
	fmt.Println(`  tools [фильтр]          список инструментов
  schema <инструмент>     аргументы инструмента; schema <инструмент> json — схема целиком
  call <инструмент> [арг] вызов; слово call можно опустить
  info                    сведения о сервере
  full on|off             печатать результаты целиком или обрезанными
  help                    эта справка
  quit                    выход (или Ctrl+D)

  Аргументы — JSON-объект или пары ключ=значение:
    search_wikipedia query=манул
    read_wikipedia title="Обыкновенная рысь" section=Питание
    taxon_children usage_key=9703 limit=5
    match_taxon {"scientific_name":"Lynx lynx"}`)
}
