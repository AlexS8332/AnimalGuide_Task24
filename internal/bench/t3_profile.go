package bench

import (
	"context"
	"fmt"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/profile"
)

// Profile — И-3, профиль: один набор вопросов трём дорожкам — детский
// профиль, профиль специалиста и без профиля. Сравнивается механизм
// profile; детская и взрослая дорожки отличаются не механизмом, а
// анкетой собеседника.
type Profile struct {
	Questions []string
	// Once — разовая просьба: поле профиля в файле после неё не меняется.
	Once      string
	OnceField string
	// Repeat — одна и та же разовая просьба дважды: после второй поле
	// закрепляется значением RepeatValue.
	Repeat      []string
	RepeatField string
	RepeatValue string
	// Fresh — вопрос в новом диалоге того же собеседника.
	Fresh string
}

// NewProfile — сценарий ТЗ: 8 вопросов.
func NewProfile() *Profile {
	return &Profile{
		Questions: []string{
			"Где живёт рысь?",
			"Чем питается манул?",
			"Какого размера бывает лось?",
			"Как зимует ёж?",
			"Чем волк отличается от собаки?",
			"Сколько живёт барсук?",
			"Как охотится сова неясыть?",
			"Кто самый близкий родственник рыси?",
		},
		Once:        "В этот раз ответь подробно: как рысь выслеживает зайца?",
		OnceField:   profile.FieldLength,
		Repeat:      []string{"В этот раз обойдись без эмодзи: где зимуют журавли?", "И сейчас тоже без эмодзи, пожалуйста: чем питается росомаха?"},
		RepeatField: profile.FieldEmoji,
		RepeatValue: "no",
		Fresh:       "Расскажи, чем питается барсук?",
	}
}

func (*Profile) ID() string    { return "И-3" }
func (*Profile) Title() string { return "Профиль" }

// withPreset — подготовка дорожки: анкета по заготовке роли.
func withPreset(id string) func(s *Stand, owner string) error {
	return func(s *Stand, owner string) error {
		pr, ok := profile.PresetOf(id)
		if !ok {
			return fmt.Errorf("нет заготовки профиля %q", id)
		}
		return s.Profiles.Save(pr.Build(owner, pr.Title))
	}
}

func (p *Profile) Run(ctx context.Context, s *Stand, r *Result) error {
	r.Goal = "ответ следует анкете собеседника; разовая просьба не меняет анкету, повторённая — закрепляется"
	base := s.env.Base
	lanes := []Lane{
		{Name: "ребёнок 7–10", Note: "анкета «ребёнок 7–10 лет»: коротко, на «ты», без латыни", Features: base, Setup: withPreset("child")},
		{Name: "специалист", Note: "анкета «специалист»: те же механизмы, другой собеседник", Features: base, Same: true, Setup: withPreset("expert")},
		{Name: "без профиля", Note: "механизм profile выключен", Features: base.With(features.Profile, false)},
	}
	r.describeLanes(s.env.Registry, lanes)
	g, err := s.Group("И-3: профиль", lanes)
	if err != nil {
		return err
	}
	r.Mechanism = g.Mechanism
	type tally struct {
		ok, total  int
		hidden     int
		latinSeen  []string
		questions  int
		checkTurns int
	}
	t := map[string]*tally{}
	for _, l := range lanes {
		t[l.Name] = &tally{}
	}
	for i, q := range p.Questions {
		steps, err := g.Send(ctx, agents.Request{Text: q})
		if err != nil {
			return err
		}
		for _, st := range steps {
			x := t[st.Lane]
			x.questions++
			var checks []profile.Check
			if st.Turn.Extra("checks", &checks) {
				x.checkTurns++
				ok, total := profile.Rate(checks)
				x.ok += ok
				x.total += total
			}
			if !HasLatin(replyOf(st)) {
				x.hidden++
			} else {
				x.latinSeen = append(x.latinSeen, fmt.Sprintf("«%s»", clip(q, 40)))
			}
			if i < 2 {
				r.sample(q, st, "")
			}
		}
	}
	child := lanes[0].Name
	for _, l := range lanes {
		x := t[l.Name]
		if l.Features.On(features.Profile) {
			if x.total == 0 {
				r.pending("соблюдение полей, видных в тексте", "≥ 85 %", l.Name, "определимых проверок нет")
			} else {
				pct := float64(x.ok) / float64(x.total) * 100
				st := Pass
				if pct < 85 {
					st = Fail
				}
				r.check(Check{What: "соблюдение полей, видных в тексте", Want: "≥ 85 %", Lane: l.Name,
					Got: fmt.Sprintf("%.0f %% (%d из %d)", pct, x.ok, x.total), Status: st})
			}
		}
		r.metric("проверок профиля соблюдено", l.Name, "%d из %d, ходов с проверкой %d из %d", x.ok, x.total, x.checkTurns, x.questions)
		r.metric("ответов без латыни", l.Name, "%d из %d", x.hidden, x.questions)
	}
	cx := t[child]
	c := Check{What: "латынь скрыта у детского профиля", Want: fmt.Sprintf("%d из %d", cx.questions, cx.questions), Lane: child,
		Got: fmt.Sprintf("%d из %d", cx.hidden, cx.questions), Status: Pass}
	if cx.hidden < cx.questions {
		c.Status, c.Note = Fail, "латынь в ответах на "+joinQ(cx.latinSeen)
	}
	r.check(c)

	d := g.Dialog(child)
	before, err := s.Profiles.Get(d.Owner, "")
	if err != nil {
		return err
	}
	if p.Once != "" {
		st, err := d.Ask(ctx, p.Once)
		if err != nil {
			return err
		}
		after, err := s.Profiles.Get(d.Owner, "")
		if err != nil {
			return err
		}
		r.yes("разовая просьба профиль не меняет", child, after.Val(p.OnceField) == before.Val(p.OnceField),
			fmt.Sprintf("%s: было «%s», стало «%s»", p.OnceField, before.Val(p.OnceField), after.Val(p.OnceField)))
		r.sample("разовая просьба", st, "")
	}
	if len(p.Repeat) > 0 {
		for _, q := range p.Repeat {
			if _, err := d.Ask(ctx, q); err != nil {
				return err
			}
		}
		after, err := s.Profiles.Get(d.Owner, "")
		if err != nil {
			return err
		}
		r.yes("повторённая просьба закрепляется", child, after.Val(p.RepeatField) == p.RepeatValue,
			fmt.Sprintf("%s: было «%s», стало «%s», ждали «%s»", p.RepeatField, before.Val(p.RepeatField), after.Val(p.RepeatField), p.RepeatValue))
	}
	if p.Fresh != "" {
		if err := d.Continue("И-3: новый диалог", ""); err != nil {
			return err
		}
		st, err := d.Ask(ctx, p.Fresh)
		if err != nil {
			return err
		}
		_, block := blockText(st.Turn, features.Profile)
		var checks []profile.Check
		checked := st.Turn.Extra("checks", &checks)
		r.yes("профиль действует в новом диалоге", child, block && checked,
			fmt.Sprintf("блок профиля в запросе: %s, проверки соблюдения: %s", yesNo(block), yesNo(checked)))
		r.sample("новый диалог", st, "")
	}
	return nil
}

func yesNo(b bool) string {
	if b {
		return "да"
	}
	return "нет"
}

func joinQ(list []string) string {
	out := ""
	for i, s := range firstN(list, 3) {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
