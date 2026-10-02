package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// replyDigest — digest из ответа инструмента, который видела модель.
func replyDigest(t *testing.T, reply string) string {
	t.Helper()
	var r struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal([]byte(reply), &r); err != nil || r.Digest == "" {
		t.Fatalf("в ответе инструмента нет digest: %s", reply)
	}
	return r.Digest
}

func refArgs(ref string, extra ...string) string {
	s := fmt.Sprintf(`{"ref":%q`, ref)
	for _, e := range extra {
		s += "," + e
	}
	return s + "}"
}

// extraTool — посторонний инструмент: исполнителю его давать нельзя.
var extraTool = tools.Func{S: tools.Spec{Name: "run_now"},
	Fn: func(context.Context, json.RawMessage) (string, error) { return "{}", nil }}

func TestRunAgentChainByRef(t *testing.T) {
	srv := newChainServer()
	var dossier, facts string
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		switch llmtest.ToolReplies(req) {
		case 0:
			return llmtest.ToolCall("search", `{"query":"манул"}`), nil
		case 1:
			dossier = replyDigest(t, llmtest.LastToolReply(req))
			return llmtest.ToolCall("summarize", refArgs(dossier)), nil
		case 2:
			facts = replyDigest(t, llmtest.LastToolReply(req))
			return llmtest.ToolCall("save_to_file", refArgs(facts, `"format":"json"`)), nil
		}
		return llmtest.Text("exports/manul.json"), nil
	}}
	var events []string
	cfg := pipeline.AgentConfig{LLM: fake, Tools: append([]tools.Tool{extraTool}, srv.Tools()...)}
	tr, err := pipeline.RunAgent(context.Background(), cfg, pipeline.Request{Query: "Манул", Format: "json"},
		func(s pipeline.Step) { events = append(events, fmt.Sprintf("%d:%s:%s", s.N, s.Tool, s.Status)) })
	if err != nil || !tr.OK {
		t.Fatalf("RunAgent: %v", err)
	}
	if tr.Mode != pipeline.ModeAgent || tr.Request.Pass != pipeline.PassRef {
		t.Errorf("след: mode=%s pass=%s", tr.Mode, tr.Request.Pass)
	}
	if len(tr.Steps) != 3 || tr.Steps[0].Digest != dossier || tr.Steps[1].Digest != facts {
		t.Fatalf("шаги: %+v", tr.Steps)
	}
	for _, s := range tr.Steps {
		if s.Status != pipeline.StepOK {
			t.Errorf("шаг %d: %s %s", s.N, s.Status, s.Error)
		}
		for _, c := range s.Checks {
			if !c.OK {
				t.Errorf("шаг %d: проверка %q: %s", s.N, c.Name, c.Note)
			}
		}
	}
	if !strings.Contains(strings.Join(events, " "), "1:search:running 1:search:ok 2:summarize:running") {
		t.Errorf("onStep: %v", events)
	}
	if tr.File == nil || tr.File.Path != "exports/manul.json" {
		t.Errorf("файл: %+v", tr.File)
	}
	// Расход: инструменты плюс сама модель.
	if tr.CostUSD <= chainToolCost {
		t.Errorf("расход %v не включает модель", tr.CostUSD)
	}

	// Модели выданы ровно три инструмента, данные досье она не видела.
	req := fake.Requests[0]
	var names []string
	for _, d := range req.Tools {
		names = append(names, d.Function.Name)
	}
	if strings.Join(names, ",") != "search,summarize,save_to_file" {
		t.Errorf("инструменты модели: %v", names)
	}
	if !strings.Contains(req.Messages[0].Content, "ref") || !strings.Contains(req.Messages[1].Content, "Манул") {
		t.Errorf("промпт: %+v", req.Messages[:2])
	}
	if reply := llmtest.LastToolReply(fake.Requests[1]); strings.Contains(reply, `"data"`) || strings.Contains(reply, "Центральной Азии") {
		t.Errorf("модель видит данные досье: %s", reply)
	}
	if reply := llmtest.LastToolReply(fake.Requests[3]); !strings.Contains(reply, "exports/manul.json") {
		t.Errorf("модель не видит путь к файлу: %s", reply)
	}
}

// Модель пропустила summarize: save_to_file с ref досье — ErrKind от
// инструмента, модель читает ошибку и исправляется.
func TestRunAgentRecoversFromSkippedStep(t *testing.T) {
	srv := newChainServer()
	var dossier string
	sawError := false
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		last := llmtest.LastToolReply(req)
		switch llmtest.ToolReplies(req) {
		case 0:
			return llmtest.ToolCall("search", `{"query":"манул"}`), nil
		case 1:
			dossier = replyDigest(t, last)
			return llmtest.ToolCall("save_to_file", refArgs(dossier)), nil
		case 2:
			sawError = strings.HasPrefix(last, "ошибка:") && strings.Contains(last, "не того вида")
			return llmtest.ToolCall("summarize", refArgs(dossier)), nil
		case 3:
			return llmtest.ToolCall("save_to_file", refArgs(replyDigest(t, last))), nil
		}
		return llmtest.Text("exports/manul.md"), nil
	}}
	tr, err := pipeline.RunAgent(context.Background(), pipeline.AgentConfig{LLM: fake, Tools: srv.Tools()},
		pipeline.Request{Query: "манул"}, nil)
	if err != nil || !tr.OK {
		t.Fatalf("RunAgent: %v", err)
	}
	if !sawError {
		t.Error("модель не получила ошибку вида данных")
	}
	got := make([]string, len(tr.Steps))
	for i, s := range tr.Steps {
		got[i] = s.Tool + ":" + s.Status
	}
	if strings.Join(got, " ") != "search:ok save_to_file:failed summarize:ok save_to_file:ok" {
		t.Errorf("шаги: %v", got)
	}
	if !errors.Is(stepErr(tr.Steps[1]), pipeline.ErrKind) {
		t.Errorf("ошибка шага 2: %s", tr.Steps[1].Error)
	}
	if tr.File == nil || tr.File.Chain[0] != tr.Steps[0].Digest || tr.File.Chain[1] != tr.Steps[2].Digest {
		t.Errorf("файл: %+v", tr.File)
	}
}

// stepErr — текст ошибки шага как ошибка: errors.Is по тексту не работает,
// поэтому сверяем с сообщением сторожевой ошибки.
func stepErr(s pipeline.Step) error {
	for _, k := range []error{pipeline.ErrKind, pipeline.ErrRef, pipeline.ErrDigest, pipeline.ErrChain} {
		if strings.Contains(s.Error, k.Error()) {
			return k
		}
	}
	return errors.New(s.Error)
}

// Выдуманный ref: ErrRef, модель берёт настоящий digest и доходит до конца.
func TestRunAgentInventedRef(t *testing.T) {
	srv := newChainServer()
	var dossier string
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		last := llmtest.LastToolReply(req)
		switch llmtest.ToolReplies(req) {
		case 0:
			return llmtest.ToolCall("search", `{"query":"манул"}`), nil
		case 1:
			dossier = replyDigest(t, last)
			return llmtest.ToolCall("summarize", refArgs("sha256:"+pipeline.Short(dossier))), nil
		case 2:
			return llmtest.ToolCall("summarize", refArgs(dossier)), nil
		case 3:
			return llmtest.ToolCall("save_to_file", refArgs(replyDigest(t, last))), nil
		}
		return llmtest.Text("готово"), nil
	}}
	tr, err := pipeline.RunAgent(context.Background(), pipeline.AgentConfig{LLM: fake, Tools: srv.Tools()},
		pipeline.Request{Query: "манул"}, nil)
	if err != nil || !tr.OK {
		t.Fatalf("RunAgent: %v", err)
	}
	if tr.Steps[1].Status != pipeline.StepFailed || !errors.Is(stepErr(tr.Steps[1]), pipeline.ErrRef) {
		t.Errorf("шаг 2: %+v", tr.Steps[1])
	}
}

// Модель закончила без save_to_file — цепочка не пройдена.
func TestRunAgentStopsEarly(t *testing.T) {
	srv := newChainServer()
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		switch llmtest.ToolReplies(req) {
		case 0:
			return llmtest.ToolCall("search", `{"query":"манул"}`), nil
		case 1:
			return llmtest.ToolCall("summarize", refArgs(replyDigest(t, llmtest.LastToolReply(req)))), nil
		}
		return llmtest.Text("Вот факты о мануле: живёт в горах."), nil
	}}
	tr, err := pipeline.RunAgent(context.Background(), pipeline.AgentConfig{LLM: fake, Tools: srv.Tools()},
		pipeline.Request{Query: "манул"}, nil)
	if !errors.Is(err, pipeline.ErrIncomplete) || tr.OK || tr.Error == "" {
		t.Fatalf("ждали ErrIncomplete, получили %v (ok=%v)", err, tr.OK)
	}
	if len(tr.Steps) != 2 || tr.File != nil {
		t.Errorf("шаги: %+v", tr.Steps)
	}
	if tr.CostUSD <= chainToolCost {
		t.Errorf("расход %v", tr.CostUSD)
	}
}

// Модель искала не тот вид — цепочка сходится, но итог не OK.
func TestRunAgentWrongQuery(t *testing.T) {
	srv := newChainServer()
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		last := llmtest.LastToolReply(req)
		switch llmtest.ToolReplies(req) {
		case 0:
			return llmtest.ToolCall("search", `{"query":"рысь"}`), nil
		case 1:
			return llmtest.ToolCall("summarize", refArgs(replyDigest(t, last))), nil
		case 2:
			return llmtest.ToolCall("save_to_file", refArgs(replyDigest(t, last))), nil
		}
		return llmtest.Text("готово"), nil
	}}
	tr, err := pipeline.RunAgent(context.Background(), pipeline.AgentConfig{LLM: fake, Tools: srv.Tools()},
		pipeline.Request{Query: "манул"}, nil)
	if err == nil || tr.OK || !strings.Contains(err.Error(), "не тот вид") {
		t.Fatalf("ждали ошибку запроса, получили %v", err)
	}
}

// Лишний успешный вызов помечается, но итог не портит.
func TestRunAgentMarksExtraCalls(t *testing.T) {
	srv := newChainServer()
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		last := llmtest.LastToolReply(req)
		switch llmtest.ToolReplies(req) {
		case 0:
			return llmtest.ToolCall("search", `{"query":"манул"}`), nil
		case 1:
			return llmtest.ToolCall("search", `{"query":"манул"}`), nil
		case 2:
			return llmtest.ToolCall("summarize", refArgs(replyDigest(t, last))), nil
		case 3:
			return llmtest.ToolCall("save_to_file", refArgs(replyDigest(t, last))), nil
		}
		return llmtest.Text("готово"), nil
	}}
	tr, err := pipeline.RunAgent(context.Background(), pipeline.AgentConfig{LLM: fake, Tools: srv.Tools()},
		pipeline.Request{Query: "манул"}, nil)
	if err != nil || !tr.OK {
		t.Fatalf("RunAgent: %v", err)
	}
	// Досье детерминированное: оба search дали один digest, в цепочку
	// вошёл последний, первый — лишний.
	last := func(s pipeline.Step) pipeline.Check { return s.Checks[len(s.Checks)-1] }
	if c := last(tr.Steps[0]); c.Name != pipeline.CheckInChain || c.OK {
		t.Errorf("первый search: %+v", c)
	}
	for _, s := range tr.Steps[1:] {
		if c := last(s); c.Name != pipeline.CheckInChain || !c.OK {
			t.Errorf("шаг %d: %+v", s.N, c)
		}
	}
}

func TestRunAgentConfig(t *testing.T) {
	srv := newChainServer()
	fake := &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) { return llmtest.Text("—"), nil }}
	_, err := pipeline.RunAgent(context.Background(), pipeline.AgentConfig{LLM: fake, Tools: srv.Tools()[:2]},
		pipeline.Request{Query: "манул"}, nil)
	if err == nil || !strings.Contains(err.Error(), "save_to_file") {
		t.Errorf("без save_to_file: %v", err)
	}
	if fake.Calls() != 0 {
		t.Error("модель вызвана без полного набора инструментов")
	}
	_, err = pipeline.RunAgent(context.Background(), pipeline.AgentConfig{LLM: fake, Tools: srv.Tools()}, pipeline.Request{}, nil)
	if !errors.Is(err, pipeline.ErrRequest) {
		t.Errorf("пустой запрос: %v", err)
	}

	// Модель зовёт инструменты бесконечно — предел ответов.
	loop := &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) {
		return llmtest.ToolCall("search", `{"query":"манул"}`), nil
	}}
	tr, err := pipeline.RunAgent(context.Background(), pipeline.AgentConfig{LLM: loop, Tools: srv.Tools(), MaxSteps: 3},
		pipeline.Request{Query: "манул"}, nil)
	if !errors.Is(err, pipeline.ErrIncomplete) || loop.Calls() != 3 || len(tr.Steps) != 3 {
		t.Errorf("предел: %v, ответов %d, шагов %d", err, loop.Calls(), len(tr.Steps))
	}
}
