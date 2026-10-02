package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
)

func init() {
	register("search", "найти в базе знаний: топ чанков с баллами и путём раздела (с -rewrite/-rerank/-filter — второй этап поиска)", runSearch)
}

func runSearch(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("search", `[флаги] "вопрос"`, errOut)
	dbPath := dbFlag(fs)
	index := fs.String("index", "all", "индекс: structure, fixed или all (каждый по очереди)")
	k := fs.Int("k", kb.DefaultK, "сколько чанков показать")
	mode := fs.String("mode", "dense", "режим: dense (с откатом на BM25) или bm25")
	which := embedderFlag(fs)
	pf := newPipeFlags(fs)
	// Вопрос — позиционный аргумент; флаги можно писать и после него.
	var query []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return exitUsage
		}
		if fs.NArg() == 0 {
			break
		}
		query = append(query, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	q := strings.TrimSpace(strings.Join(query, " "))
	if q == "" {
		fmt.Fprintln(errOut, "ошибка: нет вопроса")
		fs.Usage()
		return exitUsage
	}
	if *mode != string(kb.Dense) && *mode != string(kb.BM25) {
		fmt.Fprintf(errOut, "ошибка: неизвестный режим %q (dense, bm25)\n", *mode)
		return exitUsage
	}
	cfg, err := pf.config(*k)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitUsage
	}
	piped := pf.on()
	if piped && *mode == string(kb.BM25) {
		fmt.Fprintln(errOut, "ошибка: -mode bm25 и второй этап поиска несовместимы: конвейер сам ищет dense и BM25")
		return exitUsage
	}
	st, err := openExisting(ctx, *dbPath)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	defer st.Close()

	var ids []string
	if *index == "all" {
		all, err := st.Indexes(ctx)
		if err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitFailed
		}
		for _, ix := range all {
			ids = append(ids, ix.ID)
		}
		if len(ids) == 0 {
			fmt.Fprintln(errOut, "ошибка: в базе нет индексов — соберите kb index")
			return exitFailed
		}
	} else {
		ids = []string{*index}
	}
	s := &kb.Searcher{Store: st}
	if *mode == string(kb.Dense) {
		emb, err := pickEmbedder(ctx, which, errOut)
		if err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitUsage
		}
		s.Embedder = emb
	}
	var p *retrieve.Pipeline
	if piped {
		p = &retrieve.Pipeline{Searcher: s}
		if cfg.Rewrite == retrieve.RewriteLLM || cfg.Rerank == retrieve.RerankLLM {
			llmc, model, err := chatModel()
			if err != nil {
				fmt.Fprintln(errOut, "ошибка:", err)
				return exitUsage
			}
			p.LLM, p.Model = llmc, model
		}
	}
	for i, id := range ids {
		if i > 0 {
			fmt.Fprintln(out)
		}
		if p != nil {
			c := cfg
			c.Index = id
			t, err := p.Search(ctx, retrieve.Query{Text: q, Context: pf.context}, c)
			if err != nil {
				fmt.Fprintln(errOut, "ошибка:", err)
				return exitFailed
			}
			printTrace(out, t, *pf.trace)
			continue
		}
		hits, info, err := s.Search(ctx, q, kb.SearchOptions{Index: id, K: *k, Mode: kb.Mode(*mode)})
		if err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitFailed
		}
		line := fmt.Sprintf("[%s] режим %s", info.Index, info.Mode)
		if info.Embedder != "" {
			line += ", " + info.Embedder
		}
		if info.Fallback != "" {
			line += ", откат: " + info.Fallback
		}
		fmt.Fprintf(out, "%s, %.1f мс\n", line, info.Millis)
		if len(hits) == 0 {
			fmt.Fprintln(out, "  ничего не найдено")
		}
		for _, h := range hits {
			path := h.Section
			if len(h.Path) > 0 {
				path = strings.Join(h.Path, " › ")
			}
			fmt.Fprintf(out, "%2d. %.3f  %-28s %s › %s\n    %s\n", h.Rank, h.Score, h.ID, h.Title, path, snippet(h.Text, 160))
		}
	}
	return exitOK
}

// snippet — начало текста одной строкой.
func snippet(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}
