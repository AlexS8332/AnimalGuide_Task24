package runs

import (
	"sort"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/facts"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
)

// Summary — диалог для списка.
type Summary struct {
	ID         string         `json:"id"`
	Title      string         `json:"title"`
	Model      string         `json:"model"`
	Created    time.Time      `json:"created"`
	Updated    time.Time      `json:"updated"`
	Turns      int            `json:"turns"`
	Branches   int            `json:"branches"`
	Totals     history.Totals `json:"totals"`
	Meter      history.Meter  `json:"meter"`
	Running    bool           `json:"running"`
	Path       string         `json:"path"`
	Owners     []string       `json:"owners"`
	Group      string         `json:"group,omitempty"`
	Lane       string         `json:"lane,omitempty"`
	Collection string         `json:"collection,omitempty"`
	Features   features.Set   `json:"features"`
}

// BranchInfo — ветка для дерева диалога на пульте.
type BranchInfo struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Parent   string    `json:"parent,omitempty"`
	ForkTurn int       `json:"forkTurn"`
	Turns    int       `json:"turns"`
	Own      int       `json:"own"`
	Created  time.Time `json:"created"`
	Active   bool      `json:"active"`
}

// Detail — диалог целиком для интерфейса: путь текущей ветки, карточки,
// карточка фактов, дерево веток, механизмы и идущий ход.
type Detail struct {
	Summary
	Branch      string               `json:"branch"`
	BranchTree  []BranchInfo         `json:"branchTree"`
	Checkpoints []history.Checkpoint `json:"checkpoints"`
	TurnList    []history.Turn       `json:"turnList"`
	Messages    int                  `json:"messages"`
	Runes       int                  `json:"runes"`
	Cards       card.State           `json:"cards"`
	Facts       facts.State          `json:"facts"`
	Mechanisms  []features.Status    `json:"mechanisms"`
	Active      *View                `json:"active,omitempty"`
	// Extras — сведения механизмов о диалоге (профиль, память, подборка,
	// свод): их добавляют хуки через Describer.
	Extras map[string]any `json:"extras,omitempty"`
}

// Describer — хук, который умеет рассказать о своём состоянии для
// интерфейса (профиль собеседника, слои памяти, подборка, свод).
type Describer interface {
	Describe(c *history.Conversation) any
}

func (m *Manager) summaryLocked(c *history.Conversation) Summary {
	_, running := m.active[c.ID]
	return Summary{ID: c.ID, Title: c.Title, Model: c.Model, Created: c.Created, Updated: c.Updated,
		Turns: c.TurnCount(), Branches: len(c.Branches), Totals: c.Totals(), Meter: c.Meter, Running: running,
		Path: m.cfg.Store.DisplayPath(c.ID), Owners: append([]string{}, c.Owners...), Group: c.Group, Lane: c.Lane,
		Collection: c.Collection, Features: c.Features}
}

func (m *Manager) detailLocked(c *history.Conversation) Detail {
	cl := c.Clone()
	d := Detail{Summary: m.summaryLocked(c), Branch: cl.Active, Checkpoints: cl.Checkpoints,
		TurnList: cl.Turns(), Cards: cl.Cards(cl.Active), Facts: cl.Facts(),
		Mechanisms: m.cfg.Registry.Describe(cl.Features)}
	msgs := cl.Messages()
	d.Messages, d.Runes = len(msgs), history.Runes(msgs)
	if d.Checkpoints == nil {
		d.Checkpoints = []history.Checkpoint{}
	}
	if d.TurnList == nil {
		d.TurnList = []history.Turn{}
	}
	for _, b := range cl.Branches {
		d.BranchTree = append(d.BranchTree, BranchInfo{ID: b.ID, Name: b.Name, Parent: b.Parent, ForkTurn: b.ForkTurn,
			Turns: len(cl.PathTurns(b.ID)), Own: len(b.Turns), Created: b.Created, Active: b.ID == cl.Active})
	}
	for _, h := range m.cfg.Hooks {
		if ds, ok := h.(Describer); ok {
			if v := ds.Describe(cl); v != nil {
				if d.Extras == nil {
					d.Extras = map[string]any{}
				}
				d.Extras[h.Name()] = v
			}
		}
	}
	if s, running := m.active[c.ID]; running {
		v := s.View()
		d.Active = &v
	}
	return d
}

// List — все диалоги, свежие первыми.
func (m *Manager) List() []Summary {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Summary, 0, len(m.convs))
	for _, c := range m.convs {
		out = append(out, m.summaryLocked(c))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Updated.Equal(out[j].Updated) {
			return out[i].ID < out[j].ID
		}
		return out[i].Updated.After(out[j].Updated)
	})
	return out
}

// Get — диалог целиком.
func (m *Manager) Get(id string) (Detail, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	if !ok {
		return Detail{}, false
	}
	return m.detailLocked(c), true
}

// Conversation — копия диалога (для стенда и хуков).
func (m *Manager) Conversation(id string) (*history.Conversation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	if !ok {
		return nil, false
	}
	return c.Clone(), true
}
