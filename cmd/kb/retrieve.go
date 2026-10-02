package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
)

// Второй этап поиска (v23): флаги kb search, команды calibrate и matrix.

func init() {
	register("calibrate", "подобрать порог релевантности на dev+out: таблица «порог → recall dev, пусто на out» (в индекс — только с -write)", runCalibrate)
	register("matrix", "сравнить режимы поиска base/filter/rewrite/both/hybrid × K1 × наборы → examples/rag/filter.md (и JSON)", runMatrix)
}

// Отчёты в репозитории.
const (
	defaultMatrix    = "examples/rag/filter.md"
	defaultCalibrate = "examples/rag/calibrate.md"
)

// pipeFlags — флаги второго этапа поиска у kb search.
type pipeFlags struct {
	rewrite, rerank *string
	filter, trace   *bool
	k0              *int
	minScore, delta *float64
	context         listFlag
}

func newPipeFlags(fs *flag.FlagSet) *pipeFlags {
	pf := &pipeFlags{
		rewrite:  fs.String("rewrite", "", "переписать запрос: code (синонимы и вид из контекста) или llm (модель, платно)"),
		rerank:   fs.String("rerank", "", "реранкинг кандидатов: hybrid (RRF dense и BM25) или llm (модель, платно)"),
		filter:   fs.Bool("filter", false, "фильтр релевантности: порог косинуса, «не хуже лучшего на delta», отсев повторов текста"),
		trace:    fs.Bool("trace", false, "показать всех кандидатов с баллами dense/BM25/RRF и причиной отсева"),
		k0:       fs.Int("k0", retrieve.DefaultK0, "кандидатов до фильтра"),
		minScore: fs.Float64("min-score", 0, "абсолютный порог косинуса (0 — из индекса, иначе умолчание)"),
		delta:    fs.Float64("delta", 0, fmt.Sprintf("относительный порог (0 — %.2f)", retrieve.DefaultDelta)),
	}
	fs.Var(&pf.context, "context", "предыдущая реплика человека (для вопроса-продолжения; можно повторять)")
	return pf
}

// on — задан ли хоть один флаг второго этапа.
func (f *pipeFlags) on() bool {
	return *f.rewrite != "" || *f.rerank != "" || *f.filter || *f.trace || len(f.context) > 0
}

func (f *pipeFlags) config(k1 int) (retrieve.Config, error) {
	c := retrieve.Config{K0: *f.k0, K1: k1, Filter: *f.filter, MinScore: *f.minScore, Delta: *f.delta,
		Rewrite: retrieve.Rewrite(*f.rewrite), Rerank: retrieve.Rerank(*f.rerank)}
	switch c.Rewrite {
	case retrieve.RewriteNone, retrieve.RewriteCode, retrieve.RewriteLLM:
	default:
		return c, fmt.Errorf("неизвестный -rewrite %q (code, llm)", *f.rewrite)
	}
	switch c.Rerank {
	case retrieve.RerankNone, retrieve.RerankHybrid, retrieve.RerankLLM:
	default:
		return c, fmt.Errorf("неизвестный -rerank %q (hybrid, llm)", *f.rerank)
	}
	if c.K0 < 1 {
		return c, fmt.Errorf("-k0 ≥ 1")
	}
	return c, nil
}

// printTrace — путь поиска: запросы, итог, с trace — все кандидаты.
func printTrace(out io.Writer, t retrieve.Trace, all bool) {
	line := fmt.Sprintf("[%s] %s; режим %s", t.Info.Index, t.Config.Describe(), t.Info.Mode)
	if t.Info.Embedder != "" {
		line += ", " + t.Info.Embedder
	}
	if t.Info.Fallback != "" {
		line += ", откат: " + t.Info.Fallback
	}
	fmt.Fprintf(out, "%s, %d мс\n", line, t.Millis)
	if t.Rewritten != t.Original {
		fmt.Fprintf(out, "запрос: %s\n", t.Rewritten)
	}
	if len(t.Queries) > 1 || (len(t.Queries) == 1 && t.Queries[0] != t.Rewritten) {
		fmt.Fprintf(out, "в поиск: %s\n", strings.Join(t.Queries, " | "))
	}
	for _, e := range t.Expanded {
		fmt.Fprintf(out, "раскрыто: %s\n", e)
	}
	if len(t.QueriesBM25) > 0 {
		fmt.Fprintf(out, "в BM25: %s\n", strings.Join(t.QueriesBM25, " | "))
	}
	if t.Config.Filter && t.Info.Mode == kb.Dense {
		floor := fmt.Sprintf("порог %.3f (%s)", t.MinScore, orDash(t.MinScoreFrom))
		if len(t.Anchored) > 0 {
			floor += " не применяется: вид назван в запросе (" + strings.Join(t.Anchored, ", ") + ")"
		}
		fmt.Fprintf(out, "%s, лучший косинус %.3f, отрыв от второго %.3f, «не хуже лучшего на %.2f» → %.3f\n", floor, t.TopDense, t.Gap, t.Config.Delta, t.TopDense-t.Config.Delta)
	}
	if t.Note != "" {
		fmt.Fprintf(out, "заметка: %s\n", t.Note)
	}
	if t.Usage.Total > 0 {
		fmt.Fprintf(out, "модель: %d токенов, %s\n", t.Usage.Total, usd(t.Cost))
	}
	cut := 0
	for _, c := range t.Candidates {
		if c.FilterCut() {
			cut++
		}
	}
	fmt.Fprintf(out, "кандидатов %d, отсечено фильтром %d, в выдаче %d\n", len(t.Candidates), cut, len(t.Hits))
	if t.Empty {
		fmt.Fprintln(out, "  ничего не осталось: в базе ответа, вероятно, нет")
	}
	for _, h := range t.Hits {
		fmt.Fprintf(out, "%2d. %.3f  %-28s %s › %s\n    %s\n", h.Rank, h.Score, h.ID, h.Title, pathText(h.Chunk), snippet(h.Text, 160))
	}
	if !all {
		return
	}
	fmt.Fprintln(out, "\nкандидаты (по порядку после реранкинга):")
	fmt.Fprintln(out, "  #  dense   d#  bm25   b#  rerank   chunk_id                     судьба")
	for _, c := range t.Candidates {
		fate := "в выдаче"
		if !c.Kept {
			fate = "✗ " + c.Reason
		}
		fmt.Fprintf(out, "%3d  %.3f  %3s  %.3f  %3s  %.4f  %-28s %s\n", c.Final, c.Dense, rankStr(c.RankDense), c.BM25, rankStr(c.RankBM25),
			c.Rerank, c.ID, fate)
	}
}

func rankStr(n int) string {
	if n == 0 {
		return "—"
	}
	return strconv.Itoa(n)
}

func pathText(c kb.Chunk) string {
	if len(c.Path) > 0 {
		return strings.Join(c.Path, " › ")
	}
	return c.Section
}

// searchPipeline — база, поиск с эмбеддером и конвейер для calibrate/matrix.
func searchPipeline(ctx context.Context, dbPath string, which embedderFlags, errOut io.Writer) (*retrieve.Pipeline, func(), int) {
	st, err := openExisting(ctx, dbPath)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return nil, func() {}, exitFailed
	}
	emb, err := pickEmbedder(ctx, which, errOut)
	if err != nil {
		st.Close()
		fmt.Fprintln(errOut, "ошибка:", err)
		return nil, func() {}, exitUsage
	}
	return &retrieve.Pipeline{Searcher: &kb.Searcher{Store: st, Embedder: emb}}, func() { st.Close() }, -1
}

func runCalibrate(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("calibrate", "[флаги]", errOut)
	dbPath := dbFlag(fs)
	questions := fs.String("questions", "eval/questions.json", "контрольные вопросы")
	index := fs.String("index", rag.DefaultIndex, "индекс")
	delta := fs.Float64("delta", retrieve.DefaultDelta, "относительный порог фильтра")
	maxDrop := fs.Float64("max-drop", 0.05, "насколько может упасть recall на dev против выдачи без фильтра")
	write := fs.Bool("write", false, "записать выбранный порог в индекс (по умолчанию — только показать)")
	mdPath := fs.String("out", "", "куда записать таблицу markdown (пусто — не писать; в репозитории — "+defaultCalibrate+"); JSON — рядом")
	which := embedderFlag(fs)
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	qs, err := kb.LoadQuestions(*questions)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitUsage
	}
	p, closeKB, code := searchPipeline(ctx, *dbPath, which, errOut)
	if code >= 0 {
		return code
	}
	defer closeKB()
	cal, err := retrieve.Calibrate(ctx, p, qs, *index, *delta, *maxDrop, *write)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	fmt.Fprint(out, cal.Markdown())
	if *mdPath != "" {
		if code := writeReport(*mdPath, cal.Markdown(), cal, out, errOut); code >= 0 {
			return code
		}
	}
	return exitOK
}

func runMatrix(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("matrix", "[флаги]", errOut)
	dbPath := dbFlag(fs)
	questions := fs.String("questions", "eval/questions.json", "контрольные вопросы")
	index := fs.String("index", rag.DefaultIndex, "индекс")
	k1s := fs.String("k1", "3,5,8", "итоговых фрагментов через запятую")
	k0 := fs.Int("k0", retrieve.DefaultK0, "кандидатов до фильтра")
	splits := fs.String("splits", "dev,test,out", "наборы через запятую")
	paid := fs.Bool("paid", false, "добавить платные строки llm-rewrite и llm-rerank (запросы к модели)")
	rrfOnly := fs.Bool("rrf", false, "добавить строку rrf-only (только RRF, без фильтра и переписывания)")
	minScore := fs.Float64("min-score", 0, "абсолютный порог (0 — из индекса, иначе умолчание)")
	delta := fs.Float64("delta", 0, "относительный порог (0 — умолчание)")
	mdPath := fs.String("out", defaultMatrix, "куда записать отчёт markdown (пусто — не писать); JSON — рядом, с расширением .json")
	which := embedderFlag(fs)
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	var ks []int
	for _, s := range strings.Split(*k1s, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		k, err := strconv.Atoi(s)
		if err != nil || k < 1 {
			fmt.Fprintf(errOut, "ошибка: -k1: %q\n", s)
			return exitUsage
		}
		ks = append(ks, k)
	}
	var sp []string
	for _, s := range strings.Split(*splits, ",") {
		if s = strings.TrimSpace(s); s != "" {
			sp = append(sp, s)
		}
	}
	qs, err := kb.LoadQuestions(*questions)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitUsage
	}
	p, closeKB, code := searchPipeline(ctx, *dbPath, which, errOut)
	if code >= 0 {
		return code
	}
	defer closeKB()
	if *paid {
		llmc, model, err := chatModel()
		if err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitUsage
		}
		p.LLM, p.Model = llmc, model
	}
	configs := retrieve.Presets(*paid)
	if *rrfOnly {
		configs = append(configs, retrieve.Named{Name: "rrf-only", Config: retrieve.Config{Rerank: retrieve.RerankHybrid}})
	}
	for i := range configs {
		c := &configs[i].Config
		c.Index, c.K0 = *index, *k0
		if c.Filter {
			c.MinScore, c.Delta = *minScore, *delta
		}
	}
	fmt.Fprintf(errOut, "kb matrix: индекс %s, K1 %v, наборы %s, конфигураций %d\n", *index, ks, strings.Join(sp, ","), len(configs))
	m, err := retrieve.RunMatrix(ctx, p, qs, configs, ks, sp)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	for _, line := range m.Conclusion {
		fmt.Fprintln(out, "- "+line)
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if m.Fallback != "" && !explicit["out"] {
		// Закоммиченный отчёт — с векторным поиском (порог — косинус):
		// отчёт по BM25 поверх него выдал бы откат за результат.
		fmt.Fprintf(errOut, "ошибка: векторный поиск не состоялся (%s) — отчёт не записан в %s.\n"+
			"  Поднимите эмбеддер (-embed-url, EMBED_BASE_URL) или укажите путь отчёта явно: -out <файл>.\n", m.Fallback, *mdPath)
		return exitFailed
	}
	if *mdPath != "" {
		if code := writeReport(*mdPath, m.Markdown(), m, out, errOut); code >= 0 {
			return code
		}
	}
	return exitOK
}

// writeReport — markdown и JSON рядом; -1 — записано.
func writeReport(mdPath, md string, v any, out, errOut io.Writer) int {
	if err := writeFile(mdPath, []byte(md)); err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	fmt.Fprintln(out, "Отчёт:", mdPath)
	jsonPath := strings.TrimSuffix(mdPath, filepath.Ext(mdPath)) + ".json"
	raw, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		err = writeFile(jsonPath, append(raw, '\n'))
	}
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	fmt.Fprintln(out, "JSON:", jsonPath)
	return -1
}
