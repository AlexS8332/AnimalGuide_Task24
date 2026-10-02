package hubapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/hub"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
)

// Коды ответов раздела: 400 — неверный запрос или неизвестная заготовка;
// 404 — нет такого прогона или раздела; 405 — не тот метод; 409 — флоу уже
// идёт (в теле id идущего: одновременно идёт один прогон — он платный, и
// счётчики серверов до/после должны принадлежать одному флоу); 503 — реестр
// выключен или у приложения нет модели (в теле why и hint).

const (
	// readTimeout — предел запроса состояния серверов (server_info, маршруты).
	readTimeout = 10 * time.Second
	// connectTimeout — предел подключения ко всем серверам: stdio-процессы
	// поднимаются, демон может отвечать не сразу.
	connectTimeout = 30 * time.Second
	// maxBody — предел тела POST.
	maxBody = 64 << 10
)

// hintNoRun — что делать, если флоу недоступен, а причина не названа.
const hintNoRun = "флоу ведёт модель: задайте DEEPSEEK_API_KEY (или впишите ключ в .env.local) и перезапустите приложение; " +
	"реестр серверов включается конфигурацией mcp-servers.json"

// ServersView — ответ GET servers и POST connect. Why — почему реестр или
// флоу выключены; CanRun — можно ли запустить флоу (есть и модель, и
// реестр). ConnectError — Connect не поднял ни одного сервера (статусы и
// причины — в Servers).
type ServersView struct {
	Servers      []hub.ServerView `json:"servers"`
	Routes       []hub.Route      `json:"routes"`
	Why          string           `json:"why,omitempty"`
	CanRun       bool             `json:"canRun"`
	RoutesError  string           `json:"routesError,omitempty"`
	ConnectError string           `json:"connectError,omitempty"`
}

// PresetView — заготовка в списке GET presets.
type PresetView struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Species string `json:"species"`
}

func (a *API) handle(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(Prefix, "/")), "/")
	switch {
	case path == "servers":
		if !method(w, r, http.MethodGet) {
			return
		}
		server.WriteJSON(w, http.StatusOK, a.servers(r.Context()))
	case path == "connect":
		if !method(w, r, http.MethodPost) {
			return
		}
		a.connect(w, r)
	case path == "presets":
		if !method(w, r, http.MethodGet) {
			return
		}
		out := []PresetView{}
		for _, p := range a.presets() {
			out = append(out, PresetView{ID: p.ID, Title: p.Title, Species: p.Species})
		}
		server.WriteJSON(w, http.StatusOK, out)
	case path == "flows":
		switch r.Method {
		case http.MethodGet:
			server.WriteJSON(w, http.StatusOK, map[string]any{"flows": a.list()})
		case http.MethodPost:
			a.start(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			server.WriteError(w, http.StatusMethodNotAllowed, "нужен GET или POST")
		}
	case strings.HasPrefix(path, "flows/"):
		if !method(w, r, http.MethodGet) {
			return
		}
		id := strings.TrimPrefix(path, "flows/")
		v, ok := a.view(id)
		if !ok {
			server.WriteError(w, http.StatusNotFound, "прогона "+id+" нет — прогоны живут в памяти приложения, последние "+itoa(keepRuns))
			return
		}
		server.WriteJSON(w, http.StatusOK, v)
	default:
		server.WriteError(w, http.StatusNotFound, "нет такого раздела: "+Prefix+path)
	}
}

// method — метод запроса тот; иначе 405 уже записан.
func method(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method == want {
		return true
	}
	w.Header().Set("Allow", want)
	server.WriteError(w, http.StatusMethodNotAllowed, "нужен "+want)
	return false
}

// presets — заготовки раздела; не заданы — встроенные flow.Presets.
func (a *API) presets() []flow.Preset {
	if len(a.Presets) > 0 {
		return a.Presets
	}
	return flow.Presets()
}

// findPreset — заготовка по id; пустой id — первая.
func (a *API) findPreset(id string) (flow.Preset, bool) {
	list := a.presets()
	if len(list) == 0 {
		return flow.Preset{}, false
	}
	if id == "" {
		return list[0], true
	}
	for _, p := range list {
		if p.ID == id {
			return p, true
		}
	}
	return flow.Preset{}, false
}

// why — почему что-то выключено: Why или слова по умолчанию.
func (a *API) why() string {
	if a.Why != "" {
		return a.Why
	}
	switch {
	case a.Router == nil:
		return "реестр MCP-серверов выключен"
	case a.Run == nil:
		return "у приложения нет модели — флоу вести некому"
	}
	return ""
}

// servers — состояние реестра без подключения. Маршруты спрашиваем, только
// если хоть один сервер уже подключён: у реестра первый Routes подключается
// сам, а открытое окно не должно поднимать процессы — это делает кнопка
// «Подключить все» (POST connect).
func (a *API) servers(ctx context.Context) ServersView {
	v := ServersView{Servers: []hub.ServerView{}, Routes: []hub.Route{}, Why: a.why(),
		CanRun: a.Router != nil && a.Run != nil}
	if a.Router == nil {
		return v
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	if list := a.Router.Servers(ctx); list != nil {
		v.Servers = list
	}
	connected := false
	for _, s := range v.Servers {
		if s.Status == hub.StatusOK {
			connected = true
			break
		}
	}
	if !connected {
		return v
	}
	routes, err := a.Router.Routes(ctx)
	if err != nil {
		v.RoutesError = err.Error()
	}
	if routes != nil {
		v.Routes = routes
	}
	return v
}

func (a *API) connect(w http.ResponseWriter, r *http.Request) {
	if a.Router == nil {
		unavailable(w, a.why(), "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), connectTimeout)
	err := a.Router.Connect(ctx)
	cancel()
	v := a.servers(r.Context())
	if err != nil {
		v.ConnectError = err.Error()
	}
	server.WriteJSON(w, http.StatusOK, v)
}

// unavailable — 503 с причиной и подсказкой.
func unavailable(w http.ResponseWriter, why, hint string) {
	if hint == "" {
		hint = hintNoRun
	}
	msg := "флоу недоступен: " + why
	server.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msg, "why": why, "hint": hint})
}

// readBody — тело POST не больше maxBody; пустое тело — пустой объект.
func readBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errors.New("тело запроса больше 64 КБ")
		}
		return errors.New("тело запроса не разобралось: " + err.Error())
	}
	return nil
}
