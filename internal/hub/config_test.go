package hub

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	var names []string
	for _, s := range cfg.Servers {
		names = append(names, s.Name)
		if s.Title == "" || s.Color == "" || s.Expect == "" || len(s.Tools) == 0 {
			t.Errorf("%s: неполный %+v", s.Name, s)
		}
	}
	if !reflect.DeepEqual(names, []string{"sources", "daemon", "notes"}) {
		t.Errorf("серверы %v", names)
	}
	// Образец в корне репозитория — копия встроенной конфигурации.
	example, err := os.ReadFile(filepath.Join("..", "..", "mcp-servers.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(example, []byte("\r\n"), []byte("\n")),
		bytes.ReplaceAll(defaultJSON, []byte("\r\n"), []byte("\n"))) {
		t.Error("mcp-servers.example.json разошёлся с internal/hub/default.json")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig("", Defaults{DataDir: `D:\data`, DaemonURL: "http://127.0.0.1:9999"})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ServerConfig{}
	for _, s := range cfg.Servers {
		by[s.Name] = s
	}
	if got := by["sources"].Args; !reflect.DeepEqual(got, []string{"-mdd-sync=false", "-data", `D:\data`}) {
		t.Errorf("sources args %v", got)
	}
	if got := by["notes"].Args; !reflect.DeepEqual(got, []string{"-role", "notes", "-data", `D:\data`}) {
		t.Errorf("notes args %v", got)
	}
	if by["daemon"].URL != "http://127.0.0.1:9999" || by["daemon"].TokenEnv != "MCP_TOKEN" {
		t.Errorf("daemon %+v", by["daemon"])
	}
	// DefaultConfig не испорчен подстановкой (срезы не общие).
	if DefaultConfig().Servers[0].Args[2] != "${DATA}" {
		t.Error("подстановка изменила встроенную конфигурацию")
	}
	// Пустые значения — рабочий каталог и адрес демона по умолчанию.
	cfg, err = LoadConfig("", Defaults{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers[1].URL != feed.DefaultServer || cfg.Servers[0].Args[2] != "." {
		t.Errorf("умолчания: %+v", cfg.Servers)
	}
}

func TestLoadConfigFile(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "mcp-servers.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := LoadConfig(write(`{"servers":[
	  {"name":"a","transport":"stdio","command":"${DATA}/bin/a","args":["-data","${DATA}"],"tools":["x"]},
	  {"name":"b","transport":"http","url":"${DAEMON}/mcp","tools":["y_*"]}]}`),
		Defaults{DataDir: "/d", DaemonURL: "http://h:1"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers[0].Command != "/d/bin/a" || cfg.Servers[0].Args[1] != "/d" || cfg.Servers[1].URL != "http://h:1/mcp" {
		t.Errorf("подстановки: %+v", cfg.Servers)
	}

	for _, c := range []struct{ body, want string }{
		{`{"servers":[{"name":"a","transport":"stdio","tool":["x"]}]}`, "tool"},
		{`{"servers":[{"name":"a","transport":"stdio","tools":["x"]},{"name":"b","transport":"stdio","tools":["x"]}]}`, "x выдан двум серверам — a (x) и b (x)"},
		{`{"servers":[{"name":"a","transport":"stdio","args":["${NOPE}"],"tools":[]}]}`, "неизвестная подстановка"},
		{`не json`, ""},
	} {
		_, err := LoadConfig(write(c.body), Defaults{})
		if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.body, err)
		}
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "нет.json"), Defaults{}); !errors.Is(err, ErrConfig) {
		t.Errorf("нет файла: %v", err)
	}
}

func TestValidate(t *testing.T) {
	stdio := func(name string, tools ...string) ServerConfig {
		return ServerConfig{Name: name, Transport: TransportStdio, Tools: tools}
	}
	for _, c := range []struct {
		name string
		cfg  Config
		want string // "" — конфигурация верна
	}{
		{"верная", Config{Servers: []ServerConfig{stdio("a", "x", "y_*"), stdio("b", "z", "yy")}}, ""},
		{"пусто", Config{}, "нет ни одного"},
		{"без имени", Config{Servers: []ServerConfig{stdio("")}}, "нет имени"},
		{"дубль имени", Config{Servers: []ServerConfig{stdio("a"), stdio("a")}}, "дважды"},
		{"транспорт", Config{Servers: []ServerConfig{{Name: "a", Transport: "ws"}}}, "транспорт"},
		{"stdio с url", Config{Servers: []ServerConfig{{Name: "a", Transport: TransportStdio, URL: "http://x"}}}, "url"},
		{"http без url", Config{Servers: []ServerConfig{{Name: "a", Transport: TransportHTTP}}}, "нет url"},
		{"http с command", Config{Servers: []ServerConfig{{Name: "a", Transport: TransportHTTP, URL: "x", Command: "c"}}}, "command"},
		{"звезда в середине", Config{Servers: []ServerConfig{stdio("a", "m*d")}}, "в конце"},
		{"server_info", Config{Servers: []ServerConfig{stdio("a", "server_info")}}, "служебный"},
		{"имя и шаблон", Config{Servers: []ServerConfig{stdio("sources", "mdd_*"), stdio("daemon", "mdd_get")}},
			"mdd_get выдан двум серверам — sources (mdd_*) и daemon (mdd_get)"},
		{"два шаблона", Config{Servers: []ServerConfig{stdio("a", "nb_*"), stdio("b", "nb_o*")}}, "a (nb_*) и b (nb_o*)"},
		{"звезда", Config{Servers: []ServerConfig{stdio("a", "*"), stdio("b", "x")}}, "двум серверам"},
	} {
		err := c.cfg.Validate()
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, ждали %q", c.name, err, c.want)
		}
	}
}

func TestOverlap(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"x", "x", true}, {"x", "y", false}, {"mdd_*", "mdd_get", true}, {"mdd_get", "mdd_*", true},
		{"mdd_*", "md*", true}, {"mdd_*", "nb_*", false}, {"mdd_*", "mdd", false}, {"*", "anything", true},
	} {
		if got := overlap(c.a, c.b); got != c.want {
			t.Errorf("overlap(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func exe() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// alive — жив ли процесс. На Windows FindProcess открывает процесс по
// номеру и не находит завершённый; если нашёл (кто-то ещё держит его
// описатель), решает Wait: у завершённого он возвращается сразу. На
// остальных системах — сигнал 0.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS != "windows" {
		return p.Signal(syscall.Signal(0)) == nil
	}
	done := make(chan struct{})
	go func() {
		p.Wait()
		close(done)
	}()
	select {
	case <-done:
		return false
	case <-time.After(500 * time.Millisecond):
		return true
	}
}
