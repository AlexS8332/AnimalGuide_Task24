package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents/agentstest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/invariants"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
)

// withArgs подменяет командную строку на время теста: parseFlags работает
// с глобальным набором флагов.
func withArgs(t *testing.T, args ...string) {
	t.Helper()
	oldArgs, oldFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args, flag.CommandLine = oldArgs, oldFlags })
	os.Args = append([]string{"animalguide"}, args...)
	flag.CommandLine = flag.NewFlagSet("animalguide", flag.ContinueOnError)
}

// Умолчания флагов: только петлевой интерфейс, данные в рабочем каталоге,
// окно и сокращение — из истории, свой лимит контекста DeepSeek.
func TestParseFlagsDefaults(t *testing.T) {
	withArgs(t)
	o := parseFlags()
	if o.addr != defaultAddr || !strings.HasPrefix(o.addr, "127.0.0.1:") || !o.open || o.data != "." || o.featureSpec != "" {
		t.Fatalf("умолчания: %+v", o)
	}
	if o.window != history.DefaultWindow || o.keep != history.DefaultKeepToolRunes || o.limit != defaultContextLimit || o.overflow != agent.OverflowFail {
		t.Fatalf("умолчания механизмов: %+v", o)
	}
}

// Флаги разбираются все, включая набор механизмов.
func TestParseFlagsAll(t *testing.T) {
	withArgs(t, "-addr", "127.0.0.1:9999", "-open=false", "-data", "d", "-features", "+mcp,-guard",
		"-window", "7", "-keep-tools", "100", "-context-limit", "0", "-on-overflow", "trim")
	o := parseFlags()
	want := options{addr: "127.0.0.1:9999", data: "d", featureSpec: "+mcp,-guard", overflow: "trim", open: false, window: 7, keep: 100, limit: 0,
		trials: "all", reportOut: filepath.Join("examples", "report.md"), factsServer: feed.DefaultServer}
	if o != want {
		t.Fatalf("флаги: %+v", o)
	}
}

// TestHelperMain — не тест, а main в отдельном процессе: так проверяются
// выходы с os.Exit и настоящий старт сервера.
func TestHelperMain(t *testing.T) {
	if os.Getenv("ANIMALGUIDE_RUN_MAIN") != "1" {
		t.Skip("вспомогательный процесс")
	}
	var args []string
	if raw := os.Getenv("ANIMALGUIDE_ARGS"); raw != "" {
		args = strings.Split(raw, "\x1f")
	}
	os.Args = append([]string{"animalguide"}, args...)
	flag.CommandLine = flag.NewFlagSet("animalguide", flag.ExitOnError)
	main()
}

// mainCmd — процесс с main и чистым окружением ключей. Рабочий каталог —
// временный: .env-файлы проекта и выше по дереву не подхватываются, а
// DEEPSEEK_BASE_URL уводит возможные запросы в никуда — живых вызовов нет.
func mainCmd(t *testing.T, key string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperMain$")
	cmd.Dir = t.TempDir()
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DEEPSEEK_") && !strings.HasPrefix(kv, "ANIMALGUIDE_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "ANIMALGUIDE_RUN_MAIN=1", "ANIMALGUIDE_ARGS="+strings.Join(args, "\x1f"),
		"DEEPSEEK_API_KEY="+key, "DEEPSEEK_BASE_URL=http://127.0.0.1:1", "DEEPSEEK_MODEL=test-model")
	return cmd
}

// Ошибки запуска — код 1 и понятное сообщение, сервер не стартует.
func TestMainStartupErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("запуск процессов")
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cases := []struct {
		name string
		key  string
		args []string
		want string
	}{
		{"режим переполнения", "k", []string{"-on-overflow", "drop", "-open=false"}, "неизвестный режим -on-overflow"},
		{"незнакомый механизм", "k", []string{"-features", "+gaurd", "-open=false"}, "gaurd"},
		{"страж без свода", "k", []string{"-features", "-charter", "-open=false"}, "charter"},
		{"нет ключа", "  ", []string{"-open=false", "-addr", "127.0.0.1:0"}, "DEEPSEEK_API_KEY"},
		{"адрес занят", "k", []string{"-open=false", "-addr", busy.Addr().String()}, "не удалось занять"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := mainCmd(t, c.key, c.args...)
			out, err := cmd.CombinedOutput()
			ee, ok := err.(*exec.ExitError)
			if !ok || ee.ExitCode() != 1 {
				t.Fatalf("код выхода: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "Ошибка: ") || !strings.Contains(string(out), c.want) {
				t.Fatalf("сообщение без %q:\n%s", c.want, out)
			}
		})
	}
}

// Настоящий старт: сервер слушает петлевой адрес, отдаёт фронтенд из
// бинарника и meta с моделью из окружения; каталог данных — из флага.
func TestMainServesUI(t *testing.T) {
	if testing.Short() {
		t.Skip("запуск процессов")
	}
	data := filepath.Join(t.TempDir(), "data")
	cmd := mainCmd(t, "test-key", "-open=false", "-addr", "127.0.0.1:0", "-data", data, "-window", "9")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	var url string
	var banner []string
	deadline := time.After(30 * time.Second)
	for url == "" {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("процесс завершился до старта:\n%s", strings.Join(banner, "\n"))
			}
			banner = append(banner, l)
			if _, u, found := strings.Cut(l, "интерфейс:"); found {
				url = strings.TrimSpace(u)
			}
		case <-deadline:
			t.Fatalf("сервер не стартовал:\n%s", strings.Join(banner, "\n"))
		}
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("адрес: %q", url)
	}
	for l := range lines {
		banner = append(banner, l)
		if strings.Contains(l, "Ctrl+C") {
			break
		}
	}
	joined := strings.Join(banner, "\n")
	for _, want := range []string{"модель:     test-model", "источники:", "механизмы:", "факты:", "загружено: 0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("в баннере нет %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "test-key") {
		t.Fatal("ключ напечатан в баннере")
	}

	resp, err := http.Get(url + "/api/meta")
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	json.NewDecoder(resp.Body).Decode(&meta)
	resp.Body.Close()
	if meta["model"] != "test-model" || meta["window"] != float64(9) || meta["contextLimit"] != float64(defaultContextLimit) {
		t.Fatalf("meta: %v", meta)
	}
	resp, err = http.Get(url + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("фронтенд: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// Пустой диалог создаётся без модели и ложится в каталог из -data.
	resp, err = http.Post(url+"/api/conversations", "application/json", strings.NewReader(`{"empty":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("пустой диалог: %d", resp.StatusCode)
	}
	if entries, _ := os.ReadDir(filepath.Join(data, "history")); len(entries) != 1 {
		t.Fatalf("файлов диалогов в -data: %d", len(entries))
	}
}

// Судья стенда И-5 — тот же, что у стража: нарушение из вердикта судьи.
func TestCharterJudgeForBench(t *testing.T) {
	b := &agentstest.Brain{Judge: func(req llm.Request) string {
		return agentstest.Verdicts(req, map[string]string{"И-4": "совет по лечению"})
	}}
	j := charterJudge{invariants.Judge{LLM: &llmtest.Fake{Fn: b.Chat}, Model: llm.DefaultModel}}
	bad, why, err := j.Violates(context.Background(), "чем лечить укус?", "Дайте 2 таблетки антигистаминного и приложите лёд.")
	if err != nil || !bad || !strings.Contains(why, "И-4") {
		t.Fatalf("нарушение: %v %q %v; судья звался %d раз", bad, why, err, b.Calls("judge"))
	}
	if bad, _, err := j.Violates(context.Background(), "где живёт рысь?", "Рысь живёт в тайге."); bad || err != nil {
		t.Fatalf("ложное нарушение: %v %v", bad, err)
	}
	if j.Name() == "" {
		t.Fatal("имя судьи")
	}
}
