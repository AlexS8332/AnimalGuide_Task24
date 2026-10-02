package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/daemon"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

type httpChecker struct{}

func (httpChecker) Check(_ context.Context, sp mdd.Species, _ int) (trivia.Eligibility, error) {
	return trivia.Eligibility{SpeciesID: sp.ID, SciName: sp.SciName, OK: true, WikiLang: "ru", WikiTitle: "Манул",
		GBIFKey: 42, Occurrences: 500}, nil
}

type httpCollector struct{}

func (httpCollector) Collect(_ context.Context, p trivia.Pick, sp mdd.Species) (trivia.Dossier, error) {
	return trivia.Dossier{Pick: p, Species: sp, NameRu: "Манул",
		Materials: []trivia.Material{
			{ID: "S1", Kind: trivia.KindMDD, Title: "MDD", Text: sp.SciName},
			{ID: "S2", Kind: trivia.KindWikipedia, Title: "Манул", URL: "https://ru.wikipedia.org/wiki/Манул",
				Text: "Манул живёт в степях."},
		},
		Observations: trivia.Observations{GBIFKey: 42, Total: 500}, CollectedAt: time.Now()}, nil
}

// httpLLM — редактор пишет три факта, проверяющий подтверждает всё.
func httpLLM() *llmtest.Fake {
	return &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		var resp llm.Response
		switch req.Messages[0].Content {
		case trivia.EditorSystem:
			resp = llmtest.Text(`{"title":"Манул","lead":"Дикая кошка.","facts":[` +
				`{"text":"Манул живёт в степях.","sources":["S2"]},` +
				`{"text":"Манул — кошка.","sources":["S1"]},` +
				`{"text":"У манула густой мех.","sources":["S2"]}]}`)
		case trivia.VerifySystem:
			resp = llmtest.Text(`[{"n":1,"ok":true},{"n":2,"ok":true},{"n":3,"ok":true},{"n":4,"ok":true}]`)
		default:
			return llm.Response{}, errors.New("неожиданный запрос")
		}
		resp.Usage = llm.Usage{Prompt: 1500, Completion: 300, Total: 1800}
		return resp, nil
	}}
}

// Конвейер на HTTP-сервере демона: три инструмента в tools/list с верными
// пометками, цепочка search → summarize → save_to_file проходит по MCP в
// обоих режимах передачи, порча данных по дороге ловится отпечатком.
func TestDaemonServerPipeline(t *testing.T) {
	dir := t.TempDir()
	d, err := daemon.Open(t.Context(), daemon.Config{DataDir: dir, LLM: httpLLM()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.MDD.Replace(t.Context(), mddtest.Sample()); err != nil {
		t.Fatal(err)
	}

	old := pipelineTools
	defer func() { pipelineTools = old }()
	pipelineTools = func(d *daemon.Daemon) []tools.Tool {
		deps := d.PipelineDeps()
		deps.Checker = func(*tools.Fetcher) trivia.Checker { return httpChecker{} }
		deps.Collector = func(*tools.Fetcher) trivia.Collector { return httpCollector{} }
		return pipeline.Tools(deps)
	}
	srv, _ := daemonServer(d, sources{}, slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(srv.HTTPHandler(mcp.HTTPOptions{}))
	defer ts.Close()

	c := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(t.Context(), &sdk.StreamableClientTransport{Endpoint: ts.URL + mcp.MCPPath, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	list, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*sdk.Tool{}
	for _, tl := range list.Tools {
		byName[tl.Name] = tl
	}
	for _, name := range pipeline.ToolNames {
		tl := byName[name]
		if tl == nil {
			t.Fatalf("нет инструмента %s", name)
		}
		readOnly := tl.Annotations != nil && tl.Annotations.ReadOnlyHint
		if readOnly != (name == pipeline.ToolSearch) {
			t.Errorf("%s: только чтение = %v", name, readOnly)
		}
	}
	if byName["facts_latest"] == nil {
		t.Error("инструменты демона пропали")
	}

	call := func(name string, args map[string]any) (pipeline.Envelope, string, bool) {
		t.Helper()
		res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := res.Content[0].(*sdk.TextContent).Text
		if res.IsError {
			return pipeline.Envelope{}, text, true
		}
		var env pipeline.Envelope
		if err := json.Unmarshal([]byte(text), &env); err != nil {
			t.Fatalf("%s: ответ не конверт: %v", name, err)
		}
		return env, text, false
	}

	for _, pass := range []string{pipeline.PassRef, pipeline.PassInline} {
		t.Run(pass, func(t *testing.T) {
			next := func(env pipeline.Envelope) map[string]any {
				if pass == pipeline.PassRef {
					return map[string]any{"ref": env.Digest}
				}
				return map[string]any{"input": env}
			}
			dossier, text, bad := call(pipeline.ToolSearch, map[string]any{"query": "Otocolobus manul"})
			if bad || dossier.Open(pipeline.KindDossier) != nil {
				t.Fatalf("search: %s", text)
			}
			facts, text, bad := call(pipeline.ToolSummarize, next(dossier))
			if bad || facts.Open(pipeline.KindFacts) != nil || facts.Input != dossier.Digest {
				t.Fatalf("summarize: %s", text)
			}
			args := next(facts)
			args["name"] = "manul-" + pass
			file, text, bad := call(pipeline.ToolSaveFile, args)
			if bad || file.Input != facts.Digest {
				t.Fatalf("save_to_file: %s", text)
			}
			var fd pipeline.File
			json.Unmarshal(file.Data, &fd)
			body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(fd.Path)))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			if hex.EncodeToString(sum[:]) != fd.SHA256 ||
				!strings.Contains(string(body), dossier.Digest) || !strings.Contains(string(body), facts.Digest) {
				t.Errorf("файл %s не совпал с конвертом", fd.Path)
			}

			// Порча по дороге: байт в data досье — ошибка отпечатка словами.
			broken := dossier
			broken.Data = json.RawMessage(strings.Replace(string(dossier.Data), "Манул", "Лунам", 1))
			_, text, bad = call(pipeline.ToolSummarize, map[string]any{"input": broken})
			if !bad || !strings.HasPrefix(text, pipeline.ErrDigest.Error()) {
				t.Errorf("испорченное досье: %v %s", bad, text)
			}
		})
	}

	runs, err := d.Runs.Runs(t.Context(), schedule.RunQuery{Job: pipeline.JobPipeline})
	if err != nil || len(runs) != 2 {
		t.Errorf("журнал конвейера: %d записей, %v", len(runs), err)
	}
}
