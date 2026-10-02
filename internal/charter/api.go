package charter

import (
	"net/http"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/invariants"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
)

// Extension — окно «Свод» (ФТ-38): правила, открытые поправки, журнал
// изменений, файл и то, как свод выглядит в запросе — блоком и абзацем
// выключенного механизма. Правит свод только процедура в разговоре: окно
// ничего не меняет (ФТ-31).
func (h *Hook) Extension() []server.Extension {
	return []server.Extension{{Prefix: "/api/charter", Handler: http.HandlerFunc(h.handle)}}
}

// Meta — события процедуры для интерфейса.
func Meta() map[string]any {
	return map[string]any{"charterEvents": invariants.Events}
}

func (h *Hook) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		server.WriteError(w, http.StatusMethodNotAllowed, "нужен GET: свод меняется только поправкой в разговоре")
		return
	}
	c, err := h.Store.Get(h.id())
	if err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	path, data, _ := h.Store.Raw(h.id())
	server.WriteJSON(w, http.StatusOK, map[string]any{
		"charter": c, "summary": c.Summary(), "path": path, "json": string(data),
		"block": invariants.Prompt(c), "plain": invariants.PlainRules(c),
	})
}
