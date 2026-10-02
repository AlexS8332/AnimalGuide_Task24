package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// BinaryName — имя бинарника сервера без расширения.
const BinaryName = "animals-mcp"

// serverPackage — пакет сервера относительно корня модуля.
const serverPackage = "cmd/animals-mcp"

// buildTimeout — предел сборки сервера из исходников: первый запуск после
// клонирования собирает SDK, это десятки секунд.
const buildTimeout = 60 * time.Second

// Launcher находит и запускает процесс сервера. Порядок поиска:
//
//  1. путь из флага -mcp-server;
//  2. бинарник рядом с исполняемым файлом приложения;
//  3. бинарник в PATH;
//  4. исходники рядом (запуск через `go run .`): сервер собирается один раз
//     во временный каталог с пределом 60 с — как `go run ./cmd/animals-mcp`,
//     но процесс сервера тогда свой, а не дочерний у `go`: его номер в
//     журнале настоящий, и убить его можно, не оставив сироту.
type Launcher struct {
	// Path — путь из флага; пусто — искать.
	Path string
	// Args — аргументы сервера.
	Args   []string
	Logger *slog.Logger
	// Dir — где искать исходники; пусто — рабочий каталог и выше.
	Dir string

	mu    sync.Mutex
	built string
	tmp   string
	how   string
}

// Dial — Dialer для клиента: новый процесс сервера на каждое подключение.
func (l *Launcher) Dial(ctx context.Context) (sdk.Transport, *exec.Cmd, error) {
	path, err := l.Resolve(ctx)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(path, l.Args...)
	cmd.Stderr = &logWriter{log: l.logger()}
	return &sdk.CommandTransport{Command: cmd, TerminateDuration: 2 * time.Second}, cmd, nil
}

// How — откуда взят бинарник: флаг, рядом, PATH, сборка.
func (l *Launcher) How() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.how
}

// Resolve — путь к бинарнику сервера по порядку поиска.
func (l *Launcher) Resolve(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Path != "" {
		if _, err := os.Stat(l.Path); err != nil {
			return "", fmt.Errorf("сервер из -mcp-server не найден: %w", err)
		}
		l.how = "флаг -mcp-server"
		return l.Path, nil
	}
	name := BinaryName + exeSuffix()
	if exe, err := os.Executable(); err == nil {
		near := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(near); err == nil {
			l.how = "рядом с приложением"
			return near, nil
		}
	}
	if p, err := exec.LookPath(BinaryName); err == nil {
		l.how = "PATH"
		return p, nil
	}
	if l.built != "" {
		if _, err := os.Stat(l.built); err == nil {
			return l.built, nil
		}
	}
	p, err := l.build(ctx)
	if err != nil {
		return "", err
	}
	l.built, l.how = p, "собран из исходников"
	return p, nil
}

// build собирает сервер из исходников модуля. Вызывается под замком.
func (l *Launcher) build(ctx context.Context) (string, error) {
	root, ok := moduleRoot(l.Dir)
	if !ok {
		return "", fmt.Errorf("бинарник %s не найден: ни флага -mcp-server, ни файла рядом с приложением, ни в PATH, ни исходников %s рядом", BinaryName, serverPackage)
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("бинарник %s не найден, а собрать его нечем: go не в PATH", BinaryName)
	}
	if l.tmp == "" {
		if l.tmp, err = os.MkdirTemp("", "animalguide-mcp-"); err != nil {
			return "", fmt.Errorf("каталог для сборки сервера: %w", err)
		}
	}
	out := filepath.Join(l.tmp, BinaryName+exeSuffix())
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gobin, "build", "-o", out, "./"+serverPackage)
	cmd.Dir = root
	started := time.Now()
	if msg, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("сборка %s не уложилась в %s", serverPackage, buildTimeout)
		}
		return "", fmt.Errorf("сборка %s: %v: %s", serverPackage, err, strings.TrimSpace(string(msg)))
	}
	l.logger().Info("mcp: сервер собран из исходников", "path", out, "seconds", time.Since(started).Seconds())
	return out, nil
}

// Close удаляет собранный бинарник.
func (l *Launcher) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tmp == "" {
		return nil
	}
	err := os.RemoveAll(l.tmp)
	l.tmp, l.built = "", ""
	return err
}

func (l *Launcher) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

// moduleRoot ищет вверх от dir (или рабочего каталога) каталог с go.mod и
// исходниками сервера.
func moduleRoot(dir string) (string, bool) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", false
		}
		dir = wd
	}
	for {
		_, errMod := os.Stat(filepath.Join(dir, "go.mod"))
		_, errPkg := os.Stat(filepath.Join(dir, filepath.FromSlash(serverPackage)))
		if errMod == nil && errPkg == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// logWriter — stderr сервера построчно в журнал приложения с префиксом
// «mcp:»: stdout сервера занят протоколом, всё человеческое идёт сюда.
type logWriter struct {
	log *slog.Logger
	mu  sync.Mutex
	buf []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.emit(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *logWriter) emit(line string) {
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) != "" {
		w.log.Info("mcp: " + line)
	}
}
