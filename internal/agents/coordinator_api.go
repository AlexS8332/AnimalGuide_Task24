package agents

import (
	"context"
	"fmt"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Coordinator — координатор кодом для тех, кто ведёт ход своим агентом
// (подборка): карточки открываются тем же путём, что и в обычном ходе, а
// правки и расход собираются в итог хода.
type Coordinator struct {
	t *turn
}

// NewCoordinator — координатор хода. Путь до источников выбирается здесь же,
// по механизмам диалога.
func NewCoordinator(ctx context.Context, d Deps, req Request, em agent.Emitter) (*Coordinator, error) {
	if em == nil {
		em = agent.Nop{}
	}
	// Путь до источников пишет в журнал хода, как подключился.
	reg, effective, why, err := d.Sources.For(agent.WithEmitter(ctx, em), req.Features)
	if err != nil {
		return nil, fmt.Errorf("инструменты источников: %w", err)
	}
	if why != "" {
		mechanism(em, "sources", why, "")
	}
	return &Coordinator{t: &turn{d: d, reg: reg, fs: effective, blocks: req.Blocks, em: em, base: req.Deltas}}, nil
}

// Registry — инструменты источников хода.
func (c *Coordinator) Registry() *tools.Registry { return c.t.reg }

// Features — механизмы, с которыми ход идёт на самом деле.
func (c *Coordinator) Features() features.Set { return c.t.fs }

// OpenCard — открыть карточку: привратник, идентификатор, дерево.
func (c *Coordinator) OpenCard(ctx context.Context, name string) (*card.Card, *card.NotFound, error) {
	return c.t.openCard(ctx, name)
}

// ReadSections — разделы карточки, специалисты параллельно.
func (c *Coordinator) ReadSections(ctx context.Context, cd card.Card, keys []string) []card.Section {
	return c.t.readSections(ctx, cd, keys)
}

// Card — карточка в состоянии хода (с прочитанными разделами).
func (c *Coordinator) Card(id string) (card.Card, bool) {
	st := c.t.state()
	if cd := st.Card(id); cd != nil {
		return cd.Clone(), true
	}
	return card.Card{}, false
}

// Run — прогон своего агента с учётом расхода в итоге хода.
func (c *Coordinator) Run(ctx context.Context, spec agent.Spec, in agent.Prepared) (agent.Reply, error) {
	reply, err := c.t.d.Runner.Run(ctx, spec, in, c.t.em)
	c.t.add(reply.Stats)
	return reply, err
}

// Result — итог хода: правки карточек, расход, механизмы.
func (c *Coordinator) Result(route, user, text string, added []llm.Message) Result {
	if added == nil {
		added = []llm.Message{{Role: llm.RoleUser, Content: user}, {Role: llm.RoleAssistant, Content: text}}
	}
	return Result{Route: route, User: user, Text: text, Added: added, Deltas: c.t.deltas, Stats: c.t.stats, Effective: c.t.fs}
}
