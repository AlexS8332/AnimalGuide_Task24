package kb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
)

// Сравнение стратегий. Всё, что считается здесь, считается одинаково для
// обеих стратегий и по смещениям в Doc.Text: «разорванное доказательство»,
// «чанк на стыке разделов», «оборвано посреди предложения» — свойства
// отрезков, а не того, как стратегия себя называет.

// DefaultBudget — бюджет токенов контекста для recall при одинаковом объёме.
const DefaultBudget = 1500

// searchDepth — сколько чанков берётся из поиска для оценки: топ-5 для
// recall@k и запас, чтобы набрать бюджет из мелких чанков.
const searchDepth = 20

// evidence — фрагмент-доказательство в смещениях Doc.Text.
type evidence struct {
	doc  string
	s, e int
}

// agg — сводка по нескольким наборам (dev+test) для вывода.
type agg struct {
	n, ev, broken int
	hits          map[int]int
	rr            float64
	budget        int
	all5, allBudg int
	multi         int
	rows          []RetrievalRow
}

func (a *agg) recall(k int) float64 { return ratio(a.hits[k], a.n) }

// recallText — «, recall@1 0.50, recall@3 0.73» по тем k, что считались
// (кроме 5: он в выводе — числом вопросов).
func (a *agg) recallText() string {
	ks := make([]int, 0, len(a.hits))
	for k := range a.hits {
		if k != 5 {
			ks = append(ks, k)
		}
	}
	sort.Ints(ks)
	var b strings.Builder
	for _, k := range ks {
		fmt.Fprintf(&b, ", recall@%d %.2f", k, a.recall(k))
	}
	return b.String()
}

// Compare строит отчёт по индексам базы и сохраняет его в kb_reports.
// Режим по умолчанию — dense (с откатом, как в живом поиске); Mode == BM25
// добавляет к нему справочные строки BM25 по каждому индексу.
func Compare(ctx context.Context, s *Searcher, qs QuestionSet, o CompareOptions) (Report, error) {
	st := s.Store
	m, err := st.Manifest(ctx)
	if err != nil {
		return Report{}, err
	}
	docs, err := st.allDocs(ctx)
	if err != nil {
		return Report{}, err
	}
	if len(docs) == 0 {
		return Report{}, errors.New("в базе нет документов")
	}
	layouts := map[string]layout{}
	texts := map[string]string{}
	for _, d := range docs {
		layouts[d.ID] = newLayout(d)
		texts[d.ID] = d.Text()
	}
	ks := o.K
	if len(ks) == 0 {
		ks = []int{1, 3, 5}
	}
	splits := o.Splits
	if len(splits) == 0 {
		splits = []string{SplitDev, SplitTest}
	}
	budget := o.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	depth := searchDepth
	for _, k := range ks {
		depth = max(depth, k)
	}

	// Доказательства вопросов — в смещения один раз. inScope — без
	// повторов, только вопросы сравниваемых наборов (для WholeEvidence).
	evs := map[string][]evidence{}
	var inScope []evidence
	seenEv := map[evidence]bool{}
	for _, q := range qs.Questions {
		for _, e := range q.Evidence {
			if start, n := corpus.Find(texts[e.DocID], e.Quote); start >= 0 {
				x := evidence{doc: e.DocID, s: start, e: start + n}
				evs[q.ID] = append(evs[q.ID], x)
				if q.Answerable && contains(splits, q.Split) && !seenEv[x] {
					seenEv[x] = true
					inScope = append(inScope, x)
				}
			}
		}
	}

	all, err := st.Indexes(ctx)
	if err != nil {
		return Report{}, err
	}
	var indexes []IndexInfo
	for _, ix := range all {
		if len(o.Indexes) == 0 || contains(o.Indexes, ix.ID) {
			indexes = append(indexes, ix)
		}
	}
	if len(indexes) == 0 {
		return Report{}, fmt.Errorf("%w: в базе нет индексов для сравнения", ErrNoIndex)
	}

	r := Report{Created: time.Now().UTC(), CorpusSHA: m.CorpusSHA, Docs: len(docs), Pages: m.Pages, Budget: budget}
	r.Embedder = "нет (только BM25)"
	if s.Embedder != nil {
		r.Embedder = s.Embedder.Model()
	}
	aggs := map[string]*agg{} // index + "/" + mode
	var keys []string
	for _, ix := range indexes {
		chunks, err := st.Chunks(ctx, ix.ID, "")
		if err != nil {
			return Report{}, err
		}
		stats := indexStats(ix, chunks, layouts)
		stats.Evidence = len(inScope)
		whole := 0
		for _, e := range inScope {
			for _, c := range chunks {
				if c.DocID == e.doc && c.Start <= e.s && c.End >= e.e {
					whole++
					break
				}
			}
		}
		stats.WholeEvidence = ratio(whole, len(inScope))
		if stats.Bytes, err = st.indexBytes(ctx, ix.ID); err != nil {
			return Report{}, err
		}
		r.Stats = append(r.Stats, stats)

		byDoc := map[string][]Chunk{}
		for _, c := range chunks {
			byDoc[c.DocID] = append(byDoc[c.DocID], c)
		}
		modes := []Mode{Dense}
		if o.Mode == BM25 {
			modes = append(modes, BM25)
		}
		var got Mode // чем на деле искал dense (после отката — BM25)
		for _, mode := range modes {
			if mode == BM25 && got == BM25 {
				// dense уже откатился на BM25 — справочная строка
				// повторила бы те же числа.
				continue
			}
			for _, sp := range splits {
				ret, err := retrieval(ctx, s, ix, mode, sp, qs, evs, byDoc, ks, depth, budget)
				if err != nil {
					return Report{}, err
				}
				r.Retrieval = append(r.Retrieval, ret.Retrieval)
				key := ix.ID + "/" + string(ret.Mode)
				a := aggs[key]
				if a == nil {
					a = &agg{hits: map[int]int{}}
					aggs[key] = a
					keys = append(keys, key)
				}
				a.n += ret.N
				a.ev += ret.ev
				a.broken += ret.broken
				a.rr += ret.MRR * float64(ret.N)
				a.budget += ret.budgetHits
				a.all5 += ret.all5
				a.allBudg += ret.allBudget
				a.multi += ret.Multi
				a.rows = append(a.rows, ret.Rows...)
				for _, k := range ks {
					a.hits[k] += ret.hits[k]
				}
				if mode == Dense {
					got = ret.Mode
				}
			}
		}
	}
	r.Conclusion = conclude(r, aggs, splits)

	raw, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO kb_reports (created, report) VALUES (?, ?)`,
		r.Created.Format(time.RFC3339Nano), string(raw)); err != nil {
		return r, fmt.Errorf("отчёт не сохранён: %w", err)
	}
	return r, nil
}

// retrievalRun — строка отчёта и счётчики для сводки.
type retrievalRun struct {
	Retrieval
	ev, broken      int
	hits            map[int]int
	budgetHits      int
	all5, allBudget int
}

// retrieval — один индекс, один режим, один набор. В счёт идут только
// вопросы с доказательствами (out и «аспекта нет» — без них).
func retrieval(ctx context.Context, s *Searcher, ix IndexInfo, mode Mode, split string, qs QuestionSet,
	evs map[string][]evidence, byDoc map[string][]Chunk, ks []int, depth, budget int) (retrievalRun, error) {
	run := retrievalRun{Retrieval: Retrieval{Index: ix.ID, Mode: mode, Split: split, Recall: map[int]float64{}},
		hits: map[int]int{}}
	var rrSum float64
	for _, q := range qs.Split(split) {
		ev := evs[q.ID]
		if !q.Answerable || len(ev) == 0 {
			continue
		}
		run.N++
		for _, e := range ev {
			run.ev++
			if !coveredBy(byDoc[e.doc], e) {
				run.broken++
			}
		}
		query := strings.Join(append(append([]string(nil), q.Context...), q.Q), " ")
		hits, info, err := s.Search(ctx, query, SearchOptions{Index: ix.ID, K: depth, Mode: mode})
		if err != nil {
			return run, fmt.Errorf("%s, %s: %w", q.ID, ix.ID, err)
		}
		run.Mode = info.Mode
		if info.Fallback != "" && run.Fallback == "" {
			run.Fallback = info.Fallback
		}
		if len(ev) > 1 {
			run.Multi++
		}
		row := RetrievalRow{ID: q.ID, Q: q.Q}
		for i, h := range hits {
			if i < 5 {
				row.Top = append(row.Top, h.ID)
			}
			if row.Rank == 0 && relevant(h.Chunk, ev) {
				row.Rank = i + 1
				row.Score = h.Score
			}
		}
		if row.Rank == 0 && len(hits) > 0 {
			row.Score = hits[0].Score
		}
		// Одинаковый бюджет: чанки топа по порядку, пока влезают.
		fit, used := 0, 0
		for _, h := range hits {
			if used+h.Tokens > budget {
				break
			}
			used += h.Tokens
			fit++
		}
		// Ранг каждого доказательства: самое дальнее решает, нашлись ли все.
		last := 0
		for _, e := range ev {
			r := 0
			for i, h := range hits {
				if relevant(h.Chunk, []evidence{e}) {
					r = i + 1
					break
				}
			}
			if r == 0 {
				last = 0
				break
			}
			last = max(last, r)
		}
		row.All5 = last > 0 && last <= 5
		if row.All5 {
			run.all5++
		}
		if last > 0 && last <= fit {
			run.allBudget++
		}
		if row.Rank > 0 && row.Rank <= fit {
			run.budgetHits++
		}
		run.Rows = append(run.Rows, row)
		if row.Rank > 0 {
			rrSum += 1 / float64(row.Rank)
			for _, k := range ks {
				if row.Rank <= k {
					run.hits[k]++
				}
			}
		}
	}
	if run.Mode == "" {
		run.Mode = mode
	}
	for _, k := range ks {
		run.Recall[k] = ratio(run.hits[k], run.N)
	}
	if run.N > 0 {
		run.MRR = rrSum / float64(run.N)
		run.RecallBudget = ratio(run.budgetHits, run.N)
		run.RecallAll5 = ratio(run.all5, run.N)
		run.RecallAllBudget = ratio(run.allBudget, run.N)
	}
	run.BrokenEvidence = ratio(run.broken, run.ev)
	return run, nil
}

// relevant — чанк покрывает ≥ EvidenceCover хотя бы одного доказательства
// своего документа.
func relevant(c Chunk, ev []evidence) bool {
	for _, e := range ev {
		if e.doc == c.DocID && Covers(c.Start, c.End, e.s, e.e) >= EvidenceCover {
			return true
		}
	}
	return false
}

func coveredBy(chunks []Chunk, e evidence) bool {
	for _, c := range chunks {
		if Covers(c.Start, c.End, e.s, e.e) >= EvidenceCover {
			return true
		}
	}
	return false
}

// indexStats — структурные метрики индекса.
func indexStats(ix IndexInfo, chunks []Chunk, layouts map[string]layout) IndexStats {
	st := IndexStats{Index: ix.ID, Strategy: ix.Strategy, Params: ix.Params, Embedder: ix.Embedder,
		Chunks: len(chunks), BuildSeconds: ix.Seconds}
	if len(chunks) == 0 {
		return st
	}
	toks := make([]int, 0, len(chunks))
	byDoc := map[string][]Chunk{}
	mixed, mid := 0, 0
	for _, c := range chunks {
		toks = append(toks, c.Tokens)
		st.Tokens += c.Tokens
		byDoc[c.DocID] = append(byDoc[c.DocID], c)
		if c.Mixed {
			mixed++
		}
		if l, ok := layouts[c.DocID]; ok && midSentence(l, c.Start, c.End) {
			mid++
		}
	}
	sort.Ints(toks)
	st.P50 = percentile(toks, 50)
	st.P95 = percentile(toks, 95)
	st.MixedShare = ratio(mixed, len(chunks))
	st.MidSentence = ratio(mid, len(chunks))

	sections, split := 0, 0
	sum, union := 0, 0
	for id, cs := range byDoc {
		l, ok := layouts[id]
		if !ok {
			continue
		}
		for _, b := range l.blocks {
			bs, be := l.trim(b.start, b.end)
			if bs >= be {
				continue
			}
			sections++
			whole := false
			for _, c := range cs {
				if c.Start <= bs && c.End >= be {
					whole = true
					break
				}
			}
			if !whole {
				split++
			}
		}
		spans := make([][2]int, 0, len(cs))
		for _, c := range cs {
			sum += c.End - c.Start
			spans = append(spans, [2]int{c.Start, c.End})
		}
		union += unionLen(spans)
	}
	st.SplitSections = ratio(split, sections)
	if sum > 0 {
		st.OverlapShare = float64(sum-union) / float64(sum)
	}
	return st
}

// midSentence — отрезок оборван посреди предложения: на его конце (или
// начале) нет ни границы абзаца, ни конца предложения.
func midSentence(l layout, s, e int) bool {
	t := l.text
	// Конец: дальше абзац или конец текста — граница; иначе последний знак
	// (за закрывающими кавычками и скобками) должен заканчивать
	// предложение.
	if e < len(t) && !newlineAhead(t, e) {
		i := e - 1
		for i > s && isCloser(t[i]) {
			i--
		}
		if !isTerminal(t[i]) {
			return true
		}
	}
	// Начало: то же в обратную сторону.
	if s > 0 && !newlineBehind(t, s) {
		i := s - 1
		for i > 0 && unicode.IsSpace(t[i]) {
			i--
		}
		for i > 0 && isCloser(t[i]) {
			i--
		}
		if !isTerminal(t[i]) {
			return true
		}
	}
	return false
}

func newlineAhead(t []rune, p int) bool {
	for ; p < len(t) && unicode.IsSpace(t[p]); p++ {
		if t[p] == '\n' {
			return true
		}
	}
	return p >= len(t)
}

func newlineBehind(t []rune, p int) bool {
	for p--; p >= 0 && unicode.IsSpace(t[p]); p-- {
		if t[p] == '\n' {
			return true
		}
	}
	return p < 0
}

func unionLen(spans [][2]int) int {
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	total, curS, curE := 0, -1, -1
	for _, sp := range spans {
		if sp[0] > curE {
			total += curE - curS
			curS, curE = sp[0], sp[1]
		} else if sp[1] > curE {
			curE = sp[1]
		}
	}
	return total + curE - curS
}

// percentile — ближайший ранг по отсортированному списку.
func percentile(sorted []int, p int) int {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(float64(p)/100*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (r Report) stats(id string) (IndexStats, bool) {
	for _, s := range r.Stats {
		if s.Index == id {
			return s, true
		}
	}
	return IndexStats{}, false
}

// NoiseQuestions — с какого перевеса в вопросах парное сравнение по
// recall@5 называет одну сторону лучшей. Вопросов в сравнении — два-три
// десятка, и разница в один-два вопроса — это одна неудачная формулировка
// или одно доказательство на стыке, а не свойство стратегии.
const NoiseQuestions = 3

// pairing — парное сравнение двух прогонов на одних и тех же вопросах.
type pairing struct {
	n                    int // общих вопросов
	hitA, hitB           int // нашлось в топ-5
	onlyA, onlyB         int // в топ-5 только у A, только у B
	better, worse, equal int // ранг первого релевантного у A лучше, хуже, равен
}

// pair сопоставляет строки по id вопроса. Не найденное в топе считается
// хуже любого ранга; оба не нашли — «равен».
func pair(a, b []RetrievalRow) pairing {
	byID := map[string]RetrievalRow{}
	for _, r := range b {
		byID[r.ID] = r
	}
	key := func(rank int) int {
		if rank <= 0 {
			return math.MaxInt
		}
		return rank
	}
	var p pairing
	for _, ra := range a {
		rb, ok := byID[ra.ID]
		if !ok {
			continue
		}
		p.n++
		ha, hb := ra.Rank > 0 && ra.Rank <= 5, rb.Rank > 0 && rb.Rank <= 5
		if ha {
			p.hitA++
		}
		if hb {
			p.hitB++
		}
		switch {
		case ha && !hb:
			p.onlyA++
		case hb && !ha:
			p.onlyB++
		}
		switch ka, kb := key(ra.Rank), key(rb.Rank); {
		case ka < kb:
			p.better++
		case ka > kb:
			p.worse++
		default:
			p.equal++
		}
	}
	return p
}

// verdict — вывод парного сравнения: «лучше» только при перевесе не меньше
// NoiseQuestions вопросов по recall@5, иначе — «в пределах шума».
func (p pairing) verdict(a, b string) string {
	d := p.onlyA - p.onlyB
	switch {
	case d >= NoiseQuestions:
		return fmt.Sprintf("%s лучше (перевес по recall@5 — %d, порог — %d вопроса)", a, d, NoiseQuestions)
	case -d >= NoiseQuestions:
		return fmt.Sprintf("%s лучше (перевес по recall@5 — %d, порог — %d вопроса)", b, -d, NoiseQuestions)
	}
	return fmt.Sprintf("разница в пределах шума (перевес по recall@5 — %d, порог — %d вопроса)", abs(d), NoiseQuestions)
}

// text — парное сравнение словами: ранги и recall@5 по вопросам.
func (p pairing) text(a, b string) string {
	return fmt.Sprintf("ранг первого релевантного лучше у %s в %s, у %s — в %d, равен в %d; в топ-5 только у %s — %d, только у %s — %d",
		a, inQuestions(p.better), b, p.worse, p.equal, a, p.onlyA, b, p.onlyB)
}

// inQuestions — «в 1 вопросе», «в 7 вопросах».
func inQuestions(n int) string {
	if n%10 == 1 && n%100 != 11 {
		return fmt.Sprintf("%d вопросе", n)
	}
	return fmt.Sprintf("%d вопросах", n)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// conclude — вывод числами. Только сравнения, которые следуют из таблиц;
// разница — в вопросах, а не в долях, и «лучше» — только при перевесе
// парного сравнения по recall@5 не меньше NoiseQuestions вопросов.
func conclude(r Report, aggs map[string]*agg, splits []string) []string {
	scope := strings.Join(splits, "+")
	primary := func(id string) (*agg, Mode) {
		if a := aggs[id+"/"+string(Dense)]; a != nil {
			return a, Dense
		}
		return aggs[id+"/"+string(BM25)], BM25
	}
	var out []string
	sA, sMode := primary(string(Structure))
	fA, fMode := primary(string(Fixed))
	sS, okS := r.stats(string(Structure))
	fS, okF := r.stats(string(Fixed))
	if sA != nil && fA != nil && okS && okF {
		mode := string(sMode)
		if sMode != fMode {
			mode = fmt.Sprintf("%s/%s", sMode, fMode)
		}
		p := pair(sA.rows, fA.rows)
		out = append(out, fmt.Sprintf("structure: recall@5 %d из %d против %d из %d у fixed (%+d), recall@1 %.2f против %.2f, recall@3 %.2f против %.2f, MRR %.2f против %.2f (%s, поиск %s).",
			p.hitA, p.n, p.hitB, p.n, p.hitA-p.hitB, sA.recall(1), fA.recall(1), sA.recall(3), fA.recall(3),
			sA.rr/float64(max(sA.n, 1)), fA.rr/float64(max(fA.n, 1)), scope, mode))
		out = append(out, fmt.Sprintf("Парно по %d вопросам: %s — %s.", p.n, p.text("structure", "fixed"), p.verdict("structure", "fixed")))
		out = append(out, fmt.Sprintf("Все доказательства вопроса в топ-5 (recall_all@5): %d из %d у structure против %d из %d у fixed; в бюджете %d токенов — %d против %d (вопросов с несколькими доказательствами %d).",
			sA.all5, sA.n, fA.all5, fA.n, r.Budget, sA.allBudg, fA.allBudg, sA.multi))
		out = append(out, fmt.Sprintf("Доказательство целиком в одном чанке: %.0f %% у structure против %.0f %% у fixed (из %d); разорвано (покрыто < %.0f %%) %.0f %% против %.0f %%; чанков на стыке разделов %.0f %% против %.0f %%; разрезанных разделов %.0f %% против %.0f %%; оборванных посреди предложения %.0f %% против %.0f %%.",
			100*sS.WholeEvidence, 100*fS.WholeEvidence, sS.Evidence, 100*EvidenceCover,
			100*ratio(sA.broken, sA.ev), 100*ratio(fA.broken, fA.ev), 100*sS.MixedShare, 100*fS.MixedShare,
			100*sS.SplitSections, 100*fS.SplitSections, 100*sS.MidSentence, 100*fS.MidSentence))
		out = append(out, fmt.Sprintf("При одинаковом бюджете %d токенов первый релевантный нашёлся в %d из %d вопросов у structure против %d из %d у fixed; чанков %d против %d, p50 %d против %d токенов, перекрытие %.0f %% против %.0f %%.",
			r.Budget, sA.budget, sA.n, fA.budget, fA.n, sS.Chunks, fS.Chunks, sS.P50, fS.P50,
			100*sS.OverlapShare, 100*fS.OverlapShare))
		if contains(splits, SplitTest) {
			st, ft := splitRow(r, string(Structure), sMode, SplitTest), splitRow(r, string(Fixed), fMode, SplitTest)
			if st != nil && ft != nil {
				pt := pair(st.Rows, ft.Rows)
				out = append(out, fmt.Sprintf("На test (вопросов с доказательствами: %d): recall@5 %d против %d, MRR %.2f против %.2f — на такой выборке это иллюстрация, не вывод.",
					st.N, pt.hitA, pt.hitB, st.MRR, ft.MRR))
			}
		}
	} else {
		for _, s := range r.Stats {
			a, mode := primary(s.Index)
			if a == nil {
				continue
			}
			p := pair(a.rows, a.rows)
			out = append(out, fmt.Sprintf("%s: recall@5 %d из %d%s, MRR %.2f, все доказательства в топ-5 — %d из %d (%s, поиск %s); доказательство целиком в одном чанке %.0f %%, чанков на стыке разделов %.0f %%.",
				s.Index, p.hitA, p.n, a.recallText(), a.rr/float64(max(a.n, 1)), a.all5, a.n, scope, mode,
				100*s.WholeEvidence, 100*s.MixedShare))
		}
	}
	// Справочно: что дают эмбеддинги поверх BM25 — тем же парным
	// сравнением и тем же порогом шума.
	var parts []string
	for _, s := range r.Stats {
		d, b := aggs[s.Index+"/"+string(Dense)], aggs[s.Index+"/"+string(BM25)]
		if d != nil && b != nil {
			p := pair(d.rows, b.rows)
			parts = append(parts, fmt.Sprintf("%s — recall@5 %d из %d у dense против %d из %d у BM25 (%+d); %s — %s",
				s.Index, p.hitA, p.n, p.hitB, p.n, p.hitA-p.hitB, p.text("dense", "BM25"), p.verdict("dense", "BM25")))
		}
	}
	if len(parts) > 0 {
		out = append(out, "BM25 (справочно): "+strings.Join(parts, "; ")+".")
	}
	for _, rr := range r.Retrieval {
		if rr.Fallback != "" {
			out = append(out, "Векторный поиск не состоялся ("+rr.Fallback+"): числа поиска — BM25.")
			break
		}
	}
	return out
}

func splitRow(r Report, index string, mode Mode, split string) *Retrieval {
	for i := range r.Retrieval {
		x := &r.Retrieval[i]
		if x.Index == index && x.Mode == mode && x.Split == split {
			return x
		}
	}
	return nil
}

// LastReport — последний сохранённый отчёт (для окна); ok=false — нет.
func (s *Store) LastReport(ctx context.Context) (Report, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT report FROM kb_reports ORDER BY id DESC LIMIT 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Report{}, false, nil
	}
	if err != nil {
		return Report{}, false, err
	}
	var r Report
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Report{}, false, err
	}
	return r, true, nil
}

// Markdown — отчёт для examples/kb/chunking.md.
func (r Report) Markdown() string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	p("# Сравнение стратегий чанкинга\n\n")
	p("- Корпус: `corpus_sha` `%s`, документов %d, страниц %.1f\n", r.CorpusSHA, r.Docs, r.Pages)
	p("- Эмбеддер: %s\n", r.Embedder)
	p("- Дата: %s\n", r.Created.Format("2006-01-02 15:04 MST"))
	p("- Релевантность: чанк того же документа покрывает ≥ %.0f %% фрагмента-доказательства; бюджет топа — %d токенов\n", 100*EvidenceCover, r.Budget)
	p("- «Лучше» в выводе — только при перевесе ≥ %d вопросов в парном сравнении по recall@5; меньше — «в пределах шума»\n\n", NoiseQuestions)

	p("## Структура индексов\n\n")
	p("| Индекс | Параметры | Эмбеддер | Чанков | Токенов | p50 | p95 | На стыке разделов | Разрезано разделов | Перекрытие | Оборвано посреди предложения | Доказательство целиком в одном чанке | Сборка, с | Размер, КБ |\n")
	p("|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, s := range r.Stats {
		p("| %s | %s | %s | %d | %d | %d | %d | %s | %s | %s | %s | %s из %d | %.1f | %d |\n", s.Index, paramsText(s.Params),
			orDash(s.Embedder), s.Chunks, s.Tokens, s.P50, s.P95, pct(s.MixedShare), pct(s.SplitSections),
			pct(s.OverlapShare), pct(s.MidSentence), pct(s.WholeEvidence), s.Evidence, s.BuildSeconds, s.Bytes/1024)
	}

	p("\n## Поиск\n\n")
	p("recall_all@5 — все доказательства вопроса в топ-5; «все в бюджете» — все в чанках топа, влезающих в %d токенов.\n\n", r.Budget)
	p("| Индекс | Режим | Набор | Вопросов (многофактных) | Разорвано доказательств | recall@1 | recall@3 | recall@5 | MRR | recall при %d ток. | recall_all@5 | все в бюджете |\n", r.Budget)
	p("|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, x := range r.Retrieval {
		mode := string(x.Mode)
		if x.Fallback != "" {
			mode += " (откат)"
		}
		p("| %s | %s | %s | %d (%d) | %s | %.2f | %.2f | %.2f | %.2f | %.2f | %.2f | %.2f |\n", x.Index, mode, x.Split, x.N, x.Multi,
			pct(x.BrokenEvidence), x.Recall[1], x.Recall[3], x.Recall[5], x.MRR, x.RecallBudget, x.RecallAll5, x.RecallAllBudget)
	}

	// По вопросам test: ранг первого релевантного в каждом индексе и режиме.
	var cols []*Retrieval
	for i := range r.Retrieval {
		if r.Retrieval[i].Split == SplitTest {
			cols = append(cols, &r.Retrieval[i])
		}
	}
	if len(cols) > 0 {
		p("\n## Вопросы test\n\n")
		p("Ранг первого релевантного чанка (— — нет в топ-%d).\n\n", searchDepth)
		p("| id | Вопрос |")
		for _, c := range cols {
			p(" %s %s |", c.Index, c.Mode)
		}
		p("\n|---|---|")
		for range cols {
			p("---:|")
		}
		p("\n")
		for i, row := range cols[0].Rows {
			p("| %s | %s |", row.ID, strings.ReplaceAll(clip(row.Q, 90), "|", "/"))
			for _, c := range cols {
				rank := "—"
				if i < len(c.Rows) && c.Rows[i].Rank > 0 {
					rank = fmt.Sprint(c.Rows[i].Rank)
				}
				p(" %s |", rank)
			}
			p("\n")
		}
	}

	if len(r.Conclusion) > 0 {
		p("\n## Вывод\n\n")
		for _, c := range r.Conclusion {
			p("- %s\n", c)
		}
	}
	return b.String()
}

func paramsText(p Params) string {
	var parts []string
	if p.Size > 0 {
		parts = append(parts, fmt.Sprintf("size %d", p.Size))
	}
	if p.Overlap > 0 {
		parts = append(parts, fmt.Sprintf("overlap %d", p.Overlap))
	}
	if p.Max > 0 {
		parts = append(parts, fmt.Sprintf("max %d", p.Max))
	}
	if p.Min > 0 {
		parts = append(parts, fmt.Sprintf("min %d", p.Min))
	}
	return orDash(strings.Join(parts, ", "))
}

func pct(f float64) string { return fmt.Sprintf("%.1f %%", 100*f) }

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func clip(s string, n int) string {
	r := []rune(clean(s))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}
