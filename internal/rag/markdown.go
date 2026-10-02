package rag

import (
	"fmt"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
)

// Markdown — отчёт для examples/rag/compare.md: шапка, сводка режимов,
// таблица по вопросам (ожидание, вердикты по режимам, источники), ответы
// целиком в раскрывающихся блоках, расхождения голосов, вывод.
//
// Вывод идёт сразу после шапки: отчёт читают ради него, а таблицы и
// ответы ниже — чтобы его проверить.
func (r Report) Markdown() string {
	var b strings.Builder
	modes := r.modes()
	b.WriteString("# Ответ с базой знаний и без\n\n")
	fmt.Fprintf(&b, "Модель `%s`, температура 0. Индекс `%s`, k = %d, эмбеддер: %s. corpus_sha `%s`. Повторов: %d. %s.\n\n",
		r.Model, r.Index, r.K, orDash(r.Embedder), orDash(r.CorpusSHA), r.Repeats, r.Created.Format("2006-01-02 15:04"))
	var costs []string
	for _, s := range r.Stats {
		costs = append(costs, fmt.Sprintf("%s %s", s.Mode, usd(s.Cost)))
	}
	fmt.Fprintf(&b, "Цена: ответы %s; судья %s.\n\n", strings.Join(costs, ", "), usd(r.JudgeCost))
	b.WriteString("Режимы отличаются ровно контекстом: один и тот же отвечающий агент без инструментов и тот же системный промпт; " +
		"у `rag` к нему добавлено правило «опирайся на фрагменты, называй [chunk_id]», а к вопросу — найденные фрагменты. " +
		"Оценка — два голоса: правило кодом по ожиданию вопроса и судья-модель, которая видит один ответ и не знает режима " +
		"(ответы оцениваются в перемешанном порядке). Итог прогона — вердикт судьи, без судьи — правила; итог вопроса — " +
		"большинство повторов, ничья — в худшую сторону.\n\n")

	b.WriteString("## Вывод\n\n")
	for _, c := range r.Conclusion {
		b.WriteString("- " + c + "\n")
	}
	b.WriteString("\n## Сводка режимов\n\n")
	b.WriteString("| режим | вопросов | верно | частично | неверно | «не знаю» | уверенных ошибок на отвечаемых | «не знаю» без ответа в базе | ответ по существу без ответа в базе | верно на дискриминативных | доказательство в выдаче | согласие правила и судьи | смен вердикта | токены (кэш) | цена | мс на ответ |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	un := r.unanswerable()
	for _, s := range r.Stats {
		recall, agree := "—", "—"
		if s.Mode.UsesBase() {
			recall = fmt.Sprintf("%.0f %%", 100*s.Recall)
		}
		if s.Judged > 0 {
			agree = fmt.Sprintf("%.0f %% из %d", 100*s.Agreement, s.Judged)
		}
		flips := "—"
		if r.Repeats > 1 {
			flips = fmt.Sprintf("%d (%.0f %%)", s.Flips, 100*s.FlipRate)
		}
		fmt.Fprintf(&b, "| `%s` | %d | %d | %d | %d | %d | %d | %d из %d | %d из %d | %d из %d | %s | %s | %s | %d (%d) | %s | %d |\n",
			s.Mode, s.Questions, s.Correct, s.Partial, s.Wrong, s.Abstain, s.ConfidentWrong, s.RightAbstain, un,
			s.AnsweredUnanswerable, un, s.DiscriminativeCorrect, s.Discriminative, recall, agree, flips, s.Usage.Prompt+s.Usage.Completion, s.Usage.CacheHit, usd(s.Cost), s.AvgMillis)
	}

	if cited := r.citedStats(); len(cited) > 0 {
		b.WriteString("\n## Источники и цитаты\n\n")
		b.WriteString("Числа по прогонам: у каждого прогона свой kb_answer. Источники — у ответов по существу (answered) и у «не знаю» с непустой выдачей («ближайшее в базе»); " +
			"«не знаю» при пустой выдаче (ниже порога) источников не имеет; цитаты — у ответов по существу; " +
			"дословность — доля цитат, найденных в своём фрагменте после нормализации; смысл — судья раскладывает ответ на утверждения " +
			"и проверяет каждое по цитатам (только по ним, не по памяти).\n\n")
		b.WriteString("| режим | kb_answer | по существу | «не знаю» | из них кодом | «не знаю» на отвечаемых | «не знаю» без выдачи | с источниками | с цитатами | дословных цитат | смысл подтверждён | не проверено | отказов проверки |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, s := range cited {
			support := "—"
			if s.SupportChecked > 0 {
				support = fmt.Sprintf("%d из %d", s.Supported, s.SupportChecked)
			}
			fmt.Fprintf(&b, "| `%s` | %d | %d | %d | %d | %d | %d | %d из %d | %d из %d | %.0f %% | %s | %d | %d |\n",
				s.Mode, s.Cited, s.Answered, s.Unknown, s.ForcedUnknown, s.FalseUnknown, s.UnknownEmpty,
				s.WithSources+s.UnknownNearSources, s.Answered+s.Unknown-s.UnknownEmpty,
				s.WithQuotes, s.Answered, 100*s.Verbatim, support, s.Unverified, s.Rejects)
		}
	}

	b.WriteString("\n## Вопросы\n\n")
	b.WriteString("Вердикт — большинство повторов; в скобках — повторы по порядку. Источники — найденные `rag` фрагменты " +
		"(✓ — из ожидаемого документа) против ожидаемых; «доказательство» — есть ли в выдаче фрагмент, покрывающий " +
		"дословную цитату-доказательство.\n\n")
	b.WriteString("| id | тип | вопрос | ожидание |")
	for _, m := range modes {
		fmt.Fprintf(&b, " %s |", m)
	}
	b.WriteString(" источники |\n|---|---|---|---|")
	for range modes {
		b.WriteString("---|")
	}
	b.WriteString("---|\n")
	for _, row := range r.Rows {
		q := row.Question
		fmt.Fprintf(&b, "| %s | %s | %s | %s |", q.ID, q.Type, cell(questionText(q), 0), cell(expectText(q), 160))
		for _, m := range modes {
			fmt.Fprintf(&b, " %s |", verdictCell(row, m))
		}
		fmt.Fprintf(&b, " %s |\n", cell(sourcesCell(row), 0))
	}

	b.WriteString("\n## Ответы\n\n")
	for _, row := range r.Rows {
		q := row.Question
		fmt.Fprintf(&b, "<details><summary>%s — %s</summary>\n\n", q.ID, htmlEsc(questionText(q)))
		if e := expectText(q); e != "" {
			fmt.Fprintf(&b, "Ожидание: %s\n\n", e)
		}
		for _, m := range modes {
			for _, run := range row.Runs[m] {
				fmt.Fprintf(&b, "**%s #%d** — %s\n\n", m, run.Repeat, runLine(run))
				if run.Error != "" {
					fmt.Fprintf(&b, "> ошибка: %s\n\n", run.Error)
					continue
				}
				b.WriteString(quote(run.Answer.Text) + "\n\n")
			}
		}
		b.WriteString("</details>\n\n")
	}

	b.WriteString("## Расхождения правила и судьи\n\n")
	if len(r.Disagreements) == 0 {
		if !r.judged() {
			b.WriteString("Судьи не было.\n")
		} else {
			b.WriteString("Нет: голоса согласны во всех прогонах.\n")
		}
	}
	for _, d := range r.Disagreements {
		b.WriteString("- " + d + "\n")
	}
	return b.String()
}

// citedStats — сводки режимов с kb_answer.
func (r Report) citedStats() []ModeStats {
	var out []ModeStats
	for _, s := range r.Stats {
		if s.Cited > 0 {
			out = append(out, s)
		}
	}
	return out
}

// modes — режимы отчёта в порядке сводки.
func (r Report) modes() []Mode {
	var out []Mode
	for _, s := range r.Stats {
		out = append(out, s.Mode)
	}
	return out
}

func (r Report) judged() bool {
	for _, s := range r.Stats {
		if s.Judged > 0 {
			return true
		}
	}
	return false
}

// questionText — вопрос с предыдущими репликами.
func questionText(q kb.Question) string {
	if len(q.Context) == 0 {
		return q.Q
	}
	return strings.Join(q.Context, " → ") + " → " + q.Q
}

// expectText — ожидание кратко: Note, у неотвечаемого — «данных нет».
func expectText(q kb.Question) string {
	note := ""
	if q.Expect != nil {
		note = strings.TrimSpace(q.Expect.Note)
	}
	if !q.Answerable && note == "" {
		note = "в базе нет — ожидается «не знаю»"
	}
	return note
}

// verdictCell — большинство и повторы: «✓ correct (correct, partial)».
func verdictCell(row Row, m Mode) string {
	runs := row.Runs[m]
	if len(runs) == 0 {
		return "—"
	}
	v := row.Majority[m]
	s := mark(row.Question, v) + " " + orDash(string(v))
	if len(runs) > 1 {
		var each []string
		for _, r := range runs {
			if r.Error != "" {
				each = append(each, "ошибка")
				continue
			}
			each = append(each, string(r.Final))
		}
		s += " (" + strings.Join(each, ", ") + ")"
	}
	return s
}

// mark — значок вердикта для вопроса.
func mark(q kb.Question, v Verdict) string {
	switch goodness(q, v) {
	case 3:
		return "✓"
	case 2:
		return "≈"
	}
	if v == Abstain {
		return "?"
	}
	return "✗"
}

// sourcesCell — найденные фрагменты первого успешного прогона rag против
// ожидаемых источников.
func sourcesCell(row Row) string {
	want := map[string]bool{}
	var wants []string
	for _, s := range row.Question.Sources {
		want[s.DocID] = true
		w := s.DocID
		if s.Section != "" {
			w += " › " + s.Section
		}
		wants = append(wants, w)
	}
	var got []string
	recall := ""
	for _, r := range row.Runs[RAG] {
		if r.Error != "" {
			continue
		}
		for _, h := range r.Answer.Hits {
			m := "✗"
			if want[h.DocID] {
				m = "✓"
			}
			got = append(got, "`"+h.ID+"` "+m)
		}
		if row.Question.Answerable && len(row.Question.Evidence) > 0 {
			recall = "доказательство: " + map[bool]string{true: "✓", false: "✗"}[r.Recall]
		}
		break
	}
	var parts []string
	if len(got) > 0 {
		parts = append(parts, "найдено: "+strings.Join(got, ", "))
	}
	if len(wants) > 0 {
		parts = append(parts, "ожидалось: "+strings.Join(wants, "; "))
	} else {
		parts = append(parts, "ожидалось: — (ответа в базе нет)")
	}
	if recall != "" {
		parts = append(parts, recall)
	}
	return strings.Join(parts, "<br>")
}

// runLine — вердикты прогона: итог, правило, судья с причиной.
func runLine(r Run) string {
	if r.Error != "" {
		return "ошибка"
	}
	s := fmt.Sprintf("**%s**; правило: %s", r.Final, r.Rule.Verdict)
	if r.Rule.Note != "" {
		s += " (" + r.Rule.Note + ")"
	}
	switch {
	case r.Judge != nil:
		s += fmt.Sprintf("; судья: %s — %s", r.Judge.Verdict, r.Judge.Reason)
	case r.JudgeError != "":
		s += "; судья не ответил: " + r.JudgeError
	}
	if c := r.Answer.Cited; c != nil {
		s += "; " + citeCell(*c, r.Answer.Support, r.SupportError)
	}
	if len(r.Answer.Hits) > 0 {
		s += fmt.Sprintf("; фрагментов %d, поиск %s", len(r.Answer.Hits), r.Answer.Search.Mode)
	}
	s += fmt.Sprintf("; %d мс, %s", r.Answer.Millis, usd(r.Answer.Cost))
	return s
}

// citeCell — проверка kb_answer одной строкой: статус, отказы,
// дословность, смысл.
func citeCell(c CitedResult, sp *Support, spErr string) string {
	ck := c.Check
	parts := []string{"kb_answer: " + status(c.Cited.Status)}
	if ck.Forced {
		parts = append(parts, "«не знаю» по решению кода")
	}
	if !c.Cited.Unknown() {
		parts = append(parts, fmt.Sprintf("источников %d, цитат %d, дословных %d", len(c.Cited.Sources), len(c.Cited.Quotes),
			len(c.Cited.Quotes)-len(ck.NotVerbatim)))
	}
	if ck.Rejects > 0 {
		parts = append(parts, fmt.Sprintf("отказов проверки %d", ck.Rejects))
	}
	if ck.Unverified {
		parts = append(parts, "НЕ ПРОВЕРЕНО: "+strings.Join(ck.Problems, "; "))
	}
	if len(ck.NumbersMissing) > 0 {
		parts = append(parts, "чисел нет в цитатах: "+strings.Join(ck.NumbersMissing, ", "))
	}
	switch {
	case sp != nil && !c.Cited.Unknown():
		var bad []string
		for _, cl := range sp.Claims {
			if !cl.Supported {
				bad = append(bad, "«"+cl.Claim+"»")
			}
		}
		s := fmt.Sprintf("смысл: подтверждено %d из %d", sp.Supported, sp.Supported+sp.Unsupported)
		if len(bad) > 0 {
			s += " (нет в цитатах: " + strings.Join(bad, ", ") + ")"
		}
		parts = append(parts, s)
	case spErr != "":
		parts = append(parts, "судья смысла не ответил: "+spErr)
	}
	return strings.Join(parts, ", ")
}

// cell — текст для ячейки таблицы: одной строкой, без «|»; n > 0 — обрезка.
func cell(s string, n int) string {
	s = strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", "\\|")
	if n > 0 {
		s = clip(s, n)
	}
	if s == "" {
		return "—"
	}
	return s
}

// quote — текст ответа цитатой markdown.
func quote(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

func htmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
