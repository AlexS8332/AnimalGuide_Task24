package retrieve

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
)

// Presets — матрица режимов задания 23: base (без фильтра и rewrite),
// filter, rewrite (code), both (rewrite code + filter), hybrid (rewrite
// code + rerank hybrid + filter); дополнительные платные строки
// llm-rewrite и llm-rerank — только по запросу.
func Presets(paid bool) []Named {
	out := []Named{
		{Name: "base"},
		{Name: "filter", Config: Config{Filter: true}},
		{Name: "rewrite", Config: Config{Rewrite: RewriteCode}},
		// both — переписывание и фильтр, без гибрида: на dev гибрид в both
		// дал только −D16 (ранг 5 → 12) и MRR 0.63 → 0.53. Гибрид —
		// отдельной строкой hybrid. С v24 — с рамкой якоря (Scope):
		// фрагменты чужих видов при названном виде отсекаются.
		{Name: "both", Config: Config{Rewrite: RewriteCode, Filter: true, Scope: true}},
		{Name: "hybrid", Config: Config{Rewrite: RewriteCode, Rerank: RerankHybrid, Filter: true}},
	}
	if paid {
		out = append(out,
			Named{Name: "llm-rewrite", Config: Config{Rewrite: RewriteLLM, Filter: true}},
			Named{Name: "llm-rerank", Config: Config{Rewrite: RewriteCode, Rerank: RerankLLM, Filter: true}})
	}
	return out
}

// Describe — конфигурация словами: «rewrite code, rerank hybrid, filter».
func (c Config) Describe() string {
	var parts []string
	if c.Rewrite != RewriteNone {
		parts = append(parts, "rewrite "+string(c.Rewrite))
	}
	if c.Rerank != RerankNone {
		parts = append(parts, "rerank "+string(c.Rerank))
	}
	if c.Filter {
		parts = append(parts, "filter")
	}
	if c.Filter && c.Scope {
		parts = append(parts, "scope")
	}
	if len(parts) == 0 {
		return "dense top-K1, без фильтра и переписывания"
	}
	return strings.Join(parts, ", ")
}

// evidence — фрагмент-доказательство в рунах текста документа.
type evidence struct {
	doc  string
	s, e int
}

// evidenceSet — позиции доказательств: то же правило, что kb.Compare и
// rag.Run.Recall (чанк покрывает ≥ kb.EvidenceCover фрагмента хотя бы
// одного доказательства своего документа).
type evidenceSet struct {
	st    *kb.Store
	texts map[string]string
}

func (x *evidenceSet) of(ctx context.Context, q kb.Question) []evidence {
	var out []evidence
	for _, e := range q.Evidence {
		text, ok := x.texts[e.DocID]
		if !ok {
			d, err := x.st.Doc(ctx, e.DocID)
			if err != nil {
				x.texts[e.DocID] = ""
				continue
			}
			text = d.Text()
			x.texts[e.DocID] = text
		}
		if start, n := corpus.Find(text, e.Quote); start >= 0 {
			out = append(out, evidence{doc: e.DocID, s: start, e: start + n})
		}
	}
	return out
}

func relevant(c kb.Chunk, ev []evidence) bool {
	for _, e := range ev {
		if e.doc == c.DocID && kb.Covers(c.Start, c.End, e.s, e.e) >= kb.EvidenceCover {
			return true
		}
	}
	return false
}

// unanswerable — вопрос без ответа в базе: out и answerable=false.
func unanswerable(q kb.Question) bool { return q.Split == kb.SplitOut || !q.Answerable }

// measured — числа вопроса для накопителя строки.
type measured struct {
	relHits int // итоговых фрагментов, покрывающих доказательство
	cut     int // кандидатов, отсечённых фильтром
	relCut  int // из них покрывающих доказательство
}

// measure — вопрос в строке матрицы при данном K1 (фильтр уже применён).
func measure(q kb.Question, ev []evidence, t Trace) (MatrixQ, measured) {
	row := MatrixQ{ID: q.ID, Type: q.Type, Kept: len(t.Hits), Empty: t.Empty,
		Answerable: q.Answerable && len(ev) > 0, Unanswerable: unanswerable(q),
		TopDense: round3(t.TopDense), Anchored: len(t.Anchored) > 0}
	if t.Rewritten != "" && t.Rewritten != t.Original {
		row.Rewritten = t.Rewritten
	}
	var m measured
	if row.Answerable {
		for _, c := range t.Candidates {
			if relevant(c.Chunk, ev) {
				if row.RankBefor == 0 {
					row.RankBefor = c.Final
				}
				row.InDense = row.InDense || c.RankDense > 0
			}
		}
		for i, h := range t.Hits {
			if relevant(h.Chunk, ev) {
				m.relHits++
				if row.RankAfter == 0 {
					row.RankAfter = i + 1
				}
			}
		}
	}
	for _, c := range t.Candidates {
		if c.FilterCut() {
			m.cut++
			if row.Answerable && relevant(c.Chunk, ev) {
				m.relCut++
			}
		}
	}
	return row, m
}

// lostByFilter — доказательство было среди кандидатов, в итог не попало, и
// хотя бы один релевантный кандидат отсечён фильтром (а не K1).
func lostByFilter(ev []evidence, t Trace, row MatrixQ) bool {
	if !row.Answerable || row.RankBefor == 0 || row.RankAfter > 0 {
		return false
	}
	for _, c := range t.Candidates {
		if c.FilterCut() && relevant(c.Chunk, ev) {
			return true
		}
	}
	return false
}

// rowAcc — накопитель строки матрицы.
type rowAcc struct {
	row                 MatrixRow
	inDense, before     int
	after               int
	rr, precision       float64
	cut, cands          int
	cutAns, relCut      int
	lost, outEmpty      int
	tokens, ms, costUSD float64
	questions           int
}

// RunMatrix — матрица по конфигурациям × K1 × наборам. Поиск (с платными
// шагами) идёт один раз на конфигурацию и вопрос; фильтр и K1
// применяются к его выдаче для каждого K1 отдельно. Вопрос-продолжение
// получает Context в Query.
func RunMatrix(ctx context.Context, p *Pipeline, qs kb.QuestionSet, configs []Named, k1s []int, splits []string) (Matrix, error) {
	if p == nil || p.Searcher == nil || p.Searcher.Store == nil {
		return Matrix{}, errors.New("конвейеру не передана база знаний")
	}
	if len(configs) == 0 {
		configs = Presets(false)
	}
	if len(k1s) == 0 {
		k1s = []int{DefaultK1}
	}
	if len(splits) == 0 {
		splits = []string{kb.SplitDev, kb.SplitTest, kb.SplitOut}
	}
	st := p.Searcher.Store
	m := Matrix{Created: time.Now().UTC(), Configs: configs, Delta: DefaultDelta, K0: DefaultK0}
	if man, err := st.Manifest(ctx); err == nil {
		m.CorpusSHA = man.CorpusSHA
	}
	m.Embedder = "нет — поиск BM25"
	if p.Searcher.Embedder != nil {
		m.Embedder = p.Searcher.Embedder.Model()
	}
	first := resolve(configs[0].Config)
	m.Index = first.Index
	if first.Delta > 0 {
		m.Delta = first.Delta
	}
	m.K0 = first.K0
	ix, err := st.Index(ctx, m.Index)
	if err != nil {
		return m, err
	}
	// Порог для шапки — как его выберет конвейер: из настроек первой
	// конфигурации с фильтром, иначе из индекса, иначе умолчание.
	m.MinScore = DefaultMinScore
	if ix.MinScore > 0 {
		m.MinScore = ix.MinScore
	}
	for _, nc := range configs {
		if nc.Config.Filter && nc.Config.MinScore > 0 {
			m.MinScore = nc.Config.MinScore
			break
		}
	}
	ev := &evidenceSet{st: st, texts: map[string]string{}}
	accs := map[string]*rowAcc{}
	key := func(name, split string, k1 int) string { return fmt.Sprintf("%s\x00%s\x00%d", name, split, k1) }

	for _, nc := range configs {
		for _, sp := range splits {
			for _, k1 := range k1s {
				accs[key(nc.Name, sp, k1)] = &rowAcc{row: MatrixRow{Name: nc.Name, Split: sp, K1: k1}}
			}
			for _, q := range qs.Split(sp) {
				started := time.Now()
				t, err := p.gather(ctx, Query{Text: q.Q, Context: q.Context}, nc.Config)
				if err != nil {
					return m, fmt.Errorf("%s, %s: %w", nc.Name, q.ID, err)
				}
				ms := float64(time.Since(started).Microseconds()) / 1000
				if t.Info.Fallback != "" && m.Fallback == "" {
					m.Fallback = t.Info.Fallback
				}
				e := ev.of(ctx, q)
				for _, k1 := range k1s {
					tt := t
					tt.Candidates = append([]Candidate(nil), t.Candidates...)
					tt.Config.K1 = k1
					finish(&tt)
					a := accs[key(nc.Name, sp, k1)]
					row, mq := measure(q, e, tt)
					row.Cut = lostByFilter(e, tt, row)
					a.row.Rows = append(a.row.Rows, row)
					a.questions++
					a.ms += ms
					// Платные шаги — один раз на вопрос: у каждого K1 та же цена.
					a.costUSD += tt.Cost.USD
					a.cut += mq.cut
					a.cands += len(tt.Candidates)
					for _, h := range tt.Hits {
						a.tokens += float64(h.Tokens)
					}
					if row.Answerable {
						a.row.N++
						if row.InDense {
							a.inDense++
						}
						if row.RankBefor > 0 {
							a.before++
						}
						if row.RankAfter > 0 {
							a.after++
							a.rr += 1 / float64(row.RankAfter)
						}
						// Пустой итог на отвечаемом вопросе — точность 0.
						if len(tt.Hits) > 0 {
							a.precision += float64(mq.relHits) / float64(len(tt.Hits))
						}
						a.cutAns += mq.cut
						a.relCut += mq.relCut
						if row.Cut {
							a.lost++
						}
					}
					if row.Unanswerable {
						a.row.OutN++
						if row.Empty {
							a.outEmpty++
						}
					}
				}
			}
		}
	}
	for _, nc := range configs {
		for _, sp := range splits {
			for _, k1 := range k1s {
				a := accs[key(nc.Name, sp, k1)]
				r := a.row
				r.RecallBefore = ratio(a.inDense, r.N)
				r.RecallUnion = ratio(a.before, r.N)
				r.RecallAfter = ratio(a.after, r.N)
				if r.N > 0 {
					r.MRR = a.rr / float64(r.N)
					r.Precision = a.precision / float64(r.N)
				}
				r.CutShare = ratio(a.cut, a.cands)
				r.WrongCut = ratio(a.relCut, a.cutAns)
				r.LostQ = ratio(a.lost, r.N)
				r.OutEmpty = ratio(a.outEmpty, r.OutN)
				if a.questions > 0 {
					r.Tokens = a.tokens / float64(a.questions)
					r.Millis = a.ms / float64(a.questions)
				}
				r.CostUSD = a.costUSD
				m.Rows = append(m.Rows, r)
			}
		}
	}
	m.Conclusion = conclude(m, k1s)
	return m, nil
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// row — строка матрицы.
func (m Matrix) row(name, split string, k1 int) (MatrixRow, bool) {
	for _, r := range m.Rows {
		if r.Name == name && r.Split == split && r.K1 == k1 {
			return r, true
		}
	}
	return MatrixRow{}, false
}

// questionsOf — вопросы конфигурации при K1 на наборах (по порядку).
func (m Matrix) questionsOf(name string, k1 int, splits ...string) []MatrixQ {
	var out []MatrixQ
	for _, sp := range splits {
		if r, ok := m.row(name, sp, k1); ok {
			out = append(out, r.Rows...)
		}
	}
	return out
}

// headK1 — K1 для вывода: 5, если он считался, иначе первый.
func headK1(k1s []int) int {
	for _, k := range k1s {
		if k == DefaultK1 {
			return k
		}
	}
	if len(k1s) > 0 {
		return k1s[0]
	}
	return DefaultK1
}

// conclude — вывод кодом, числами в вопросах: на dev+test отвечаемых около
// 27, на неотвечаемых около 10, и доля в одну сотую тут ничего не значит.
func conclude(m Matrix, k1s []int) []string {
	k1 := headK1(k1s)
	var out []string
	answerSplits := []string{kb.SplitDev, kb.SplitTest}
	base := m.questionsOf("base", k1, answerSplits...)
	hitSet := func(rows []MatrixQ) map[string]bool {
		s := map[string]bool{}
		for _, r := range rows {
			if r.Answerable && r.RankAfter > 0 {
				s[r.ID] = true
			}
		}
		return s
	}
	count := func(rows []MatrixQ, f func(MatrixQ) bool) int {
		n := 0
		for _, r := range rows {
			if f(r) {
				n++
			}
		}
		return n
	}
	n := count(base, func(r MatrixQ) bool { return r.Answerable })
	baseHits := hitSet(base)
	var parts []string
	for _, nc := range m.Configs {
		rows := m.questionsOf(nc.Name, k1, answerSplits...)
		if len(rows) == 0 {
			continue
		}
		hits := hitSet(rows)
		s := fmt.Sprintf("%s %d", nc.Name, len(hits))
		if nc.Name != "base" && len(base) > 0 {
			var gain, loss []string
			for id := range hits {
				if !baseHits[id] {
					gain = append(gain, id)
				}
			}
			for id := range baseHits {
				if !hits[id] {
					loss = append(loss, id)
				}
			}
			sort.Strings(gain)
			sort.Strings(loss)
			var d []string
			if len(gain) > 0 {
				d = append(d, "+"+strings.Join(gain, ", "))
			}
			if len(loss) > 0 {
				d = append(d, "−"+strings.Join(loss, ", "))
			}
			if len(d) > 0 {
				s += " (" + strings.Join(d, "; ") + ")"
			}
		}
		parts = append(parts, s)
	}
	if n > 0 {
		out = append(out, fmt.Sprintf("K1 = %d, dev+test, %d вопросов с доказательством: доказательство в итоговой выдаче — %s.",
			k1, n, strings.Join(parts, ", ")))
	}

	all := []string{kb.SplitDev, kb.SplitTest, kb.SplitOut}
	var un, tok []string
	unN := 0
	for _, nc := range m.Configs {
		rows := m.questionsOf(nc.Name, k1, all...)
		if len(rows) == 0 {
			continue
		}
		u := count(rows, func(r MatrixQ) bool { return r.Unanswerable })
		unN = u
		e := count(rows, func(r MatrixQ) bool { return r.Unanswerable && r.Empty })
		un = append(un, fmt.Sprintf("%s %d", nc.Name, e))
		var sum float64
		var qn int
		for _, sp := range all {
			if r, ok := m.row(nc.Name, sp, k1); ok {
				sum += r.Tokens * float64(len(r.Rows))
				qn += len(r.Rows)
			}
		}
		if qn > 0 {
			tok = append(tok, fmt.Sprintf("%s %.0f", nc.Name, sum/float64(qn)))
		}
	}
	if unN > 0 {
		out = append(out, fmt.Sprintf("Неотвечаемые (out и answerable=false, %d): пусто после фильтра — %s из %d.", unN, strings.Join(un, ", "), unN))
	}
	if len(tok) > 0 {
		out = append(out, fmt.Sprintf("Токенов итоговых фрагментов на вопрос (K1 = %d, все наборы): %s.", k1, strings.Join(tok, ", ")))
	}
	for _, nc := range m.Configs {
		if !nc.Config.Filter {
			continue
		}
		rows := m.questionsOf(nc.Name, k1, answerSplits...)
		var wrong, beyond []string
		for _, r := range rows {
			if r.Answerable && r.RankBefor > 0 && r.RankAfter == 0 {
				if b := findQ(base, r.ID); b != nil && b.RankAfter > 0 {
					if r.Cut {
						wrong = append(wrong, fmt.Sprintf("%s (ранг до %d)", r.ID, r.RankBefor))
					} else {
						beyond = append(beyond, fmt.Sprintf("%s (ранг %d)", r.ID, r.RankBefor))
					}
				}
			}
		}
		cut := 0.0
		cn := 0
		for _, sp := range all {
			if r, ok := m.row(nc.Name, sp, k1); ok {
				cut += r.CutShare
				cn++
			}
		}
		line := fmt.Sprintf("%s: фильтр отсёк в среднем %.0f %% кандидатов", nc.Name, 100*cut/math.Max(float64(cn), 1))
		if len(wrong) > 0 {
			line += "; фильтр отсёк доказательство, которое base показывал: " + strings.Join(wrong, ", ")
		} else {
			line += "; доказательств, которые base показывал, фильтр не отсёк"
		}
		if len(beyond) > 0 {
			line += "; ушло за K1 после реранкинга: " + strings.Join(beyond, ", ")
		}
		out = append(out, line+".")
	}
	if m.Fallback != "" {
		out = append(out, "Поиск шёл без векторов ("+m.Fallback+"): "+noteBM25+" — числа не о пороге по косинусу.")
	}
	if n > 0 {
		out = append(out, fmt.Sprintf("Выборка мала: %d отвечаемых и %d неотвечаемых вопросов — один вопрос это %.2f доли recall; выводы — по вопросам, а не по долям, и только как направление.",
			n, unN, 1/float64(n)))
	}
	return out
}

func findQ(rows []MatrixQ, id string) *MatrixQ {
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	return nil
}

// Markdown — для examples/rag/filter.md.
func (m Matrix) Markdown() string {
	var b strings.Builder
	b.WriteString("# Фильтр релевантности и переписывание запроса\n\n")
	fmt.Fprintf(&b, "Индекс `%s`, эмбеддер: %s, corpus_sha `%s`. Кандидатов K0 = %d; порог: абсолютный %.3f (косинус dense), относительный «не хуже лучшего на %.2f»; отсев повторов текста. Пол косинуса не применяется к запросу, где назван вид корпуса (якорь). %s.\n\n",
		m.Index, orText(m.Embedder, "—"), orText(m.CorpusSHA, "—"), m.K0, m.MinScore, m.Delta, m.Created.Format("2006-01-02 15:04"))
	if m.Fallback != "" {
		fmt.Fprintf(&b, "**Поиск шёл без векторов** (%s): %s.\n\n", m.Fallback, noteBM25)
	}
	b.WriteString("Метрики — поиск без модели: «доказательство» — фрагмент, покрывающий ≥ 80 % дословной цитаты-доказательства вопроса " +
		"(то же правило, что в `kb eval` и `kb qa`). «Recall до» — доказательство в dense-выдаче K0 (как у base), «dense∪BM25» — среди всех кандидатов " +
		"(dense и BM25 всех запросов), «после» — в итоговой выдаче K1, которую увидит модель. Precision — среднее по отвечаемым вопросам доли " +
		"итоговых фрагментов с доказательством (пустой итог — 0). «Релевантных среди отсечённых» — доля фрагментов с доказательством среди " +
		"отсечённых фильтром кандидатов отвечаемых вопросов; «доказательство снято» — доля вопросов, где фильтр снял доказательство, которое было " +
		"среди кандидатов. Неотвечаемые — `out` и `answerable=false` из других наборов: для них хорош пустой итог.\n\n")
	b.WriteString("## Вывод\n\n")
	for _, c := range m.Conclusion {
		b.WriteString("- " + c + "\n")
	}
	b.WriteString("\n## Конфигурации\n\n")
	for _, nc := range m.Configs {
		fmt.Fprintf(&b, "- `%s` — %s\n", nc.Name, nc.Config.Describe())
	}
	b.WriteString("\n## Матрица\n\n")
	b.WriteString("| конфигурация | набор | K1 | N | recall до (dense K0) | dense∪BM25 | recall после | MRR | precision | отсечено | релевантных среди отсечённых | доказательство снято | пусто на неотвечаемых | токенов | мс | цена |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range m.Rows {
		out := "—"
		if r.OutN > 0 {
			out = fmt.Sprintf("%d из %d", int(math.Round(r.OutEmpty*float64(r.OutN))), r.OutN)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %d | %d | %.2f | %.2f | %.2f | %.2f | %.2f | %.0f %% | %.2f | %.2f | %s | %.0f | %.0f | $%.4f |\n",
			r.Name, r.Split, r.K1, r.N, r.RecallBefore, r.RecallUnion, r.RecallAfter, r.MRR, r.Precision, 100*r.CutShare, r.WrongCut, r.LostQ, out,
			r.Tokens, r.Millis, r.CostUSD)
	}
	k1 := DefaultK1
	if len(m.Rows) > 0 {
		var ks []int
		for _, r := range m.Rows {
			ks = append(ks, r.K1)
		}
		k1 = headK1(ks)
	}
	for _, sp := range []string{kb.SplitTest, kb.SplitDev, kb.SplitOut} {
		if len(m.questionsOf(m.Configs[0].Name, k1, sp)) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## Вопросы %s (K1 = %d)\n\n", sp, k1)
		b.WriteString("Ранг доказательства: «до → после» (до — среди кандидатов после реранкинга, после — в итоге; «—» — нет; «отсечено» — релевантный кандидат снят фильтром, а не K1); " +
			"«пусто» — фильтр отсёк всё. Лучший косинус — у запроса первой конфигурации (реплика как есть); «якорь» — в реплике назван вид, пол не применяется. " +
			"Переписанный запрос — первой конфигурации с переписыванием.\n\n")
		b.WriteString("| id | тип | лучший косинус |")
		for _, nc := range m.Configs {
			fmt.Fprintf(&b, " %s |", nc.Name)
		}
		b.WriteString(" переписанный запрос |\n|---|---|---|")
		for range m.Configs {
			b.WriteString("---|")
		}
		b.WriteString("---|\n")
		for _, q := range m.questionsOf(m.Configs[0].Name, k1, sp) {
			top := fmt.Sprintf("%.3f", q.TopDense)
			if q.Anchored {
				top += ", якорь"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |", q.ID, q.Type, top)
			rewritten := ""
			for _, nc := range m.Configs {
				x := findQ(m.questionsOf(nc.Name, k1, sp), q.ID)
				if x == nil {
					b.WriteString(" — |")
					continue
				}
				if rewritten == "" && x.Rewritten != "" {
					rewritten = x.Rewritten
				}
				fmt.Fprintf(&b, " %s |", rankCell(*x))
			}
			fmt.Fprintf(&b, " %s |\n", cell(rewritten))
		}
	}
	return b.String()
}

func rankCell(q MatrixQ) string {
	r := func(n int) string {
		if n == 0 {
			return "—"
		}
		return fmt.Sprint(n)
	}
	s := ""
	if q.Answerable {
		s = r(q.RankBefor) + " → " + r(q.RankAfter)
	} else {
		s = fmt.Sprintf("оставлено %d", q.Kept)
	}
	if q.Cut {
		s += ", отсечено"
	}
	if q.Empty {
		s += ", пусто"
	}
	return s
}

func cell(s string) string {
	s = strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", "\\|")
	if s == "" {
		return "—"
	}
	return s
}
