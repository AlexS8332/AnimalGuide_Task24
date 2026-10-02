// Package hub — реестр MCP-серверов справочника: к каким серверам
// подключаться, какие инструменты каждого выдавать модели и куда уходит
// каждый вызов.
//
// Серверов по умолчанию три: sources (stdio — Википедия и GBIF), daemon
// (HTTP — MDD и выпуски «Интересных фактов») и notes (stdio — блокнот
// натуралиста). Модель видит исходные имена инструментов, без префиксов:
// каждое имя закреплено ровно за одним сервером явным списком Tools в
// конфигурации, поэтому маршрут однозначно следует из имени. Одно имя в
// списках двух серверов — ошибка конфигурации (ErrConfig), а не «кто первый
// в файле». Всё, что сервер отдал в tools/list, но чего нет в его списке,
// модели не выдаётся и видно в Routes со скрытой строкой и причиной:
// «дубль → sources» (у демона те же источники, что у stdio-сервера) или
// «не разрешён» (платные run_now, summarize…; запрет держится отсутствием
// инструмента, ИП-7). server_info не выдаётся никогда — его читает Snapshot.
//
// Сервер опознаётся по имени в ответе initialize (Expect): если по адресу
// ответил не тот сервер (старый бинарник, чужой процесс), его статус —
// mismatch, и инструменты его модели не выдаются.
//
// Недоступный сервер не роняет реестр: у него статус down и подсказка, а
// инструменты остальных работают.
package hub

import (
	"context"
	"errors"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Transport — как подключаться к серверу.
type Transport string

const (
	TransportStdio Transport = "stdio" // дочерний процесс (mcp.Launcher)
	TransportHTTP  Transport = "http"  // Streamable HTTP (mcp.HTTPDialer)
)

// ServerConfig — один сервер в конфигурации (mcp-servers.json).
//
// Command — путь к бинарнику stdio-сервера; пусто — animals-mcp, найденный
// как у механизма mcp (рядом с приложением → PATH → сборка из исходников).
// В Args и URL подставляются ${DATA} (каталог данных приложения) и
// ${DAEMON} (адрес демона из -facts-server). TokenEnv — имя переменной
// окружения с Bearer-токеном HTTP-сервера; значение в конфиг не пишется.
// Tools — какие инструменты сервера выдавать модели; допускается шаблон с
// «*» в конце (mdd_*).
type ServerConfig struct {
	Name      string    `json:"name"`
	Title     string    `json:"title"`
	Color     string    `json:"color,omitempty"` // цвет дорожки в интерфейсе, CSS
	Transport Transport `json:"transport"`
	Command   string    `json:"command,omitempty"`
	Args      []string  `json:"args,omitempty"`
	URL       string    `json:"url,omitempty"`
	TokenEnv  string    `json:"token_env,omitempty"`
	Expect    string    `json:"expect,omitempty"` // имя сервера в initialize; пусто — не сверять
	Tools     []string  `json:"tools"`
}

// Config — реестр целиком. Порядок серверов — порядок показа и порядок
// инструментов для модели (стабильный — кэш префикса у DeepSeek).
type Config struct {
	Servers []ServerConfig `json:"servers"`
}

// Defaults — значения подстановок в конфигурации.
type Defaults struct {
	DataDir   string // ${DATA}
	DaemonURL string // ${DAEMON}
}

// Route — куда уходит инструмент. Hidden — сервер его отдаёт, но модели он
// не выдан; Reason — почему («дубль → sources», «не разрешён»).
type Route struct {
	Tool   string `json:"tool"`
	Server string `json:"server"`
	Hidden bool   `json:"hidden,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Статусы сервера в ServerView.
const (
	StatusIdle     = "idle"     // ещё не подключались
	StatusOK       = "ok"       // подключён, имя совпало
	StatusDown     = "down"     // не отвечает / процесс не поднялся
	StatusDenied   = "denied"   // HTTP 401: токен
	StatusMismatch = "mismatch" // ответил не тот сервер (Expect)
)

// ServerView — сервер для показа: окно «MCP-серверы», CLI -servers.
type ServerView struct {
	Name      string    `json:"name"`
	Title     string    `json:"title"`
	Color     string    `json:"color,omitempty"`
	Transport Transport `json:"transport"`
	Addr      string    `json:"addr"` // URL или команда stdio (без токена)
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	Hint      string    `json:"hint,omitempty"`
	Reported  string    `json:"reported,omitempty"` // имя из initialize
	Version   string    `json:"version,omitempty"`
	PID       int       `json:"pid,omitempty"`
	Tools     int       `json:"tools"`  // выдано модели
	Hidden    int       `json:"hidden"` // скрыто
	Calls     int       `json:"calls"`  // вызовов через реестр за жизнь процесса
}

// Bound — инструмент, выданный модели, и его маршрут. Tool.Call уходит
// на сервер Route.Server; Spec — как сервер отдал её в tools/list (Via —
// tools.ViaMCP, Untrusted).
type Bound struct {
	Route Route
	Tool  tools.Tool
}

// Snapshot — server_info каждого подключённого сервера: счётчики вызовов по
// инструментам и PID. Разница снимков до и после флоу — свидетельство
// самих серверов, что вызов дошёл именно до них.
type Snapshot map[string]mcp.Info

// Router — то, чем пользуются флоу, REST и CLI. *Hub его реализует; тесты
// флоу подставляют свой.
type Router interface {
	// Servers — состояние серверов без подключения.
	Servers(ctx context.Context) []ServerView
	// Connect подключается ко всем серверам (initialize, tools/list, сверка
	// имени). Ошибка — только если не поднялся ни один; частичный набор —
	// нормальный случай, статусы в Servers.
	Connect(ctx context.Context) error
	// Routes — маршруты всех инструментов всех подключённых серверов,
	// включая скрытые, в порядке конфигурации.
	Routes(ctx context.Context) ([]Route, error)
	// Tools — инструменты для модели (без скрытых), в порядке конфигурации.
	Tools(ctx context.Context) ([]Bound, error)
	// Snapshot — server_info подключённых серверов.
	Snapshot(ctx context.Context) (Snapshot, error)
}

// ErrConfig — конфигурация реестра неверна: ошибка оборачивает его.
var ErrConfig = errors.New("конфигурация MCP-серверов")

var _ Router = (*Hub)(nil)
