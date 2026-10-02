// Package server — HTTP API и раздача фронтенда. Обработчики ничего не
// знают об агентах: они заводят диалоги и ходы, отдают состояние и поток
// событий хода. Состояние прогона живёт на сервере, идентификатор — в
// адресе страницы (ФТ-40).
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
)

const (
	maxRequestBody    = 64 << 10
	keepAliveInterval = 20 * time.Second
)

// Extension — раздел API, который приносит механизм (профиль, память,
// подборка, свод): свой префикс пути и свой обработчик. Ядро сервера про
// механизмы не знает (Р-2 для интерфейса).
type Extension struct {
	Prefix  string
	Handler http.Handler
}

// Server — HTTP-обработчик приложения.
type Server struct {
	runs    *runs.Manager
	mux     *http.ServeMux
	started time.Time
	meta    map[string]any
}

// New собирает сервер. static — встроенный фронтенд; meta — сведения для
// интерфейса, которые дополняют механизмы (например, анкета профиля).
func New(m *runs.Manager, static fs.FS, meta map[string]any, ext ...Extension) *Server {
	s := &Server{runs: m, mux: http.NewServeMux(), started: time.Now(), meta: meta}
	s.mux.Handle("/", http.FileServer(http.FS(static)))
	s.mux.HandleFunc("/api/meta", s.handleMeta)
	s.mux.HandleFunc("/api/conversations", s.handleConversations)
	s.mux.HandleFunc("/api/conversations/", s.handleConversation)
	s.mux.HandleFunc("/api/turns/", s.handleTurn)
	s.mux.HandleFunc("/api/groups", s.handleGroups)
	s.mux.HandleFunc("/api/groups/", s.handleGroup)
	for _, e := range ext {
		s.mux.Handle(e.Prefix, e.Handler)
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// handleMeta — всё, что интерфейсу нужно знать о продукте: реестр
// механизмов, разделы карточки, модель, каталоги и время старта (по нему
// лента отмечает шов «сервер перезапущен»). Реестр уходит с сервера целиком:
// вторая копия в JavaScript рано или поздно разошлась бы с кодом.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{
		"mechanisms":    s.runs.Registry().Describe(s.runs.Defaults()),
		"topics":        card.Topics,
		"aspects":       card.Aspects,
		"historyDir":    s.runs.DisplayDir(),
		"serverStarted": s.started,
		"pid":           os.Getpid(),
	}
	for k, v := range s.meta {
		payload[k] = v
	}
	writeJSON(w, http.StatusOK, payload)
}

// turnRequest — тело запроса на ход.
type turnRequest struct {
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Name     string `json:"name"`
	CardID   string `json:"cardId"`
	Topic    string `json:"topic"`
	NodeKey  int    `json:"nodeKey"`
	NodeName string `json:"nodeName"`
	A        string `json:"a"`
	B        string `json:"b"`
}

func (t turnRequest) request() agents.Request {
	return agents.Request{Kind: t.Kind, Text: strings.TrimSpace(t.Text), Name: t.Name, CardID: t.CardID, Topic: t.Topic,
		NodeKey: t.NodeKey, NodeName: t.NodeName, A: t.A, B: t.B}
}

type startRequest struct {
	turnRequest
	// Features — механизмы нового диалога строкой флага: «+mcp,-guard»;
	// пусто — умолчания сервера.
	Features string   `json:"features"`
	Owners   []string `json:"owners"`
	Title    string   `json:"title"`
	// Empty — завести диалог без первого хода.
	Empty bool `json:"empty"`
}

func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"conversations": s.runs.List()})
	case http.MethodPost:
		var body startRequest
		if !readJSON(w, r, &body) {
			return
		}
		fs, err := s.runs.Registry().Parse(body.Features, s.runs.Defaults())
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		opts := runs.StartOptions{Request: body.request(), Features: fs, Owners: body.Owners, Title: body.Title}
		if body.Empty {
			d, err := s.runs.Create(opts)
			if err != nil {
				writeError(w, statusOf(err), err.Error())
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"conversation": d})
			return
		}
		sess, err := s.runs.Start(opts)
		if err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, turnPayload(sess.View()))
	default:
		writeError(w, http.StatusMethodNotAllowed, "нужен GET или POST")
	}
}

func turnPayload(v runs.View) map[string]any {
	return map[string]any{"turn": v, "conversationId": v.ConversationID, "events": "/api/turns/" + v.ID + "/events"}
}

// handleConversation — /api/conversations/{id}[/действие].
func (s *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/conversations/")
	id, action, _ := strings.Cut(rest, "/")
	if id == "" {
		writeError(w, http.StatusNotFound, "не указан диалог")
		return
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		d, ok := s.runs.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, runs.ErrNotFound.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"conversation": d})
	case action == "" && r.Method == http.MethodDelete:
		if err := s.runs.Delete(id); err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	case action == "turns" && r.Method == http.MethodPost:
		var body turnRequest
		if !readJSON(w, r, &body) {
			return
		}
		sess, err := s.runs.Send(id, body.request())
		if err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, turnPayload(sess.View()))
	case action == "checkpoints" && r.Method == http.MethodPost:
		var body struct{ Name string }
		if !readJSON(w, r, &body) {
			return
		}
		s.detail(w, func() (runs.Detail, error) { return s.runs.Mark(id, body.Name) })
	case action == "branches" && r.Method == http.MethodPost:
		var body struct {
			Checkpoint string `json:"checkpoint"`
			Name       string `json:"name"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		s.detail(w, func() (runs.Detail, error) { return s.runs.Fork(id, body.Checkpoint, body.Name) })
	case action == "switch" && r.Method == http.MethodPost:
		var body struct{ Branch string }
		if !readJSON(w, r, &body) {
			return
		}
		s.detail(w, func() (runs.Detail, error) { return s.runs.Switch(id, body.Branch) })
	case action == "features" && r.Method == http.MethodPost:
		var body struct {
			Name string `json:"name"`
			On   bool   `json:"on"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		s.detail(w, func() (runs.Detail, error) { return s.runs.SetFeature(id, features.Name(body.Name), body.On) })
	case action == "raw" && r.Method == http.MethodGet:
		path, data, err := s.runs.Raw(id)
		if err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": path, "json": string(data)})
	case action == "export" && r.Method == http.MethodGet:
		name, md, err := s.runs.Export(id, r.URL.Query().Get("kind"), r.URL.Query().Get("id"))
		if err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
		io.WriteString(w, md)
	default:
		writeError(w, http.StatusNotFound, "нет такого действия: "+r.Method+" "+action)
	}
}

func (s *Server) detail(w http.ResponseWriter, fn func() (runs.Detail, error)) {
	d, err := fn()
	if err != nil {
		writeError(w, statusOf(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation": d})
}

// handleTurn — /api/turns/{id} и поток событий /api/turns/{id}/events.
// Поток начинается снимком (состояние, журнал, промежуточные результаты),
// дальше — события по мере хода; закрывается, когда ход записан.
func (s *Server) handleTurn(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/turns/")
	id, action, _ := strings.Cut(rest, "/")
	sess, ok := s.runs.Turn(id)
	if !ok {
		writeError(w, http.StatusNotFound, "хода нет в памяти сервера: он завершён давно или сервер перезапущен — журнал лежит в диалоге")
		return
	}
	if action != "events" {
		writeJSON(w, http.StatusOK, map[string]any{"turn": sess.View(), "events": sess.Events()})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "поток не поддерживается")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	snap, ch, unsub := sess.Subscribe()
	defer unsub()
	writeEvent(w, "snapshot", snap)
	flusher.Flush()
	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case msg, open := <-ch:
			if !open {
				writeEvent(w, "done", sess.View())
				flusher.Flush()
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.Event, msg.Data)
			flusher.Flush()
		case <-ticker.C:
			io.WriteString(w, ": keep-alive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// handleGroups — стенд дорожек (ФТ-47): POST заводит, GET перечисляет.
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"groups": s.runs.Groups()})
	case http.MethodPost:
		var body struct {
			Title string `json:"title"`
			Lanes []struct {
				Name     string `json:"name"`
				Features string `json:"features"`
			} `json:"lanes"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		var lanes []runs.Lane
		for _, l := range body.Lanes {
			fs, err := s.runs.Registry().Parse(l.Features, s.runs.Defaults())
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			lanes = append(lanes, runs.Lane{Name: l.Name, Features: fs})
		}
		group, details, err := s.runs.StartGroup(body.Title, lanes)
		if err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"group": group, "lanes": details})
	default:
		writeError(w, http.StatusMethodNotAllowed, "нужен GET или POST")
	}
}

// handleGroup — /api/groups/{id} и /api/groups/{id}/turns.
func (s *Server) handleGroup(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	group, action, _ := strings.Cut(rest, "/")
	switch {
	case action == "" && r.Method == http.MethodGet:
		lanes, ok := s.runs.Group(group)
		if !ok {
			writeError(w, http.StatusNotFound, runs.ErrNotFound.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"group": group, "lanes": lanes})
	case action == "turns" && r.Method == http.MethodPost:
		var body turnRequest
		if !readJSON(w, r, &body) {
			return
		}
		sessions, err := s.runs.SendGroup(group, body.request())
		if err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		var out []map[string]any
		for _, sess := range sessions {
			out = append(out, turnPayload(sess.View()))
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"turns": out})
	default:
		writeError(w, http.StatusNotFound, "нет такого действия")
	}
}

// ReadJSON — тело запроса в структуру; ошибка уже записана в ответ.
func ReadJSON(w http.ResponseWriter, r *http.Request, v any) bool { return readJSON(w, r, v) }

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "тело запроса не разобралось: "+err.Error())
		return false
	}
	return true
}

// WriteJSON — ответ JSON.
func WriteJSON(w http.ResponseWriter, status int, v any) { writeJSON(w, status, v) }

// WriteError — ответ с ошибкой.
func WriteError(w http.ResponseWriter, status int, msg string) { writeError(w, status, msg) }

// StatusOf — код ответа по ошибке.
func StatusOf(err error) int { return statusOf(err) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeEvent(w io.Writer, event string, v any) {
	data, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

func statusOf(err error) int {
	switch {
	case errors.Is(err, runs.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return http.StatusNotFound
	case errors.Is(err, runs.ErrBusy):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}
