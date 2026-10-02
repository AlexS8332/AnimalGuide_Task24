package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
)

// journal печатает ход цепочки для человека: шаг, инструмент, аргументы,
// итог с отпечатками, проверки ✓/✗, время, размер и цена.
type journal struct {
	w     io.Writer
	agent bool
	// shown — сколько проверок шага уже напечатано: RunAgent после конца
	// работы модели отдаёт шаги ещё раз, с итоговой проверкой цепочки
	// кодом, — печатается только она, а не шаг заново.
	shown map[int]int
}

// step — onStep для Run и RunAgent. pending не печатается: у кода все три
// шага объявлены заранее, у модели их нет.
func (j *journal) step(s pipeline.Step) {
	switch s.Status {
	case pipeline.StepRunning:
		fmt.Fprintf(j.w, "\n%s %s  %s\n", j.num(s), s.Tool, s.Args)
	case pipeline.StepOK, pipeline.StepFailed:
		if n, ok := j.shown[s.N]; ok {
			j.more(s, n)
			return
		}
		j.shown[s.N] = len(s.Checks)
		j.done(s)
	}
}

func (j *journal) num(s pipeline.Step) string {
	if j.agent {
		return fmt.Sprintf("[%d]", s.N)
	}
	return fmt.Sprintf("[%d/%d]", s.N, len(pipeline.ToolNames))
}

func (j *journal) done(s pipeline.Step) {
	mark := "✓"
	if s.Status == pipeline.StepFailed {
		mark = "✗"
	}
	if s.Digest != "" {
		line := fmt.Sprintf("    %s %s %s", mark, s.Kind, pipeline.Short(s.Digest))
		if s.Input != "" {
			line += " ← " + pipeline.Short(s.Input)
		}
		if s.Summary != "" {
			line += "  " + s.Summary
		}
		fmt.Fprintln(j.w, line)
	}
	if len(s.Checks) > 0 {
		var cs []string
		for _, c := range s.Checks {
			m := "✓"
			if !c.OK {
				m = "✗"
			}
			cs = append(cs, m+" "+c.Name)
		}
		fmt.Fprintln(j.w, "    "+strings.Join(cs, "  "))
	}
	if s.Status == pipeline.StepFailed && s.Error != "" {
		fmt.Fprintln(j.w, "    ✗ "+s.Error)
	}
	meta := []string{seconds(s.Took)}
	if s.Bytes > 0 {
		meta = append(meta, size(s.Bytes))
	}
	if s.CostUSD > 0 {
		meta = append(meta, usd(s.CostUSD))
	}
	fmt.Fprintln(j.w, "    "+strings.Join(meta, " · "))
}

// summary — итог цепочки: файл, цепочка в нём, начало текста.
func (j *journal) summary(tr pipeline.Trace, hint string) {
	fmt.Fprintln(j.w)
	if !tr.OK {
		fmt.Fprintf(j.w, "✗ Цепочка не пройдена: %s\n", tr.Error)
		if hint != "" {
			fmt.Fprintln(j.w, "  Что сделать: "+hint)
		}
	} else {
		fmt.Fprintf(j.w, "✓ Цепочка пройдена за %s, расход %s\n", seconds(tr.Took), usd(tr.CostUSD))
	}
	f := tr.File
	if f == nil {
		return
	}
	fmt.Fprintf(j.w, "Файл: %s (%s, %s, sha256 %s)\n", f.Path, f.Format, size(f.Bytes), pipeline.Short(f.SHA256))
	if len(f.Chain) > 0 {
		var ds []string
		for _, d := range f.Chain {
			ds = append(ds, pipeline.Short(d))
		}
		fmt.Fprintf(j.w, "Цепочка в файле: %s\n", strings.Join(ds, " → "))
	}
	if p := strings.TrimSpace(f.Preview); p != "" {
		fmt.Fprintln(j.w, "\n--- начало файла ---")
		fmt.Fprintln(j.w, p)
		fmt.Fprintln(j.w, "---")
	}
}

func seconds(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d мс", d.Milliseconds())
	}
	return fmt.Sprintf("%.1f с", d.Seconds())
}

func size(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d Б", n)
	}
	return fmt.Sprintf("%.1f КБ", float64(n)/1024)
}

func usd(v float64) string { return fmt.Sprintf("$%.4f", v) }

// more — проверки, добавленные к уже напечатанному шагу.
func (j *journal) more(s pipeline.Step, n int) {
	if n >= len(s.Checks) {
		return
	}
	var cs []string
	for _, c := range s.Checks[n:] {
		m := "✓"
		if !c.OK {
			m = "✗"
		}
		cs = append(cs, m+" "+c.Name)
	}
	j.shown[s.N] = len(s.Checks)
	fmt.Fprintf(j.w, "    %s %s: %s\n", j.num(s), s.Tool, strings.Join(cs, "  "))
}
