package persona

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/memory"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/paths"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/profile"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
)

// Extension — окна «Картотека профилей» и «Память целиком» (ФТ-38): правит
// человек (ФТ-18: раскладку предлагает извлекатель, проверяют правила,
// правит человек).
func (h *Hook) Extension() []server.Extension {
	return []server.Extension{
		{Prefix: "/api/people", Handler: http.HandlerFunc(h.handlePeople)},
		{Prefix: "/api/people/", Handler: http.HandlerFunc(h.handlePerson)},
		{Prefix: "/api/memory", Handler: http.HandlerFunc(h.handleMemory)},
		{Prefix: "/api/memory/", Handler: http.HandlerFunc(h.handleMemoryEdit)},
	}
}

// Meta — анкета и заготовки для интерфейса.
func Meta() map[string]any {
	return map[string]any{"profileFields": profile.Fields(), "profilePresets": profile.Presets()}
}

// Person — человек в картотеке: анкета и что о нём известно.
type Person struct {
	ID      string            `json:"id"`
	Title   string            `json:"title,omitempty"`
	Profile profile.Profile   `json:"profile"`
	Summary string            `json:"summary"`
	Long    memory.Card       `json:"long"`
	Paths   map[string]string `json:"paths"`
}

func (h *Hook) person(id string) (Person, error) {
	p, err := h.Profiles.Get(id, "")
	if err != nil {
		return Person{}, err
	}
	long, err := h.Memory.Card(memory.LayerLong, id, p.Title)
	if err != nil {
		return Person{}, err
	}
	return Person{ID: id, Title: p.Title, Profile: p, Summary: p.Summary(), Long: long, Paths: map[string]string{
		"profile": h.Profiles.DisplayPath(id), "long": h.Memory.DisplayPath(memory.LayerLong, id)}}, nil
}

// handlePeople — картотека: все, у кого есть анкета или долговременная
// память.
func (h *Hook) handlePeople(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		server.WriteError(w, http.StatusMethodNotAllowed, "нужен GET")
		return
	}
	ids := map[string]bool{}
	profs, problems := h.Profiles.List()
	for _, p := range profs {
		ids[p.ID] = true
	}
	longs, more := h.Memory.List(memory.LayerLong)
	problems = append(problems, more...)
	for _, c := range longs {
		ids[c.ID] = true
	}
	var out []Person
	for id := range ids {
		if p, err := h.person(id); err == nil {
			out = append(out, p)
		} else {
			problems = append(problems, err)
		}
	}
	msgs := make([]string, len(problems))
	for i, p := range problems {
		msgs[i] = p.Error()
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"people": out, "problems": msgs})
}

type personEdit struct {
	Op     string `json:"op"`
	Field  string `json:"field"`
	Value  string `json:"value"`
	Text   string `json:"text"`
	Preset string `json:"preset"`
	Layer  string `json:"layer"`
	Key    string `json:"key"`
	Name   string `json:"name"`
}

// handlePerson — /api/people/{id}, /api/people/{id}/profile,
// /api/people/{id}/memory, /api/people/{id}/bookmark.
func (h *Hook) handlePerson(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/people/")
	id, action, _ := strings.Cut(rest, "/")
	if !paths.ValidID(id) {
		server.WriteError(w, http.StatusBadRequest, "некорректный идентификатор собеседника")
		return
	}
	if r.Method == http.MethodGet && action == "" {
		p, err := h.person(id)
		if err != nil {
			server.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		server.WriteJSON(w, http.StatusOK, map[string]any{"person": p})
		return
	}
	if r.Method != http.MethodPost {
		server.WriteError(w, http.StatusMethodNotAllowed, "нужен POST")
		return
	}
	var body personEdit
	if !server.ReadJSON(w, r, &body) {
		return
	}
	var err error
	switch action {
	case "profile":
		err = h.editProfile(id, body)
	case "memory":
		switch body.Op {
		case "put":
			_, _, err = h.Memory.Put(memory.LayerLong, id, "", body.Key, body.Value, 0)
		case "forget":
			_, _, err = h.Memory.Forget(memory.LayerLong, id, body.Key)
		default:
			err = fmt.Errorf("неизвестное действие с памятью %q; допустимы put, forget", body.Op)
		}
	case "bookmark":
		_, _, err = h.Memory.AddToList(id, "", memory.KeyBookmarks, body.Name, 0)
	default:
		server.WriteError(w, http.StatusNotFound, "нет такого действия")
		return
	}
	if err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := h.person(id)
	if err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"person": p})
}

// editProfile — правка анкеты руками: значение из перечня, как и у
// извлекателя; свободный текст — только в ограничения.
func (h *Hook) editProfile(id string, e personEdit) error {
	_, err := h.Profiles.Update(id, "", func(p *profile.Profile) (bool, error) {
		switch e.Op {
		case "set":
			f, ok := profile.FieldOf(e.Field)
			if !ok {
				return false, fmt.Errorf("в анкете нет поля %q", e.Field)
			}
			v, ok := profile.Normalize(f.Key, e.Value)
			if !ok {
				return false, fmt.Errorf("значение %q для поля «%s» не из перечня", e.Value, f.Title)
			}
			return p.Set(f.Key, profile.Value{Value: v, Source: profile.SourceHuman}), nil
		case "clear":
			f, ok := profile.FieldOf(e.Field)
			if !ok {
				return false, fmt.Errorf("в анкете нет поля %q", e.Field)
			}
			return p.Clear(f.Key), nil
		case "limit":
			added, _ := p.AddLimit(profile.Limit{Text: e.Text, Source: profile.SourceHuman})
			return added, nil
		case "unlimit":
			return p.DropLimit(e.Text), nil
		case "preset":
			pr, ok := profile.PresetOf(e.Preset)
			if !ok {
				return false, fmt.Errorf("неизвестная заготовка %q", e.Preset)
			}
			built := pr.Build(p.ID, p.Title)
			built.Asks, built.Limits, built.Created = p.Asks, p.Limits, p.Created
			built.Version = p.Version + 1
			*p = built
			return true, nil
		case "reset":
			fresh := profile.New(p.ID, p.Title)
			fresh.Version = p.Version + 1
			*p = fresh
			return true, nil
		}
		return false, fmt.Errorf("неизвестное действие %q; допустимы set, clear, limit, unlimit, preset, reset", e.Op)
	})
	return err
}

// handleMemory — «Память целиком»: все слои всех владельцев.
func (h *Hook) handleMemory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		server.WriteError(w, http.StatusMethodNotAllowed, "нужен GET")
		return
	}
	longs, p1 := h.Memory.List(memory.LayerLong)
	works, p2 := h.Memory.List(memory.LayerWork)
	var msgs []string
	for _, p := range append(p1, p2...) {
		msgs = append(msgs, p.Error())
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"long": longs, "work": works, "problems": msgs})
}

// handleMemoryEdit — /api/memory/{layer}/{id}: put/forget руками.
func (h *Hook) handleMemoryEdit(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/memory/")
	layer, id, _ := strings.Cut(rest, "/")
	if r.Method == http.MethodGet {
		path, data, err := h.Memory.Raw(layer, id)
		if err != nil {
			server.WriteError(w, http.StatusNotFound, err.Error())
			return
		}
		server.WriteJSON(w, http.StatusOK, map[string]any{"path": path, "json": string(data)})
		return
	}
	var body personEdit
	if !server.ReadJSON(w, r, &body) {
		return
	}
	var c memory.Card
	var err error
	switch body.Op {
	case "put":
		c, _, err = h.Memory.Put(layer, id, "", body.Key, body.Value, 0)
	case "forget":
		c, _, err = h.Memory.Forget(layer, id, body.Key)
	default:
		err = fmt.Errorf("неизвестное действие %q; допустимы put, forget", body.Op)
	}
	if err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, map[string]any{"card": c})
}
