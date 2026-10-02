package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
)

func init() {
	register("ask", "спросить отвечающего агента: ответ без базы и с базой знаний рядом, найденные фрагменты, цена", runAsk)
}

func runAsk(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("ask", `[флаги] "вопрос"`, errOut)
	mode := fs.String("mode", "both", "режимы через запятую: norag (без базы), rag (с базой), rag+filter, rag+rewrite, rag+both, rag+cite (источники и цитаты); both — norag и rag")
	var prior listFlag
	fs.Var(&prior, "context", "предыдущая реплика пользователя (вопрос-продолжение); флаг можно повторять")
	af := newAnswerFlags(fs)
	// Вопрос — позиционный аргумент; флаги можно писать и после него.
	var query []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return exitUsage
		}
		if fs.NArg() == 0 {
			break
		}
		query = append(query, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	q := strings.TrimSpace(strings.Join(query, " "))
	if q == "" {
		fmt.Fprintln(errOut, "ошибка: нет вопроса")
		fs.Usage()
		return exitUsage
	}
	modes, err := parseModes(*mode)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitUsage
	}
	a, closeKB, code := af.answerer(ctx, needsKB(modes), errOut)
	if code >= 0 {
		return code
	}
	defer closeKB()

	question := rag.Question{Text: q, Context: prior}
	var total llm.Cost
	for i, m := range modes {
		if i > 0 {
			fmt.Fprintln(out)
		}
		ans, err := a.Answer(ctx, question, m)
		if err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitFailed
		}
		total = total.Add(ans.Cost)
		fmt.Fprintf(out, "== %s ==\n", m)
		if m.UsesBase() {
			if t := ans.Trace; t != nil && t.Rewritten != t.Original {
				fmt.Fprintf(out, "Запрос в поиск: %s\n", t.Rewritten)
			}
			if t := ans.Trace; t != nil && t.Config.Filter && t.MinScoreFrom != "" {
				anchor := "якоря нет — пол действует"
				if len(t.Anchored) > 0 {
					anchor = "вид назван в запросе (" + strings.Join(t.Anchored, ", ") + ") — пол не применяется"
				}
				if len(t.Scope) > 0 {
					anchor += "; рамка якоря — только статьи " + strings.Join(t.Scope, ", ") + " (и документы без вида)"
				}
				fmt.Fprintf(out, "Порог %.3f (%s); лучший косинус %.3f, отрыв %.3f; %s\n", t.MinScore, t.MinScoreFrom, t.TopDense, t.Gap, anchor)
			}
			line := fmt.Sprintf("Найдено (%s, %s", ans.Search.Index, ans.Search.Mode)
			if ans.Search.Embedder != "" {
				line += " " + ans.Search.Embedder
			}
			if ans.Search.Fallback != "" {
				line += ", откат: " + ans.Search.Fallback
			}
			fmt.Fprintf(out, "%s, %.1f мс):\n", line, ans.Search.Millis)
			if len(ans.Hits) == 0 {
				fmt.Fprintln(out, "  ничего не найдено")
			}
			for _, h := range ans.Hits {
				path := h.Section
				if len(h.Path) > 0 {
					path = strings.Join(h.Path, " › ")
				}
				fmt.Fprintf(out, "%2d. %.3f  %-28s %s › %s\n", h.Rank, h.Score, h.ID, h.Title, path)
			}
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out, ans.Text)
		if ans.Cited != nil {
			printCheck(out, ans)
		}
		fmt.Fprintf(out, "(%s; токены %d → %d, из кэша %d; %s; %d мс)\n", a.Model, ans.Usage.Prompt, ans.Usage.Completion,
			ans.Usage.CacheHit, usd(ans.Cost), ans.Millis)
	}
	if len(modes) > 1 {
		fmt.Fprintf(out, "\nИтого: %s\n", usd(total))
	}
	return exitOK
}

// printCheck — проверка kb_answer кодом (rag+cite): статус, «не знаю» по
// решению кода, источники и цитаты, отказы, что не так.
func printCheck(out io.Writer, ans rag.Answer) {
	r := ans.Cited
	ck := r.Check
	line := "Проверка kb_answer: " + r.Cited.Status
	switch {
	case ck.Forced:
		why := ck.GateReason
		if why == "" {
			_, why = rag.Gate(ans.Trace, ans.Hits)
		}
		line += " — «не знаю» по решению кода: " + why
	case r.Cited.Unknown():
		line += fmt.Sprintf(" — «не знаю» по решению модели; ближайшее в базе — источников %d", len(r.Cited.Sources))
	default:
		line += fmt.Sprintf("; источников %d, цитат %d, дословных %d из %d", len(r.Cited.Sources), len(r.Cited.Quotes),
			len(r.Cited.Quotes)-len(ck.NotVerbatim), len(r.Cited.Quotes))
	}
	line += fmt.Sprintf("; отказов проверки %d", ck.Rejects)
	switch {
	case ck.Unverified:
		line += "; НЕ ПРОВЕРЕНО"
	case ck.OK:
		line += "; принят"
	}
	fmt.Fprintln(out, line)
	for _, p := range ck.Problems {
		fmt.Fprintln(out, "  - "+p)
	}
	if len(ck.NumbersMissing) > 0 {
		fmt.Fprintln(out, "  числа ответа без цитаты: "+strings.Join(ck.NumbersMissing, ", "))
	}
	if len(ck.SpeciesMismatch) > 0 {
		fmt.Fprintln(out, "  вид в ответе без цитаты из своей статьи: "+strings.Join(ck.SpeciesMismatch, ", "))
	}
}
