package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
)

// DefaultProbeRepeats — сколько раз проба задаёт вопрос модели без базы.
const DefaultProbeRepeats = 3

// Probe — каждый отвечаемый вопрос (test и dev) задаётся модели без базы
// Repeats раз (по умолчанию 3); вопрос недискриминативен, если верных
// ответов ≥ 2 из 3. Итог — для examples/rag/probe.md и для поля
// discriminative в eval/questions.json.
//
// Вердикт повтора — судья, если он есть и ответил, иначе правило: проба
// ищет вопросы, на которые модель и без базы отвечает верно, и тут важна
// именно верность по существу, а её лучше видит судья. При другом числе
// повторов порог тот же по доле: верных ≥ 2/3 повторов — недискриминативен.
func Probe(ctx context.Context, a *Answerer, qs kb.QuestionSet, j *Judge, repeats int) ([]ProbeRow, error) {
	if repeats <= 0 {
		repeats = DefaultProbeRepeats
	}
	var out []ProbeRow
	for _, split := range []string{kb.SplitTest, kb.SplitDev} {
		for _, q := range qs.Split(split) {
			if !q.Answerable {
				continue
			}
			row := ProbeRow{ID: q.ID, Q: questionText(q)}
			correct := 0
			for i := 0; i < repeats; i++ {
				ans, err := a.Answer(ctx, QuestionOf(q), NoRAG)
				if err != nil {
					return out, fmt.Errorf("%s: %w", q.ID, err)
				}
				row.Cost = row.Cost.Add(ans.Cost)
				v := Rule(q, ans.Text).Verdict
				if j != nil {
					jr, err := j.Grade(ctx, q, ans.Text)
					row.Cost = row.Cost.Add(jr.Cost)
					if ctx.Err() != nil {
						return out, ctx.Err()
					}
					if err == nil {
						v = jr.Verdict
					}
				}
				row.Verdicts = append(row.Verdicts, v)
				row.Answers = append(row.Answers, ans.Text)
				if v == Correct {
					correct++
				}
			}
			row.Discriminative = correct*3 < repeats*2
			out = append(out, row)
		}
	}
	return out, nil
}

// ProbeMarkdown — examples/rag/probe.md: какие вопросы модель знает и без
// базы. Недискриминативный вопрос не меряет базу: на нём оба режима верны.
func ProbeMarkdown(rows []ProbeRow, model string, repeats int, judged bool, at time.Time) string {
	if repeats <= 0 {
		repeats = DefaultProbeRepeats
	}
	var b strings.Builder
	b.WriteString("# Проба дискриминативности\n\n")
	who := "правило кодом"
	if judged {
		who = "судья-модель (правило — если судья не ответил)"
	}
	fmt.Fprintf(&b, "Модель `%s` без базы знаний, температура 0, повторов %d на вопрос; оценка — %s. %s.\n\n",
		model, repeats, who, at.Format("2006-01-02 15:04"))
	b.WriteString("Вопрос **недискриминативен**, если модель без базы отвечает верно в ≥ 2 из 3 повторов: " +
		"на нём база ничего не меняет, и в сравнение режимов он вносит только шум.\n\n")
	var non []string
	var cost float64
	for _, r := range rows {
		cost += r.Cost.USD
		if !r.Discriminative {
			non = append(non, r.ID)
		}
	}
	fmt.Fprintf(&b, "Итог: вопросов %d, недискриминативных %d", len(rows), len(non))
	if len(non) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(non, ", "))
	}
	fmt.Fprintf(&b, ". Цена пробы $%.4f.\n\n", cost)
	b.WriteString("| id | вопрос | вердикты | дискриминативен |\n|---|---|---|---|\n")
	for _, r := range rows {
		vs := make([]string, len(r.Verdicts))
		for i, v := range r.Verdicts {
			vs[i] = string(v)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", r.ID, cell(r.Q, 0), strings.Join(vs, ", "),
			map[bool]string{true: "да", false: "**нет**"}[r.Discriminative])
	}
	b.WriteString("\n## Ответы без базы\n\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "<details><summary>%s — %s</summary>\n\n", r.ID, htmlEsc(r.Q))
		for i, a := range r.Answers {
			v := ""
			if i < len(r.Verdicts) {
				v = string(r.Verdicts[i])
			}
			fmt.Fprintf(&b, "**#%d** — %s\n\n%s\n\n", i+1, v, quote(a))
		}
		b.WriteString("</details>\n\n")
	}
	return b.String()
}

// idLine — строка «"id": "T01"» вопроса в файле.
var idLine = regexp.MustCompile(`^(\s*)"id":\s*"([^"]+)"`)

// SetDiscriminative проставляет поле discriminative вопросам в тексте
// eval/questions.json, не переписывая файл целиком: разметка написана
// руками (массивы в строку, свой порядок полей), и json.MarshalIndent
// превратил бы правку двух десятков полей в diff на весь файл.
//
// Поле есть — меняется значение в той же строке; нет — вставляется
// строкой перед "note" вопроса (порядок полей kb.Question: evidence,
// discriminative, note), а без note — последним полем. Итог сверяется:
// файл разбирается, и от исходного он отличается ровно полями
// discriminative указанных вопросов, иначе — ошибка, файл не трогается.
func SetDiscriminative(raw []byte, values map[string]bool) ([]byte, error) {
	nl := "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		nl = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	seen := map[string]bool{}
	for i := 0; i < len(lines); i++ {
		m := idLine.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		v, ok := values[m[2]]
		if !ok {
			continue
		}
		seen[m[2]] = true
		indent := m[1]
		end := objectEnd(lines, i, indent)
		if end < 0 {
			return nil, fmt.Errorf("%s: не найден конец вопроса", m[2])
		}
		val := fmt.Sprintf("%t", v)
		field := indent + `"discriminative": `
		done := false
		note := -1
		for k := i; k < end; k++ {
			if strings.HasPrefix(lines[k], field) {
				comma := ""
				if strings.HasSuffix(strings.TrimRight(lines[k], " "), ",") {
					comma = ","
				}
				lines[k] = field + val + comma
				done = true
				break
			}
			if note < 0 && strings.HasPrefix(lines[k], indent+`"note":`) {
				note = k
			}
		}
		if done {
			continue
		}
		if note >= 0 {
			lines = insert(lines, note, field+val+",")
			end++
		} else {
			last := end - 1
			for last > i && strings.TrimSpace(lines[last]) == "" {
				last--
			}
			lines[last] = strings.TrimRight(lines[last], " ") + ","
			lines = insert(lines, last+1, field+val)
			end++
		}
		i = end
	}
	for id := range values {
		if !seen[id] {
			return nil, fmt.Errorf("вопроса %s нет в файле", id)
		}
	}
	out := []byte(strings.Join(lines, "\n"))
	if nl != "\n" {
		out = bytes.ReplaceAll(out, []byte("\n"), []byte(nl))
	}
	if err := sameExceptDiscriminative(raw, out, values); err != nil {
		return nil, err
	}
	return out, nil
}

// objectEnd — строка закрывающей скобки объекта вопроса: первая строка с
// «}» на отступ меньше полей.
func objectEnd(lines []string, from int, indent string) int {
	if len(indent) < 2 {
		return -1
	}
	closing := indent[:len(indent)-2] + "}"
	for k := from + 1; k < len(lines); k++ {
		if strings.HasPrefix(lines[k], closing) {
			return k
		}
	}
	return -1
}

func insert(lines []string, at int, s string) []string {
	lines = append(lines, "")
	copy(lines[at+1:], lines[at:])
	lines[at] = s
	return lines
}

// sameExceptDiscriminative — после правки файл разбирается и отличается от
// исходного только полями discriminative указанных вопросов.
func sameExceptDiscriminative(before, after []byte, values map[string]bool) error {
	var a, b kb.QuestionSet
	if err := json.Unmarshal(before, &a); err != nil {
		return fmt.Errorf("исходный файл: %w", err)
	}
	if err := json.Unmarshal(after, &b); err != nil {
		return fmt.Errorf("после правки файл не разбирается: %w", err)
	}
	for i := range a.Questions {
		if v, ok := values[a.Questions[i].ID]; ok {
			a.Questions[i].Discriminative = &v
		}
	}
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("после правки файл отличается не только полями discriminative")
	}
	return nil
}
