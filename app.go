package main

import (
	"context"
	"fmt"
	"os"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/charter"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/collection"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/compiler"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/extract"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/hub"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/hubapi"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/invariants"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kbapi"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/memory"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/persona"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/profile"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// app — собранное приложение над одним каталогом данных.
type app struct {
	Manager *runs.Manager
	People  *persona.Hook
	Compile *compiler.Hook
	Guide   *charter.Hook
	Local   *tools.Registry
	// Fetcher — HTTP-клиент источников в процессе: стенд считает по нему
	// запросы в сеть.
	Fetcher *tools.Fetcher
	// Sources — путь до источников на ход: в процессе или через MCP-сервер.
	Sources *mcp.Switch
	// Feed — «Интересные факты»: клиент к демону, механизм trivia и REST
	// раздела. Демона может не быть — сборка от этого не зависит.
	Feed *feed.Hook
	// Pipes — конвейер search → summarize → save_to_file у того же демона:
	// REST запуска и следа; исполнитель-модель — та же модель, что у ходов.
	Pipes *feed.Pipelines
	// Hub — реестр MCP-серверов (sources, daemon, notes) и длинный флоу
	// агента через них: REST окна «MCP-серверы». Серверы поднимаются по
	// кнопке «Подключить все» или при первом флоу, не при старте.
	Hub *hubapi.API
	// KB — база знаний (kb.db): REST окна «База знаний». Базы может не
	// быть — окно тогда подскажет, как её собрать.
	KB *kbapi.API
	// Close гасит клиент и процесс MCP-сервера, если он запускался, и
	// соединение с демоном фактов.
	Close func()
}

// wire собирает менеджер ходов: источники, агенты и механизмы вокруг хода.
// Им пользуются и сервер, и стенд -report: стенд обязан гонять ровно то,
// что увидит человек, поэтому сборка одна. wikiBase — адрес Википедии
// вместо заданного окружением (подставные статьи стенда); пусто — как есть.
func wire(o options, registry *features.Registry, defaults features.Set, runner agent.Runner, dataDir, wikiBase string) (app, error) {
	if wikiBase == "" {
		wikiBase = os.Getenv("WIKIPEDIA_BASE_URL")
	}
	fetcher := tools.NewFetcher()
	localTools := tools.LocalTools(fetcher, wikiBase, os.Getenv("GBIF_BASE_URL"))
	local := tools.MustRegistry(localTools...)
	// Путь до источников выбирается на каждый ход по механизмам диалога:
	// mcp выключен — вызов в процессе, включён — через MCP-сервер. Процесс
	// сервера запускается при первом ходе с mcp, не раньше; адреса
	// источников у него те же, что у вызова в процессе.
	launcher := &mcp.Launcher{Path: o.mcpServer}
	if wikiBase != "" {
		launcher.Args = append(launcher.Args, "-wiki-base", wikiBase)
	}
	client := mcp.NewClient(mcp.Options{Dial: launcher.Dial, Want: tools.Fingerprint(localTools)})
	sources := &mcp.Switch{Local: local, Client: client, How: launcher.How}
	data := store.NewDir(dataDir)
	people := &persona.Hook{
		Memory:    memory.NewStore(data),
		Profiles:  profile.NewStore(data),
		Extractor: extract.Extractor{LLM: runner.LLM, Model: runner.Model},
	}
	deps := agents.Deps{Runner: runner, Features: registry, Sources: sources}
	compile := &compiler.Hook{Agents: deps, Store: collection.NewStore(data)}
	// Свод лежит на диске с первого запуска: его читают и правят и без
	// приложения. Судья — тот же клиент и та же модель, без инструментов.
	rules := invariants.NewStore(data)
	if _, err := rules.Ensure(invariants.GuideID); err != nil {
		return app{}, fmt.Errorf("свод справочника: %w", err)
	}
	guide := &charter.Hook{Store: rules, Judge: invariants.Judge{LLM: runner.LLM, Model: runner.Model}}
	// Демон фактов — отдельный процесс; клиент подключается при первом
	// обращении, так что сборка (и стенд, где механизм выключен) демон не
	// трогает.
	trivia := &feed.Hook{Remote: feed.NewRemote(o.facts(), os.Getenv("MCP_TOKEN"), nil)}
	// База знаний открывается до менеджера ходов: её поиск нужен механизму
	// rag. Базы нет — хук откатывает механизм с причиной в журнале.
	know := openKB(o, dataDir)
	retrieval := &rag.Hook{Searcher: know.api.Searcher, Why: know.api.Why}
	// Отвечающий агент и судья окна «База знаний» — та же модель, без
	// инструментов: сравнение «с базой / без базы» идёт на них, а не на
	// ведущем с живыми источниками.
	if know.api.Searcher != nil {
		// Второй этап поиска (v23): общий у вкладки «Поиск» и режимов
		// rag+filter, rag+rewrite, rag+both.
		pipe := &retrieve.Pipeline{Searcher: know.api.Searcher, LLM: runner.LLM, Model: runner.Model}
		know.api.Pipeline = pipe
		retrieval.Pipeline = pipe
		know.api.Answerer = &rag.Answerer{LLM: runner.LLM, Model: runner.Model, Searcher: know.api.Searcher, Pipeline: pipe}
		know.api.Judge = &rag.Judge{LLM: runner.LLM, Model: runner.Model}
	}
	know.api.Questions = o.kbQuestions()
	manager := runs.NewManager(runs.Config{
		Agents:   deps,
		Store:    history.NewStore(data),
		Registry: registry, Defaults: defaults, Timeout: turnTimeout,
		Window: o.window, KeepToolRunes: o.keep,
		// Составитель первым: его ход видит блоки свода, профиля и памяти.
		// Страж свода — раньше человека: соблюдение профиля проверяется по
		// тому ответу, который дойдёт до человека.
		// Факты последними: ведущий и составитель читают инструменты хода
		// уже после всех хуков, так что место на выдачу не влияет, а в
		// журнале заметка о демоне встаёт после событий памяти и профиля.
		Hooks: []runs.Hook{compile, guide, people, trivia, retrieval},
	})
	servers, err := openHub(o, dataDir, runner)
	if err != nil {
		return app{}, err
	}
	return app{Manager: manager, People: people, Compile: compile, Guide: guide, Local: local, Fetcher: fetcher,
		Sources: sources, Feed: trivia,
		Pipes: &feed.Pipelines{Remote: trivia.Remote, LLM: runner.LLM, Model: runner.Model},
		Hub:   servers.api, KB: know.api,
		Close: func() { client.Close(); launcher.Close(); trivia.Remote.Close(); servers.close(); know.close() }}, nil
}

// hubParts — реестр серверов и его REST.
type hubParts struct {
	api   *hubapi.API
	close func()
}

// openHub собирает реестр MCP-серверов по конфигурации (-mcp-config) и
// раздел REST с длинным флоу. Подключения здесь нет — только разбор
// конфигурации, поэтому сборка и стенд -report серверы не трогают.
// stdio-серверам без своего command достаётся бинарник из -mcp-server —
// тот же, что у механизма mcp.
func openHub(o options, dataDir string, runner agent.Runner) (hubParts, error) {
	cfg, err := hub.LoadConfig(o.hubConfig(), hub.Defaults{DataDir: dataDir, DaemonURL: o.facts()})
	if err != nil {
		return hubParts{}, err
	}
	for i := range cfg.Servers {
		if s := &cfg.Servers[i]; s.Transport == hub.TransportStdio && s.Command == "" {
			s.Command = o.mcpServer
		}
	}
	h, err := hub.Open(cfg, nil)
	if err != nil {
		return hubParts{}, err
	}
	api := &hubapi.API{Router: h, Presets: flow.Presets(),
		Run: func(ctx context.Context, p flow.Preset, species string, onCall func(flow.Call)) (flow.Trace, error) {
			return flow.Run(ctx, flow.Config{Runner: runner, Router: h}, p, species, onCall)
		}}
	return hubParts{api: api, close: h.Close}, nil
}
