package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseArgs(t *testing.T) {
	o, err := parseArgs([]string{"снежный", "барс"}, env(map[string]string{"MCP_TOKEN": "tok"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.req.Query != "снежный барс" || o.req.Format != "md" || o.req.Pass != "inline" || o.agent || o.json {
		t.Errorf("умолчания: %+v", o)
	}
	if o.url != feed.DefaultServer || o.token != "tok" || o.timeout != 5*time.Minute {
		t.Errorf("подключение: %+v", o)
	}

	o, err = parseArgs([]string{"-url", "127.0.0.1:9000", "-token", "x", "-random", "-format", "json",
		"-pass", "ref", "-json", "-timeout", "30s"}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !o.req.Random || o.req.Query != "" || o.req.Format != "json" || o.req.Pass != "ref" || !o.json ||
		o.url != "127.0.0.1:9000" || o.token != "x" || o.timeout != 30*time.Second {
		t.Errorf("флаги: %+v", o)
	}

	// -agent всегда по ref.
	o, err = parseArgs([]string{"-agent", "-model", "deepseek-v4-pro", "манул"}, env(nil), io.Discard)
	if err != nil || !o.agent || o.req.Pass != "ref" || o.model != "deepseek-v4-pro" {
		t.Errorf("-agent: %+v %v", o, err)
	}
	o, _ = parseArgs([]string{"-agent", "манул"}, env(map[string]string{"DEEPSEEK_MODEL": "m"}), io.Discard)
	if o.model != "m" {
		t.Errorf("модель из окружения: %q", o.model)
	}
}

func TestParseArgsErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"нет вида":            {},
		"вид и random":        {"-random", "манул"},
		"формат":              {"-format", "pdf", "манул"},
		"передача":            {"-pass", "mail", "манул"},
		"пустой адрес":        {"-url", " ", "манул"},
		"агент и inline":      {"-agent", "-pass", "inline", "манул"},
		"неизвестный флаг":    {"-fast", "манул"},
		"нулевой предел":      {"-timeout", "0s", "манул"},
		"предел не разобрать": {"-timeout", "скоро", "манул"},
	} {
		if _, err := parseArgs(args, env(nil), io.Discard); err == nil {
			t.Errorf("%s: разбор %q прошёл", name, args)
		}
	}
	var out bytes.Buffer
	if _, err := parseArgs([]string{"-h"}, env(nil), &out); !errors.Is(err, errHelp) || !strings.Contains(out.String(), "-agent") {
		t.Errorf("-h: %v\n%s", err, out.String())
	}
}

// cliDaemon — демон по HTTP с тремя простыми инструментами конвейера.
func cliDaemon(t *testing.T) string {
	t.Helper()
	store := map[string]pipeline.Envelope{}
	input := func(args json.RawMessage, kind string) (pipeline.Envelope, error) {
		var a struct {
			Input *pipeline.Envelope `json:"input"`
			Ref   string             `json:"ref"`
		}
		_ = json.Unmarshal(args, &a)
		in := store[a.Ref]
		if a.Input != nil {
			in = *a.Input
		}
		return in, in.Open(kind)
	}
	reply := func(env pipeline.Envelope, err error) (string, error) {
		if err != nil {
			return "", err
		}
		store[env.Digest] = env
		b, err := json.Marshal(env)
		return string(b), err
	}
	fn := map[string]tools.CallFunc{
		pipeline.ToolSearch: func(_ context.Context, args json.RawMessage) (string, error) {
			return reply(pipeline.Seal(pipeline.KindDossier, pipeline.Dossier{Query: "манул", Resolved: "тест"}, ""))
		},
		pipeline.ToolSummarize: func(_ context.Context, args json.RawMessage) (string, error) {
			in, err := input(args, pipeline.KindDossier)
			if err != nil {
				return "", err
			}
			env, err := pipeline.Seal(pipeline.KindFacts, pipeline.Facts{}, in.Digest)
			env.Summary, env.CostUSD = "3 факта из 4", 0.0012
			return reply(env, err)
		},
		pipeline.ToolSaveFile: func(_ context.Context, args json.RawMessage) (string, error) {
			in, err := input(args, pipeline.KindFacts)
			if err != nil {
				return "", err
			}
			f := pipeline.File{Path: "exports/manul.md", Format: "md", Bytes: 2100, SHA256: "sha256:abcdef0123456789",
				Chain: []string{in.Input, in.Digest}, Preview: "# Манул\n\nФакты."}
			return reply(pipeline.Seal(pipeline.KindFile, f, in.Digest))
		},
	}
	var ts []tools.Tool
	for _, name := range pipeline.ToolNames {
		ts = append(ts, tools.Func{S: tools.Spec{Name: name, Description: name}, Fn: fn[name]})
	}
	hs := httptest.NewServer(mcp.NewServer(ts, mcp.ServerOptions{}).HTTPHandler(mcp.HTTPOptions{Token: "tok"}))
	t.Cleanup(hs.Close)
	return hs.URL
}

func TestRunJournal(t *testing.T) {
	url := cliDaemon(t)
	o, err := parseArgs([]string{"-url", url, "-token", "tok", "манул"}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run(context.Background(), o, &out); code != 0 {
		t.Fatalf("код %d\n%s", code, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"[1/3] search  {\"query\":\"манул\"}",
		"[2/3] summarize  {\"input\":\"<конверт dossier sha256:",
		"✓ ответ — конверт  ✓ вид данных  ✓ отпечаток ответа  ✓ вход = выход шага 1",
		"✓ цепочка в файле",
		"3 факта из 4",
		"$0.0012",
		"✓ Цепочка пройдена",
		"Файл: exports/manul.md (md, 2.1 КБ, sha256 abcdef012345)",
		"# Манул",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в журнале нет %q:\n%s", want, text)
		}
	}

	// -json: след целиком.
	o.json = true
	out.Reset()
	if code := run(context.Background(), o, &out); code != 0 {
		t.Fatalf("-json: код %d", code)
	}
	var tr pipeline.Trace
	if err := json.Unmarshal(out.Bytes(), &tr); err != nil || !tr.OK || len(tr.Steps) != 3 || tr.File == nil {
		t.Errorf("-json: %v\n%s", err, out.String())
	}
}

func TestRunFailsWithHint(t *testing.T) {
	url := cliDaemon(t)
	o, _ := parseArgs([]string{"-url", url, "-token", "не тот", "манул"}, env(nil), io.Discard)
	var out bytes.Buffer
	if code := run(context.Background(), o, &out); code != 1 {
		t.Fatalf("код %d", code)
	}
	if !strings.Contains(out.String(), "✗ Цепочка не пройдена") || !strings.Contains(out.String(), "MCP_TOKEN") {
		t.Errorf("журнал:\n%s", out.String())
	}
}

func TestRunAgentNeedsKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	o, _ := parseArgs([]string{"-agent", "-url", "127.0.0.1:1", "манул"}, env(nil), io.Discard)
	var out bytes.Buffer
	if code := run(context.Background(), o, &out); code != 1 || !strings.Contains(out.String(), "DEEPSEEK_API_KEY") {
		t.Errorf("код %d:\n%s", code, out.String())
	}
}
