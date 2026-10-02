package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
)

func init() {
	register("qa", "контрольные вопросы: ответы без базы и с базой, оценка правилом и судьёй → examples/rag/compare.md (и JSON)", runQA)
}

// defaultCompare — отчёт сравнения режимов в репозитории (рядом — compare.json).
const defaultCompare = "examples/rag/compare.md"

func runQA(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("qa", "[флаги]", errOut)
	questions := fs.String("questions", "eval/questions.json", "контрольные вопросы")
	splits := fs.String("splits", kb.SplitTest, "наборы через запятую: test, dev, out")
	modes := fs.String("modes", "norag,rag", "режимы через запятую: norag, rag, rag+filter, rag+rewrite, rag+both, rag+cite (out — для «не знаю»: -splits test,out)")
	repeat := fs.Int("repeat", 1, "повторов каждого режима (≥ 2 — замер шума модели)")
	judge := fs.Bool("judge", true, "оценка судьёй-моделью (без судьи — только правило)")
	seed := fs.Int64("seed", 22, "порядок, в котором судья видит ответы")
	mdPath := fs.String("out", defaultCompare, "куда записать отчёт markdown (пусто — не писать); JSON — рядом, с расширением .json")
	jsonPath := fs.String("json", "", "куда записать отчёт JSON (пусто — рядом с -out)")
	af := newAnswerFlags(fs)
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	ms, err := parseModes(*modes)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitUsage
	}
	if *repeat < 1 {
		fmt.Fprintln(errOut, "ошибка: -repeat ≥ 1")
		return exitUsage
	}
	qs, err := kb.LoadQuestions(*questions)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitUsage
	}
	var sp []string
	for _, s := range strings.Split(*splits, ",") {
		if s = strings.TrimSpace(s); s != "" {
			sp = append(sp, s)
		}
	}
	a, closeKB, code := af.answerer(ctx, needsKB(ms), errOut)
	if code >= 0 {
		return code
	}
	defer closeKB()
	o := rag.EvalOptions{Splits: sp, Modes: ms, Repeats: *repeat, Seed: *seed,
		Progress: func(row rag.Row) {
			var parts []string
			for _, m := range ms {
				parts = append(parts, fmt.Sprintf("%s %s", m, orDash(string(row.Majority[m]))))
			}
			fmt.Fprintf(errOut, "  %s %s: %s\n", row.Question.ID, row.Question.Type, strings.Join(parts, ", "))
		}}
	if *judge {
		o.Judge = &rag.Judge{LLM: a.LLM, Model: a.Model}
	}
	fmt.Fprintf(errOut, "kb qa: модель %s, наборы %s, режимы %s, повторов %d, судья %v\n", a.Model, strings.Join(sp, ","), *modes, *repeat, *judge)
	printThresholds(ctx, a, ms, errOut)
	r, err := rag.Eval(ctx, a, qs, o)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	for _, line := range r.Conclusion {
		fmt.Fprintln(out, "- "+line)
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if why := ragFallback(r); why != "" && !explicit["out"] {
		// Закоммиченный отчёт — с векторным поиском; отчёт по BM25 поверх
		// него выдал бы откат за результат (как у kb eval).
		fmt.Fprintf(errOut, "ошибка: векторный поиск не состоялся (%s) — отчёт не записан в %s.\n"+
			"  Поднимите эмбеддер (-embed-url, EMBED_BASE_URL) или укажите путь отчёта явно: -out <файл>.\n", why, *mdPath)
		return exitFailed
	}
	if *mdPath != "" && *jsonPath == "" {
		*jsonPath = strings.TrimSuffix(*mdPath, filepath.Ext(*mdPath)) + ".json"
	}
	if *mdPath != "" {
		if err := writeFile(*mdPath, []byte(r.Markdown())); err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitFailed
		}
		fmt.Fprintln(out, "Отчёт:", *mdPath)
	}
	if *jsonPath != "" {
		raw, err := json.MarshalIndent(r, "", "  ")
		if err == nil {
			err = writeFile(*jsonPath, append(raw, '\n'))
		}
		if err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitFailed
		}
		fmt.Fprintln(out, "JSON:", *jsonPath)
	}
	return exitOK
}

// ragFallback — почему поиск rag шёл не по векторам; пусто — по векторам
// или rag не было.
func ragFallback(r rag.Report) string {
	for _, row := range r.Rows {
		for m, runs := range row.Runs {
			if !m.UsesBase() {
				continue
			}
			for _, run := range runs {
				if run.Answer.Search.Fallback != "" {
					return run.Answer.Search.Fallback
				}
			}
		}
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// printThresholds — фактический порог фильтра у режимов с фильтром и откуда
// он: настройки, индекс (kb calibrate -write) или умолчание.
func printThresholds(ctx context.Context, a *rag.Answerer, ms []rag.Mode, out io.Writer) {
	for _, m := range ms {
		if !m.Pipelined() || a.Pipeline == nil {
			continue
		}
		c := a.Config(m)
		if !c.Filter {
			continue
		}
		v, from, err := a.Pipeline.MinScore(ctx, c)
		if err != nil {
			fmt.Fprintf(out, "kb qa: %s — порог не определён: %v\n", m, err)
			continue
		}
		fmt.Fprintf(out, "kb qa: %s — порог %.3f (%s)\n", m, v, from)
	}
}
