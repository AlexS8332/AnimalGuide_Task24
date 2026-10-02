package feed

import (
	"context"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// turn — ход с набором механизмов поверх умолчаний («+trivia»); у запроса
// уже есть инструмент другого механизма — хук дописывает, а не заменяет.
func turn(t *testing.T, spec string) (*runs.Turn, *agent.Recorder) {
	t.Helper()
	reg := features.Catalog()
	fs, err := reg.Parse(spec, reg.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	rec := &agent.Recorder{}
	tr := &runs.Turn{ID: "t1", Features: fs, Em: rec}
	tr.Request.Features = fs
	tr.Request.Tools = []tools.Tool{tools.Func{S: tools.Spec{Name: "invariant_check"}}}
	return tr, rec
}

func names(ts []tools.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Spec().Name
	}
	return out
}

func TestHookAddsTools(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(testToken))
	h := &Hook{Remote: newRemote(t, srv.URL, testToken)}
	if h.Name() != "trivia" {
		t.Fatalf("имя: %q", h.Name())
	}
	tr, rec := turn(t, "+trivia")
	if err := h.Before(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(tr.Request.Tools), ","); got != "invariant_check,facts_get,facts_latest" {
		t.Fatalf("инструменты хода: %s", got)
	}
	if !strings.Contains(tr.Request.Rules, "created_at") || !strings.Contains(tr.Request.Rules, "facts_get") {
		t.Fatalf("правило ведущему: %q", tr.Request.Rules)
	}
	if !tr.Request.Features.On(features.Trivia) {
		t.Fatal("механизм выключен в наборе хода")
	}
	if len(rec.Events) != 1 || rec.Events[0].Mechanism != "trivia" || rec.Events[0].Via != tools.ViaMCP ||
		!strings.Contains(rec.Events[0].Title, "facts_get, facts_latest") || !strings.Contains(rec.Events[0].Title, "18.0.0") {
		t.Fatalf("журнал: %+v", rec.Events)
	}
	// Инструмент рабочий: вызов идёт в демон.
	if out, err := tr.Request.Tools[1].Call(context.Background(), []byte(`{"id":3}`)); err != nil || !strings.Contains(out, "created_at") {
		t.Fatalf("вызов: %v %s", err, out)
	}
	if err := h.After(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
}

// Механизм выключен — ни инструментов, ни правила, ни обращения к демону.
func TestHookOff(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(""))
	h := &Hook{Remote: newRemote(t, srv.URL, "")}
	tr, rec := turn(t, "")
	if tr.Features.On(features.Trivia) {
		t.Fatal("trivia включён по умолчанию")
	}
	h.Before(context.Background(), tr)
	if len(tr.Request.Tools) != 1 || tr.Request.Rules != "" || len(rec.Events) != 0 {
		t.Fatalf("выключенный механизм что-то сделал: %v %q %+v", names(tr.Request.Tools), tr.Request.Rules, rec.Events)
	}
	h.Remote.mu.Lock()
	connected := h.Remote.sess != nil
	h.Remote.mu.Unlock()
	if connected || d.count("schedule_status") != 0 {
		t.Fatal("выключенный механизм подключился к демону")
	}
}

// Демона нет — ход идёт без инструментов, механизм в наборе хода
// выключен, в журнале — причина и что сделать.
func TestHookWithoutDaemon(t *testing.T) {
	cases := []struct {
		name   string
		remote *Remote
		want   string
	}{
		{"не отвечает", NewRemote(deadAddr(t), "", nil), "не отвечает"},
		{"не настроен", NewRemote("", "", nil), "не настроен"},
		{"нет клиента", nil, "не настроен"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.remote != nil {
				t.Cleanup(c.remote.Close)
			}
			h := &Hook{Remote: c.remote}
			tr, rec := turn(t, "+trivia")
			if err := h.Before(context.Background(), tr); err != nil {
				t.Fatalf("ошибка хука: %v", err)
			}
			if len(tr.Request.Tools) != 1 || tr.Request.Rules != "" {
				t.Fatalf("инструменты без демона: %v %q", names(tr.Request.Tools), tr.Request.Rules)
			}
			if tr.Request.Features.On(features.Trivia) {
				t.Fatal("механизм остался включён в наборе хода")
			}
			if len(rec.Events) != 1 {
				t.Fatalf("журнал: %+v", rec.Events)
			}
			ev := rec.Events[0]
			if ev.Kind != agent.EventMechanism || ev.Mechanism != "trivia" || !strings.Contains(ev.Title, c.want) ||
				!strings.Contains(ev.Title, "ход идёт без инструментов демона") {
				t.Fatalf("событие: %+v", ev)
			}
			if c.name == "не отвечает" && !strings.Contains(ev.Detail, "animals-mcp -http") {
				t.Fatalf("без подсказки: %q", ev.Detail)
			}
		})
	}
}

// Токен не тот — в журнале подсказка про MCP_TOKEN.
func TestHookDenied(t *testing.T) {
	d := &fakeDaemon{}
	srv := serve(t, "", d.handler(testToken))
	h := &Hook{Remote: newRemote(t, srv.URL, "чужой")}
	tr, rec := turn(t, "+trivia")
	h.Before(context.Background(), tr)
	if len(tr.Request.Tools) != 1 || len(rec.Events) != 1 || !strings.Contains(rec.Events[0].Detail, "MCP_TOKEN") ||
		strings.Contains(rec.Events[0].Title+rec.Events[0].Detail, "чужой") {
		t.Fatalf("журнал: %+v", rec.Events)
	}
}
