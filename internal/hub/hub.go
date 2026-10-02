package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
)

const (
	// connectTimeout — предел на подключение к одному серверу (initialize,
	// tools/list, server_info) после того, как бинарник найден. Серверы
	// локальные: дольше — значит, завис.
	connectTimeout = 15 * time.Second
	// resolveTimeout — предел на поиск бинарника stdio-сервера. Сборка из
	// исходников у Launcher ограничена своими 60 с; здесь запас сверху,
	// чтобы предел Launcher сработал первым и сказал словами, что не так.
	resolveTimeout = 90 * time.Second
)

// Hub — реестр серверов. Реализует Router. Безопасен для одновременных
// вызовов.
type Hub struct {
	cfg     Config
	log     *slog.Logger
	servers []*server

	connMu sync.Mutex // одно подключение за раз: Connect и первый Tools/Routes

	mu        sync.Mutex // состояние серверов и флаги ниже
	connected bool       // Connect уже был
	closed    bool
}

// server — сервер реестра: конфигурация, клиент и то, что о нём известно.
// Изменяемые поля — под Hub.mu.
type server struct {
	cfg      ServerConfig
	remote   *feed.Remote
	launcher *mcp.Launcher // у stdio через Launcher; иначе nil

	status, reason, hint string
	reported, version    string
	pid                  int
	list                 []*sdk.Tool
	calls                int
}

// Open собирает реестр по конфигурации; не подключается (это Connect или
// первый Tools/Routes).
//
// stdio-сервер запускается mcp.Launcher: пустой Command — бинарник
// animals-mcp, найденный как у механизма mcp (рядом с приложением → PATH →
// сборка из исходников). http-сервер — mcp.HTTPDialer с токеном из
// переменной TokenEnv; значение читается здесь и дальше не показывается.
func Open(cfg Config, log *slog.Logger) (*Hub, error) {
	return OpenWith(cfg, log, nil)
}

// OpenWith — Open с готовыми способами подключения по имени сервера:
// тесты подключают реестр к серверам в памяти процесса и за httptest, не
// запуская процессов. Сервер, которого нет в dial, подключается как в
// Open.
func OpenWith(cfg Config, log *slog.Logger, dial map[string]mcp.Dialer) (*Hub, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	h := &Hub{cfg: cfg, log: log}
	for _, sc := range cfg.Servers {
		s := &server{cfg: sc, status: StatusIdle}
		d, ok := dial[sc.Name]
		if !ok {
			switch sc.Transport {
			case TransportStdio:
				s.launcher = &mcp.Launcher{Path: sc.Command, Args: sc.Args, Logger: log.With("server", sc.Name)}
				d = s.launcher.Dial
			case TransportHTTP:
				token := ""
				if sc.TokenEnv != "" {
					token = os.Getenv(sc.TokenEnv)
				}
				d = mcp.HTTPDialer(sc.URL, token, nil)
			}
		}
		s.remote = feed.NewRemoteDial(sc.Name, d, log.With("server", sc.Name))
		h.servers = append(h.servers, s)
	}
	return h, nil
}

// Close закрывает соединения и останавливает stdio-процессы. Закрытие
// сессии stdio у SDK синхронное: закрыть stdin, подождать выхода, при
// нужде убить процесс — поэтому после Close дочерних процессов не
// остаётся. Собранный Launcher'ом бинарник удаляется после, когда процесс
// его уже не держит (на Windows файл запущенной программы не удалить).
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range h.servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.remote.Close()
			if s.launcher != nil {
				if err := s.launcher.Close(); err != nil {
					h.log.Warn("hub: собранный сервер не удалён", "server", s.cfg.Name, "err", err)
				}
			}
		}()
	}
	wg.Wait()
}

// Servers — состояние серверов без подключения.
func (h *Hub) Servers(ctx context.Context) []ServerView {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ServerView, 0, len(h.servers))
	for _, s := range h.servers {
		v := ServerView{
			Name: s.cfg.Name, Title: s.cfg.Title, Color: s.cfg.Color, Transport: s.cfg.Transport,
			Addr: s.addr(), Status: s.status, Reason: s.reason, Hint: s.hint,
			Reported: s.reported, Version: s.version, PID: s.pid, Calls: s.calls,
		}
		if s.status == StatusOK {
			for _, t := range s.list {
				if h.route(s, t.Name).Hidden {
					v.Hidden++
				} else {
					v.Tools++
				}
			}
		}
		out = append(out, v)
	}
	return out
}

// addr — куда подключаемся, для показа: URL или команда stdio. Токен в
// адрес не попадает — он живёт в заголовке.
func (s *server) addr() string {
	if s.cfg.Transport == TransportHTTP {
		return s.cfg.URL
	}
	cmd := s.cfg.Command
	if cmd == "" {
		cmd = mcp.BinaryName
	}
	return strings.TrimSpace(cmd + " " + strings.Join(s.cfg.Args, " "))
}

// Connect подключается ко всем серверам параллельно: initialize,
// tools/list, сверка имени, server_info (номер процесса). Ошибка — только
// если не поднялся ни один; частичный набор — нормальный случай.
//
// Повторный Connect переподключает то, что лежало: клиент поднимает
// соединение заново, если прежнее оборвалось.
func (h *Hub) Connect(ctx context.Context) error {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	return h.connect(ctx)
}

func (h *Hub) connect(ctx context.Context) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return errors.New("реестр MCP-серверов закрыт")
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range h.servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.connectOne(ctx, s)
		}()
	}
	wg.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()
	h.connected = true
	var fails []string
	for _, s := range h.servers {
		if s.status == StatusOK {
			return nil
		}
		fails = append(fails, s.cfg.Name+": "+s.reason)
	}
	return fmt.Errorf("ни один MCP-сервер не подключился — %s", strings.Join(fails, "; "))
}

// ensure — первый Tools/Routes подключается сам.
func (h *Hub) ensure(ctx context.Context) error {
	h.mu.Lock()
	done := h.connected
	h.mu.Unlock()
	if done {
		return nil
	}
	h.connMu.Lock()
	defer h.connMu.Unlock()
	h.mu.Lock()
	done = h.connected
	h.mu.Unlock()
	if done {
		return nil
	}
	return h.connect(ctx)
}

// connectOne — подключение к одному серверу; итог — в его статусе.
func (h *Hub) connectOne(ctx context.Context, s *server) {
	// Бинарник stdio-сервера ищется (и, может быть, собирается) до
	// подключения и со своим пределом: клиент дал бы на всё десять секунд,
	// а первая сборка после клонирования дольше.
	if s.launcher != nil {
		rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
		_, err := s.launcher.Resolve(rctx)
		cancel()
		if err != nil {
			h.setDown(s, err)
			return
		}
	}
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	list, ver, err := s.remote.List(cctx)
	if err != nil {
		h.setDown(s, err)
		return
	}
	reported := ""
	if init := s.remote.Init(); init != nil && init.ServerInfo != nil {
		reported = init.ServerInfo.Name
	}
	if s.cfg.Expect != "" && reported != s.cfg.Expect {
		h.mu.Lock()
		s.status, s.reported, s.version, s.pid, s.list = StatusMismatch, reported, ver, 0, nil
		s.reason = fmt.Sprintf("ответил сервер %q, а ждали %q — его инструменты модели не выдаются", reported, s.cfg.Expect)
		s.hint = s.hintMismatch()
		h.mu.Unlock()
		h.log.Warn("hub: не тот сервер", "server", s.cfg.Name, "reported", reported, "expect", s.cfg.Expect)
		return
	}
	pid := 0
	if raw, err := s.remote.Call(cctx, mcp.InfoTool, nil); err == nil {
		var info mcp.Info
		if json.Unmarshal(raw, &info) == nil {
			pid = info.PID
		}
	}
	h.mu.Lock()
	s.status, s.reason, s.hint = StatusOK, "", ""
	s.reported, s.version, s.pid, s.list = reported, ver, pid, list
	h.mu.Unlock()
	h.log.Info("hub: сервер подключён", "server", s.cfg.Name, "reported", reported, "version", ver,
		"pid", pid, "tools", len(list))
}

// setDown — сервер недоступен: down или denied (401), причина словами и
// подсказка. Инструменты его модели не выдаются, пока Connect не поднимет
// его снова.
func (h *Hub) setDown(s *server, err error) {
	status, hint := StatusDown, s.hintDown()
	if errors.Is(err, feed.ErrDenied) || errors.Is(err, mcp.ErrUnauthorized) {
		status, hint = StatusDenied, s.hintToken()
	}
	reason := s.unavailable(err).Error()
	h.mu.Lock()
	s.status, s.reason, s.hint, s.list = status, reason, hint, nil
	h.mu.Unlock()
	h.log.Warn("hub: сервер недоступен", "server", s.cfg.Name, "status", status, "err", reason)
}

// hintDown — как поднять сервер.
func (s *server) hintDown() string {
	if s.cfg.Transport == TransportHTTP {
		host := s.cfg.URL
		raw := host
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			host = u.Host
		}
		hint := "запусти animals-mcp -http " + host
		if s.cfg.TokenEnv != "" {
			hint += "; если задан " + s.cfg.TokenEnv + ", у сервера он должен быть тот же"
		}
		return hint
	}
	if s.cfg.Command != "" {
		return "проверь путь " + s.cfg.Command + "; бинарник собирается так: go build -o . ./cmd/animals-mcp"
	}
	return "пересобери animals-mcp: go build -o . ./cmd/animals-mcp"
}

// hintToken — что сделать при 401.
func (s *server) hintToken() string {
	env := s.cfg.TokenEnv
	if env == "" {
		return "сервер требует токен, а в конфигурации у него нет token_env"
	}
	if os.Getenv(env) == "" {
		return "задай " + env + " — тот же токен, с которым запущен сервер"
	}
	return "проверь " + env + ": у приложения и у сервера он должен совпадать"
}

// hintMismatch — ответил не тот сервер.
func (s *server) hintMismatch() string {
	if s.cfg.Transport == TransportHTTP {
		return "по адресу " + s.cfg.URL + " работает другой сервер: запусти там animals-mcp -http или поправь url"
	}
	return "бинарник старый или не тот: пересобери animals-mcp — go build -o . ./cmd/animals-mcp"
}

// unavailable — сбой клиента словами этого сервера. Тексты ErrDown и
// ErrDenied пакета feed говорят про демон «Интересных фактов»: для
// блокнота или источников они неверны. Исходная ошибка остаётся в цепочке
// (errors.Is(err, feed.ErrDown) работает).
func (s *server) unavailable(err error) error {
	var te *feed.ToolError
	if errors.As(err, &te) {
		return err
	}
	msg := err.Error()
	switch {
	case errors.Is(err, feed.ErrDenied), errors.Is(err, mcp.ErrUnauthorized):
		msg = "отверг токен (401) — " + s.hintToken()
	case errors.Is(err, feed.ErrDown):
		msg = strings.TrimPrefix(msg, feed.ErrDown.Error())
		msg = strings.TrimPrefix(msg, ": ")
		if msg == "" {
			msg = "не отвечает"
		}
	}
	return &callError{msg: fmt.Sprintf("MCP-сервер %s недоступен: %s", s.cfg.Name, msg), err: err}
}

// callError — текст для человека и модели, исходная ошибка — для errors.Is.
type callError struct {
	msg string
	err error
}

func (e *callError) Error() string { return e.msg }
func (e *callError) Unwrap() error { return e.err }
