// Команда kb — база знаний справочника: снимок корпуса, индекс, поиск,
// сравнение стратегий чанкинга и ответ по базе (ask, qa, probe — нужен
// DEEPSEEK_API_KEY).
//
//	kb fetch                          # снять корпус заново: статьи ru-Википедии
//	kb fetch -only manul,harza        # переснять отдельные статьи
//	kb fetch -mdd-db data/trivia.db   # заодно пересобрать документ MDD
//	kb index -strategy all            # корпус → чанки → эмбеддинги → kb.db
//	kb search "чем питается манул"    # топ чанков каждого индекса
//	kb stats                          # что в базе
//	kb eval -bm25                     # сравнение стратегий → examples/kb/chunking.md
//	kb ask "сколько видов малых панд в MDD v2.5?"  # ответ без базы и с базой рядом
//	kb qa -repeat 2                   # контрольные вопросы → examples/rag/compare.md
//	kb probe -write                   # знает ли модель ответ без базы → examples/rag/probe.md
//	kb search -rewrite code -rerank hybrid -filter -trace "…"  # второй этап поиска с баллами кандидатов
//	kb calibrate -out examples/rag/calibrate.md  # порог релевантности на dev+out (в индекс — с -write)
//	kb matrix -k1 3,5,8 -rrf          # режимы base/filter/rewrite/both/hybrid → examples/rag/filter.md
//	kb qa -modes rag,rag+both         # ответы с конвейером поиска v23
//	kb qa -modes rag+both,rag+cite -splits test,out  # источники, цитаты и «не знаю» (v24)
//	kb help [команда]                 # список команд или справка по одной
//
// Подкоманды регистрируются в своих файлах (cmd/kb/<команда>.go) вызовом
// register в init: добавить команду — один файл, main.go не меняется.
//
// Код выхода 0 — успех; 1 — команда не выполнена; 2 — неверные аргументы
// или окружение.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
)

// Коды выхода.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

// command — подкоманда kb. run получает аргументы после имени команды и
// возвращает код выхода; результат пишет в out, ход работы и ошибки — в
// errOut.
type command struct {
	// summary — одна строка для списка команд.
	summary string
	run     func(ctx context.Context, args []string, out, errOut io.Writer) int
}

// commands — реестр подкоманд; заполняется register из init файлов команд.
var commands = map[string]command{}

// register добавляет подкоманду. Повтор имени — ошибка программиста,
// поэтому паника при старте, а не тихая подмена.
func register(name, summary string, run func(ctx context.Context, args []string, out, errOut io.Writer) int) {
	if _, dup := commands[name]; dup {
		panic("kb: команда " + name + " зарегистрирована дважды")
	}
	commands[name] = command{summary: summary, run: run}
}

func main() {
	enableUTF8Console()
	loadEnvFiles()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(dispatch(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// dispatch выбирает подкоманду по первому аргументу.
func dispatch(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		usage(errOut)
		return exitUsage
	}
	name, rest := args[0], args[1:]
	switch name {
	case "help", "-h", "-help", "--help":
		if len(rest) > 0 {
			if c, ok := commands[rest[0]]; ok {
				// Справка команды — её же разбор флагов с -h.
				return c.run(ctx, []string{"-h"}, out, out)
			}
			fmt.Fprintf(errOut, "ошибка: нет команды %q\n", rest[0])
			usage(errOut)
			return exitUsage
		}
		usage(out)
		return exitOK
	}
	c, ok := commands[name]
	if !ok {
		fmt.Fprintf(errOut, "ошибка: нет команды %q\n", name)
		usage(errOut)
		return exitUsage
	}
	return c.run(ctx, rest, out, errOut)
}

// usage — список команд по алфавиту.
func usage(w io.Writer) {
	fmt.Fprintln(w, "Использование: kb <команда> [флаги]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Команды:")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  %-10s %s\n", n, commands[n].summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Справка по команде: kb help <команда> или kb <команда> -h")
}

// newFlagSet — набор флагов подкоманды с общей обработкой справки: заголовок
// «kb <команда> …», затем флаги.
func newFlagSet(name, synopsis string, errOut io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("kb "+name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintf(errOut, "Использование: kb %s %s\n\n", name, synopsis)
		if c, ok := commands[name]; ok {
			fmt.Fprintln(errOut, c.summary)
			fmt.Fprintln(errOut)
		}
		fmt.Fprintln(errOut, "Флаги:")
		fs.PrintDefaults()
	}
	return fs
}

// parseFlags разбирает флаги и переводит итог в код выхода: -h — exitOK,
// ошибка — exitUsage, иначе -1 (продолжать).
func parseFlags(fs *flag.FlagSet, args []string) int {
	switch err := fs.Parse(args); {
	case err == flag.ErrHelp:
		return exitOK
	case err != nil:
		return exitUsage
	case fs.NArg() > 0:
		fmt.Fprintf(fs.Output(), "ошибка: лишние аргументы: %v\n", fs.Args())
		return exitUsage
	}
	return -1
}
