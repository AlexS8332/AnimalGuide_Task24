package feed

import (
	"context"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Name — имя хука в журнале хода.
const Name = "trivia"

// LeadTools — инструменты демона, которые получает ведущий. Только чтение:
// run_now и summary_build тратят деньги, их запускает человек из раздела,
// а не модель по ходу разговора.
var LeadTools = []string{"facts_get", "facts_latest"}

// leadRule — абзац системного промпта при выданных инструментах. Описания
// от демона говорят, что такое выпуск и что факты проверены, но не
// говорят, как ими пользоваться в разговоре: когда звать (ведущий иначе
// считает источниками только Википедию и GBIF) и что дату сбора надо
// назвать — выпуск мог устареть. Блоком это быть не может: у механизма нет
// места в запросе (KindTool), а блок без места реестр выбрасывает.
const leadRule = `«Интересные факты» (facts_get, facts_latest) — готовые выпуски демона справочника: факты в них уже проверены по источникам, перепроверять их не нужно. Зови facts_get, когда спрашивают, что интересного известно о виде или что нового собрано, facts_latest — когда спрашивают о последних выпусках; карточку животного они не заменяют. Пересказывая выпуск, скажи, что это выпуск «Интересных фактов», и назови дату сбора (created_at); ссылки на источники бери из выпуска.`

// Hook — механизм trivia: инструменты демона для ведущего. Демона может
// не быть — тогда ход идёт без них, а в журнале — почему.
type Hook struct {
	Remote *Remote
}

func (h *Hook) Name() string { return Name }

// Before — инструменты демона в запрос хода. Их получает агент, который
// пишет человеку (ведущий или составитель подборки): оба читают
// t.Request.Tools уже после всех хуков, поэтому место хука в списке на
// выдачу не влияет.
func (h *Hook) Before(ctx context.Context, t *runs.Turn) error {
	if !t.Features.On(features.Trivia) {
		return nil
	}
	if h.Remote == nil || !h.Remote.Configured() {
		h.off(t, "демон не настроен (-facts-server \"\")", "")
		return nil
	}
	list, err := h.Remote.Tools(ctx, LeadTools...)
	if err != nil {
		h.off(t, err.Error(), h.Remote.Hint(err))
		return nil
	}
	t.Request.Tools = append(t.Request.Tools, list...)
	t.Request.Rules = join(t.Request.Rules, leadRule)
	names := make([]string, len(list))
	for i, tl := range list {
		names[i] = tl.Spec().Name
	}
	st := h.Remote.version()
	t.Em.Log(agent.Event{Agent: Name, Kind: agent.EventMechanism, Mechanism: string(features.Trivia), Via: tools.ViaMCP,
		Title:  "факты: " + strings.Join(names, ", ") + " от демона " + h.Remote.Server() + st,
		Detail: "Инструменты «Интересных фактов» выданы ведущему; описания и схемы — от демона (tools/list)."})
	return nil
}

// off — механизм не сработал: ход идёт без инструментов, в журнале —
// причина, а в итоговом наборе хода механизм выключен (как у mcp).
func (h *Hook) off(t *runs.Turn, reason, hint string) {
	t.Request.Features = t.Request.Features.With(features.Trivia, false)
	detail := "Ход идёт без facts_get и facts_latest: ведущий отвечает по источникам хода."
	if hint != "" {
		detail += "\nЧто сделать: " + hint + "."
	}
	t.Em.Log(agent.Event{Agent: Name, Kind: agent.EventMechanism, Mechanism: string(features.Trivia),
		Title: "факты: " + reason + " — ход идёт без инструментов демона", Detail: detail})
}

// After — ничего: выпуски не проверяются и не пишутся ходом.
func (h *Hook) After(ctx context.Context, t *runs.Turn) error { return nil }

func join(a, b string) string {
	switch {
	case strings.TrimSpace(a) == "":
		return b
	case strings.TrimSpace(b) == "":
		return a
	}
	return a + "\n\n" + b
}

var _ runs.Hook = (*Hook)(nil)
