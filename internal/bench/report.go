package bench

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

// Печать отчёта. Разделы идут по убыванию доказательности: сначала
// проверки с порогами, потом отчётные числа и цена с разбивкой запроса по
// блокам механизмов, и только в конце — сами ответы. Ответы в отчёте есть,
// а не пересказаны: признаки кодом ошибаются, и проверить их можно только
// чтением.

// Meta — о прогоне целиком.
type Meta struct {
	Model   string
	Started time.Time
	Elapsed time.Duration
	// Registry — реестр механизмов, чтобы подписать блоки названиями.
	Registry *features.Registry
	// Samples — сколько ответов показывать на испытание; 0 — 6.
	Samples int
}

// Markdown — отчёт прогона.
func Markdown(meta Meta, results []*Result) string {
	var b strings.Builder
	b.WriteString("# Регрессионный набор испытаний\n\n")
	fmt.Fprintf(&b, "Модель: `%s`, температура 0. Прогон: %s, длительность %s.", orDash(meta.Model),
		meta.Started.Format("2006-01-02 15:04"), meta.Elapsed.Round(time.Second))
	if cost, ok := totalCost(results); ok {
		fmt.Fprintf(&b, " Цена прогона: $%.4f.", cost)
	}
	b.WriteString("\n\n")
	b.WriteString("Каждое испытание сравнивает дорожки, которые отличаются **ровно одним** механизмом реестра: " +
		"стенд проверяет это до первого хода и иначе не запускается. Дорожки идут в ногу — один вопрос всем, " +
		"следующий только после ответа всех. Жёсткие проверки — с порогом из ТЗ; отчётные числа — для сравнения " +
		"дорожек. «?» — проверку на этом прогоне не определить: механизм, по следам которого она считается, " +
		"не подключён или сценарий до неё не дошёл.\n\n")

	b.WriteString("## Сводка\n\n| испытание | сравнивается | проверок принято | итог |\n|---|---|---|---|\n")
	for _, r := range results {
		mech := "—"
		if r.Mechanism != "" {
			mech = "`" + string(r.Mechanism) + "`"
		}
		checks := fmt.Sprintf("%d из %d", r.Count(Pass), len(r.Checks))
		if n := r.Count(Pending); n > 0 {
			checks += fmt.Sprintf(" (не определено %d)", n)
		}
		verdict := verdictWord(r.Verdict())
		switch {
		case r.Skipped != "":
			verdict, checks = "не гонялось", "—"
		case r.Err != "":
			verdict = "стенд сломался"
		}
		fmt.Fprintf(&b, "| %s. %s | %s | %s | %s |\n", r.ID, r.Title, mech, checks, verdictMark(r.Verdict())+" "+verdict)
	}
	b.WriteString("\n")

	for _, r := range results {
		writeTrial(&b, meta, r)
	}
	return b.String()
}

func writeTrial(b *strings.Builder, meta Meta, r *Result) {
	fmt.Fprintf(b, "## %s. %s\n\n", r.ID, r.Title)
	if r.Goal != "" {
		b.WriteString(capitalize(r.Goal) + ".\n\n")
	}
	if r.Skipped != "" {
		b.WriteString("Не гонялось: " + r.Skipped + ".\n\n")
		return
	}
	if r.Err != "" {
		b.WriteString("**Стенд сломался:** " + escape(r.Err) + "\n\n")
	}
	if len(r.Lanes) > 0 {
		b.WriteString("### Дорожки\n\n| дорожка | отличие от основной | что это |\n|---|---|---|\n")
		for _, l := range r.Lanes {
			fmt.Fprintf(b, "| **%s** | %s | %s |\n", escape(l.Name), escape(l.Diff), escape(l.Note))
		}
		b.WriteString("\n")
	}
	if len(r.Checks) > 0 {
		b.WriteString("### Проверки\n\n| | проверка | дорожка | порог | получено |\n|---|---|---|---|---|\n")
		for _, c := range r.Checks {
			got := escape(c.Got)
			if c.Note != "" {
				got += " — " + escape(clip(c.Note, 300))
			}
			fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", verdictMark(c.Status), escape(c.What), escape(orDash(c.Lane)), escape(c.Want), got)
		}
		b.WriteString("\n")
	}
	writeMetrics(b, r)
	writeStats(b, meta, r)
	if len(r.Notes) > 0 {
		b.WriteString("### Заметки\n\n")
		for _, n := range r.Notes {
			b.WriteString("- " + escape(n) + "\n")
		}
		b.WriteString("\n")
	}
	writeSamples(b, meta, r)
}

// writeMetrics — отчётные числа таблицей «число × дорожка».
func writeMetrics(b *strings.Builder, r *Result) {
	if len(r.Metrics) == 0 {
		return
	}
	var lanes, whats []string
	cell := map[[2]string]string{}
	for _, m := range r.Metrics {
		lane := m.Lane
		if lane == "" {
			lane = "—"
		}
		if !contains(lanes, lane) {
			lanes = append(lanes, lane)
		}
		if !contains(whats, m.What) {
			whats = append(whats, m.What)
		}
		cell[[2]string{m.What, lane}] = m.Value
	}
	b.WriteString("### Отчётные числа\n\n| |")
	for _, l := range lanes {
		if l == "—" {
			b.WriteString(" значение |")
			continue
		}
		b.WriteString(" **" + escape(l) + "** |")
	}
	b.WriteString("\n|---" + strings.Repeat("|---", len(lanes)) + "|\n")
	for _, w := range whats {
		b.WriteString("| " + escape(w) + " |")
		for _, l := range lanes {
			v, ok := cell[[2]string{w, l}]
			if !ok {
				v = "—"
			}
			b.WriteString(" " + escape(v) + " |")
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

// writeStats — цена дорожек и разбивка запроса по блокам механизмов.
func writeStats(b *strings.Builder, meta Meta, r *Result) {
	if len(r.Stats) == 0 {
		return
	}
	b.WriteString("### Цена и разбивка запроса\n\n")
	b.WriteString("Разбивка — оценка до отправки, в токенах на ход, по самому тяжёлому запросу хода. " +
		"Выключенный механизм в разбивке просто отсутствует. Калибровка — оценка против `usage` по всем запросам дорожки.\n\n")
	b.WriteString("| |")
	for _, s := range r.Stats {
		b.WriteString(" **" + escape(s.Lane) + "** |")
	}
	b.WriteString("\n|---" + strings.Repeat("|---", len(r.Stats)) + "|\n")
	row := func(title string, f func(LaneStats) string) {
		b.WriteString("| " + title + " |")
		for _, s := range r.Stats {
			b.WriteString(" " + f(s) + " |")
		}
		b.WriteString("\n")
	}
	row("ходов", func(s LaneStats) string {
		if s.Failed > 0 {
			return fmt.Sprintf("%d (неудачных %d)", s.Turns, s.Failed)
		}
		return fmt.Sprint(s.Turns)
	})
	row("запросов к модели", func(s LaneStats) string { return fmt.Sprintf("%d (%.1f на ход)", s.LLMCalls, s.PerTurn()) })
	row("вызовов инструментов", func(s LaneStats) string { return fmt.Sprint(s.ToolCalls) })
	row("токенов запроса / ответа", func(s LaneStats) string { return fmt.Sprintf("%d / %d", s.Usage.Prompt, s.Usage.Completion) })
	row("из кэша", func(s LaneStats) string { return fmt.Sprintf("%.0f %%", s.CacheShare()*100) })
	row("стоимость", func(s LaneStats) string { return money(s.Cost) })
	row("время", func(s LaneStats) string { return fmt.Sprintf("%.0f с", s.Seconds) })
	row("брак (Effective ≠ Requested)", func(s LaneStats) string { return fmt.Sprint(s.Brak) })
	row("**постоянная часть**, сред. / макс.", func(s LaneStats) string { return fmt.Sprintf("%d / %d", s.Constant, s.ConstantMax) })
	row("системный промпт", func(s LaneStats) string { return fmt.Sprint(s.System) })
	for _, n := range blockNames(r.Stats, meta.Registry) {
		title := "блок `" + string(n) + "`"
		if meta.Registry != nil {
			if m, ok := meta.Registry.Get(n); ok {
				title = "блок «" + m.Title + "»"
			}
		}
		row(title, func(s LaneStats) string {
			for _, bt := range s.Blocks {
				if bt.Feature == n {
					return fmt.Sprintf("%d (в %d ходах)", bt.Tokens, bt.Turns)
				}
			}
			return "—"
		})
	}
	row("описания инструментов", func(s LaneStats) string { return fmt.Sprint(s.Tools) })
	row("окно истории", func(s LaneStats) string { return fmt.Sprint(s.History) })
	row("новая реплика", func(s LaneStats) string { return fmt.Sprint(s.User) })
	row("калибровка: ошибка без поправки / после", func(s LaneStats) string {
		if s.Calibration.Pairs == 0 {
			return "—"
		}
		return fmt.Sprintf("%+.1f %% / %.1f %% (×%.3f, пар %d)", s.Calibration.RawPct, s.Calibration.ResidualPct, s.Calibration.Factor, s.Calibration.Pairs)
	})
	b.WriteString("\n")
}

// blockNames — механизмы, чьи блоки встретились хоть у одной дорожки, в
// порядке места в запросе.
func blockNames(stats []LaneStats, reg *features.Registry) []features.Name {
	seen := map[features.Name]bool{}
	var out []features.Name
	for _, s := range stats {
		for _, bt := range s.Blocks {
			if !seen[bt.Feature] {
				seen[bt.Feature] = true
				out = append(out, bt.Feature)
			}
		}
	}
	place := func(n features.Name) int {
		if reg != nil {
			if m, ok := reg.Get(n); ok {
				return int(m.Place)
			}
		}
		return 1 << 30
	}
	sort.SliceStable(out, func(i, j int) bool { return place(out[i]) < place(out[j]) })
	return out
}

func writeSamples(b *strings.Builder, meta Meta, r *Result) {
	if len(r.Samples) == 0 {
		return
	}
	limit := meta.Samples
	if limit <= 0 {
		limit = 6
	}
	b.WriteString("### Что получил человек\n\n")
	for i, s := range r.Samples {
		if i >= limit {
			fmt.Fprintf(b, "…и ещё ответов: %d — они в файлах диалогов каталога прогона.\n\n", len(r.Samples)-limit)
			break
		}
		fmt.Fprintf(b, "**%s** — %s", escape(s.Topic), escape(s.Lane))
		if s.Note != "" {
			b.WriteString(" (" + escape(clip(s.Note, 200)) + ")")
		}
		b.WriteString("\n\n> " + escape(clip(s.User, 200)) + "\n\n```\n" + fence(clipLines(s.Reply, 900)) + "\n```\n\n")
	}
}

func totalCost(results []*Result) (float64, bool) {
	var sum llm.Cost
	for _, r := range results {
		for _, s := range r.Stats {
			sum = sum.Add(s.Cost)
		}
	}
	return sum.USD, sum.Known
}

func money(c llm.Cost) string {
	if !c.Known {
		if c.USD == 0 {
			return "—"
		}
		return fmt.Sprintf("≈$%.4f", c.USD)
	}
	return fmt.Sprintf("$%.4f", c.USD)
}

func verdictMark(s Status) string {
	switch s {
	case Pass:
		return "✓"
	case Fail:
		return "✗"
	}
	return "?"
}

// escape — текст для ячейки таблицы markdown.
func escape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.Join(strings.Fields(s), " ")
}

func fence(s string) string { return strings.ReplaceAll(s, "```", "ʼʼʼ") }

// clipLines — обрезка с сохранением переносов строк.
func clipLines(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func capitalize(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
