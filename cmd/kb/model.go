package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
)

// Общее у команд, которые зовут модель (ask, qa, probe): ключ и модель из
// окружения, база и поиск — по тем же флагам, что у search и eval.

// chatModel — клиент DeepSeek: ключ DEEPSEEK_API_KEY (в окружении или в
// .env/.env.local — их подхватывает main), адрес DEEPSEEK_BASE_URL, модель
// DEEPSEEK_MODEL или llm.DefaultModel. Ключ никуда не печатается.
func chatModel() (llm.Chatter, string, error) {
	key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if key == "" {
		return nil, "", errors.New("нужен DEEPSEEK_API_KEY — в окружении или в .env/.env.local")
	}
	model := strings.TrimSpace(os.Getenv("DEEPSEEK_MODEL"))
	if model == "" {
		model = llm.DefaultModel
	}
	return llm.NewClient(key, os.Getenv("DEEPSEEK_BASE_URL")), model, nil
}

// answerFlags — флаги отвечающего агента: база, индекс, k, эмбеддер.
type answerFlags struct {
	db, index *string
	k         *int
	emb       embedderFlags
}

func newAnswerFlags(fs *flag.FlagSet) answerFlags {
	return answerFlags{
		db:    dbFlag(fs),
		index: fs.String("index", rag.DefaultIndex, "индекс поиска для режимов с базой: structure или fixed"),
		k:     fs.Int("k", rag.DefaultK, "сколько фрагментов приложить к вопросу в режиме rag (до 10)"),
		emb:   embedderFlag(fs),
	}
}

// answerer собирает отвечающего агента. База нужна только режиму rag:
// norag отвечает и без kb.db. closeKB закрывает базу, если она открывалась.
func (f answerFlags) answerer(ctx context.Context, needKB bool, errOut io.Writer) (a *rag.Answerer, closeKB func(), code int) {
	closeKB = func() {}
	if *f.k <= 0 || *f.k > rag.MaxK {
		fmt.Fprintf(errOut, "ошибка: -k от 1 до %d\n", rag.MaxK)
		return nil, closeKB, exitUsage
	}
	llmc, model, err := chatModel()
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return nil, closeKB, exitUsage
	}
	a = &rag.Answerer{LLM: llmc, Model: model, Index: *f.index, K: *f.k}
	if !needKB {
		return a, closeKB, -1
	}
	st, err := openExisting(ctx, *f.db)
	if err != nil {
		fmt.Fprintln(errOut, "ошибка:", err)
		return nil, closeKB, exitFailed
	}
	if _, err := st.Index(ctx, *f.index); err != nil {
		st.Close()
		fmt.Fprintf(errOut, "ошибка: индекс %s: %v — соберите его командой kb index\n", *f.index, err)
		return nil, closeKB, exitFailed
	}
	emb, err := pickEmbedder(ctx, f.emb, errOut)
	if err != nil {
		st.Close()
		fmt.Fprintln(errOut, "ошибка:", err)
		return nil, closeKB, exitUsage
	}
	a.Searcher = &kb.Searcher{Store: st, Embedder: emb}
	// Конвейер режимов v23 (rag+filter, rag+rewrite, rag+both): бесплатные
	// шаги, модель ему не нужна — платные строки только в kb matrix -paid.
	a.Pipeline = &retrieve.Pipeline{Searcher: a.Searcher, Model: model}
	return a, func() { st.Close() }, -1
}

// parseModes — «norag,rag», «both» → режимы по порядку.
func parseModes(s string) ([]rag.Mode, error) {
	if strings.TrimSpace(s) == "both" {
		return []rag.Mode{rag.NoRAG, rag.RAG}, nil
	}
	var out []rag.Mode
	seen := map[rag.Mode]bool{}
	for _, p := range strings.Split(s, ",") {
		m := rag.Mode(strings.TrimSpace(p))
		switch {
		case m == "":
			continue
		case !m.Known():
			return nil, fmt.Errorf("неизвестный режим %q (norag, rag, rag+filter, rag+rewrite, rag+both; both — norag и rag)", m)
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("не задан ни один режим")
	}
	return out, nil
}

// needsKB — нужна ли база хоть одному режиму.
func needsKB(ms []rag.Mode) bool {
	for _, m := range ms {
		if m.UsesBase() {
			return true
		}
	}
	return false
}

// listFlag — повторяемый флаг (-context "…" -context "…").
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, "; ") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// usd — цена строкой; неизвестный прайс — «?».
func usd(c llm.Cost) string {
	if !c.Known {
		return "?"
	}
	return fmt.Sprintf("$%.4f", c.USD)
}
