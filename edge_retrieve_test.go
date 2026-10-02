//go:build edge

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
)

// edgeRetrieve — подставной второй этап поиска (v23) вместо
// retrieve.Pipeline: кандидаты — настоящий поиск BM25 по временной kb.db
// (chunk_id настоящие), косинусы dense — правдоподобные (0,76–0,89, у e5 они
// сжаты), ранги, RRF, порог и причины отсечения — как у настоящего
// конвейера. Запрос с «кошачий медведь» переписывается в «малая панда
// Ailurus fulgens», запрос о динозаврах или фоссе — пустой итог (лучший
// косинус ниже порога), запрос о ксенофобе — разметка в раскрытом синониме
// и заметке: она должна остаться буквами.
type edgeRetrieve struct {
	s *kb.Searcher
}

// edgeFill — запрос добора кандидатов до K0, если BM25 нашёл меньше.
const edgeFill = "питание размножение распространение образ жизни"

func (e *edgeRetrieve) search(ctx context.Context, q retrieve.Query, c retrieve.Config) (retrieve.Trace, error) {
	return e.trace(ctx, q, c, nil)
}

// trace — путь поиска; extra — слова, добавляемые к запросу BM25 (у ответа
// по вопросу набора — разделы источников, как в edgeRAG.ask).
func (e *edgeRetrieve) trace(ctx context.Context, q retrieve.Query, c retrieve.Config, extra []string) (retrieve.Trace, error) {
	started := time.Now()
	if c.Index == "" {
		c.Index = rag.DefaultIndex
	}
	if c.K0 == 0 {
		c.K0 = retrieve.DefaultK0
	}
	if c.K1 == 0 {
		c.K1 = retrieve.DefaultK1
	}
	minScore, delta := retrieve.DefaultMinScore, retrieve.DefaultDelta
	if c.MinScore > 0 {
		minScore = c.MinScore
	}
	if c.Delta > 0 {
		delta = c.Delta
	}
	tr := retrieve.Trace{Original: q.Text, Rewritten: q.Text, Config: c, MinScore: minScore, MinScoreFrom: retrieve.MinScoreDefault}
	if c.MinScore > 0 {
		tr.MinScoreFrom = retrieve.MinScoreConfig
	}
	low := strings.ToLower(q.Text)
	// Вид назван в реплике (якорь): пол к запросу не применяется.
	if strings.Contains(low, "манул") {
		tr.Anchored = []string{"манул"}
	}
	if c.Rewrite != retrieve.RewriteNone {
		tr.RewriteBy = string(c.Rewrite)
		switch {
		case strings.Contains(low, "кошачий медвед") || strings.Contains(low, "кошачьего медвед"):
			tr.Rewritten = q.Text + " малая панда"
			tr.QueriesBM25 = []string{tr.Rewritten + " Ailurus fulgens"}
			tr.Expanded = []string{"кошачий медведь → малая панда"}
		case strings.Contains(low, "ксенофоб"):
			tr.Rewritten = q.Text + ` <b>проверочный зверёк</b>`
			tr.Expanded = []string{`<i>ксенофоб</i> → <b>проверочный зверёк</b> <img src=x onerror="window.__xss=46">`}
			tr.Note = `заметка конвейера <img src=x onerror="window.__xss=47"> — буквами`
		case len(q.Context) > 0:
			tr.Rewritten = q.Context[len(q.Context)-1] + " — " + q.Text
			tr.Expanded = []string{"вид из контекста: «" + q.Context[len(q.Context)-1] + "»"}
		}
	}
	tr.Queries = []string{tr.Rewritten}
	query := strings.Join(append([]string{tr.Rewritten}, extra...), " ")
	hits, _, err := e.s.Search(ctx, query, kb.SearchOptions{Index: c.Index, K: c.K0, Mode: kb.BM25})
	if err != nil {
		return retrieve.Trace{}, err
	}
	if len(hits) < c.K0 {
		more, _, err := e.s.Search(ctx, edgeFill, kb.SearchOptions{Index: c.Index, K: c.K0, Mode: kb.BM25})
		if err != nil {
			return retrieve.Trace{}, err
		}
		seen := map[string]bool{}
		for _, h := range hits {
			seen[h.ID] = true
		}
		for _, h := range more {
			if len(hits) >= c.K0 {
				break
			}
			if !seen[h.ID] {
				hits = append(hits, h)
			}
		}
	}
	// Вне базы — косинусы ниже порога у всех: фильтр отсечёт всё.
	empty := strings.Contains(low, "динозавр") || strings.Contains(low, "фосс")
	cands := make([]retrieve.Candidate, len(hits))
	for i, h := range hits {
		f := fnv.New32a()
		f.Write([]byte(h.ID))
		jitter := float64(f.Sum32()%35)/1000 - 0.017
		cos := 0.872 - 0.0052*float64(i) + jitter
		if empty {
			cos = 0.787 - 0.0015*float64(i) + jitter/2
		}
		if cos > 0.889 {
			cos = 0.889
		}
		cands[i] = retrieve.Candidate{Hit: h, Dense: cos, BM25: h.Score, RankBM25: i + 1}
	}
	// Ранг dense — по косинусу.
	byDense := make([]int, len(cands))
	for i := range byDense {
		byDense[i] = i
	}
	sort.SliceStable(byDense, func(a, b int) bool { return cands[byDense[a]].Dense > cands[byDense[b]].Dense })
	for r, i := range byDense {
		cands[i].RankDense = r + 1
	}
	if c.Rerank == retrieve.RerankHybrid {
		for i := range cands {
			cands[i].Rerank = 1/float64(retrieve.RRFK+cands[i].RankDense) + 1/float64(retrieve.RRFK+cands[i].RankBM25)
		}
		sort.SliceStable(cands, func(a, b int) bool { return cands[a].Rerank > cands[b].Rerank })
	} else {
		sort.SliceStable(cands, func(a, b int) bool { return cands[a].RankDense < cands[b].RankDense })
	}
	top := 0.0
	for _, x := range cands {
		if x.Dense > top {
			top = x.Dense
		}
	}
	sections := map[string]bool{}
	kept := 0
	for i := range cands {
		x := &cands[i]
		x.Final = i + 1
		section := x.DocID + "|" + strings.Join(x.Path, "/")
		switch {
		case c.Filter && len(tr.Anchored) == 0 && x.Dense < minScore:
			x.Reason = fmt.Sprintf("порог %.2f", minScore)
		case c.Filter && x.Dense < top-delta:
			x.Reason = fmt.Sprintf("хуже лучшего на %.2f", delta)
		case c.Filter && sections[section]:
			x.Reason = "повтор текста"
		case kept >= c.K1:
			x.Reason = "за пределами K1"
		default:
			x.Kept = true
			kept++
			sections[section] = true
			h := x.Hit
			h.Rank, h.Score = kept, x.Dense
			tr.Hits = append(tr.Hits, h)
		}
	}
	tr.Candidates = cands
	tr.TopDense = top
	second := 0.0
	for _, x := range cands {
		if x.Dense < top && x.Dense > second {
			second = x.Dense
		}
	}
	tr.Gap = top - second
	tr.Empty = c.Filter && kept == 0
	tr.Info = kb.SearchInfo{Index: c.Index, Mode: kb.Dense, Embedder: "hash-256", Millis: 23.4}
	tr.Millis = time.Since(started).Milliseconds() + 31
	if tr.Hits == nil {
		tr.Hits = []kb.Hit{}
	}
	return tr, nil
}

// edgeModeConfig — настройки конвейера режимов ответа v23 (как
// rag.ModeConfig у настоящего агента).
func edgeModeConfig(m rag.Mode) retrieve.Config {
	c := retrieve.Config{Index: rag.DefaultIndex, K0: retrieve.DefaultK0, K1: rag.DefaultK}
	switch m {
	case rag.RAGFilter:
		c.Filter = true
	case rag.RAGRewrite:
		c.Rewrite = retrieve.RewriteCode
	case rag.RAGBoth:
		c.Rewrite, c.Filter = retrieve.RewriteCode, true
	}
	return c
}

// edgeMatrix — матрица режимов, как её пишет kb matrix: base, filter,
// rewrite, both × K1 {3, 5, 8} × test, dev, out.
func edgeMatrix() retrieve.Matrix {
	type base struct{ before, after, mrr, prec, cut, wrong, outEmpty, tokens, ms float64 }
	presets := []struct {
		name string
		c    retrieve.Config
		b    base
	}{
		{"base", retrieve.Config{}, base{0.90, 0.80, 0.62, 0.34, 0, 0, 0, 1450, 41}},
		{"filter", retrieve.Config{Filter: true}, base{0.90, 0.80, 0.66, 0.52, 0.58, 0, 0.67, 830, 43}},
		{"rewrite", retrieve.Config{Rewrite: retrieve.RewriteCode}, base{1.00, 0.90, 0.71, 0.40, 0, 0, 0, 1470, 44}},
		{"both", retrieve.Config{Rewrite: retrieve.RewriteCode, Filter: true}, base{1.00, 0.90, 0.78, 0.61, 0.66, 0, 0.83, 760, 47}},
	}
	m := retrieve.Matrix{Created: time.Date(2026, 10, 1, 18, 40, 0, 0, time.Local), CorpusSHA: "6f1c2d9e8a7b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c9d",
		Embedder: "intfloat/multilingual-e5-base", Index: "structure", MinScore: 0.815, Delta: 0.05}
	for _, split := range []string{kb.SplitTest, kb.SplitDev, kb.SplitOut} {
		for _, k1 := range []int{3, 5, 8} {
			kf := float64(k1-5) * 0.03
			for _, p := range presets {
				b := p.b
				r := retrieve.MatrixRow{Name: p.name, Split: split, K1: k1, N: 10, Millis: b.ms, CutShare: b.cut}
				r.Tokens = b.tokens * float64(k1) / 5
				if p.c.Filter {
					r.Tokens = b.tokens * (1 + float64(k1-5)*0.06)
				}
				switch split {
				case kb.SplitOut:
					r.N, r.OutN, r.OutEmpty = 6, 6, b.outEmpty
					r.CutShare = b.cut + 0.2*boolF(p.c.Filter)
				default:
					if split == kb.SplitDev {
						r.N = 20
					}
					r.RecallBefore = b.before
					r.RecallUnion = b.before
					r.RecallAfter = clamp01(b.after + kf)
					r.MRR = clamp01(b.mrr + kf/2)
					r.Precision = clamp01(b.prec - kf*1.5)
					if p.c.Filter && k1 == 3 {
						r.WrongCut, r.LostQ = 0.02, 0.05
					}
				}
				m.Rows = append(m.Rows, r)
			}
		}
	}
	m.Conclusion = []string{
		"both (rewrite + фильтр) при K1 = 5: recall на test 0,90 против 0,80 у base, precision 0,61 против 0,34.",
		"Фильтр отсекает 66 % кандидатов, доказательство ошибочно не отсечено ни разу на test при K1 ≥ 5.",
		"На out после фильтра пусто в 5 из 6 вопросов (И-11: не меньше 5 из 6) — у base ни в одном.",
		"rewrite поднимает recall@5 на вопросах с синонимами: T07 «кошачий медведь» — доказательство с 7-го места на 1-е.",
		"Контекст модели при both — 760 токенов против 1450 у base: в 1,9 раза меньше.",
	}
	return m
}

func boolF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// edgeCalibration — калибровка порога, как её пишет kb calibrate: порог
// 0,780–0,850 с шагом 0,005, выбран 0,815 (recall dev падает на 0,05).
func edgeCalibration() retrieve.Calibration {
	cal := retrieve.Calibration{Index: "structure", Embedder: "intfloat/multilingual-e5-base", Delta: 0.05, MaxDrop: 0.05, Base: 1, Chosen: 0.815,
		Rule: retrieve.CalibMaxDrop, OutMax: 0.823, OutMaxID: "O05", EvidenceMin: 0.812, EvidenceMinID: "D12", Gap: -0.011, MarginOut: -0.008, MarginDev: -0.003,
		FloorDev: []string{"D07", "D12"}, FloorOut: []string{"O01", "O02", "O05"}, FloorTest: []string{"T06", "T08"},
		Note:   "зазора нет: лучший косинус вопроса вне базы 0.823 (O05) не ниже косинуса доказательства неякорного dev 0.812 (D12)",
		DevTop: []float64{0.889, 0.884, 0.879, 0.876, 0.871, 0.868, 0.866, 0.861, 0.858, 0.853, 0.851, 0.847, 0.843, 0.838, 0.836, 0.831, 0.827, 0.822, 0.818, 0.812},
		OutTop: []float64{0.771, 0.784, 0.789, 0.797, 0.806, 0.823}}
	for i := 0; i <= 14; i++ {
		t := 0.78 + 0.005*float64(i)
		row := retrieve.CalibRow{MinScore: t}
		var lost []string
		for j, v := range cal.DevTop {
			if v < t-1e-9 {
				lost = append(lost, fmt.Sprintf("D%02d", 20-j))
			}
		}
		row.DevRecall = float64(len(cal.DevTop)-len(lost)) / float64(len(cal.DevTop))
		row.LostDev = lost
		n := 0
		for _, v := range cal.OutTop {
			if v < t-1e-9 {
				n++
			}
		}
		row.OutEmpty = float64(n) / float64(len(cal.OutTop))
		cal.Table = append(cal.Table, row)
	}
	return cal
}

// edgeWriteJSON — файл JSON во временном каталоге теста.
func edgeWriteJSON(t *testing.T, dir, name string, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
