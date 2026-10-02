package retrieve

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
)

// CalibStep — шаг перебора абсолютного порога.
const CalibStep = 0.005

// Calibrate подбирает порог и (если write) пишет его в индекс
// (kb.Store.SetMinScore).
//
// Конфигурация — `filter` (без переписывания и реранкинга) с заданным
// Delta и K1 = DefaultK1: порог подбирается для того фильтра, который его
// применяет. Поиск — один раз на вопрос; перебор порога — пересчёт фильтра
// по той же выдаче. Порог по BM25 не калибруется: при откате поиска —
// ошибка.
//
// Пол касается только вопросов без якоря (вид в реплике не назван), поэтому
// и выбирается по ним:
//
//   - сверху — максимум лучшего косинуса у вопросов вне базы (out-of-base
//     без якоря): выше него пол отсекает их все;
//   - снизу — минимум косинуса доказательства у неякорных dev-вопросов,
//     чьё доказательство доходит до итога фильтра без пола: ниже него пол
//     доказательства не трогает.
//
// Если между ними зазор (минимум доказательства больше максимума out),
// порог — середина зазора: запас с обеих сторон одинаковый. Если зазора нет —
// прежнее правило (наибольший порог из перебора, при котором доказательство
// на dev теряется не больше чем на maxDrop против выдачи без фильтра) и
// предупреждение в Note. Перебор (таблица) строится в обоих случаях: от
// наименьшего лучшего косинуса минус 0.03 до наибольшего плюс шаг.
func Calibrate(ctx context.Context, p *Pipeline, qs kb.QuestionSet, index string, delta, maxDrop float64, write bool) (Calibration, error) {
	if p == nil || p.Searcher == nil || p.Searcher.Store == nil {
		return Calibration{}, errors.New("конвейеру не передана база знаний")
	}
	cfg := resolve(Config{Index: index, Filter: true, Delta: delta})
	if maxDrop < 0 {
		maxDrop = 0
	}
	cal := Calibration{Index: cfg.Index, Delta: cfg.Delta, MaxDrop: maxDrop, Created: time.Now().UTC()}
	cal.Embedder = "нет"
	if p.Searcher.Embedder != nil {
		cal.Embedder = p.Searcher.Embedder.Model()
	}
	al, err := p.aliases(ctx)
	if err != nil {
		return cal, err
	}
	ev := &evidenceSet{st: p.Searcher.Store, texts: map[string]string{}}
	type item struct {
		id  string
		dev bool
		ev  []evidence
		t   Trace
	}
	var items []item
	cal.OutMax, cal.EvidenceMin = -1, -1
	for _, sp := range []string{kb.SplitDev, kb.SplitOut} {
		for _, q := range qs.Split(sp) {
			it := item{id: q.ID}
			switch {
			case sp == kb.SplitDev && q.Answerable:
				if it.ev = ev.of(ctx, q); len(it.ev) == 0 {
					continue
				}
				it.dev = true
			case sp == kb.SplitOut || !q.Answerable:
			default:
				continue
			}
			t, err := p.gather(ctx, Query{Text: q.Q, Context: q.Context}, cfg)
			if err != nil {
				return cal, fmt.Errorf("%s: %w", q.ID, err)
			}
			if t.Info.Mode != kb.Dense {
				return cal, fmt.Errorf("порог — косинус dense, а поиск откатился на BM25 (%s): калибровать нечего — поднимите эмбеддер",
					orText(t.Info.Fallback, "нет векторов"))
			}
			it.t = t
			items = append(items, it)
			anchored := len(t.Anchored) > 0
			if anchored {
				cal.Anchored = append(cal.Anchored, q.ID)
			}
			if it.dev {
				cal.DevN++
				cal.DevTop = append(cal.DevTop, round3(t.TopDense))
				if anchored {
					continue
				}
				cal.FloorDev = append(cal.FloorDev, q.ID)
			} else {
				cal.OutN++
				cal.OutTop = append(cal.OutTop, round3(t.TopDense))
				if anchored {
					continue
				}
				cal.FloorOut = append(cal.FloorOut, q.ID)
				if q.Type == "out-of-base" && t.TopDense > cal.OutMax {
					cal.OutMax, cal.OutMaxID = round3(t.TopDense), q.ID
				}
			}
		}
	}
	if cal.DevN == 0 {
		return cal, errors.New("в dev нет отвечаемых вопросов с доказательствами — калибровать не на чем")
	}
	// Test — только счёт неякорных (вид в реплике не назван): на test порог
	// проверяется, а не подбирается, и поиск по нему здесь не идёт.
	for _, q := range qs.Split(kb.SplitTest) {
		if len(al.Species(q.Q)) == 0 {
			cal.FloorTest = append(cal.FloorTest, q.ID)
		}
	}
	// apply — фильтр с порогом (th < 0 — без фильтра): у каких dev
	// доказательство в итоге, сколько out пусты.
	apply := func(th float64) (hit map[string]bool, empty int) {
		hit = map[string]bool{}
		for _, it := range items {
			t := it.t
			t.Candidates = append([]Candidate(nil), it.t.Candidates...)
			if th < 0 {
				t.Config.Filter = false
			} else {
				t.MinScore = th
			}
			finish(&t)
			if it.dev {
				for _, h := range t.Hits {
					if relevant(h.Chunk, it.ev) {
						hit[it.id] = true
						break
					}
				}
			} else if t.Empty {
				empty++
			}
		}
		return hit, empty
	}
	baseHit, _ := apply(-1)
	cal.Base = ratio(len(baseHit), cal.DevN)
	// Косинус доказательства — у неякорных dev, чьё доказательство доходит
	// до итога фильтра без пола (порог 0: относительный порог, повторы и K1
	// действуют). Не дошло и без пола — пол его не теряет, и в зазор вопрос
	// не входит (FloorMissed): так продолжение без контекста («а сколько их
	// всего осталось?» у filter) не тянет порог вниз.
	noFloor, _ := apply(0)
	floorDev := map[string]bool{}
	for _, id := range cal.FloorDev {
		floorDev[id] = true
	}
	for _, it := range items {
		if !it.dev || !floorDev[it.id] {
			continue
		}
		if !noFloor[it.id] {
			cal.FloorMissed = append(cal.FloorMissed, it.id)
			continue
		}
		if c := evidenceCos(it.t, it.ev); c > 0 && (cal.EvidenceMin < 0 || c < cal.EvidenceMin) {
			cal.EvidenceMin, cal.EvidenceMinID = round3(c), it.id
		}
	}
	row := func(th float64) CalibRow {
		hit, empty := apply(th)
		r := CalibRow{MinScore: th, DevRecall: ratio(len(hit), cal.DevN), OutEmpty: ratio(empty, cal.OutN)}
		for id := range baseHit {
			if !hit[id] {
				r.LostDev = append(r.LostDev, id)
			}
		}
		sort.Strings(r.LostDev)
		return r
	}

	tops := append(append([]float64(nil), cal.DevTop...), cal.OutTop...)
	sort.Float64s(tops)
	lo := math.Floor((tops[0]-0.03)/CalibStep) * CalibStep
	hi := math.Ceil((tops[len(tops)-1]+CalibStep)/CalibStep) * CalibStep
	byDrop := -1.0
	for th := lo; th <= hi+1e-9; th += CalibStep {
		r := row(round3(th))
		cal.Table = append(cal.Table, r)
		if r.DevRecall >= cal.Base-maxDrop-1e-9 {
			byDrop = r.MinScore
		}
	}

	if cal.OutMax >= 0 && cal.EvidenceMin >= 0 {
		cal.Gap = round3(cal.EvidenceMin - cal.OutMax)
	}
	switch {
	case cal.OutMax >= 0 && cal.EvidenceMin >= 0 && cal.Gap > 0:
		cal.Rule = CalibGap
		cal.Chosen = round3((cal.OutMax + cal.EvidenceMin) / 2)
	default:
		cal.Rule = CalibMaxDrop
		cal.Chosen = byDrop
		if cal.OutMax < 0 || cal.EvidenceMin < 0 {
			cal.Note = "зазор не определён (нет неякорных вопросов вне базы или неякорных dev, чьё доказательство доходит до итога без пола) — порог по правилу max-drop"
		} else {
			cal.Note = fmt.Sprintf("зазора нет: лучший косинус вопроса вне базы %.3f (%s) не ниже косинуса доказательства неякорного dev %.3f (%s) — "+
				"любой порог или пропустит вопрос вне базы, или отсечёт доказательство; порог по правилу max-drop",
				cal.OutMax, cal.OutMaxID, cal.EvidenceMin, cal.EvidenceMinID)
		}
		if cal.Chosen < 0 {
			// Даже самый низкий порог теряет больше MaxDrop: теряет
			// относительный порог, а не абсолютный.
			cal.Chosen = round3(lo)
			cal.Note = joinNote(cal.Note, fmt.Sprintf("recall падает больше чем на %.2f уже при самом низком пороге: теряет относительный порог (Delta %.2f) — его и надо менять", maxDrop, cal.Delta))
		}
	}
	if cal.OutMax >= 0 {
		cal.MarginOut = round3(cal.Chosen - cal.OutMax)
	}
	if cal.EvidenceMin >= 0 {
		cal.MarginDev = round3(cal.EvidenceMin - cal.Chosen)
	}
	cal.At = row(cal.Chosen)
	if write {
		if err := p.Searcher.Store.SetMinScore(ctx, cal.Index, cal.Chosen); err != nil {
			return cal, fmt.Errorf("порог не записан: %w", err)
		}
		cal.Written = true
	}
	return cal, nil
}

// Правила выбора порога (Calibration.Rule).
const (
	CalibGap     = "gap"      // середина зазора между out и доказательствами dev
	CalibMaxDrop = "max-drop" // наибольший порог с потерей dev ≤ MaxDrop
)

// evidenceCos — косинус доказательства: лучший косинус кандидатов,
// покрывающих доказательство; 0 — доказательства среди кандидатов нет.
func evidenceCos(t Trace, ev []evidence) float64 {
	best := 0.0
	for _, c := range t.Candidates {
		if relevant(c.Chunk, ev) {
			best = math.Max(best, c.Dense)
		}
	}
	return best
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

// chosenRow — строка с выбранным порогом (At; у старых отчётов — из таблицы).
func (c Calibration) chosenRow() (CalibRow, bool) {
	if c.At.MinScore > 0 {
		return c.At, true
	}
	for _, r := range c.Table {
		if math.Abs(r.MinScore-c.Chosen) < 1e-9 {
			return r, true
		}
	}
	return CalibRow{}, false
}

// Markdown — для examples/rag/calibrate.md (добавление к контракту): выбор
// и его правило, вопросы, чувствительные к полу, зазор и запасы,
// гистограммы лучших косинусов dev и out, таблица «порог → recall dev,
// пусто на out, потерянные dev».
func (c Calibration) Markdown() string {
	var b strings.Builder
	b.WriteString("# Калибровка порога релевантности\n\n")
	fmt.Fprintf(&b, "Индекс `%s`, эмбеддер %s. %s. Вопросы: dev (отвечаемые с доказательством) — %d, out — %d. Test не используется: на нём порог проверяется, а не подбирается.\n\n",
		c.Index, c.Embedder, c.Created.Format("2006-01-02 15:04"), c.DevN, c.OutN)
	fmt.Fprintf(&b, "Фильтр — как у конфигурации `filter`: абсолютный пол косинуса, относительный порог «не хуже лучшего на %.2f», отсев повторов текста, K1 = %d. "+
		"Пол касается только вопросов без якоря (вид в реплике не назван), и порог выбирается по ним: если лучший косинус вопросов вне базы (out-of-base) "+
		"ниже косинуса доказательства у dev — порог посередине зазора; иначе — наибольший порог из перебора (шаг %.3f), при котором доказательство в итоге "+
		"на dev теряется не больше чем на %.2f против выдачи без фильтра (%.2f).\n\n",
		c.Delta, DefaultK1, CalibStep, c.MaxDrop, c.Base)
	b.WriteString("## Выбор\n\n")
	rule := "середина зазора"
	if c.Rule == CalibMaxDrop {
		rule = fmt.Sprintf("правило max-drop %.2f", c.MaxDrop)
	}
	if r, ok := c.chosenRow(); ok {
		fmt.Fprintf(&b, "Порог **%.3f** (%s): recall на dev %.2f (без фильтра %.2f), пусто на out %d из %d", c.Chosen, rule, r.DevRecall, c.Base,
			int(math.Round(r.OutEmpty*float64(c.OutN))), c.OutN)
		if len(r.LostDev) > 0 {
			fmt.Fprintf(&b, ", потеряно на dev: %s", strings.Join(r.LostDev, ", "))
		}
		b.WriteString(".")
	} else {
		fmt.Fprintf(&b, "Порог **%.3f** (%s).", c.Chosen, rule)
	}
	if c.Written {
		b.WriteString(" Записан в индекс.\n")
	} else {
		b.WriteString(" В индекс не записан (`kb calibrate -write` запишет).\n")
	}
	if c.Note != "" {
		b.WriteString("\n**Внимание:** " + c.Note + ".\n")
	}
	b.WriteString("\n## Зазор\n\n")
	fmt.Fprintf(&b, "Чувствительны к полу (вид в реплике не назван): dev %d из %d (%s), out %d из %d (%s), test %d (%s).\n\n",
		len(c.FloorDev), c.DevN, orText(strings.Join(c.FloorDev, ", "), "—"), len(c.FloorOut), c.OutN, orText(strings.Join(c.FloorOut, ", "), "—"),
		len(c.FloorTest), orText(strings.Join(c.FloorTest, ", "), "—"))
	if c.OutMax >= 0 {
		fmt.Fprintf(&b, "- максимум лучшего косинуса вне базы (out-of-base без якоря): %.3f (%s);\n", c.OutMax, c.OutMaxID)
	} else {
		b.WriteString("- вопросов вне базы без якоря нет;\n")
	}
	if c.EvidenceMin >= 0 {
		fmt.Fprintf(&b, "- минимум косинуса доказательства у неякорных dev, чьё доказательство доходит до итога без пола: %.3f (%s);\n", c.EvidenceMin, c.EvidenceMinID)
	} else {
		b.WriteString("- неякорных dev, чьё доказательство доходит до итога без пола, нет;\n")
	}
	if len(c.FloorMissed) > 0 {
		fmt.Fprintf(&b, "- не дошли до итога и без пола (пол их не теряет, в зазор не входят): %s;\n", strings.Join(c.FloorMissed, ", "))
	}
	if c.OutMax >= 0 && c.EvidenceMin >= 0 {
		fmt.Fprintf(&b, "- зазор %+.3f; порог %.3f; запас до вопросов вне базы %+.3f, до доказательств %+.3f.\n", c.Gap, c.Chosen, c.MarginOut, c.MarginDev)
	}
	b.WriteString("\n## Лучший косинус кандидатов\n\n")
	fmt.Fprintf(&b, "- dev: %s\n- out: %s\n", floats(c.DevTop), floats(c.OutTop))
	if len(c.Anchored) > 0 {
		fmt.Fprintf(&b, "\nВ реплике назван вид корпуса (якорь: пол к ним не применяется, и порог их не касается): %s.\n", strings.Join(c.Anchored, ", "))
	}
	b.WriteString("\n" + histogram(c.DevTop, c.OutTop))
	b.WriteString("\n## Таблица\n\n| порог | recall dev | пусто на out | потеряно на dev |\n|---|---|---|---|\n")
	for _, r := range c.Table {
		mark := ""
		if math.Abs(r.MinScore-c.Chosen) < CalibStep/2 && (c.Rule != CalibGap || r.MinScore <= c.Chosen) {
			mark = " ◀"
		}
		fmt.Fprintf(&b, "| %.3f%s | %.2f | %d из %d | %s |\n", r.MinScore, mark, r.DevRecall,
			int(math.Round(r.OutEmpty*float64(c.OutN))), c.OutN, orText(strings.Join(r.LostDev, ", "), "—"))
	}
	return b.String()
}

func floats(xs []float64) string {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	parts := make([]string, len(s))
	for i, x := range s {
		parts[i] = fmt.Sprintf("%.3f", x)
	}
	return orText(strings.Join(parts, " "), "—")
}

// histogram — столбики по корзинам 0.01: «0.83 | dev ███ | out █».
func histogram(dev, out []float64) string {
	all := append(append([]float64(nil), dev...), out...)
	if len(all) == 0 {
		return ""
	}
	sort.Float64s(all)
	lo := math.Floor(all[0]*100) / 100
	hi := math.Floor(all[len(all)-1]*100) / 100
	var b strings.Builder
	b.WriteString("```\n")
	for x := lo; x <= hi+1e-9; x += 0.01 {
		d, o := 0, 0
		for _, v := range dev {
			if v >= x-1e-9 && v < x+0.01-1e-9 {
				d++
			}
		}
		for _, v := range out {
			if v >= x-1e-9 && v < x+0.01-1e-9 {
				o++
			}
		}
		fmt.Fprintf(&b, "%.2f  dev %-12s out %s\n", x, strings.Repeat("█", d), strings.Repeat("█", o))
	}
	b.WriteString("```\n")
	return b.String()
}
