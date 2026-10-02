package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
)

func init() {
	register("probe", "проба дискриминативности: знает ли модель ответ без базы → examples/rag/probe.md, -write — в eval/questions.json", runProbe)
}

// defaultProbe — отчёт пробы в репозитории.
const defaultProbe = "examples/rag/probe.md"

func runProbe(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("probe", "[флаги]", errOut)
	questions := fs.String("questions", "eval/questions.json", "контрольные вопросы")
	repeat := fs.Int("repeat", rag.DefaultProbeRepeats, "сколько раз задать каждый вопрос модели без базы")
	judge := fs.Bool("judge", true, "оценка судьёй-моделью (без судьи — только правило)")
	mdPath := fs.String("out", defaultProbe, "куда записать отчёт markdown (пусто — не писать)")
	write := fs.Bool("write", false, "проставить поле discriminative в файле вопросов (формат файла сохраняется)")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	if *repeat < 1 {
		fmt.Fprintln(errOut, "ошибка: -repeat ≥ 1")
		return exitUsage
	}
	raw, err := os.ReadFile(*questions)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitUsage
	}
	qs, err := kb.LoadQuestions(*questions)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitUsage
	}
	llmc, model, err := chatModel()
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitUsage
	}
	a := &rag.Answerer{LLM: llmc, Model: model}
	var j *rag.Judge
	if *judge {
		j = &rag.Judge{LLM: llmc, Model: model}
	}
	fmt.Fprintf(errOut, "kb probe: модель %s без базы, повторов %d, судья %v\n", model, *repeat, *judge)
	rows, err := rag.Probe(ctx, a, qs, j, *repeat)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	var non []string
	vals := map[string]bool{}
	for _, r := range rows {
		vals[r.ID] = r.Discriminative
		if !r.Discriminative {
			non = append(non, r.ID)
		}
	}
	fmt.Fprintf(out, "Вопросов %d, недискриминативных %d", len(rows), len(non))
	if len(non) > 0 {
		fmt.Fprintf(out, ": %s", strings.Join(non, ", "))
	}
	fmt.Fprintln(out)
	for _, r := range rows {
		if !r.Discriminative {
			fmt.Fprintf(out, "  %s %s — %v\n", r.ID, r.Q, r.Verdicts)
		}
	}
	if *mdPath != "" {
		if err := writeFile(*mdPath, []byte(rag.ProbeMarkdown(rows, model, *repeat, j != nil, time.Now()))); err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitFailed
		}
		fmt.Fprintln(out, "Отчёт:", *mdPath)
	}
	if *write {
		updated, err := rag.SetDiscriminative(raw, vals)
		if err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitFailed
		}
		if err := writeAtomic(*questions, updated); err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitFailed
		}
		fmt.Fprintln(out, "Поле discriminative записано:", *questions)
	}
	return exitOK
}
