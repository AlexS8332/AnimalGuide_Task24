package rag

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
)

// Eval прогоняет вопросы набора в режимах.
//
// Порядок: вопрос за вопросом; внутри вопроса — повтор за повтором, и в
// каждом повторе все режимы подряд (дрейф провайдера во времени ложится на
// режимы поровну). Когда ответы вопроса готовы, судья оценивает их в
// перемешанном порядке (Seed) и по одному, без пометки режима; затем строка
// отдаётся в Progress. Ошибка ответа не обрывает прогон: прогон с Error не
// входит в большинство и сводку. Обрывает только отмена контекста и
// неверные параметры.
func Eval(ctx context.Context, a *Answerer, qs kb.QuestionSet, o EvalOptions) (Report, error) {
	if a == nil || a.LLM == nil {
		return Report{}, errors.New("отвечающему агенту не передана модель")
	}
	modes := o.Modes
	if len(modes) == 0 {
		modes = []Mode{NoRAG, RAG}
	}
	for _, m := range modes {
		switch {
		case !m.Known():
			return Report{}, fmt.Errorf("неизвестный режим %q (%s)", m, modeList())
		case m == RAG && a.Searcher == nil:
			return Report{}, errors.New("режим rag без базы знаний: соберите её командой kb index")
		case m.Pipelined() && (a.Pipeline == nil || a.Pipeline.Searcher == nil):
			return Report{}, fmt.Errorf("режим %s без конвейера поиска (Answerer.Pipeline)", m)
		}
	}
	repeats := max(o.Repeats, 1)
	splits := o.Splits
	if len(splits) == 0 {
		splits = []string{kb.SplitTest}
	}
	var questions []kb.Question
	for _, sp := range splits {
		questions = append(questions, qs.Split(sp)...)
	}
	if len(questions) == 0 {
		return Report{}, fmt.Errorf("в наборах %s нет вопросов", strings.Join(splits, ", "))
	}

	rep := Report{Created: time.Now(), Model: a.model(), Index: a.index(), K: a.k(), Repeats: repeats}
	searcher := a.Searcher
	if searcher == nil && a.Pipeline != nil {
		searcher = a.Pipeline.Searcher
	}
	ev := &evidenceIndex{s: searcher, texts: map[string]string{}}
	if searcher != nil {
		if m, err := searcher.Store.Manifest(ctx); err == nil {
			rep.CorpusSHA = m.CorpusSHA
		}
		rep.Embedder = "нет — поиск BM25"
		if searcher.Embedder != nil {
			rep.Embedder = searcher.Embedder.Model()
		}
	}
	rng := rand.New(rand.NewSource(o.Seed))
	fallback := ""

	for _, q := range questions {
		row := Row{Question: q, Runs: map[Mode][]Run{}, Majority: map[Mode]Verdict{}, Flips: map[Mode]int{}}
		for i := 1; i <= repeats; i++ {
			for _, m := range modes {
				ans, err := a.Answer(ctx, QuestionOf(q), m)
				if ctx.Err() != nil {
					return rep, ctx.Err()
				}
				run := Run{Repeat: i, Answer: ans}
				if err != nil {
					run.Answer.Mode, run.Error = m, err.Error()
				} else {
					run.Rule = Rule(q, GradedText(ans))
					run.Final = run.Rule.Verdict
					if m.UsesBase() {
						// Recall — по выдаче, которую увидела модель: у режимов
						// v23 это итог конвейера после фильтра.
						run.Recall = ev.covered(ctx, q, ans.Hits)
						if ans.Search.Fallback != "" && fallback == "" {
							fallback = ans.Search.Fallback
						}
					}
				}
				row.Runs[m] = append(row.Runs[m], run)
			}
		}
		if o.Judge != nil {
			// Судья — в перемешанном порядке: по нему нельзя угадать режим
			// (сначала norag, потом rag), и дрейф судьи во времени не
			// ложится на один режим.
			type ref struct {
				m Mode
				i int
			}
			var refs []ref
			for _, m := range modes {
				for i, r := range row.Runs[m] {
					if r.Error == "" {
						refs = append(refs, ref{m, i})
					}
				}
			}
			rng.Shuffle(len(refs), func(i, j int) { refs[i], refs[j] = refs[j], refs[i] })
			for _, x := range refs {
				run := &row.Runs[x.m][x.i]
				jr, err := o.Judge.Grade(ctx, q, GradedText(run.Answer))
				rep.JudgeCost = rep.JudgeCost.Add(jr.Cost)
				if ctx.Err() != nil {
					return rep, ctx.Err()
				}
				if err != nil {
					run.JudgeError = err.Error()
				} else {
					run.Judge = &jr
					run.Final = jr.Verdict
				}
				if c := run.Answer.Cited; c != nil {
					// Смысл ответа против его цитат — отдельный запрос судьи
					// смысла (v24): вердикт «верно по эталону» и «сказано в
					// цитатах» — разные вопросы.
					sp, err := o.Judge.CheckSupport(ctx, c.Cited)
					rep.JudgeCost = rep.JudgeCost.Add(sp.Cost)
					if ctx.Err() != nil {
						return rep, ctx.Err()
					}
					if err != nil {
						run.SupportError = err.Error()
						continue
					}
					run.Answer.Support = &sp
				}
			}
		}
		for _, m := range modes {
			row.Majority[m], row.Flips[m] = majority(q, row.Runs[m])
		}
		rep.Rows = append(rep.Rows, row)
		if o.Progress != nil {
			o.Progress(row)
		}
	}
	if fallback != "" {
		rep.Embedder += " — откат на BM25: " + fallback
	}
	for _, m := range modes {
		rep.Stats = append(rep.Stats, modeStats(m, rep.Rows))
	}
	rep.Disagreements = disagreements(rep.Rows, modes)
	rep.Conclusion = conclude(rep)
	return rep, nil
}

// GradedText — что оценивают правило и судья: у rag+cite — сам ответ
// (Cited.EvalText) без обвязки «Источники/Цитаты», у остальных — текст.
func GradedText(a Answer) string {
	if a.Cited != nil {
		return a.Cited.Cited.EvalText()
	}
	return a.Text
}

// goodness — насколько вердикт хорош для этого вопроса: у неотвечаемого
// хорош только отказ.
func goodness(q kb.Question, v Verdict) int {
	if !q.Answerable {
		if v == Abstain {
			return 3
		}
		return 0
	}
	switch v {
	case Correct:
		return 3
	case Partial:
		return 2
	case Abstain:
		return 1
	}
	return 0
}

// majority — вердикт большинства повторов и число смен вердикта между
// соседними повторами. Ничья (при двух повторах — любое расхождение)
// решается в худшую для вопроса сторону: одинаково строго к обоим режимам,
// и случайный удачный повтор не засчитывается за верный ответ.
func majority(q kb.Question, runs []Run) (Verdict, int) {
	count := map[Verdict]int{}
	var order []Verdict
	var prev Verdict
	flips := 0
	for _, r := range runs {
		if r.Error != "" || r.Final == "" {
			continue
		}
		if prev != "" && r.Final != prev {
			flips++
		}
		prev = r.Final
		if count[r.Final] == 0 {
			order = append(order, r.Final)
		}
		count[r.Final]++
	}
	var best Verdict
	for _, v := range order {
		switch {
		case best == "":
			best = v
		case count[v] > count[best]:
			best = v
		case count[v] == count[best] && goodness(q, v) < goodness(q, best):
			best = v
		}
	}
	return best, flips
}

// confidentWrong — уверенная ошибка на отвечаемом: wrong.
func confidentWrong(q kb.Question, v Verdict) bool {
	return q.Answerable && v == Wrong
}

// answeredUnanswerable — ответ по существу на неотвечаемом: всё, кроме
// «не знаю».
func answeredUnanswerable(q kb.Question, v Verdict) bool {
	return !q.Answerable && v != "" && v != Abstain
}

// modeStats — сводка режима по строкам.
func modeStats(m Mode, rows []Row) ModeStats {
	st := ModeStats{Mode: m}
	var ms int64
	answered, transitions, recallN, recallHit, agree := 0, 0, 0, 0, 0
	quotes, verbatim := 0, 0
	for _, row := range rows {
		runs := row.Runs[m]
		if len(runs) == 0 {
			continue
		}
		st.Questions++
		q := row.Question
		v := row.Majority[m]
		switch v {
		case Correct:
			st.Correct++
		case Partial:
			st.Partial++
		case Wrong:
			st.Wrong++
		case Abstain:
			st.Abstain++
		}
		if confidentWrong(q, v) {
			st.ConfidentWrong++
		}
		if !q.Answerable && v == Abstain {
			st.RightAbstain++
		}
		if answeredUnanswerable(q, v) {
			st.AnsweredUnanswerable++
		}
		if q.Discriminative != nil && *q.Discriminative {
			st.Discriminative++
			if v == Correct {
				st.DiscriminativeCorrect++
			}
		}
		st.Flips += row.Flips[m]
		valid := 0
		for _, r := range runs {
			if r.Error != "" {
				continue
			}
			valid++
			answered++
			ms += r.Answer.Millis
			st.Usage = st.Usage.Add(r.Answer.Usage)
			st.Cost = st.Cost.Add(r.Answer.Cost)
			if m.UsesBase() && q.Answerable && len(q.Evidence) > 0 {
				recallN++
				if r.Recall {
					recallHit++
				}
			}
			if r.Judge != nil {
				st.Judged++
				if r.Judge.Verdict == r.Rule.Verdict {
					agree++
				}
			}
			if c := r.Answer.Cited; c != nil {
				citeStats(&st, q, *c, r.Answer.Support, &quotes, &verbatim)
			}
		}
		transitions += max(valid-1, 0)
	}
	if answered > 0 {
		st.AvgMillis = ms / int64(answered)
	}
	if recallN > 0 {
		st.Recall = float64(recallHit) / float64(recallN)
	}
	if st.Judged > 0 {
		st.Agreement = float64(agree) / float64(st.Judged)
	}
	if transitions > 0 {
		st.FlipRate = float64(st.Flips) / float64(transitions)
	}
	if quotes > 0 {
		st.Verbatim = float64(verbatim) / float64(quotes)
	}
	return st
}

// citeStats — числа rag+cite одного прогона: источники и цитаты у
// answered, дословность цитат, смысл (судья), «не проверено», «не знаю»
// кодом и ложные «не знаю» на отвечаемых.
func citeStats(st *ModeStats, q kb.Question, r CitedResult, sp *Support, quotes, verbatim *int) {
	st.Cited++
	st.Rejects += r.Check.Rejects
	if r.Check.Unverified {
		st.Unverified++
	}
	if r.Cited.Unknown() {
		st.Unknown++
		if r.Check.Forced {
			st.ForcedUnknown++
		}
		if q.Answerable {
			st.FalseUnknown++
		}
		return
	}
	st.Answered++
	if r.Check.HasSources {
		st.WithSources++
	}
	if r.Check.HasQuotes {
		st.WithQuotes++
	}
	n := len(r.Cited.Quotes)
	*quotes += n
	*verbatim += n - len(r.Check.NotVerbatim)
	if sp != nil {
		st.SupportChecked++
		if sp.OK {
			st.Supported++
		}
	}
}

// disagreements — где правило и судья разошлись: для ручного просмотра.
func disagreements(rows []Row, modes []Mode) []string {
	out := []string{}
	for _, row := range rows {
		for _, m := range modes {
			for _, r := range row.Runs[m] {
				if r.Judge == nil || r.Judge.Verdict == r.Rule.Verdict {
					continue
				}
				out = append(out, fmt.Sprintf("%s %s #%d: правило %s, судья %s — %s",
					row.Question.ID, m, r.Repeat, r.Rule.Verdict, r.Judge.Verdict, clip(r.Judge.Reason, 200)))
			}
		}
	}
	return out
}

// statOf — сводка режима из отчёта.
func (r Report) statOf(m Mode) (ModeStats, bool) {
	for _, s := range r.Stats {
		if s.Mode == m {
			return s, true
		}
	}
	return ModeStats{}, false
}

// unanswerable — сколько в отчёте вопросов без ответа в базе.
func (r Report) unanswerable() int {
	n := 0
	for _, row := range r.Rows {
		if !row.Question.Answerable {
			n++
		}
	}
	return n
}

// conclude — вывод числами, собранный кодом. Разница режимов засчитывается,
// только если она больше шума — смен вердикта между повторами обоих
// режимов вместе: при меньшей разнице её мог дать один неудачный повтор.
func conclude(r Report) []string {
	var out []string
	ra, hasRAG := r.statOf(RAG)
	no, hasNo := r.statOf(NoRAG)
	un := r.unanswerable()
	if hasRAG && hasNo {
		d := ra.Correct - no.Correct
		out = append(out, fmt.Sprintf("Верных ответов (большинство повторов): rag %d из %d, norag %d из %d — разница %+d.",
			ra.Correct, ra.Questions, no.Correct, no.Questions, d))
		noise := ra.Flips + no.Flips
		switch {
		case r.Repeats <= 1:
			out = append(out, fmt.Sprintf("Повтор один — без замера шума: разница %+d не проверена на шум модели (повторите с -repeat 2).", d))
		case abs(d) > noise:
			out = append(out, fmt.Sprintf("Разница %+d больше шума (смен вердикта между повторами: rag %d, norag %d) — засчитана в пользу %s.",
				d, ra.Flips, no.Flips, map[bool]string{true: "rag", false: "norag"}[d > 0]))
		default:
			out = append(out, fmt.Sprintf("Разница %+d не больше шума (смен вердикта между повторами: rag %d, norag %d) — не засчитана.",
				d, ra.Flips, no.Flips))
		}
	} else {
		for _, s := range r.Stats {
			out = append(out, fmt.Sprintf("%s: верно %d из %d, частично %d, неверно %d, «не знаю» %d.",
				s.Mode, s.Correct, s.Questions, s.Partial, s.Wrong, s.Abstain))
		}
	}
	for _, s := range r.Stats {
		if s.Mode.Pipelined() && hasRAG && hasNo {
			out = append(out, fmt.Sprintf("%s: верно %d из %d (rag %d) — разница с rag %+d.", s.Mode, s.Correct, s.Questions, ra.Correct, s.Correct-ra.Correct))
		}
		if s.Mode.UsesBase() {
			out = append(out, fmt.Sprintf("Доказательство в выдаче %s (индекс %s, k = %d): %.0f %% прогонов на отвечаемых вопросах.",
				s.Mode, r.Index, r.K, 100*s.Recall))
		}
	}
	if hasRAG && hasNo && ra.Discriminative > 0 {
		out = append(out, fmt.Sprintf("Дискриминативные вопросы (без базы модель их не знает, %d): верно rag %d, norag %d — разница %+d (на них разница — вклад базы, а не общих знаний модели).",
			ra.Discriminative, ra.DiscriminativeCorrect, no.DiscriminativeCorrect, ra.DiscriminativeCorrect-no.DiscriminativeCorrect))
	}
	var cw, ab, au []string
	for _, s := range r.Stats {
		cw = append(cw, fmt.Sprintf("%s %d", s.Mode, s.ConfidentWrong))
		ab = append(ab, fmt.Sprintf("%s %d", s.Mode, s.RightAbstain))
		au = append(au, fmt.Sprintf("%s %d", s.Mode, s.AnsweredUnanswerable))
	}
	out = append(out, "Уверенных ошибок на отвечаемых вопросах (wrong): "+strings.Join(cw, ", ")+".")
	if un > 0 {
		out = append(out, fmt.Sprintf("Вопросы без ответа в базе (%d): «не знаю» — %s; ответ по существу — %s. "+
			"У norag это ответ из памяти модели (база его не подтверждает и не опровергает), у rag — ответ мимо фрагментов.",
			un, strings.Join(ab, ", "), strings.Join(au, ", ")))
	}
	for _, s := range r.Stats {
		if s.Cited > 0 {
			out = append(out, citeLine(s))
		}
	}
	judged, agree := 0, 0.0
	var costs []string
	for _, s := range r.Stats {
		judged += s.Judged
		agree += s.Agreement * float64(s.Judged)
		costs = append(costs, fmt.Sprintf("%s %s", s.Mode, usd(s.Cost)))
	}
	tail := "Цена ответов: " + strings.Join(costs, ", ")
	if judged > 0 {
		tail = fmt.Sprintf("Согласие правила и судьи: %.0f %% из %d прогонов (расхождений %d). ", 100*agree/float64(judged), judged,
			len(r.Disagreements)) + tail + ", судья " + usd(r.JudgeCost)
	} else {
		tail = "Судьи не было — итог по правилу. " + tail
	}
	out = append(out, tail+".")
	return out
}

// citeLine — вывод по rag+cite числами.
func citeLine(s ModeStats) string {
	line := fmt.Sprintf("%s: ответов по существу %d, «не знаю» %d (по решению кода %d, на отвечаемых вопросах %d); "+
		"с источниками %d из %d, с цитатами %d из %d, дословных цитат %.0f %%; не проверено %d, отказов проверки %d",
		s.Mode, s.Answered, s.Unknown, s.ForcedUnknown, s.FalseUnknown, s.WithSources, s.Answered, s.WithQuotes, s.Answered,
		100*s.Verbatim, s.Unverified, s.Rejects)
	if s.SupportChecked > 0 {
		line += fmt.Sprintf("; смысл подтверждён цитатами у %d из %d", s.Supported, s.SupportChecked)
	}
	return line + "."
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// usd — цена для отчёта; неизвестный прайс — «?», а не ноль.
func usd(c llm.Cost) string {
	if !c.Known && c.USD == 0 {
		if c.Tariff == "" {
			return "$0"
		}
		return "?"
	}
	s := fmt.Sprintf("$%.4f", c.USD)
	if !c.Known {
		s += " (не всё посчитано)"
	}
	return s
}

// evSpan — фрагмент-доказательство в рунах текста документа.
type evSpan struct {
	doc  string
	s, e int
}

// evidenceIndex — позиции доказательств в документах базы. Recall — то же
// правило, что в kb.Compare: чанк покрывает ≥ kb.EvidenceCover фрагмента
// хотя бы одного доказательства своего документа. Тексты документов
// читаются из базы при первом обращении.
type evidenceIndex struct {
	s     *kb.Searcher
	texts map[string]string // doc_id → текст
}

func (x *evidenceIndex) of(ctx context.Context, q kb.Question) []evSpan {
	if x.s == nil {
		return nil
	}
	var out []evSpan
	for _, e := range q.Evidence {
		text, ok := x.texts[e.DocID]
		if !ok {
			d, err := x.s.Store.Doc(ctx, e.DocID)
			if err != nil {
				continue
			}
			text = d.Text()
			x.texts[e.DocID] = text
		}
		if start, n := corpus.Find(text, e.Quote); start >= 0 {
			out = append(out, evSpan{doc: e.DocID, s: start, e: start + n})
		}
	}
	return out
}

// covered — есть ли в выдаче чанк, покрывающий доказательство вопроса.
func (x *evidenceIndex) covered(ctx context.Context, q kb.Question, hits []kb.Hit) bool {
	evs := x.of(ctx, q)
	for _, h := range hits {
		for _, e := range evs {
			if h.DocID == e.doc && kb.Covers(h.Start, h.End, e.s, e.e) >= kb.EvidenceCover {
				return true
			}
		}
	}
	return false
}

// RankDepth — глубина поиска для разбора промахов выдачи: ранг первого
// релевантного фрагмента в топ-20 (как searchDepth в kb.Compare).
const RankDepth = 20

// RankRow — ранг первого фрагмента, покрывающего доказательство вопроса, в
// поиске на глубину RankDepth; 0 — в топе его нет.
type RankRow struct {
	ID    string `json:"id"`
	Split string `json:"split"`
	Rank  int    `json:"rank"`
}

// Ranks — поиск без модели: для каждого отвечаемого вопроса с
// доказательствами из наборов splits — ранг первого релевантного фрагмента
// (то же правило покрытия, что в kb.Compare и Run.Recall). Запрос — тот же,
// что у отвечающего агента (Question.Query), поэтому промах Answerer'а
// разбирается этим же рангом. Второе значение — режим поиска (с откатом).
func Ranks(ctx context.Context, s *kb.Searcher, qs kb.QuestionSet, splits []string, index string) ([]RankRow, kb.SearchInfo, error) {
	if s == nil {
		return nil, kb.SearchInfo{}, errors.New("нет базы знаний")
	}
	ev := &evidenceIndex{s: s, texts: map[string]string{}}
	var out []RankRow
	var info kb.SearchInfo
	for _, sp := range splits {
		for _, q := range qs.Split(sp) {
			// Как в kb.Compare: в счёт — вопросы, чьи доказательства нашлись
			// в тексте документов базы.
			if !q.Answerable || len(ev.of(ctx, q)) == 0 {
				continue
			}
			hits, si, err := s.Search(ctx, QuestionOf(q).Query(), kb.SearchOptions{Index: orIndex(index), K: RankDepth})
			if err != nil {
				return out, info, fmt.Errorf("%s: %w", q.ID, err)
			}
			if info.Mode == "" || si.Fallback != "" {
				info = si
			}
			row := RankRow{ID: q.ID, Split: sp}
			for i, h := range hits {
				if ev.covered(ctx, q, []kb.Hit{h}) {
					row.Rank = i + 1
					break
				}
			}
			out = append(out, row)
		}
	}
	return out, info, nil
}

// RecallAt — доля строк с рангом 1…k и их число.
func RecallAt(rows []RankRow, k int) (float64, int) {
	hit := 0
	for _, r := range rows {
		if r.Rank > 0 && r.Rank <= k {
			hit++
		}
	}
	if len(rows) == 0 {
		return 0, 0
	}
	return float64(hit) / float64(len(rows)), hit
}

// PipelineRanks — то же, что Ranks, но через конвейер retrieve с
// настройками c (режимы v23, добавление): ранг первого релевантного
// фрагмента в итоговой выдаче конвейера (после фильтра) при K1 =
// RankDepth; 0 — его нет (не найден или отсечён). RecallAt по этим строкам —
// «доказательство в топ-k после фильтра».
func PipelineRanks(ctx context.Context, p *retrieve.Pipeline, qs kb.QuestionSet, splits []string, c retrieve.Config) ([]RankRow, kb.SearchInfo, error) {
	if p == nil || p.Searcher == nil {
		return nil, kb.SearchInfo{}, errors.New("нет базы знаний")
	}
	ev := &evidenceIndex{s: p.Searcher, texts: map[string]string{}}
	c.K1 = RankDepth
	c.K0 = max(c.K0, RankDepth)
	var out []RankRow
	var info kb.SearchInfo
	for _, sp := range splits {
		for _, q := range qs.Split(sp) {
			if !q.Answerable || len(ev.of(ctx, q)) == 0 {
				continue
			}
			t, err := p.Search(ctx, retrieve.Query{Text: q.Q, Context: q.Context}, c)
			if err != nil {
				return out, info, fmt.Errorf("%s: %w", q.ID, err)
			}
			if info.Mode == "" || t.Info.Fallback != "" {
				info = t.Info
			}
			row := RankRow{ID: q.ID, Split: sp}
			for i, h := range t.Hits {
				if ev.covered(ctx, q, []kb.Hit{h}) {
					row.Rank = i + 1
					break
				}
			}
			out = append(out, row)
		}
	}
	return out, info, nil
}
