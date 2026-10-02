package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
)

func init() {
	register("fetch", "снять снимок корпуса: статьи ru-Википедии из списка и документ MDD", runFetch)
}

// sources — corpus/sources.json: что снимает fetch.
type sources struct {
	Schema    int `json:"schema"`
	Wikipedia []struct {
		ID      string          `json:"id"`
		Title   string          `json:"title"`
		Species *corpus.Species `json:"species"`
	} `json:"wikipedia"`
	MDD *struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"mdd"`
}

// readSources читает и проверяет список: id уникальны, заголовки не пусты.
func readSources(path string) (sources, error) {
	var s sources
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("список статей: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("список статей %s: %w", path, err)
	}
	if s.Schema != corpus.Schema {
		return s, fmt.Errorf("список статей %s: схема %d, ожидается %d", path, s.Schema, corpus.Schema)
	}
	seen := map[string]bool{}
	for _, w := range s.Wikipedia {
		if w.ID == "" || strings.TrimSpace(w.Title) == "" {
			return s, fmt.Errorf("список статей: пустой id или title (%q, %q)", w.ID, w.Title)
		}
		if seen[w.ID] {
			return s, fmt.Errorf("список статей: id %s повторяется", w.ID)
		}
		seen[w.ID] = true
	}
	if s.MDD != nil && s.MDD.ID != corpus.MDDDocID {
		return s, fmt.Errorf("список статей: документ MDD должен называться %s, а не %s", corpus.MDDDocID, s.MDD.ID)
	}
	return s, nil
}

// fetchPause — пауза между статьями: снимок — разовая операция, спешить
// незачем, а API Википедии просит не слать запросы залпом.
const fetchPause = time.Second

func runFetch(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := newFlagSet("fetch", "[флаги]", errOut)
	dir := fs.String("corpus", "corpus", "каталог корпуса")
	srcPath := fs.String("sources", "", "список статей (по умолчанию <corpus>/sources.json)")
	mddDB := fs.String("mdd-db", "", "база с загруженным MDD (trivia.db); без флага документ MDD остаётся прежним")
	only := fs.String("only", "", "снять только эти документы (id через запятую), остальные оставить из снимка")
	wikiBase := fs.String("wiki-base", "", "адрес Википедии (по умолчанию https://ru.wikipedia.org)")
	pause := fs.Duration("pause", fetchPause, "пауза между статьями")
	if code := parseFlags(fs, args); code >= 0 {
		return code
	}
	if *srcPath == "" {
		*srcPath = filepath.Join(*dir, corpus.SourcesFile)
	}

	src, err := readSources(*srcPath)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return exitUsage
	}
	known := map[string]bool{}
	for _, w := range src.Wikipedia {
		known[w.ID] = true
	}
	if src.MDD != nil {
		known[src.MDD.ID] = true
	}
	want := map[string]bool{}
	for _, id := range strings.Split(*only, ",") {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if !known[id] {
			fmt.Fprintf(errOut, "ошибка: документа %q нет в списке %s\n", id, *srcPath)
			return exitUsage
		}
		want[id] = true
	}
	selected := func(id string) bool { return len(want) == 0 || want[id] }
	rebuildMDD := src.MDD != nil && *mddDB != "" && selected(src.MDD.ID)
	if *mddDB != "" && !rebuildMDD && src.MDD != nil {
		fmt.Fprintln(errOut, "предупреждение: -mdd-db задан, но документ", src.MDD.ID, "не выбран в -only")
	}

	// Прежний снимок нужен, когда что-то остаётся как было: -only или MDD
	// без -mdd-db. Повреждённый снимок в этом случае — ошибка: иначе правка
	// руками попала бы в новый манифест как законная.
	keepsOld := len(want) > 0 || (src.MDD != nil && !rebuildMDD)
	prev := map[string]corpus.Doc{}
	if keepsOld {
		docs, _, err := corpus.Load(*dir)
		switch {
		case err == nil:
			for _, d := range docs {
				prev[d.ID] = d
			}
		case errors.Is(err, os.ErrNotExist):
			// Снимка ещё нет — снимаем с нуля.
		default:
			fmt.Fprintln(errOut, "ошибка: прежний снимок не прошёл проверку:", err)
			fmt.Fprintln(errOut, "Снимите корпус целиком: kb fetch -mdd-db <trivia.db> (без -only).")
			return exitFailed
		}
	}

	var mddDoc *corpus.Doc
	if rebuildMDD {
		d, err := buildMDD(ctx, *mddDB)
		if err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitFailed
		}
		mddDoc = &d
	}

	f := corpus.Fetcher{Base: *wikiBase, HTTP: &http.Client{Timeout: 60 * time.Second}, UserAgent: corpus.DefaultUserAgent}
	var docs []corpus.Doc
	var fresh = map[string]bool{}
	requests := 0
	for _, w := range src.Wikipedia {
		if !selected(w.ID) {
			if d, ok := prev[w.ID]; ok {
				docs = append(docs, d)
			} else {
				fmt.Fprintf(errOut, "предупреждение: %s нет в прежнем снимке и он не выбран — в корпус не попадёт\n", w.ID)
			}
			continue
		}
		if requests > 0 && *pause > 0 {
			select {
			case <-ctx.Done():
				fmt.Fprintln(errOut, "прервано")
				return exitFailed
			case <-time.After(*pause):
			}
		}
		requests++
		d, err := f.Wikipedia(ctx, w.ID, w.Title, w.Species)
		if err != nil {
			fmt.Fprintln(errOut, "ошибка:", err)
			return exitFailed
		}
		if d.Title != w.Title {
			// Перенаправление: часто это нормально («Снежный барс» → «Ирбис»),
			// но бывает и статья совсем о другом — пусть человек посмотрит.
			fmt.Fprintf(errOut, "заметка: %s: «%s» → «%s» (перенаправление)\n", w.ID, w.Title, d.Title)
		}
		fresh[d.ID] = true
		docs = append(docs, d)
	}
	if src.MDD != nil {
		switch {
		case mddDoc != nil:
			fresh[mddDoc.ID] = true
			docs = append(docs, *mddDoc)
		case prev[src.MDD.ID].ID != "":
			docs = append(docs, prev[src.MDD.ID])
		default:
			fmt.Fprintf(errOut, "предупреждение: документа %s нет — для сборки нужен -mdd-db\n", src.MDD.ID)
		}
	}
	if len(docs) == 0 {
		fmt.Fprintln(errOut, "ошибка: корпус пуст")
		return exitFailed
	}

	m, err := corpus.Save(*dir, docs)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка: сохранение:", err)
		return exitFailed
	}
	if err := corpus.WriteLicense(*dir, docs); err != nil {
		fmt.Fprintln(errOut, "ошибка: лицензия:", err)
		return exitFailed
	}

	for i, e := range m.Entries {
		mark := ""
		if !fresh[e.ID] {
			mark = "  (из прежнего снимка)"
		}
		rev := "-"
		if e.RevID != 0 {
			rev = fmt.Sprint(e.RevID)
		}
		fmt.Fprintf(out, "%-24s %-48s rev %-10s %7d симв.%s\n", e.ID, docs[i].Title, rev, e.Chars, mark)
	}
	fmt.Fprintf(out, "Документов: %d, символов: %d, страниц: %.1f (по %d знаков), corpus_sha: %s\n",
		len(m.Entries), m.Chars, m.Pages, corpus.PageChars, m.CorpusSHA)
	return exitOK
}

// buildMDD открывает базу справочника и собирает документ MDD. Базу не
// создаёт: несуществующий путь — ошибка, а не пустая новая база.
func buildMDD(ctx context.Context, path string) (corpus.Doc, error) {
	if _, err := os.Stat(path); err != nil {
		return corpus.Doc{}, fmt.Errorf("база MDD: %w", err)
	}
	conn, err := db.Open(ctx, path)
	if err != nil {
		return corpus.Doc{}, err
	}
	defer conn.Close()
	st, err := mdd.NewSQLite(ctx, conn)
	if err != nil {
		return corpus.Doc{}, err
	}
	return corpus.MDDDoc(ctx, st)
}
