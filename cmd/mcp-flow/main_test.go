package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow/flowtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/hub"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseArgs(t *testing.T) {
	o, err := parseArgs(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.config != "" || o.data != "." || o.daemon != feed.DefaultServer || o.servers || o.preset != "" ||
		o.species != "" || o.json || o.repeat != 1 || o.timeout != 5*time.Minute {
		t.Errorf("умолчания: %+v", o)
	}

	o, err = parseArgs([]string{"-config", "s.json", "-data", "D:/data", "-preset", "passport", "-model", "deepseek-v4-pro",
		"-json", "-repeat", "3", "-timeout", "30s", "снежный", "барс"},
		env(map[string]string{"TRIVIA_SERVER": "http://127.0.0.1:9001"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.config != "s.json" || o.data != "D:/data" || o.daemon != "http://127.0.0.1:9001" || o.preset != "passport" ||
		o.model != "deepseek-v4-pro" || !o.json || o.repeat != 3 || o.timeout != 30*time.Second || o.species != "снежный барс" {
		t.Errorf("флаги: %+v", o)
	}
	o, _ = parseArgs([]string{"-species", "рысь", "-daemon", "http://h:1"}, env(map[string]string{"DEEPSEEK_MODEL": "m"}), io.Discard)
	if o.species != "рысь" || o.daemon != "http://h:1" || o.model != "m" {
		t.Errorf("-species, -daemon, модель из окружения: %+v", o)
	}
}

func TestParseArgsErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"вид дважды":          {"-species", "рысь", "манул"},
		"нулевой повтор":      {"-repeat", "0"},
		"нулевой предел":      {"-timeout", "0s"},
		"нет заготовки":       {"-preset", "nope"},
		"неизвестный флаг":    {"-fast"},
		"повтор не разобрать": {"-repeat", "много"},
	} {
		if _, err := parseArgs(args, env(nil), io.Discard); err == nil {
			t.Errorf("%s: разбор %q прошёл", name, args)
		}
	}
	var out bytes.Buffer
	if _, err := parseArgs([]string{"-h"}, env(nil), &out); !errors.Is(err, errHelp) || !strings.Contains(out.String(), "-servers") {
		t.Errorf("-h: %v\n%s", err, out.String())
	}
}

// fake подменяет реестр и модель на время теста.
func fake(t *testing.T, b *flowtest.Brain) *flowtest.Router {
	t.Helper()
	r := flowtest.NewRouter()
	t.Cleanup(r.Close)
	oldRouter, oldModel := openRouter, newModel
	t.Cleanup(func() { openRouter, newModel = oldRouter, oldModel })
	openRouter = func(options) (hub.Router, func(), error) { return r, func() {}, nil }
	newModel = func(options) (llm.Chatter, error) { return b.Fake(), nil }
	return r
}

func opts(t *testing.T, args ...string) options {
	t.Helper()
	o, err := parseArgs(args, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestRunJournal(t *testing.T) {
	fake(t, &flowtest.Brain{})
	var out, errOut bytes.Buffer
	if code := run(context.Background(), opts(t), &out, &errOut); code != exitOK {
		t.Fatalf("код %d\n%s\n%s", code, out.String(), errOut.String())
	}
	s := out.String()
	t.Log("\n" + s)
	for _, re := range []string{
		`Флоу «Паспорт вида в блокнот» — вид «манул», модель deepseek-v4-flash`,
		`#3  \[daemon\]  mdd_search \{"text":"manul"\}  ✓ \d+ мс`,
		`#4  \[daemon\]  mdd_get \{"id":1006010\}  ✓ \d+ мс  ← №3`,
		`#13 \[notes\]  nb_close .*← №9`,
		`✓ данные: mdd_search → mdd_get\.id — №4 mdd_get\.id = 1006010 ← №3 species\.0\.id`,
		`✓ серверы подтвердили`,
		`daemon   pid 4102  facts_get 1/1, mdd_get 1/1, mdd_search 1/1`,
		`Файл: notes/nb-`,
		`Итог модели: Блокнот о мануле`,
		`Цена: \$0\.\d{4}`,
		`Итог: ✓ флоу пройден`,
	} {
		if !regexp.MustCompile(re).MatchString(s) {
			t.Errorf("в журнале нет %s\n%s", re, s)
		}
	}
	if !strings.Contains(errOut.String(), "· #13 nb_close ✓") {
		t.Errorf("ход прогона:\n%s", errOut.String())
	}
}

func TestRunFailedCheck(t *testing.T) {
	fake(t, &flowtest.Brain{BadMDDID: mddtest.Lynx})
	var out bytes.Buffer
	if code := run(context.Background(), opts(t), &out, io.Discard); code != exitFailed {
		t.Fatalf("код %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ данные: mdd_search → mdd_get.id") || !strings.Contains(out.String(), "Итог: ✗ провалено проверок") {
		t.Errorf("журнал:\n%s", out.String())
	}
}

func TestRunJSONAndRepeat(t *testing.T) {
	fake(t, &flowtest.Brain{})
	var out bytes.Buffer
	if code := run(context.Background(), opts(t, "-json"), &out, io.Discard); code != exitOK {
		t.Fatalf("код %d", code)
	}
	var tr flow.Trace
	if err := json.Unmarshal(out.Bytes(), &tr); err != nil || !tr.OK || len(tr.Calls) != 13 {
		t.Fatalf("-json: %v %+v", err, tr)
	}

	out.Reset()
	if code := run(context.Background(), opts(t, "-repeat", "2"), &out, io.Discard); code != exitOK {
		t.Fatalf("-repeat: код %d", code)
	}
	for _, w := range []string{"прогон 2 из 2", "Итог 2 прогонов: пройдено 2 из 2", "100%  выбор: mdd_get"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("нет %q\n%s", w, out.String())
		}
	}

	out.Reset()
	if code := run(context.Background(), opts(t, "-repeat", "2", "-json"), &out, io.Discard); code != exitOK {
		t.Fatalf("-repeat -json: код %d", code)
	}
	var trs []flow.Trace
	if err := json.Unmarshal(out.Bytes(), &trs); err != nil || len(trs) != 2 {
		t.Errorf("-repeat -json: %v, трасс %d", err, len(trs))
	}
}

func TestRunServers(t *testing.T) {
	fake(t, &flowtest.Brain{})
	var out bytes.Buffer
	if code := run(context.Background(), opts(t, "-servers"), &out, io.Discard); code != exitOK {
		t.Fatalf("код %d", code)
	}
	for _, w := range []string{"sources  ok", "daemon   ok", "notes    ok", "→ nb_open, nb_add, nb_close",
		"run_now (не разрешён)", "search_wikipedia (дубль → sources)"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("нет %q\n%s", w, out.String())
		}
	}
}

func TestRunSetupErrors(t *testing.T) {
	r := fake(t, &flowtest.Brain{})
	real := hubOpen
	openRouter = func(options) (hub.Router, func(), error) {
		return nil, nil, errors.New("конфигурация MCP-серверов: одно имя у двух серверов")
	}
	var errOut bytes.Buffer
	if code := run(context.Background(), opts(t), io.Discard, &errOut); code != exitSetup || !strings.Contains(errOut.String(), "двух серверов") {
		t.Errorf("конфигурация: %s", errOut.String())
	}

	openRouter = func(options) (hub.Router, func(), error) { return r, func() {}, nil }
	newModel = func(options) (llm.Chatter, error) { return nil, errors.New("нужен DEEPSEEK_API_KEY") }
	errOut.Reset()
	if code := run(context.Background(), opts(t), io.Discard, &errOut); code != exitSetup || !strings.Contains(errOut.String(), "DEEPSEEK_API_KEY") {
		t.Errorf("ключ: %s", errOut.String())
	}

	// Встроенный путь до настоящего реестра компилируется и отвечает
	// ошибкой, а не паникой, пока реестр не готов или конфигурация плоха.
	if _, _, err := real(options{config: "нет-такого-файла.json"}); err == nil {
		t.Error("несуществующая конфигурация открылась")
	}
}
