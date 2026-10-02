package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
)

func TestRunInline(t *testing.T) {
	srv := newChainServer()
	var events []string
	tr, err := pipeline.Run(context.Background(), srv, pipeline.Request{Query: "манул"}, func(s pipeline.Step) {
		events = append(events, s.Tool+":"+s.Status)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !tr.OK || tr.Mode != pipeline.ModeCode || tr.Error != "" {
		t.Fatalf("след: ok=%v mode=%s err=%q", tr.OK, tr.Mode, tr.Error)
	}
	if tr.Request.Format != "md" || tr.Request.Pass != "inline" {
		t.Errorf("умолчания запроса: %+v", tr.Request)
	}
	want := []string{
		"search:pending", "summarize:pending", "save_to_file:pending",
		"search:running", "search:ok",
		"summarize:running", "summarize:ok",
		"save_to_file:running", "save_to_file:ok",
	}
	if strings.Join(events, " ") != strings.Join(want, " ") {
		t.Errorf("onStep:\n получили %v\n ждали    %v", events, want)
	}
	if got := srv.Calls(); strings.Join(got, ",") != "search,summarize,save_to_file" {
		t.Errorf("вызовы сервера: %v", got)
	}

	// Цепочка: Input каждого шага — Digest предыдущего, в файле — оба.
	s := tr.Steps
	if len(s) != 3 {
		t.Fatalf("шагов %d", len(s))
	}
	if s[0].Input != "" || s[1].Input != s[0].Digest || s[2].Input != s[1].Digest {
		t.Errorf("цепочка отпечатков: %s → %s → %s", s[0].Digest, s[1].Input, s[2].Input)
	}
	if tr.File == nil || tr.File.Path != "exports/manul.md" {
		t.Fatalf("файл: %+v", tr.File)
	}
	if strings.Join(tr.File.Chain, ",") != s[0].Digest+","+s[1].Digest {
		t.Errorf("цепочка в файле: %v", tr.File.Chain)
	}
	names := func(cs []pipeline.Check) string {
		var out []string
		for _, c := range cs {
			if !c.OK {
				t.Errorf("проверка %q не прошла: %s", c.Name, c.Note)
			}
			out = append(out, c.Name)
		}
		return strings.Join(out, "; ")
	}
	if got := names(s[0].Checks); got != "ответ — конверт; вид данных; отпечаток ответа" {
		t.Errorf("проверки шага 1: %s", got)
	}
	if got := names(s[1].Checks); got != "ответ — конверт; вид данных; отпечаток ответа; вход = выход шага 1" {
		t.Errorf("проверки шага 2: %s", got)
	}
	if got := names(s[2].Checks); got != "ответ — конверт; вид данных; отпечаток ответа; вход = выход шага 2; цепочка в файле" {
		t.Errorf("проверки шага 3: %s", got)
	}
	if tr.CostUSD != chainToolCost || s[1].CostUSD != chainToolCost {
		t.Errorf("расход: %v, шаг 2 %v", tr.CostUSD, s[1].CostUSD)
	}
	if tr.Finished.Before(tr.Started) || tr.Took < 0 || s[0].Bytes == 0 || s[0].Started.IsZero() {
		t.Errorf("время и размеры: %+v", tr)
	}

	// Сервер получил конверт целиком, а в журнал ушла строка вместо него.
	var in pipeline.Envelope
	if err := json.Unmarshal(srv.Args(1)["input"], &in); err != nil || in.Digest != s[0].Digest {
		t.Errorf("summarize получил input %s (%v)", srv.Args(1)["input"], err)
	}
	var logged map[string]string
	if err := json.Unmarshal(s[1].Args, &logged); err != nil {
		t.Fatalf("Args шага 2: %s", s[1].Args)
	}
	if want := "<конверт dossier sha256:" + pipeline.Short(s[0].Digest) + "…>"; logged["input"] != want {
		t.Errorf("Args шага 2: %q, ждали %q", logged["input"], want)
	}
	if !strings.Contains(string(s[2].Args), `"format":"md"`) {
		t.Errorf("Args шага 3: %s", s[2].Args)
	}
}

func TestRunRef(t *testing.T) {
	srv := newChainServer()
	tr, err := pipeline.Run(context.Background(), srv, pipeline.Request{Random: true, Format: "json", Pass: "ref"}, nil)
	if err != nil || !tr.OK {
		t.Fatalf("Run: %v", err)
	}
	if string(srv.Args(0)["random"]) != "true" {
		t.Errorf("search: %s", srv.Args(0))
	}
	var ref string
	if err := json.Unmarshal(srv.Args(1)["ref"], &ref); err != nil || ref != tr.Steps[0].Digest {
		t.Errorf("summarize ref %q, ждали %q", ref, tr.Steps[0].Digest)
	}
	if _, ok := srv.Args(1)["input"]; ok {
		t.Error("при ref конверт тоже ушёл")
	}
	if tr.File == nil || tr.File.Format != "json" {
		t.Errorf("файл: %+v", tr.File)
	}
	if !strings.Contains(string(tr.Steps[2].Args), `"ref":"sha256:`) {
		t.Errorf("Args шага 3: %s", tr.Steps[2].Args)
	}
}

// failedAt — общая проверка провала: шаг n failed, дальше pending.
func failedAt(t *testing.T, tr pipeline.Trace, n int) {
	t.Helper()
	if tr.OK || tr.Error == "" {
		t.Errorf("след не провален: ok=%v err=%q", tr.OK, tr.Error)
	}
	for i, s := range tr.Steps {
		want := pipeline.StepOK
		switch {
		case i+1 == n:
			want = pipeline.StepFailed
		case i+1 > n:
			want = pipeline.StepPending
		}
		if s.Status != want {
			t.Errorf("шаг %d (%s): %s, ждали %s", s.N, s.Tool, s.Status, want)
		}
	}
	if s := tr.Steps[n-1]; s.Error == "" {
		t.Errorf("у шага %d нет ошибки", n)
	}
}

func TestRunDetectsCorruptedResponse(t *testing.T) {
	srv := newChainServer()
	srv.tamper = func(tool string, env *pipeline.Envelope) {
		if tool == pipeline.ToolSearch { // данные изменены, отпечаток прежний
			env.Data = json.RawMessage(strings.Replace(string(env.Data), "Манул", "Мамонт", 1))
		}
	}
	tr, err := pipeline.Run(context.Background(), srv, pipeline.Request{Query: "манул"}, nil)
	if !errors.Is(err, pipeline.ErrDigest) {
		t.Fatalf("ждали ErrDigest, получили %v", err)
	}
	failedAt(t, tr, 1)
	if len(srv.Calls()) != 1 {
		t.Errorf("после порчи вызваны ещё: %v", srv.Calls())
	}
	var bad *pipeline.Check
	for i, c := range tr.Steps[0].Checks {
		if c.Name == pipeline.CheckDigest {
			bad = &tr.Steps[0].Checks[i]
		}
	}
	if bad == nil || bad.OK {
		t.Errorf("проверка отпечатка: %+v", tr.Steps[0].Checks)
	}
}

func TestRunDetectsWrongInput(t *testing.T) {
	srv := newChainServer()
	srv.tamper = func(tool string, env *pipeline.Envelope) {
		if tool == pipeline.ToolSummarize { // «обработал не то»
			env.Input = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		}
	}
	tr, err := pipeline.Run(context.Background(), srv, pipeline.Request{Query: "манул", Pass: "ref"}, nil)
	if !errors.Is(err, pipeline.ErrChain) {
		t.Fatalf("ждали ErrChain, получили %v", err)
	}
	failedAt(t, tr, 2)
	if !strings.Contains(err.Error(), "шаг 2 (summarize)") {
		t.Errorf("текст ошибки: %v", err)
	}
}

func TestRunDetectsWrongKind(t *testing.T) {
	srv := newChainServer()
	srv.tamper = func(tool string, env *pipeline.Envelope) {
		if tool == pipeline.ToolSummarize {
			env.Kind = pipeline.KindDossier
		}
	}
	tr, err := pipeline.Run(context.Background(), srv, pipeline.Request{Query: "манул"}, nil)
	if !errors.Is(err, pipeline.ErrKind) {
		t.Fatalf("ждали ErrKind, получили %v", err)
	}
	failedAt(t, tr, 2)
}

func TestRunDetectsWrongFileChain(t *testing.T) {
	srv := newChainServer()
	srv.tamper = func(tool string, env *pipeline.Envelope) {
		if tool != pipeline.ToolSaveFile {
			return
		}
		var f pipeline.File
		_ = json.Unmarshal(env.Data, &f)
		f.Chain = f.Chain[1:]
		re, _ := pipeline.Seal(env.Kind, f, env.Input)
		*env = re
	}
	tr, err := pipeline.Run(context.Background(), srv, pipeline.Request{Query: "манул"}, nil)
	if !errors.Is(err, pipeline.ErrChain) {
		t.Fatalf("ждали ErrChain, получили %v", err)
	}
	failedAt(t, tr, 3)
	if tr.File == nil {
		t.Error("данные файла не попали в след")
	}
}

func TestRunToolErrorLeavesRestPending(t *testing.T) {
	srv := newChainServer()
	srv.fail[pipeline.ToolSummarize] = errors.New("редактор недоступен")
	var last []pipeline.Step
	tr, err := pipeline.Run(context.Background(), srv, pipeline.Request{Query: "манул"}, func(s pipeline.Step) {
		last = append(last, s)
	})
	if err == nil || !strings.Contains(err.Error(), "редактор недоступен") {
		t.Fatalf("ошибка: %v", err)
	}
	failedAt(t, tr, 2)
	if got := last[len(last)-1]; got.Tool != pipeline.ToolSummarize || got.Status != pipeline.StepFailed {
		t.Errorf("последнее событие: %+v", got)
	}
	if tr.File != nil {
		t.Error("файл при провале")
	}
}

// Ошибка передачи, пришедшая от инструмента только текстом (как по MCP),
// узнаётся errors.Is.
func TestRunToolTransferErrorByText(t *testing.T) {
	srv := newChainServer()
	srv.fail[pipeline.ToolSaveFile] = pipeline.ErrRef
	_, err := pipeline.Run(context.Background(), srv, pipeline.Request{Query: "манул", Pass: "ref"}, nil)
	if !errors.Is(err, pipeline.ErrRef) {
		t.Fatalf("ждали ErrRef, получили %v", err)
	}
	var se *pipeline.StepError
	if !errors.As(err, &se) || se.N != 3 || se.Tool != pipeline.ToolSaveFile {
		t.Errorf("StepError: %+v", se)
	}
}

func TestRunValidatesRequest(t *testing.T) {
	for _, req := range []pipeline.Request{
		{},
		{Query: "  "},
		{Query: "манул", Random: true},
		{Query: "манул", Format: "pdf"},
		{Query: "манул", Pass: "mail"},
	} {
		srv := newChainServer()
		tr, err := pipeline.Run(context.Background(), srv, req, nil)
		if !errors.Is(err, pipeline.ErrRequest) || tr.OK || tr.Error == "" {
			t.Errorf("%+v: %v", req, err)
		}
		if len(srv.Calls()) != 0 {
			t.Errorf("%+v: сервер вызван", req)
		}
	}
	r, err := pipeline.Request{Query: " манул ", Format: "JSON", Pass: "Ref"}.Normalize()
	if err != nil || r.Query != "манул" || r.Format != "json" || r.Pass != "ref" {
		t.Errorf("Normalize: %+v %v", r, err)
	}
}

func TestLogArgs(t *testing.T) {
	if got := string(pipeline.LogArgs(json.RawMessage(`{"ref":"sha256:abc"}`))); got != `{"ref":"sha256:abc"}` {
		t.Errorf("ref: %s", got)
	}
	env, _ := pipeline.Seal(pipeline.KindFacts, map[string]int{"a": 1}, "")
	raw, _ := json.Marshal(map[string]any{"input": env, "format": "md"})
	got := string(pipeline.LogArgs(raw))
	if !strings.Contains(got, `"input":"<конверт facts sha256:`) {
		t.Errorf("input: %s", got)
	}
	if strings.Contains(got, `"data"`) {
		t.Errorf("данные в журнале: %s", got)
	}
}
