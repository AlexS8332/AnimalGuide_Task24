package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/hub"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Сколько символов аргументов показывать в строке вызова: текст раздела
// блокнота длинный, а в журнале нужно видеть, что за вызов.
const argsRunes = 100

// progress — ход прогона по мере вызовов: одна строка на завершённый
// вызов. Стрелок «← №k» здесь нет — их находит Verify после прогона, и
// они есть в журнале printTrace.
func progress(w io.Writer) func(flow.Call) {
	return func(c flow.Call) {
		if !c.OK && c.Error == "" {
			return // старт вызова
		}
		fmt.Fprintf(w, "  · #%d %s %s\n", c.N, c.Tool, mark(c))
	}
}

func mark(c flow.Call) string {
	if c.OK {
		return "✓"
	}
	return "✗"
}

// callLine — строка журнала:
//
//	#3  [daemon]  mdd_search {"text":"manul"}  ✓ 120 мс  ← №2
func callLine(c flow.Call) string {
	server := c.Server
	if server == "" {
		server = "?"
	}
	line := fmt.Sprintf("#%-2d [%s]  %s %s  %s %s", c.N, server, c.Tool,
		tools.Truncate(strings.TrimSpace(string(c.Args)), argsRunes), mark(c), ms(c.Took))
	if len(c.From) > 0 {
		from := make([]string, len(c.From))
		for i, n := range c.From {
			from[i] = fmt.Sprintf("№%d", n)
		}
		line += "  ← " + strings.Join(from, ", ")
	}
	switch {
	case c.Error != "":
		line += "\n      ошибка: " + tools.Truncate(c.Error, 200)
	case c.Summary != "":
		line += "\n      " + c.Summary
	}
	return line
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%d мс", d.Milliseconds())
}

func levelMark(l flow.Level) string {
	switch l {
	case flow.LevelOK:
		return "✓"
	case flow.LevelFail:
		return "✗"
	}
	return "⚠"
}

// printTrace — журнал прогона: вызовы, проверки, дельты серверов, файл,
// итог модели и цена.
func printTrace(w io.Writer, tr flow.Trace) {
	fmt.Fprintf(w, "\nВызовы (%d, ответов модели %d):\n", len(tr.Calls), tr.Turns)
	turn := 0
	for _, c := range tr.Calls {
		if c.Turn != turn && turn != 0 {
			fmt.Fprintln(w)
		}
		turn = c.Turn
		fmt.Fprintln(w, indent(callLine(c)))
	}

	fmt.Fprintln(w, "\nПроверки:")
	for _, c := range tr.Verdict.Checks {
		line := fmt.Sprintf("  %s %s", levelMark(c.Level), c.Name)
		if c.Note != "" {
			line += " — " + c.Note
		}
		fmt.Fprintln(w, line)
	}

	fmt.Fprintln(w, "\nСерверы (насчитал сервер / в трассе):")
	for _, d := range tr.Servers {
		fmt.Fprintf(w, "  %-8s", d.Server)
		if d.PID != 0 {
			fmt.Fprintf(w, " pid %d ", d.PID)
		}
		var parts []string
		for _, tool := range union(d.Served, d.Traced) {
			served := "?"
			if d.Served != nil {
				served = fmt.Sprint(d.Served[tool])
			}
			parts = append(parts, fmt.Sprintf("%s %s/%d", tool, served, d.Traced[tool]))
		}
		if len(parts) == 0 {
			parts = []string{"вызовов нет"}
		}
		fmt.Fprintln(w, " "+strings.Join(parts, ", "))
	}

	if tr.File != "" {
		fmt.Fprintln(w, "\nФайл:", tr.File)
		if tr.Preview != "" {
			fmt.Fprintln(w, indent(tools.Truncate(strings.TrimSpace(tr.Preview), 400)))
		}
	}
	if tr.Answer != "" {
		fmt.Fprintln(w, "\nИтог модели:", tr.Answer)
	}
	fmt.Fprintf(w, "\nЦена: $%.4f, токенов %d (кэш %d), время %s\n",
		tr.CostUSD, tr.Usage.Total, tr.Usage.CacheHit, tr.Took.Round(100*time.Millisecond))
	var failed []string
	for _, c := range tr.Verdict.Checks {
		if c.Level == flow.LevelFail {
			failed = append(failed, c.Name)
		}
	}
	switch {
	case tr.OK:
		fmt.Fprintln(w, "Итог: ✓ флоу пройден")
	case tr.Error != "" && len(failed) == 0:
		fmt.Fprintln(w, "Итог: ✗", tr.Error)
	default:
		msg := fmt.Sprintf("Итог: ✗ провалено проверок %d: %s", len(failed), strings.Join(failed, "; "))
		if tr.Error != "" {
			msg += "; " + tr.Error
		}
		fmt.Fprintln(w, msg)
	}
}

// printRates — доля прохождения каждой проверки за -repeat прогонов.
// Имена проверок стабильны между прогонами, поэтому их можно складывать.
func printRates(w io.Writer, traces []flow.Trace) {
	type rate struct{ ok, warn, fail int }
	var order []string
	rates := map[string]*rate{}
	passed := 0
	var cost float64
	for _, tr := range traces {
		if tr.OK {
			passed++
		}
		cost += tr.CostUSD
		for _, c := range tr.Verdict.Checks {
			r, ok := rates[c.Name]
			if !ok {
				r = &rate{}
				rates[c.Name] = r
				order = append(order, c.Name)
			}
			switch c.Level {
			case flow.LevelOK:
				r.ok++
			case flow.LevelWarn:
				r.warn++
			default:
				r.fail++
			}
		}
	}
	n := len(traces)
	fmt.Fprintf(w, "\n━━ Итог %d прогонов: пройдено %d из %d, цена $%.4f\n", n, passed, n, cost)
	for _, name := range order {
		r := rates[name]
		line := fmt.Sprintf("  %3.0f%%  %s", 100*float64(n-r.fail)/float64(n), name)
		if r.fail > 0 {
			line += fmt.Sprintf("  ✗ %d", r.fail)
		}
		if r.warn > 0 {
			line += fmt.Sprintf("  ⚠ %d", r.warn)
		}
		fmt.Fprintln(w, line)
	}
}

// printServers — серверы реестра и маршруты всех инструментов, со скрытыми
// и причинами.
func printServers(ctx context.Context, w io.Writer, r hub.Router) {
	routes, rerr := r.Routes(ctx)
	fmt.Fprintln(w, "Серверы:")
	for _, v := range r.Servers(ctx) {
		fmt.Fprintf(w, "  %-8s %-9s %s — %s %s", v.Name, v.Status, v.Title, v.Transport, v.Addr)
		if v.Reported != "" {
			fmt.Fprintf(w, ", ответил %s", v.Reported)
			if v.Version != "" {
				fmt.Fprintf(w, " %s", v.Version)
			}
		}
		if v.PID != 0 {
			fmt.Fprintf(w, ", pid %d", v.PID)
		}
		fmt.Fprintf(w, "; инструментов %d, скрыто %d\n", v.Tools, v.Hidden)
		if v.Reason != "" {
			fmt.Fprintln(w, "           причина:", v.Reason)
		}
		if v.Hint != "" {
			fmt.Fprintln(w, "           подсказка:", v.Hint)
		}
		var shown, hidden []string
		for _, rt := range routes {
			if rt.Server != v.Name {
				continue
			}
			if rt.Hidden {
				hidden = append(hidden, rt.Tool+" ("+rt.Reason+")")
			} else {
				shown = append(shown, rt.Tool)
			}
		}
		if len(shown) > 0 {
			fmt.Fprintln(w, "           → "+strings.Join(shown, ", "))
		}
		if len(hidden) > 0 {
			fmt.Fprintln(w, "           скрыты: "+strings.Join(hidden, ", "))
		}
	}
	if rerr != nil {
		fmt.Fprintln(w, "  маршруты:", rerr)
	}
}

// downServers — предупреждение о серверах, которые не подключились: флоу
// пойдёт без их инструментов и почти наверняка провалится, и лучше, чтобы
// причина была видна сразу.
func downServers(ctx context.Context, w io.Writer, r hub.Router) {
	for _, v := range r.Servers(ctx) {
		if v.Status == hub.StatusOK || v.Status == hub.StatusIdle {
			continue
		}
		line := fmt.Sprintf("⚠ сервер %s: %s", v.Name, v.Status)
		if v.Reason != "" {
			line += " — " + v.Reason
		}
		if v.Hint != "" {
			line += "; " + v.Hint
		}
		fmt.Fprintln(w, line)
	}
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

func union(a, b map[string]int) []string {
	var out []string
	for k := range a {
		out = append(out, k)
	}
	for k := range b {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
