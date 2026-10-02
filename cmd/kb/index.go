package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
)

func init() {
	register("index", "собрать индекс базы знаний: корпус → чанки → эмбеддинги → kb.db", runIndex)
}

// defaultDB — kb.db в рабочем каталоге: пересобираемый артефакт, в git не
// попадает. Переменная KB_DB (та же, что у приложения) задаёт другой путь.
const defaultDB = "kb.db"

// envDB — имя переменной окружения с путём к базе.
const envDB = "KB_DB"

// dbFlag — общий флаг -db: по умолчанию KB_DB, иначе kb.db.
func dbFlag(fs *flag.FlagSet) *string {
	def := defaultDB
	if v := strings.TrimSpace(os.Getenv(envDB)); v != "" {
		def = v
	}
	return fs.String("db", def, "файл базы знаний (по умолчанию "+envDB+", иначе "+defaultDB+")")
}

// e5Tokens — предел входа e5 (512 токенов) с запасом на префикс и путь
// раздела в EmbedText: длиннее — модель молча обрежет конец чанка.
const e5Tokens = 480

// embedderFlags — общие флаги выбора эмбеддера index/search/eval: вид
// (-embedder) и адрес (-embed-url). У приложения -embedder — это адрес;
// здесь адрес — -embed-url, а -embedder выбирает вид, потому что kb умеет
// ещё тестовый hash и сборку без векторов.
type embedderFlags struct{ kind, url *string }

func embedderFlag(fs *flag.FlagSet) embedderFlags {
	return embedderFlags{
		kind: fs.String("embedder", "auto",
			"вид эмбеддера: auto (HTTP, если отвечает, иначе без векторов), http, hash (тестовый), none"),
		url: fs.String("embed-url", "",
			"адрес эмбеддера (OpenAI-совместимый /v1/embeddings) для auto и http; пусто — "+embed.EnvBaseURL+", иначе "+embed.DefaultBaseURL),
	}
}

// pickEmbedder переводит флаги в эмбеддер. auto — HTTP по embed.FromEnv
// (адрес — -embed-url, если задан), если Health ok, иначе nil с
// предупреждением; http — HTTP без проверки (ошибка всплывёт при первом
// запросе). nil — без векторов.
func pickEmbedder(ctx context.Context, f embedderFlags, errOut io.Writer) (embed.Embedder, error) {
	fromEnv := func() *embed.HTTP {
		h := embed.FromEnv()
		if u := strings.TrimSpace(*f.url); u != "" {
			h.BaseURL = u
		}
		return h
	}
	switch kind := *f.kind; kind {
	case "none":
		return nil, nil
	case "hash":
		return embed.Hash{}, nil
	case "http":
		return fromEnv(), nil
	case "auto", "":
		h := fromEnv()
		st := h.Health(ctx)
		if st.OK {
			fmt.Fprintf(errOut, "эмбеддер: %s (%s", h.Model(), h.BaseURL)
			if st.Device != "" {
				fmt.Fprintf(errOut, ", %s", st.Device)
			}
			fmt.Fprintln(errOut, ")")
			return h, nil
		}
		fmt.Fprintf(errOut, "предупреждение: эмбеддер %s недоступен: %s\n  %s\n", h.BaseURL, st.Why, st.Hint)
		return nil, nil
	}
	return nil, fmt.Errorf("неизвестный эмбеддер %q (auto, http, hash, none)", *f.kind)
}

// openExisting открывает базу, которая уже должна быть: search/stats/eval
// не создают пустую kb.db на месте опечатки в пути.
func openExisting(ctx context.Context, path string) (*kb.Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("базы %s нет: соберите её командой kb index", path)
	}
	return kb.Open(ctx, path)
}

func runIndex(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("index", "[флаги]", errOut)
	dbPath := dbFlag(fs)
	dir := fs.String("corpus", "corpus", "каталог корпуса")
	strategy := fs.String("strategy", "all", "стратегия: fixed, structure или all (structure, затем fixed по медиане structure)")
	which := embedderFlag(fs)
	size := fs.Int("size", 0, "fixed: размер окна в символах (0 — медиана structure при all, иначе 800)")
	overlap := fs.Int("overlap", -1, "fixed: перекрытие в символах (0 — без перекрытия; по умолчанию 15 % размера)")
	maxLen := fs.Int("max", kb.DefaultMax, "structure: самый длинный чанк в символах")
	minLen := fs.Int("min", kb.DefaultMin, "structure: короче — склеивается с соседом того же родителя")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	switch *strategy {
	case "fixed", "structure", "all":
	default:
		fmt.Fprintf(errOut, "ошибка: неизвестная стратегия %q (fixed, structure, all)\n", *strategy)
		return exitUsage
	}
	emb, err := pickEmbedder(ctx, which, errOut)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitUsage
	}

	docs, m, err := corpus.Load(*dir)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	st, err := kb.Open(ctx, *dbPath)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	defer st.Close()
	if err := st.PutCorpus(ctx, docs, m); err != nil {
		fmt.Fprintln(errOut, "ошибка: корпус в базу:", err)
		return exitFailed
	}
	fmt.Fprintf(out, "Корпус: %d документов, %.1f страниц, corpus_sha %s\n", len(docs), m.Pages, m.CorpusSHA)

	build := func(ch kb.Chunker) (kb.IndexInfo, bool) {
		var cached *embed.Cached
		var e embed.Embedder
		if emb != nil {
			cached = &embed.Cached{E: emb, C: st}
			e = cached
		}
		name := string(ch.Strategy())
		last := time.Time{}
		progress := func(done, total int) {
			if done < total && time.Since(last) < 500*time.Millisecond {
				return
			}
			last = time.Now()
			fmt.Fprintf(errOut, "\r  %s: закодировано %d из %d", name, done, total)
		}
		info, err := st.Build(ctx, ch, e, progress)
		if emb != nil {
			fmt.Fprintln(errOut)
		}
		if err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return info, false
		}
		vecs := "без векторов (только BM25)"
		if cached != nil {
			vecs = fmt.Sprintf("%s, %d изм., кэш: %d попаданий, %d промахов", info.Embedder, info.Dims, cached.Hits, cached.Misses)
		}
		fmt.Fprintf(out, "%-9s %s: чанков %d, токенов %d, %.1f с; %s\n", name, paramsLine(info.Params),
			info.Chunks, info.Tokens, info.Seconds, vecs)
		if chunks, err := st.Chunks(ctx, info.ID, ""); err == nil {
			long := 0
			for _, c := range chunks {
				if c.Tokens > e5Tokens {
					long++
				}
			}
			if long > 0 {
				fmt.Fprintf(errOut, "предупреждение: %s: %d чанков длиннее %d токенов — e5 обрежет их конец\n", name, long, e5Tokens)
			}
		}
		return info, true
	}

	if *strategy == "structure" || *strategy == "all" {
		if _, ok := build(kb.NewStructure(*maxLen, *minLen)); !ok {
			return exitFailed
		}
	}
	if *strategy == "fixed" || *strategy == "all" {
		n := *size
		if n == 0 && *strategy == "all" {
			// Размер окна — медиана структурных чанков: сравниваются
			// границы, а не размер.
			chunks, err := st.Chunks(ctx, string(kb.Structure), "")
			if err != nil {
				fmt.Fprintln(errOut, "ошибка:", err)
				return exitFailed
			}
			n = kb.MedianChars(chunks)
		}
		ov := *overlap
		if ov < 0 && n > 0 {
			ov = n * kb.DefaultOverlapPct / 100
		}
		if _, ok := build(kb.NewFixed(n, ov)); !ok {
			return exitFailed
		}
	}
	fmt.Fprintln(out, "База:", *dbPath)
	return exitOK
}

func paramsLine(p kb.Params) string {
	if p.Size > 0 {
		return fmt.Sprintf("(size %d, overlap %d)", p.Size, p.Overlap)
	}
	return fmt.Sprintf("(max %d, min %d)", p.Max, p.Min)
}
