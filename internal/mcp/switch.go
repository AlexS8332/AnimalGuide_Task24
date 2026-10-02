package mcp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// События журнала хода.
const (
	// EventConnect — с каким сервером идёт ход: имя, версия, протокол,
	// номер процесса.
	EventConnect = "mcp.connect"
	// EventTools — что сервер ответил на tools/list: каждый инструмент со
	// своей inputSchema.
	EventTools = "mcp.tools"
)

// Switch выбирает путь до инструментов источников по механизмам диалога:
// механизм mcp выключен — инструменты в процессе, включён — через сервер.
// Реализует agents.ToolSets; агенты и прогоны про MCP не знают.
type Switch struct {
	Local  *tools.Registry
	Client *Client
	// How — откуда взят бинарник сервера (для журнала и окна); может быть
	// nil.
	How func() string
}

// For — реестр на ход. Выключенный механизм не запускает процесс вовсе
// (ФТ-45). Если сервер не поднялся на старте хода, ход идёт в процессе,
// механизм в итоговом наборе выключен, причина уходит в журнал (ФТ-48).
// Внутри хода путь не меняется: упавший посреди хода сервер даёт модели
// ошибку словами, а не тихий переход на локальный вызов.
func (s *Switch) For(ctx context.Context, fs features.Set) (*tools.Registry, features.Set, string, error) {
	if !fs.On(features.MCP) {
		return s.Local, fs, "", nil
	}
	if s.Client == nil {
		return s.Local, fs.With(features.MCP, false), "MCP-сервер не настроен — ход идёт в процессе", nil
	}
	reg, fresh, err := s.Client.Registry(ctx)
	if err != nil {
		return s.Local, fs.With(features.MCP, false), "MCP-сервер недоступен — ход идёт в процессе: " + err.Error(), nil
	}
	s.announce(agent.EmitterFrom(ctx), fresh)
	return reg, fs, "", nil
}

// announce — события mcp.connect и mcp.tools в журнал хода. Они пишутся на
// каждом ходе с MCP, а не только на подключении: журнал читают по ходу, и
// у каждого хода должно быть видно, через какой сервер он шёл.
func (s *Switch) announce(em agent.Emitter, fresh bool) {
	st := s.Client.State()
	how := ""
	if s.How != nil {
		how = s.How()
	}
	if cn := st.Conn; cn != nil {
		state := "соединение открыто раньше"
		if fresh {
			state = "подключились"
			if cn.N > 1 {
				state = fmt.Sprintf("перезапуск №%d", cn.N-1)
			}
		}
		title := fmt.Sprintf("MCP: %s %s, протокол %s", cn.Server, cn.Version, cn.Protocol)
		if cn.PID > 0 {
			title += fmt.Sprintf(", pid %d", cn.PID)
		}
		detail := "initialize → tools/list → сверка отпечатка: " + state
		if how != "" {
			detail += "\nбинарник: " + how
		}
		em.Log(agent.Event{Agent: "mcp", Kind: EventConnect, Mechanism: string(features.MCP), Via: tools.ViaMCP,
			Title: title + " — " + state, Detail: detail,
			Data: map[string]any{"server": cn.Server, "version": cn.Version, "protocol": cn.Protocol, "pid": cn.PID, "n": cn.N, "fresh": fresh, "binary": how}})
	}
	names := make([]string, len(st.Tools))
	schemas := make([]map[string]any, len(st.Tools))
	var b strings.Builder
	for i, t := range st.Tools {
		names[i] = t.Name
		schemas[i] = map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
		fmt.Fprintf(&b, "• %s %s\n", t.Name, string(t.InputSchema))
	}
	if len(st.Skipped) > 0 {
		fmt.Fprintf(&b, "пропущены (не инструменты источников): %s\n", strings.Join(st.Skipped, ", "))
	}
	em.Log(agent.Event{Agent: "mcp", Kind: EventTools, Mechanism: string(features.MCP), Via: tools.ViaMCP,
		Title:  fmt.Sprintf("MCP: tools/list — %d инструментов, отпечаток %s сошёлся с локальным", len(names), st.Fingerprint),
		Detail: strings.TrimSpace(b.String()),
		Data:   map[string]any{"tools": schemas, "skipped": st.Skipped, "fingerprint": st.Fingerprint}})
}

// View — состояние для окна «MCP-сервер» и пульта.
type View struct {
	State
	Binary string `json:"binary,omitempty"`
	// Server — счётчики самого сервера (server_info); только если сервер
	// жив и окно их попросило.
	Server      *Info  `json:"server,omitempty"`
	ServerError string `json:"serverError,omitempty"`
}

// Extension — раздел API: GET /api/mcp — состояние клиента; с ?server=1 —
// ещё и счётчики самого сервера. Процесс ради этого не запускается.
func (s *Switch) Extension() []server.Extension {
	return []server.Extension{{Prefix: "/api/mcp", Handler: http.HandlerFunc(s.handle)}}
}

func (s *Switch) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		server.WriteError(w, http.StatusMethodNotAllowed, "нужен GET")
		return
	}
	v := View{State: State{Status: StatusOff, Tools: []ToolInfo{}}}
	if s.Client != nil {
		v.State = s.Client.State()
	}
	if s.How != nil {
		v.Binary = s.How()
	}
	if r.URL.Query().Get("server") != "" && s.Client != nil && v.Status == StatusReady {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if info, err := s.Client.ServerInfo(ctx); err != nil {
			v.ServerError = err.Error()
		} else {
			v.Server = info
		}
	}
	server.WriteJSON(w, http.StatusOK, v)
}
