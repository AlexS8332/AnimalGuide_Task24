package main

import (
	"context"
	"fmt"
	"io"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
)

func init() {
	register("stats", "что в базе знаний: документы, страницы, corpus_sha, индексы", runStats)
}

func runStats(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("stats", "[флаги]", errOut)
	dbPath := dbFlag(fs)
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	st, err := openExisting(ctx, *dbPath)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	defer st.Close()
	m, err := st.Manifest(ctx)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	docs, err := st.Docs(ctx)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	indexes, err := st.Indexes(ctx)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	sources := map[string]int{}
	for _, d := range docs {
		sources[d.Source]++
	}
	fmt.Fprintf(out, "База: %s\n", *dbPath)
	fmt.Fprintf(out, "Документов: %d (%s %d, %s %d), символов %d, страниц %.1f (по %d знаков)\n", len(docs),
		corpus.SourceWikipedia, sources[corpus.SourceWikipedia], corpus.SourceMDD, sources[corpus.SourceMDD],
		m.Chars, m.Pages, corpus.PageChars)
	fmt.Fprintf(out, "corpus_sha: %s\n", m.CorpusSHA)
	if len(indexes) == 0 {
		fmt.Fprintln(out, "Индексов нет: kb index")
		return exitOK
	}
	fmt.Fprintln(out, "Индексы:")
	for _, ix := range indexes {
		emb := ix.Embedder
		if emb == "" {
			emb = "без векторов"
		} else {
			emb = fmt.Sprintf("%s, %d изм.", emb, ix.Dims)
		}
		stale := ""
		if ix.CorpusSHA != m.CorpusSHA {
			stale = "  (собран по другому корпусу — пересоберите)"
		}
		fmt.Fprintf(out, "  %-9s %s: чанков %d, токенов %d, %s, собран %s за %.1f с%s\n", ix.ID, paramsLine(ix.Params),
			ix.Chunks, ix.Tokens, emb, ix.BuiltAt.Local().Format("2006-01-02 15:04"), ix.Seconds, stale)
	}
	if r, ok, err := st.LastReport(ctx); err == nil && ok {
		fmt.Fprintf(out, "Последнее сравнение: %s\n", r.Created.Local().Format("2006-01-02 15:04"))
	}
	return exitOK
}
