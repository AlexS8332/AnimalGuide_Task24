package compiler

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/collection"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
)

// Extension — окно «Подборки» (ФТ-38): список, файл, выгрузка и
// продолжение подборки в любом диалоге (С-6, ФТ-28).
func (h *Hook) Extension(m *runs.Manager) []server.Extension {
	return []server.Extension{
		{Prefix: "/api/collections", Handler: http.HandlerFunc(h.handleList)},
		{Prefix: "/api/collections/", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.handleOne(w, r, m) })},
	}
}

// Summary — подборка для списка.
type Summary struct {
	ID       string              `json:"id"`
	Title    string              `json:"title"`
	Summary  string              `json:"summary"`
	Stage    collection.Stage    `json:"stage"`
	Paused   bool                `json:"paused"`
	Items    int                 `json:"items"`
	Done     int                 `json:"done"`
	Expected collection.Expected `json:"expected"`
	Path     string              `json:"path"`
}

func (h *Hook) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		server.WriteError(w, http.StatusMethodNotAllowed, "нужен GET")
		return
	}
	list, problems := h.Store.List()
	out := make([]Summary, 0, len(list))
	for _, st := range list {
		out = append(out, Summary{ID: st.ID, Title: st.Title, Summary: st.Summary(), Stage: st.Stage, Paused: st.IsPaused(),
			Items: len(st.Items), Done: st.DoneItems(), Expected: st.Expected(), Path: h.Store.DisplayPath(st.ID)})
	}
	msgs := make([]string, len(problems))
	for i, p := range problems {
		msgs[i] = p.Error()
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"collections": out, "problems": msgs})
}

// handleOne — /api/collections/{id}, /export, /continue.
func (h *Hook) handleOne(w http.ResponseWriter, r *http.Request, m *runs.Manager) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/collections/")
	id, action, _ := strings.Cut(rest, "/")
	if !h.Store.Has(id) {
		server.WriteError(w, http.StatusNotFound, collection.ErrNotFound.Error())
		return
	}
	st, err := h.Store.Get(id, "")
	if err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		path, data, _ := h.Store.Raw(id)
		server.WriteJSON(w, http.StatusOK, map[string]any{"state": st, "path": path, "json": string(data)})
	case action == "export" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape("Подборка — "+st.Title+".md"))
		io.WriteString(w, collection.Markdown(st))
	case action == "continue" && r.Method == http.MethodPost:
		var body struct {
			Conversation string `json:"conversation"`
		}
		if !server.ReadJSON(w, r, &body) {
			return
		}
		d, err := m.SetCollection(body.Conversation, id, st.Title)
		if err != nil {
			server.WriteError(w, server.StatusOf(err), err.Error())
			return
		}
		server.WriteJSON(w, http.StatusOK, map[string]any{"conversation": d})
	default:
		server.WriteError(w, http.StatusNotFound, "нет такого действия")
	}
}
