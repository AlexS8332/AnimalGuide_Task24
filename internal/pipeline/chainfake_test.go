package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// chainServer — подставной демон с тремя инструментами конвейера. Протокол
// конвертов честный, как у настоящих инструментов: search отдаёт досье,
// summarize принимает ровно один из input и ref, проверяет вид и отпечаток
// и отдаёт факты с Input = digest входа, save_to_file — файл с цепочкой.
// Выходы держит в памяти для ref. Модели и сети нет.
type chainServer struct {
	mu    sync.Mutex
	store map[string]pipeline.Envelope
	calls []string
	args  []json.RawMessage

	// tamper — порча ответа после Seal: так тест изображает сервер,
	// который испортил данные или обработал не то.
	tamper func(tool string, env *pipeline.Envelope)
	// fail — ошибка инструмента по имени.
	fail map[string]error
}

func newChainServer() *chainServer {
	return &chainServer{store: map[string]pipeline.Envelope{}, fail: map[string]error{}}
}

// chainArgs — аргументы всех трёх инструментов.
type chainArgs struct {
	Query  string             `json:"query"`
	Random bool               `json:"random"`
	Input  *pipeline.Envelope `json:"input"`
	Ref    string             `json:"ref"`
	Format string             `json:"format"`
}

const chainToolCost = 0.002 // расход summarize на «модель»

func (s *chainServer) handle(_ context.Context, tool string, raw json.RawMessage) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, tool)
	s.args = append(s.args, append(json.RawMessage(nil), raw...))
	failErr := s.fail[tool]
	s.mu.Unlock()
	if failErr != nil {
		return "", failErr
	}
	var a chainArgs
	if err := tools.ParseArgs(raw, &a); err != nil {
		return "", err
	}
	var env pipeline.Envelope
	var err error
	switch tool {
	case pipeline.ToolSearch:
		env, err = s.search(a)
	case pipeline.ToolSummarize:
		env, err = s.summarize(a)
	case pipeline.ToolSaveFile:
		env, err = s.save(a)
	default:
		return "", fmt.Errorf("нет инструмента %s", tool)
	}
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.store[env.Digest] = env
	tamper := s.tamper
	s.mu.Unlock()
	if tamper != nil {
		tamper(tool, &env)
	}
	out, err := json.Marshal(env)
	return string(out), err
}

func (s *chainServer) search(a chainArgs) (pipeline.Envelope, error) {
	q := strings.TrimSpace(a.Query)
	if q == "" && !a.Random {
		return pipeline.Envelope{}, errors.New("нужен query или random")
	}
	resolved := "русская Википедия: Манул → Otocolobus manul"
	if a.Random {
		resolved = "случайный вид"
	}
	d := pipeline.Dossier{Query: q, Resolved: resolved, Dossier: trivia.Dossier{
		Species: mdd.Species{ID: 1001, SciName: "Otocolobus manul", CommonName: "Pallas's Cat"},
		NameRu:  "Манул",
		Materials: []trivia.Material{
			{ID: "S1", Kind: "mdd", Title: "MDD: Otocolobus manul", Text: "Семейство Felidae."},
			{ID: "S2", Kind: "wikipedia", Title: "Манул", Text: "Манул — дикая кошка Центральной Азии."},
		},
		CollectedAt: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		Took:        1500 * time.Millisecond,
	}}
	env, err := pipeline.Seal(pipeline.KindDossier, d, "")
	env.Summary = "Манул: 2 материала"
	return env, err
}

// in — вход шага по правилам протокола.
func (s *chainServer) in(a chainArgs, kind string) (pipeline.Envelope, error) {
	if (a.Input == nil) == (a.Ref == "") {
		return pipeline.Envelope{}, pipeline.ErrInput
	}
	env := pipeline.Envelope{}
	if a.Input != nil {
		env = *a.Input
	} else {
		s.mu.Lock()
		e, ok := s.store[a.Ref]
		s.mu.Unlock()
		if !ok {
			return env, fmt.Errorf("%w: %s", pipeline.ErrRef, pipeline.Short(a.Ref))
		}
		env = e
	}
	return env, env.Open(kind)
}

func (s *chainServer) summarize(a chainArgs) (pipeline.Envelope, error) {
	in, err := s.in(a, pipeline.KindDossier)
	if err != nil {
		return pipeline.Envelope{}, err
	}
	var d pipeline.Dossier
	if err := json.Unmarshal(in.Data, &d); err != nil {
		return pipeline.Envelope{}, err
	}
	f := pipeline.Facts{Issue: trivia.Issue{SciName: d.Dossier.Species.SciName, NameRu: d.Dossier.NameRu,
		Title: "Манул", Status: "ok", Facts: []trivia.Fact{{Text: "Манул живёт в горах.", Sources: []string{"S2"}}}}}
	env, err := pipeline.Seal(pipeline.KindFacts, f, in.Digest)
	env.Summary, env.CostUSD = "1 факт из 1", chainToolCost
	return env, err
}

func (s *chainServer) save(a chainArgs) (pipeline.Envelope, error) {
	format := a.Format
	if format == "" {
		format = pipeline.FormatMarkdown
	}
	in, err := s.in(a, pipeline.KindFacts)
	if err != nil {
		return pipeline.Envelope{}, err
	}
	text := "# Манул\n\n- Манул живёт в горах.\n"
	f := pipeline.File{Path: "exports/manul." + format, Format: format, Bytes: len(text),
		SHA256: "sha256:file", Chain: []string{in.Input, in.Digest}, Preview: text}
	env, err := pipeline.Seal(pipeline.KindFile, f, in.Digest)
	env.Summary = f.Path
	return env, err
}

// Call — Caller в памяти: аргументы идут через JSON, ошибка — только
// текстом, как по MCP.
func (s *chainServer) Call(ctx context.Context, tool string, args any) (json.RawMessage, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	out, err := s.handle(ctx, tool, raw)
	if err != nil {
		return nil, errors.New(err.Error())
	}
	return json.RawMessage(out), nil
}

// Tools — три инструмента для MCP-сервера и для модели.
func (s *chainServer) Tools() []tools.Tool {
	schema := json.RawMessage(`{"type":"object","properties":{` +
		`"query":{"type":"string"},"random":{"type":"boolean"},` +
		`"input":{"type":"object"},"ref":{"type":"string"},"format":{"type":"string","enum":["md","json"]}}}`)
	var out []tools.Tool
	for _, name := range pipeline.ToolNames {
		out = append(out, tools.Func{
			S: tools.Spec{Name: name, Description: "шаг конвейера " + name, Parameters: schema},
			Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
				return s.handle(ctx, name, args)
			},
		})
	}
	return out
}

func (s *chainServer) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *chainServer) Args(i int) map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m map[string]json.RawMessage
	_ = json.Unmarshal(s.args[i], &m)
	return m
}
