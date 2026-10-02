package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
)

func init() {
	register("eval", "сравнить стратегии на контрольных вопросах: отчёт markdown (и JSON)", runEval)
}

func runEval(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("eval", "[флаги]", errOut)
	dbPath := dbFlag(fs)
	questions := fs.String("questions", "eval/questions.json", "контрольные вопросы")
	mdPath := fs.String("out", defaultReport, "куда записать отчёт markdown (пусто — не писать); JSON — рядом, с расширением .json")
	jsonPath := fs.String("json", "", "куда записать отчёт JSON (пусто — рядом с -out)")
	bm25 := fs.Bool("bm25", false, "добавить справочные строки BM25")
	budget := fs.Int("budget", kb.DefaultBudget, "бюджет токенов топа для recall при одинаковом объёме")
	which := embedderFlag(fs)
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	qs, err := kb.LoadQuestions(*questions)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitUsage
	}
	st, err := openExisting(ctx, *dbPath)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	defer st.Close()
	emb, err := pickEmbedder(ctx, which, errOut)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitUsage
	}
	o := kb.CompareOptions{Budget: *budget}
	if *bm25 {
		o.Mode = kb.BM25
	}
	r, err := kb.Compare(ctx, &kb.Searcher{Store: st, Embedder: emb}, qs, o)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitFailed
	}
	for _, line := range r.Conclusion {
		fmt.Fprintln(out, "- "+line)
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if why := fallback(r); why != "" && !explicit["out"] {
		// Закоммиченный отчёт — живое сравнение с векторами; отчёт по BM25
		// поверх него выдал бы откат за результат.
		fmt.Fprintf(errOut, "ошибка: векторный поиск не состоялся (%s) — отчёт по BM25 не записан в %s.\n"+
			"  Поднимите эмбеддер (-embed-url, EMBED_BASE_URL) или укажите путь отчёта явно: -out <файл>.\n", why, *mdPath)
		return exitFailed
	}
	if *mdPath != "" && *jsonPath == "" {
		*jsonPath = strings.TrimSuffix(*mdPath, filepath.Ext(*mdPath)) + ".json"
	}
	if *mdPath != "" {
		if err := writeFile(*mdPath, []byte(r.Markdown())); err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitFailed
		}
		fmt.Fprintln(out, "Отчёт:", *mdPath)
	}
	if *jsonPath != "" {
		raw, err := json.MarshalIndent(r, "", "  ")
		if err == nil {
			err = writeFile(*jsonPath, append(raw, '\n'))
		}
		if err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitFailed
		}
		fmt.Fprintln(out, "JSON:", *jsonPath)
	}
	return exitOK
}

// defaultReport — отчёт сравнения в репозитории (рядом — chunking.json).
const defaultReport = "examples/kb/chunking.md"

// fallback — почему отчёт не по векторам: dense откатился на BM25 (нет
// эмбеддера, индекс без векторов, эмбеддер упал); пусто — dense был.
func fallback(r kb.Report) string {
	for _, x := range r.Retrieval {
		if x.Fallback != "" {
			return x.Fallback
		}
	}
	return ""
}

func writeFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

// writeAtomic пишет файл через временный рядом и переименование: прерванная
// запись (Ctrl+C, сбой диска) не оставит набор вопросов обрезанным —
// читатель видит либо старый файл, либо новый целиком.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}
