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
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

// DefaultIndex — индекс конвейера по умолчанию (rag.DefaultIndex; rag
// импортирует retrieve, поэтому константа своя).
const DefaultIndex = string(kb.Structure)

// BM25Floor — относительный порог при откате на BM25: кандидат должен
// набрать не меньше этой доли балла лучшего. Шкала BM25 нормирована к
// лучшему (1), и Delta косинуса к ней неприменима: 0.05 от единицы отсекла
// бы всё, кроме первого. Половина лучшего — грубо, но честно названо в Note.
const BM25Floor = 0.5

// Причины отсева (Candidate.Reason).
const (
	ReasonDup    = "повтор текста"
	ReasonBeyond = "за пределами K1"
	ReasonLLM    = "модель: не относится"
)

// Причины-шаблоны с числом: «порог 0.800», «хуже лучшего на 0.05».
func reasonFloor(v float64) string    { return fmt.Sprintf("порог %.3f", v) }
func reasonRelative(d float64) string { return fmt.Sprintf("хуже лучшего на %.2f", d) }
func reasonBM25() string              { return fmt.Sprintf("BM25 ниже %.0f %% лучшего", 100*BM25Floor) }

// noteBM25 — заметка фильтра при откате поиска на BM25.
const noteBM25 = "BM25: абсолютного порога нет, только относительный и дубли"

// FilterCut — кандидат отсечён фильтром (порог, относительный порог, дубль,
// оценка модели), а не просто не поместился в K1.
func (c Candidate) FilterCut() bool { return !c.Kept && c.Reason != "" && c.Reason != ReasonBeyond }

// resolve — настройки с умолчаниями (кроме MinScore: он зависит от индекса).
func resolve(c Config) Config {
	if strings.TrimSpace(c.Index) == "" {
		c.Index = DefaultIndex
	}
	if c.K1 <= 0 {
		c.K1 = DefaultK1
	}
	if c.K0 <= 0 {
		c.K0 = DefaultK0
	}
	c.K0 = max(c.K0, c.K1)
	if c.Delta <= 0 {
		c.Delta = DefaultDelta
	}
	return c
}

// Search — поиск в два этапа по настройкам.
//
// Шаги: rewrite (запросы) → кандидаты K0 (dense и BM25 по каждому запросу,
// объединение по chunk_id) → косинус для кандидатов только из BM25
// (kb.Searcher.Score) → rerank (порядок Final) → filter → первые K1. Каждый
// кандидат остаётся в Trace с баллами и причиной отсева.
//
// Правило для кандидатов, которых нашёл только BM25 (RankDense = 0): их
// косинус считается отдельно, и абсолютный пол к ним применяется как ко
// всем — пол отделяет «в базе есть» от «в базе нет», и его подбирают на
// out. Относительный порог «не хуже лучшего на Delta» меряет близость к
// лучшему dense-кандидату и потому по построению против лексических
// находок («кошачий медведь» dense не видит в топ-20, а BM25 — видит).
// Поэтому при гибридном реранкинге BM25-находка, которую RRF поставил в
// первую половину K1 (Final ≤ ⌈K1/2⌉), от относительного порога
// освобождена — её место в топе заработано совпадением слов в нескольких
// списках, а не одним косинусом. Ниже половины K1 — общие правила. При
// RerankLLM так же освобождён кандидат с оценкой модели 3.
//
// Контекст (Query.Context) — только для переписывания: без Rewrite в поиск
// идёт одна реплика, а RewriteCode берёт из контекста вид, только если
// реплика — продолжение (см. rewriteCode).
//
// Относительный порог меряется от лучшего косинуса СВОЕГО запроса: у
// подзапросов RewriteLLM («вес харзы» и «вес барханного кота») лучшие
// разные, и общий лучший отсекал бы всё найденное по второму.
//
// Якорь (Trace.Anchored): если в самой реплике назван вид корпуса
// (каноном, синонимом, латынью; вид, унаследованный из контекста, якорем
// не считается), абсолютный пол не применяется. Пол нужен, чтобы сказать «в
// базе об этом ничего нет», а статья о названном виде в базе есть точно;
// есть ли в ней нужный аспект («сколько лет живёт харза»), косинус не
// различает — у таких вопросов лучший косинус 0.85–0.88, выше любого
// разумного пола, и «не знаю» там — дело ответа, а не поиска. Зато без
// якоря пол режет синонимы, которые dense понимает плохо: на dev без
// переписывания лучший косинус «палласова кота» (D12) — 0.820, «хаусов»
// (D17) — 0.811, а вопросов вне базы — до 0.822. Относительный порог и
// отсев повторов к якорным запросам применяются как ко всем.
func (p *Pipeline) Search(ctx context.Context, q Query, c Config) (Trace, error) {
	started := time.Now()
	t, err := p.gather(ctx, q, c)
	if err != nil {
		return t, err
	}
	finish(&t)
	t.Millis = time.Since(started).Milliseconds()
	return t, nil
}

// gather — всё до фильтра: переписывание, кандидаты, порядок. Матрица и
// калибровка зовут его один раз и затем применяют фильтр с разными K1 и
// порогами (finish), не повторяя поиск и платные запросы.
func (p *Pipeline) gather(ctx context.Context, q Query, c Config) (Trace, error) {
	c = resolve(c)
	t := Trace{Original: strings.TrimSpace(q.Text), Config: c}
	if p == nil || p.Searcher == nil || p.Searcher.Store == nil {
		return t, errors.New("конвейеру не передана база знаний")
	}
	if t.Original == "" {
		return t, errors.New("пустой запрос")
	}
	if (c.Rewrite == RewriteLLM || c.Rerank == RerankLLM) && p.LLM == nil {
		return t, errors.New("переписывание и реранкинг моделью требуют модели (Pipeline.LLM)")
	}
	switch c.Rewrite {
	case RewriteNone, RewriteCode, RewriteLLM:
	default:
		return t, fmt.Errorf("неизвестный rewrite %q (code, llm)", c.Rewrite)
	}
	switch c.Rerank {
	case RerankNone, RerankHybrid, RerankLLM:
	default:
		return t, fmt.Errorf("неизвестный rerank %q (hybrid, llm)", c.Rerank)
	}
	idx, err := p.Searcher.Store.Index(ctx, c.Index)
	if err != nil {
		return t, err
	}

	al, err := p.aliases(ctx)
	if err != nil {
		return t, err
	}

	// 1. Запросы. Без переписывания — одна реплика: контекст нужен только
	// переписыванию, а склейка с прошлыми репликами тянула бы поиск к
	// прошлой теме («Где водится харза?» → «Сколько весит жираф?»).
	switch c.Rewrite {
	case RewriteNone:
		t.Rewritten = t.Original
		t.Queries = []string{t.Original}
	case RewriteCode:
		t.RewriteBy = string(RewriteCode)
		var bm25 string
		t.Rewritten, bm25, t.Expanded, t.Note = rewriteCode(al, q)
		t.Queries = []string{t.Rewritten}
		t.QueriesBM25 = []string{bm25}
	case RewriteLLM:
		if err := p.rewriteLLM(ctx, al, q, &t); err != nil {
			return t, err
		}
	}
	if len(t.QueriesBM25) > 0 && strings.Join(t.QueriesBM25, "\x00") == strings.Join(t.Queries, "\x00") {
		t.QueriesBM25 = nil
	}

	// Якорь — виды, названные в самой реплике (не из контекста).
	t.Anchored = al.Species(t.Original)

	// 2. Кандидаты.
	if err := p.candidates(ctx, c, &t); err != nil {
		return t, err
	}
	dense := t.Info.Mode == kb.Dense
	if dense {
		t.MinScore, t.MinScoreFrom = MinScoreOf(c, idx)
	} else {
		t.Note = joinNote(t.Note, noteBM25+" (поиск без векторов: "+orText(t.Info.Fallback, "нет эмбеддера")+")")
	}

	// 3. Порядок.
	if c.Rerank == RerankLLM {
		if err := p.rerankLLM(ctx, &t); err != nil {
			return t, err
		}
	}
	order(&t)
	return t, nil
}

// MinScoreOf — абсолютный порог конвейера и откуда он: Config.MinScore
// («настройки») → порог индекса kb.IndexInfo.MinScore, записанный
// калибровкой («индекс») → DefaultMinScore («умолчание»). Одно правило для
// хука чата, отвечающего агента, окна «База знаний» и kb.
func MinScoreOf(c Config, idx kb.IndexInfo) (float64, string) {
	switch {
	case c.MinScore > 0:
		return c.MinScore, MinScoreConfig
	case idx.MinScore > 0:
		return idx.MinScore, MinScoreIndex
	}
	return DefaultMinScore, MinScoreDefault
}

// Источники порога (Trace.MinScoreFrom).
const (
	MinScoreConfig  = "настройки"
	MinScoreIndex   = "индекс"
	MinScoreDefault = "умолчание"
)

// MinScore — порог, который конвейер применит с настройками c, и откуда он
// (для kb qa и kb ask: напечатать до прогона).
func (p *Pipeline) MinScore(ctx context.Context, c Config) (float64, string, error) {
	if p == nil || p.Searcher == nil || p.Searcher.Store == nil {
		return 0, "", errors.New("конвейеру не передана база знаний")
	}
	c = resolve(c)
	idx, err := p.Searcher.Store.Index(ctx, c.Index)
	if err != nil {
		return 0, "", err
	}
	v, from := MinScoreOf(c, idx)
	return v, from, nil
}

// joinContext — предыдущие реплики и вопрос через пробел: запрос
// продолжения, когда вида нет ни в одной реплике.
func joinContext(q Query) string {
	parts := make([]string, 0, len(q.Context)+1)
	for _, c := range q.Context {
		if c = strings.TrimSpace(c); c != "" {
			parts = append(parts, c)
		}
	}
	return strings.Join(append(parts, strings.TrimSpace(q.Text)), " ")
}

// rewriteCode — переписывание кодом; запросы для dense и BM25 и заметка:
//
//   - синонимы раскрываются в канон (Aliases.Queries); если вид назван
//     каноном — dense-запрос = реплика без изменений;
//   - латынь названных видов — только в BM25-запрос: MDD в корпусе записан
//     латынью («Ailurus fulgens»), и BM25 по ней находит MDD, а dense от
//     латыни сбивается (на dev доказательство уходило с 1-го места на 4-е);
//   - реплика без названного вида, которая продолжает разговор
//     (continuation: местоимение, «а …», «этот», короткая реплика без
//     животного), получает прошлую реплику, где вид назван (последнюю):
//     запрос = прошлая реплика + текущая. Не одно название вида: прошлая
//     реплика несёт и тему («статус МСОП» → «а сколько их осталось?» ищет
//     численность в разделе о статусе). Если вида нет ни в одной реплике —
//     контекст и вопрос (с заметкой);
//   - реплика с названным видом или самостоятельная («Сколько весит
//     взрослый жираф?») — без контекста: прошлые реплики тянули бы поиск к
//     прошлой теме.
func rewriteCode(al *Aliases, q Query) (dense, bm25 string, expanded []string, note string) {
	text := strings.TrimSpace(q.Text)
	dense, bm25, expanded = al.Queries(text)
	if len(q.Context) == 0 || len(al.Species(text)) > 0 || !continuation(al, text) {
		return dense, bm25, expanded, ""
	}
	for i := len(q.Context) - 1; i >= 0; i-- {
		prev := strings.TrimSpace(q.Context[i])
		sp := al.Species(prev)
		if len(sp) == 0 {
			continue
		}
		pd, pb, pl := al.Queries(prev)
		expanded = append(append(pl, expanded...), "вид из контекста → "+strings.Join(sp, ", "))
		return pd + " " + dense, pb + " " + bm25, expanded, ""
	}
	return joinContext(Query{Text: dense, Context: q.Context}), joinContext(Query{Text: bm25, Context: q.Context}), expanded,
		"вид в контексте не назван — в поиск ушли контекст и вопрос"
}

// continuationWords — местоимения и указательные слова, по которым реплика
// без названного вида считается продолжением («а сколько она весит?»,
// «какой у этого хвост?»).
var continuationWords = map[string]bool{
	"он": true, "она": true, "оно": true, "они": true, "его": true, "ее": true, "их": true, "ему": true, "ей": true, "им": true,
	"него": true, "нее": true, "них": true, "нему": true, "ней": true, "ним": true, "нем": true, "ими": true, "ею": true, "нею": true,
	"этот": true, "эта": true, "эти": true, "этого": true, "этой": true, "этих": true, "этому": true, "этим": true,
	"такой": true, "такая": true, "такие": true, "такого": true, "таких": true,
}

// shortReply — реплика короче стольких слов без животного — продолжение.
const shortReply = 5

// continuation — реплика продолжает разговор: в ней нет животного (вида
// корпуса или другого — animalNouns: «жираф», «ягуар») и есть местоимение
// или указательное слово, она начинается с «а» или короче shortReply слов.
func continuation(al *Aliases, text string) bool {
	toks := tokenize(text)
	if len(toks) == 0 {
		return false
	}
	for _, t := range toks {
		if al.animal(t.stem) {
			return false
		}
	}
	if toks[0].word == "а" {
		return true
	}
	for _, t := range toks {
		if continuationWords[t.word] {
			return true
		}
	}
	return len(toks) < shortReply
}

// aliases — словарь: заданный или из базы при первом вызове.
func (p *Pipeline) aliases(ctx context.Context) (*Aliases, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Aliases != nil {
		return p.Aliases, nil
	}
	a, err := LoadAliases(ctx, p.Searcher.Store)
	if err != nil {
		return nil, fmt.Errorf("словарь названий: %w", err)
	}
	p.Aliases = a
	return a, nil
}

// candidates — dense и BM25 по каждому запросу (BM25 — по QueriesBM25,
// если он задан), объединение по chunk_id: лучший балл и лучший ранг
// каждого вида поиска, сумма RRF по всем спискам. Затем косинус для
// кандидатов, которых не было в dense-выдаче, и отрыв каждого кандидата от
// лучшего косинуса своего запроса (lead).
func (p *Pipeline) candidates(ctx context.Context, c Config, t *Trace) error {
	s := p.Searcher
	byID := map[string]int{}
	// cos[i] — косинусы кандидатов по запросу i (для относительного порога).
	cos := make([]map[string]float64, len(t.Queries))
	add := func(h kb.Hit, mode kb.Mode) {
		i, ok := byID[h.ID]
		if !ok {
			i = len(t.Candidates)
			byID[h.ID] = i
			x := Candidate{Hit: h}
			x.Score, x.Rank = 0, 0
			t.Candidates = append(t.Candidates, x)
		}
		x := &t.Candidates[i]
		x.Rerank += 1 / float64(RRFK+h.Rank) // RRF; для других порядков пересчитывается в order
		switch mode {
		case kb.Dense:
			x.Dense = math.Max(x.Dense, h.Score)
			if x.RankDense == 0 || h.Rank < x.RankDense {
				x.RankDense = h.Rank
			}
		case kb.BM25:
			x.BM25 = math.Max(x.BM25, h.Score)
			if x.RankBM25 == 0 || h.Rank < x.RankBM25 {
				x.RankBM25 = h.Rank
			}
		}
	}
	dense := true
	for i, query := range t.Queries {
		cos[i] = map[string]float64{}
		hits, info, err := s.Search(ctx, query, kb.SearchOptions{Index: c.Index, K: c.K0, Mode: kb.Dense})
		if err != nil {
			return fmt.Errorf("поиск: %w", err)
		}
		if i == 0 {
			t.Info = info
		}
		if info.Mode != kb.Dense {
			// Откат: «dense»-выдача — это уже BM25 по тому же запросу.
			dense = false
			t.Info.Mode, t.Info.Fallback = info.Mode, info.Fallback
			for _, h := range hits {
				add(h, kb.BM25)
			}
			continue
		}
		for _, h := range hits {
			add(h, kb.Dense)
			cos[i][h.ID] = h.Score
		}
		hits, _, err = s.Search(ctx, bm25Query(t, i), kb.SearchOptions{Index: c.Index, K: c.K0, Mode: kb.BM25})
		if err != nil {
			return fmt.Errorf("поиск BM25: %w", err)
		}
		for _, h := range hits {
			add(h, kb.BM25)
		}
	}
	if !dense {
		// Часть запросов могла пройти dense до отката: баллы разных шкал не
		// смешиваются — dense обнуляется у всех.
		for i := range t.Candidates {
			t.Candidates[i].Dense, t.Candidates[i].RankDense = 0, 0
		}
		return nil
	}
	var missing []string
	for _, x := range t.Candidates {
		if x.RankDense == 0 {
			missing = append(missing, x.ID)
		}
	}
	if len(missing) > 0 {
		for i, query := range t.Queries {
			got, err := s.Score(ctx, c.Index, query, missing)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				t.Note = joinNote(t.Note, "косинус BM25-кандидатов не посчитан: "+err.Error())
				break
			}
			for id, v := range got {
				x := &t.Candidates[byID[id]]
				x.Dense = math.Max(x.Dense, v)
				cos[i][id] = v
			}
		}
	}
	leads(t, cos)
	return nil
}

// bm25Query — запрос i для BM25: из QueriesBM25, если он там есть.
func bm25Query(t *Trace, i int) string {
	if i < len(t.QueriesBM25) && strings.TrimSpace(t.QueriesBM25[i]) != "" {
		return t.QueriesBM25[i]
	}
	return t.Queries[i]
}

// leads — отрыв кандидата от лучшего косинуса своего запроса: max по
// запросам (cos_i − лучший cos_i). У одного запроса это Dense − TopDense.
func leads(t *Trace, cos []map[string]float64) {
	tops := make([]float64, len(cos))
	for i, m := range cos {
		for _, v := range m {
			tops[i] = math.Max(tops[i], v)
		}
	}
	for j := range t.Candidates {
		x := &t.Candidates[j]
		x.hasLead = false
		for i, m := range cos {
			v, ok := m[x.ID]
			if !ok {
				continue
			}
			if d := v - tops[i]; !x.hasLead || d > x.lead {
				x.lead, x.hasLead = d, true
			}
		}
	}
}

// order — порядок кандидатов (Final) по реранкингу:
//
//   - без реранкинга — по косинусу dense (это и есть порядок dense-выдачи;
//     при откате — по BM25);
//   - hybrid — по RRF рангов dense и BM25 всех запросов (k = 60);
//   - llm — по оценке модели, при равной — по RRF.
//
// Ничьи — по косинусу, затем по chunk_id: одинаковый вопрос — одинаковый
// порядок.
func order(t *Trace) {
	cs := t.Candidates
	rrf := make(map[string]float64, len(cs))
	second := 0.0
	for i := range cs {
		rrf[cs[i].ID] = cs[i].Rerank
		switch d := cs[i].Dense; {
		case d > t.TopDense:
			second, t.TopDense = t.TopDense, d
		case d > second:
			second = d
		}
	}
	if len(cs) > 1 && t.TopDense > 0 {
		t.Gap = round3(t.TopDense - second)
	}
	var less func(a, b Candidate) bool
	tie := func(a, b Candidate) bool {
		if a.Dense != b.Dense {
			return a.Dense > b.Dense
		}
		if a.BM25 != b.BM25 {
			return a.BM25 > b.BM25
		}
		return a.ID < b.ID
	}
	switch t.Config.Rerank {
	case RerankHybrid:
		less = func(a, b Candidate) bool {
			if a.Rerank != b.Rerank {
				return a.Rerank > b.Rerank
			}
			return tie(a, b)
		}
	case RerankLLM:
		if t.scores == nil {
			// Модель ответила неразборчиво: порядок RRF (заметка — в Note).
			less = func(a, b Candidate) bool {
				if a.Rerank != b.Rerank {
					return a.Rerank > b.Rerank
				}
				return tie(a, b)
			}
			break
		}
		for i := range cs {
			cs[i].Rerank = t.scores[cs[i].ID]
		}
		less = func(a, b Candidate) bool {
			if a.Rerank != b.Rerank {
				return a.Rerank > b.Rerank
			}
			if rrf[a.ID] != rrf[b.ID] {
				return rrf[a.ID] > rrf[b.ID]
			}
			return tie(a, b)
		}
	default:
		for i := range cs {
			cs[i].Rerank = 0
		}
		less = tie
	}
	sort.SliceStable(cs, func(i, j int) bool { return less(cs[i], cs[j]) })
	for i := range cs {
		cs[i].Final = i + 1
	}
}

// finish — фильтр и K1 по уже упорядоченным кандидатам (идемпотентен:
// матрица применяет его к одной выдаче с разными K1, калибровка — с
// разными порогами).
func finish(t *Trace) {
	c := t.Config
	dense := t.Info.Mode == kb.Dense
	topBM25 := 0.0
	for _, x := range t.Candidates {
		topBM25 = math.Max(topBM25, x.BM25)
	}
	half := (c.K1 + 1) / 2
	llmScored := c.Rerank == RerankLLM && t.scores != nil
	var keptSpans []Candidate
	t.Hits = nil
	kept := 0
	for i := range t.Candidates {
		x := &t.Candidates[i]
		x.Kept, x.Reason = false, ""
		if c.Filter {
			switch {
			case dense && len(t.Anchored) == 0 && x.Dense < t.MinScore:
				x.Reason = reasonFloor(t.MinScore)
			case dense && x.behind(t.TopDense) > c.Delta+1e-12 && !exempt(*x, c, half):
				x.Reason = reasonRelative(c.Delta)
			case !dense && x.BM25 < BM25Floor*topBM25:
				x.Reason = reasonBM25()
			case llmScored && x.Rerank == 0:
				x.Reason = ReasonLLM
			}
			if x.Reason == "" && duplicate(*x, keptSpans) {
				x.Reason = ReasonDup
			}
		}
		if x.Reason == "" && kept >= c.K1 {
			x.Reason = ReasonBeyond
		}
		if x.Reason != "" {
			continue
		}
		kept++
		x.Kept = true
		keptSpans = append(keptSpans, *x)
		h := x.Hit
		h.Rank = kept
		h.Score = x.Dense
		if !dense {
			h.Score = x.BM25
		}
		t.Hits = append(t.Hits, h)
	}
	t.Empty = len(t.Hits) == 0
}

// duplicate — кандидат повторяет текст уже оставленного: тот же текст
// (text_sha) или тот же документ и перекрытие не меньше половины его
// длины (окна fixed с перекрытием).
//
// Не «один фрагмент на раздел», как задумывалось в контракте: структурный
// чанкер режет длинный раздел по абзацам без перекрытия, и фрагменты
// одного раздела — разные факты, а не повторы. На dev правило «один на
// раздел» отсекало доказательство: у D17 вступление статьи «Камышовый
// кот» разрезано на два фрагмента, и ответ («хаус» в России) — во втором,
// который выбрасывался как повтор первого.
func duplicate(x Candidate, kept []Candidate) bool {
	for _, k := range kept {
		if x.SHA != "" && x.SHA == k.SHA {
			return true
		}
		if x.DocID != k.DocID || x.End <= x.Start {
			continue
		}
		lo, hi := max(x.Start, k.Start), min(x.End, k.End)
		if hi > lo && float64(hi-lo) >= 0.5*float64(x.End-x.Start) {
			return true
		}
	}
	return false
}

// behind — насколько кандидат хуже лучшего косинуса своего запроса (≥ 0):
// lead, если посчитан, иначе — от общего лучшего.
func (c Candidate) behind(top float64) float64 {
	if c.hasLead {
		return -c.lead
	}
	return top - c.Dense
}

// exempt — кандидат освобождён от относительного порога (см. Search).
func exempt(x Candidate, c Config, half int) bool {
	switch c.Rerank {
	case RerankHybrid:
		return x.RankDense == 0 && x.Final <= half
	case RerankLLM:
		return x.Rerank == 3
	}
	return false
}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

func orText(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// addCost — учёт запроса к модели.
func (p *Pipeline) addCost(t *Trace, u llm.Usage) {
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	t.Usage = t.Usage.Add(u)
	t.Cost = t.Cost.Add(llm.PriceOf(p.model(), u, now))
}

func (p *Pipeline) model() string {
	if strings.TrimSpace(p.Model) == "" {
		return llm.DefaultModel
	}
	return p.Model
}

// normQuery — ключ запроса для отсева повторов.
func normQuery(s string) string { return corpus.Normalize(s) }
