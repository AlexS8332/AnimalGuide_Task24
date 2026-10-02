package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

// syncBuffer — буфер журнала, в который пишут несколько горутин.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Настоящий cmd/animals-mcp: собирается из исходников тем же путём, что и
// при `go run .`, говорит по stdio, умирает посреди вызова и поднимается
// следующим вызовом.
func TestRealServerOverStdio(t *testing.T) {
	if testing.Short() {
		t.Skip("собирает и запускает сервер; пропущен при -short")
	}
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	defer wiki.Close()
	defer gbif.Close()
	// GBIF, который умеет тормозить: сервер убивают, пока он ждёт ответа.
	var slow atomic.Bool
	gate := make(chan struct{})
	slowGBIF := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		gbif.Config.Handler.ServeHTTP(w, r)
	}))
	defer slowGBIF.Close()
	defer close(gate)

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	l := &Launcher{Args: []string{"-wiki-base", wiki.URL, "-gbif-base", slowGBIF.URL, "-v",
		"-data", t.TempDir(), "-mdd-sync=false"}, Logger: logger}
	defer l.Close()
	local := tools.LocalTools(tools.NewFetcher(), wiki.URL, slowGBIF.URL)
	c := NewClient(Options{Dial: l.Dial, Want: tools.Fingerprint(local), Logger: logger})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	started := time.Now()
	reg, fresh, err := c.Registry(ctx)
	if err != nil || !fresh {
		t.Fatalf("подключение: %v\n%s", err, logs)
	}
	t.Logf("холодный старт со сборкой: %s", time.Since(started).Round(time.Millisecond))
	if l.How() != "собран из исходников" {
		t.Fatalf("бинарник: %q", l.How())
	}
	match, _ := reg.Get("match_taxon")
	args := json.RawMessage(`{"scientific_name":"Lynx lynx"}`)
	want, _ := tools.MustRegistry(local...).Pick("match_taxon")[0].Call(ctx, args)
	if got, err := match.Call(ctx, args); err != nil || got != want {
		t.Fatalf("вызов: %v\n%s\n%s", err, got, want)
	}
	first := c.State().Conn
	info, err := c.ServerInfo(ctx)
	if err != nil || first.PID == 0 || info.PID != first.PID || info.Calls["match_taxon"] != 1 {
		t.Fatalf("server_info: %v %+v, pid клиента %d", err, info, first.PID)
	}

	// Сервер убит посреди вызова: модель получает ошибку словами.
	slow.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := match.Call(ctx, json.RawMessage(`{"scientific_name":"Otocolobus manul"}`))
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if err := c.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.HasPrefix(err.Error(), "MCP-сервер недоступен: ") {
			t.Fatalf("вызов посреди падения: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("вызов повис после падения сервера")
	}
	slow.Store(false)
	waitStatus(t, c, StatusDead)

	// Следующий вызов поднимает новый процесс.
	if got, err := match.Call(ctx, args); err != nil || got != want {
		t.Fatalf("после перезапуска: %v %s", err, got)
	}
	second := c.State().Conn
	if second.N != 2 || second.PID == first.PID || second.PID == 0 {
		t.Fatalf("перезапуск: было %+v, стало %+v", first, second)
	}
	if !strings.Contains(logs.String(), "mcp: ") || !strings.Contains(logs.String(), "вызов инструмента") {
		t.Fatalf("stderr сервера не попал в журнал:\n%s", logs)
	}
}

func TestLauncherResolve(t *testing.T) {
	ctx := context.Background()
	if _, err := (&Launcher{Path: filepath.Join(t.TempDir(), "нет")}).Resolve(ctx); err == nil || !strings.Contains(err.Error(), "-mcp-server") {
		t.Fatalf("несуществующий путь: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "srv")
	os.WriteFile(bin, nil, 0o755)
	l := &Launcher{Path: bin}
	if p, err := l.Resolve(ctx); err != nil || p != bin || l.How() != "флаг -mcp-server" {
		t.Fatalf("флаг: %q %v %q", p, err, l.How())
	}
	if _, cmd, err := l.Dial(ctx); err != nil || cmd == nil || cmd.Path != bin {
		t.Fatalf("Dial: %v", err)
	}
	if _, _, err := (&Launcher{Path: filepath.Join(t.TempDir(), "нет")}).Dial(ctx); err == nil {
		t.Fatal("Dial без бинарника")
	}

	// Рядом с исполняемым файлом.
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	near := filepath.Join(filepath.Dir(exe), BinaryName+exeSuffix())
	if _, err := os.Stat(near); os.IsNotExist(err) {
		if err := os.WriteFile(near, nil, 0o755); err == nil {
			defer os.Remove(near)
			l := &Launcher{}
			if p, err := l.Resolve(ctx); err != nil || p != near || l.How() != "рядом с приложением" {
				t.Fatalf("рядом: %q %v %q", p, err, l.How())
			}
			os.Remove(near)
		}
	}

	// Ни бинарника, ни исходников.
	t.Setenv("PATH", t.TempDir())
	if _, err := (&Launcher{Dir: t.TempDir()}).Resolve(ctx); err == nil || !strings.Contains(err.Error(), "не найден") {
		t.Fatalf("без исходников: %v", err)
	}
	// Исходники есть, а go нет.
	root, ok := moduleRoot("")
	if !ok {
		t.Fatal("корень модуля не найден")
	}
	if _, err := (&Launcher{Dir: root}).Resolve(ctx); err == nil || !strings.Contains(err.Error(), "go не в PATH") {
		t.Fatalf("без go: %v", err)
	}
	if err := (&Launcher{}).Close(); err != nil {
		t.Fatal(err)
	}
}
