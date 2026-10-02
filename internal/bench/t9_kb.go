package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
)

// laneKB — единственная дорожка И-9: индекс, а не диалог.
const laneKB = "индекс"

// Пути по умолчанию — от корня репозитория: -report запускается оттуда,
// как и для testdata/legacy у И-6.
const (
	DefaultCorpusDir = "corpus"
	DefaultQuestions = "eval/questions.json"
)

// KB — И-9: «Индекс базы знаний». Офлайновое, как И-8, только без модели
// вовсе: стенд приложения не нужен, испытание собирает обе стратегии
// чанкинга во временной kb.db в каталоге прогона и проверяет индекс и
// разметку. Модель не зовётся — испытание бесплатно.
//
// Жёсткие проверки: корпус ≥ 30 страниц и сходится с манифестом; у всех
// чанков есть source, title, section, chunk_id; chunk_id уникальны; ни
// один чанк structure не захватил заголовок чужого раздела (текст
// подразделов не дублируется); две сборки дают одинаковые chunk_id и
// text_sha; разметка вопросов проходит Verify; вывод сравнения назван
// числами. Не хуже ли structure, чем fixed, по recall@5 — проверка
// «не определено», если нет: тогда вывод с числами — в заметках.
type KB struct {
	// CorpusDir, Questions — пусто → DefaultCorpusDir, DefaultQuestions.
	CorpusDir string
	Questions string
	// Embedder — nil → embed.FromEnv, если сайдкар отвечает; иначе индекс
	// без векторов, и проверки векторного поиска — «не определено».
	Embedder embed.Embedder
	// CacheDB — общий кэш векторов: kb.db, из kb_embed_cache которой
	// векторы берутся только на чтение (новые пишутся во временную базу
	// прогона). Пусто → DefaultCacheDB, если файл есть; "-" — без общего
	// кэша. Без него прогон с настоящей моделью кодирует корпус с нуля
	// (минуты на CPU).
	CacheDB string
}

// DefaultCacheDB — kb.db в корне репозитория (её собирает kb index).
const DefaultCacheDB = "kb.db"

// NewKB — И-9 с настройками по умолчанию.
func NewKB() *KB { return &KB{} }

func (t *KB) ID() string    { return "И-9" }
func (t *KB) Title() string { return "Индекс базы знаний" }

func (t *KB) Run(ctx context.Context, s *Stand, r *Result) error {
	dir := orDefault(t.CorpusDir, DefaultCorpusDir)
	qpath := orDefault(t.Questions, DefaultQuestions)
	r.Goal = "индекс корпуса воспроизводим, у чанков полные метаданные, стратегии сравнены числами"
	r.Lanes = append(r.Lanes, LaneInfo{Name: laneKB, Note: "kb.db во временном каталоге: structure и fixed по медиане structure",
		Diff: "не диалог: индекс и поиск без модели"})

	// 1. Корпус: Load сверяет sha256 каждого файла и corpus_sha с манифестом.
	docs, m, err := corpus.Load(dir)
	r.yes("корпус загружен, sha файлов и corpus_sha сходятся с манифестом", laneKB, err == nil, errText(err))
	if err != nil {
		return nil
	}
	pages := m.Pages
	st := Pass
	if pages < 30 {
		st = Fail
	}
	r.check(Check{What: "объём корпуса", Want: "≥ 30 страниц", Lane: laneKB,
		Got: fmt.Sprintf("%.1f страниц, %d документов", pages, len(docs)), Status: st})
	r.metric("corpus_sha", laneKB, "%s", m.CorpusSHA)

	// 2. Разметка вопросов.
	qs, err := kb.LoadQuestions(qpath)
	if err != nil {
		r.yes("разметка вопросов проходит проверку", laneKB, false, err.Error())
		return nil
	}
	var bad []string
	for _, e := range qs.Verify(docs) {
		bad = append(bad, e.Error())
	}
	r.zero("нарушения разметки вопросов (Verify)", laneKB, len(bad), bad)

	// 3. Эмбеддер.
	emb := t.Embedder
	why := ""
	if emb == nil {
		h := embed.FromEnv()
		if hs := h.Health(ctx); hs.OK {
			emb = h
		} else {
			why = "эмбеддер недоступен: " + hs.Why
		}
	}
	if emb != nil {
		r.metric("эмбеддер", laneKB, "%s", emb.Model())
	} else {
		r.note("Без эмбеддера: индексы без векторов, поиск — BM25 (%s).", why)
	}

	// 4. Две сборки обеих стратегий.
	path := filepath.Join(s.Dir(), "kb.db")
	if err := os.MkdirAll(s.Dir(), 0o755); err != nil {
		return err
	}
	store, err := kb.Open(ctx, path)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.PutCorpus(ctx, docs, m); err != nil {
		return err
	}
	var cached *embed.Cached
	if emb != nil {
		cached = &embed.Cached{E: emb, C: store}
		if shared := t.sharedCache(ctx, r, emb.Model()); shared != nil {
			defer shared.Close()
			cached.C = embed.Layered{Own: store, Shared: shared}
		}
		emb = cached
	}
	build := func() (map[string][]kb.Chunk, error) {
		out := map[string][]kb.Chunk{}
		if _, err := store.Build(ctx, kb.NewStructure(0, 0), emb, nil); err != nil {
			return nil, err
		}
		sc, err := store.Chunks(ctx, string(kb.Structure), "")
		if err != nil {
			return nil, err
		}
		size := kb.MedianChars(sc)
		if _, err := store.Build(ctx, kb.NewFixed(size, size*kb.DefaultOverlapPct/100), emb, nil); err != nil {
			return nil, err
		}
		fc, err := store.Chunks(ctx, string(kb.Fixed), "")
		if err != nil {
			return nil, err
		}
		out[string(kb.Structure)], out[string(kb.Fixed)] = sc, fc
		return out, nil
	}
	s.env.logf("  И-9: сборка индексов (%s)", orDefault(modelOf(emb), "без векторов"))
	first, err := build()
	if err != nil {
		return fmt.Errorf("И-9: сборка: %w", err)
	}
	if cached != nil {
		r.metric("кэш эмбеддингов (первая сборка)", laneKB, "%d попаданий, %d промахов", cached.Hits, cached.Misses)
	}
	second, err := build()
	if err != nil {
		return fmt.Errorf("И-9: повторная сборка: %w", err)
	}

	// 5. Метаданные, уникальность, дубли, детерминизм.
	byDoc := map[string]corpus.Doc{}
	for _, d := range docs {
		byDoc[d.ID] = d
	}
	seen := map[string]bool{}
	var noMeta, dups, diff []string
	total := 0
	for _, id := range []string{string(kb.Structure), string(kb.Fixed)} {
		a, b := first[id], second[id]
		total += len(a)
		for _, c := range a {
			if c.ID == "" || c.Source == "" || c.Title == "" || c.Section == "" || c.DocID == "" {
				noMeta = append(noMeta, c.ID)
			}
			if seen[c.ID] {
				dups = append(dups, c.ID)
			}
			seen[c.ID] = true
		}
		if len(a) != len(b) {
			diff = append(diff, fmt.Sprintf("%s: %d и %d чанков", id, len(a), len(b)))
			continue
		}
		for i := range a {
			if a[i].ID != b[i].ID || a[i].SHA != b[i].SHA {
				diff = append(diff, a[i].ID)
			}
		}
	}
	r.zero(fmt.Sprintf("чанки без source/title/section/chunk_id (из %d)", total), laneKB, len(noMeta), noMeta)
	r.zero("повторы chunk_id", laneKB, len(dups), dups)
	var foreign []string
	perDoc := map[string][]kb.Chunk{}
	for _, c := range first[string(kb.Structure)] {
		perDoc[c.DocID] = append(perDoc[c.DocID], c)
	}
	for id, cs := range perDoc {
		foreign = append(foreign, kb.ForeignHeadings(byDoc[id], cs)...)
	}
	r.zero("чанки structure с заголовком чужого раздела (дубли текста подразделов)", laneKB, len(foreign), foreign)
	r.zero("расхождения chunk_id и text_sha двух сборок", laneKB, len(diff), diff)

	// 6. Сравнение стратегий.
	rep, err := kb.Compare(ctx, &kb.Searcher{Store: store, Embedder: emb}, qs, kb.CompareOptions{Mode: kb.BM25})
	if err != nil {
		return fmt.Errorf("И-9: сравнение: %w", err)
	}
	for _, x := range rep.Stats {
		r.metric("чанков", x.Index, "%d (p50 %d, p95 %d токенов)", x.Chunks, x.P50, x.P95)
		r.metric("на стыке разделов / разрезано разделов", x.Index, "%.1f %% / %.1f %%", 100*x.MixedShare, 100*x.SplitSections)
		r.metric("перекрытие / оборвано посреди предложения", x.Index, "%.1f %% / %.1f %%", 100*x.OverlapShare, 100*x.MidSentence)
		r.metric("доказательство целиком в одном чанке", x.Index, "%.1f %% из %d", 100*x.WholeEvidence, x.Evidence)
		r.metric("сборка", x.Index, "%.1f с, %d КБ", x.BuildSeconds, x.Bytes/1024)
	}
	for _, x := range rep.Retrieval {
		lane := x.Index + " " + string(x.Mode)
		r.metric("recall@1/3/5, MRR — "+x.Split, lane, "%.2f / %.2f / %.2f, %.2f (вопросов %d, разорвано доказательств %.0f %%; все доказательства в топ-5 — %.2f)",
			x.Recall[1], x.Recall[3], x.Recall[5], x.MRR, x.N, 100*x.BrokenEvidence, x.RecallAll5)
	}
	named := len(rep.Conclusion) > 0 && strings.ContainsAny(rep.Conclusion[0], "0123456789")
	r.yes("вывод сравнения назван числами", laneKB, named, strings.Join(firstN(rep.Conclusion, 1), ""))
	for _, c := range rep.Conclusion {
		r.note("%s", c)
	}

	what := "structure не хуже fixed по recall@5 (dev+test)"
	// Без эмбеддера строк dense нет вовсе (dense откатился на BM25 —
	// строки помечены bm25): берём их, иначе в причине было бы «0.00
	// против 0.00».
	mode := kb.Dense
	if emb == nil {
		mode = kb.BM25
	}
	sR, fR, n := recallAt5(rep, string(kb.Structure), mode), recallAt5(rep, string(kb.Fixed), mode), 0
	for _, x := range rep.Retrieval {
		if x.Index == string(kb.Structure) && x.Mode == kb.Dense {
			n += x.N
		}
	}
	got := fmt.Sprintf("%.2f против %.2f", sR, fR)
	switch {
	case emb == nil:
		r.pending(what, "≥", laneKB, "векторного поиска нет ("+why+"); BM25: "+got)
	case n == 0:
		r.pending(what, "≥", laneKB, "dense откатился на BM25 — см. заметки")
	case sR+1e-9 >= fR:
		r.check(Check{What: what, Want: "≥", Lane: laneKB, Got: got, Status: Pass})
	default:
		r.check(Check{What: what, Want: "≥", Lane: laneKB, Got: got, Status: Pending,
			Note: "fixed лучше — честный вывод в заметках"})
	}
	return nil
}

// sharedCache — общий кэш векторов (CacheDB) только для чтения; nil — его
// нет или он не открылся (это не ошибка испытания: кодирование просто
// пойдёт в модель — об этом заметка).
func (t *KB) sharedCache(ctx context.Context, r *Result, model string) *kb.Store {
	path := t.CacheDB
	switch path {
	case "-":
		return nil
	case "":
		path = DefaultCacheDB
		if _, err := os.Stat(path); err != nil {
			return nil
		}
	}
	shared, err := kb.OpenCache(ctx, path)
	if err != nil {
		r.note("Общий кэш эмбеддингов %s не открылся (%v): векторы кодируются заново.", path, err)
		return nil
	}
	n, err := shared.CacheSize(ctx, model)
	if err != nil {
		shared.Close()
		r.note("Общий кэш эмбеддингов %s не читается (%v): векторы кодируются заново.", path, err)
		return nil
	}
	r.metric("общий кэш эмбеддингов (только чтение)", laneKB, "%s: %d векторов %s", path, n, model)
	return shared
}

// recallAt5 — recall@5 режима по dev+test вместе (взвешенно по числу
// вопросов).
func recallAt5(rep kb.Report, index string, mode kb.Mode) float64 {
	hit, n := 0.0, 0
	for _, x := range rep.Retrieval {
		if x.Index == index && x.Mode == mode {
			hit += x.Recall[5] * float64(x.N)
			n += x.N
		}
	}
	if n == 0 {
		return 0
	}
	return hit / float64(n)
}

func modelOf(e embed.Embedder) string {
	if e == nil {
		return ""
	}
	return e.Model()
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
