package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/bench"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/invariants"
)

// legacyDir — памятники прошлых форматов: проверка совместимости И-6.
// Путь от корня репозитория: -report запускается оттуда.
const legacyDir = "testdata/legacy"

// runReport — опыт -report: все выбранные испытания на живой модели, отчёт
// в markdown. Каждое испытание работает в своём каталоге внутри
// <data>/bench/<время>: диалоги, профили и подборки прогона остаются там,
// и по ним можно пройти за любым числом отчёта.
func runReport(o options, registry *features.Registry, defaults features.Set, runner agent.Runner, model string) error {
	trials, err := bench.Select(bench.Trials(), o.trials)
	if err != nil {
		return err
	}
	// Каждое испытание собирает приложение в своём каталоге; процессы
	// MCP-сервера, если испытание их поднимало, гасятся в конце прогона.
	var closers []func()
	defer func() {
		for _, c := range closers {
			c()
		}
	}()
	root := filepath.Join(o.data, "bench", time.Now().Format("20060102-150405"))
	env := &bench.Env{
		Registry: registry, Base: defaults, Root: root, Legacy: legacyDir, Model: model,
		Timeout: turnTimeout, Progress: os.Stdout, LLM: runner.LLM, KB: o.kbPath(o.data),
		Judge: charterJudge{invariants.Judge{LLM: runner.LLM, Model: model}},
		Open: func(dir string, bo bench.Options) (bench.Build, error) {
			// База знаний сборки — та, что задал стенд (И-10): иначе
			// механизм rag искал бы kb.db в каталоге испытания.
			oo := o
			if bo.KB != "" {
				oo.kb = bo.KB
			}
			a, err := wire(oo, registry, defaults, runner, dir, bo.WikiBase)
			if err != nil {
				return bench.Build{}, err
			}
			closers = append(closers, a.Close)
			// И-7 убивает процесс сервера этой сборки посреди хода и
			// сверяет журнал с его счётчиками.
			return bench.Build{Manager: a.Manager, MCP: a.Sources.Client, HTTP: a.Fetcher.Requests}, nil
		},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Printf("Опыт -report: испытаний %d, модель %s, каталог прогона %s\n", len(trials), model, root)
	started := time.Now()
	results := bench.Run(ctx, env, trials)
	md := bench.Markdown(bench.Meta{Model: model, Started: started, Elapsed: time.Since(started), Registry: registry}, results)
	if err := os.MkdirAll(filepath.Dir(o.reportOut), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(o.reportOut, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Println()
	for _, r := range results {
		fmt.Printf("  %s %-40s проверок %d из %d\n", r.ID, r.Title, r.Count(bench.Pass), len(r.Checks))
	}
	fmt.Println("Отчёт: " + o.reportOut)
	return nil
}

// charterJudge — судья стража для И-5: тот же отбор кодом и тот же запрос
// судьи, что проверяют живые ответы, над исходной редакцией свода.
type charterJudge struct{ judge invariants.Judge }

func (charterJudge) Name() string { return "судья свода (как у стража)" }

func (c charterJudge) Violates(ctx context.Context, question, reply string) (bool, string, error) {
	rules := invariants.Preset()
	rev := c.judge.Check(ctx, rules, invariants.Screen(rules, reply), question, reply)
	if rev.Err != "" {
		return false, "", errors.New(rev.Err)
	}
	var why []string
	for _, v := range rev.Broken {
		why = append(why, v.Invariant+": "+v.Why)
	}
	return len(why) > 0, strings.Join(why, "; "), nil
}
