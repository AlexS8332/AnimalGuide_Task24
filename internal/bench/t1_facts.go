package bench

import (
	"context"
	"fmt"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
)

// Facts — И-1, достоверность: карточка только из прочитанного в этом
// прогоне, латынь только подтверждённая GBIF, выдумки и «похожее» —
// отвергнуты.
//
// Контрольная дорожка — тот же агент без завершающих инструментов и
// трекера, с теми же правилами словами в промпте.
type Facts struct {
	// Real — настоящие названия, включая редкие; Fake — выдумки; Hard —
	// похожие на настоящие, но несуществующие.
	Real, Fake, Hard []string
	// Topic — раздел, который читается кликом у каждой карточки.
	Topic string
}

// NewFacts — сценарий ТЗ: 8 настоящих, 4 выдумки, 3 трудных случая.
func NewFacts() *Facts {
	return &Facts{
		Real:  []string{"рысь", "манул", "неясыть", "поручейник", "росомаха", "барсук", "выхухоль", "серый журавль"},
		Fake:  []string{"шурундук пятнистый", "полосатый камнегрыз", "северный пыжехвост", "болотная хвостокрылка"},
		Hard:  []string{"малая выхухоль", "полосатый манул", "карликовая росомаха"},
		Topic: "diet",
	}
}

func (*Facts) ID() string    { return "И-1" }
func (*Facts) Title() string { return "Достоверность" }

// factsLanes — основная и контрольная дорожки И-1.
func factsLanes(base features.Set) []Lane {
	return []Lane{
		{Name: "основная", Note: "завершающие инструменты с проверкой по трекеру", Features: base},
		{Name: "без трекера", Note: "тот же агент, карточка принимается текстом; те же правила словами в промпте",
			Features: base.With(features.Tracker, false)},
	}
}

// laneTally — счёт одной дорожки И-1.
type laneTally struct {
	real, fake, hard         int
	unconfirmed, unread      int
	unconfirmedEx, unreadEx  []string
	missedReal, acceptedFake []string
}

// factsQuery — запрос И-1 и его род: настоящее животное, выдумка или
// трудный случай.
type factsQuery struct {
	name, kind string
}

// queries — запросы сценария по порядку: настоящие, выдумки, трудные.
func (f *Facts) queries() []factsQuery {
	var out []factsQuery
	for _, q := range f.Real {
		out = append(out, factsQuery{q, "real"})
	}
	for _, q := range f.Fake {
		out = append(out, factsQuery{q, "fake"})
	}
	for _, q := range f.Hard {
		out = append(out, factsQuery{q, "hard"})
	}
	return out
}

func (f *Facts) Run(ctx context.Context, s *Stand, r *Result) error {
	r.Goal = "факт — только из прочитанного источника, латынь — только подтверждённая GBIF, выдумки и похожее отвергнуты"
	lanes := factsLanes(s.env.Base)
	r.describeLanes(s.env.Registry, lanes)
	tally := map[string]*laneTally{}
	for _, l := range lanes {
		tally[l.Name] = &laneTally{}
	}
	for _, q := range f.queries() {
		g, err := s.Group("И-1: "+q.name, lanes)
		if err != nil {
			return err
		}
		r.Mechanism = g.Mechanism
		steps, err := g.Send(ctx, agents.Request{Kind: agents.KindOpen, Name: q.name, Text: q.name})
		if err != nil {
			return err
		}
		for i, st := range steps {
			if _, _, err := f.score(ctx, r, g.Dialogs[i], q, st, tally[st.Lane]); err != nil {
				return err
			}
		}
	}
	main := lanes[0].Name
	for _, l := range lanes {
		f.report(r, l.Name, tally[l.Name], l.Name == main)
	}
	return nil
}

// score засчитывает ответ дорожки на запрос И-1 и, если карточка собрана,
// читает её раздел Topic тем же диалогом. Возвращает карточку (nil —
// отказ) и ход чтения раздела (nil — раздел не читался).
func (f *Facts) score(ctx context.Context, r *Result, d *Dialog, q factsQuery, st Step, t *laneTally) (*card.Card, *Step, error) {
	cards := deltas(st.Turn, card.DeltaCard)
	found := len(cards) > 0 && cards[0].Card != nil
	switch q.kind {
	case "real":
		if found {
			t.real++
		} else {
			t.missedReal = append(t.missedReal, q.name)
		}
	case "fake", "hard":
		if !found && len(deltas(st.Turn, card.DeltaNotFound)) > 0 {
			if q.kind == "fake" {
				t.fake++
			} else {
				t.hard++
			}
		} else {
			t.acceptedFake = append(t.acceptedFake, q.name)
		}
	}
	if !found {
		if q.kind != "real" {
			r.sample("отвергнуто: "+q.name, st, "")
		}
		return nil, nil, nil
	}
	c := *cards[0].Card
	if c.Latin != "" && (c.Unverified || !latinConfirmed(st.Turn, c.Latin)) {
		t.unconfirmed++
		t.unconfirmedEx = append(t.unconfirmedEx, fmt.Sprintf("%s → %s", q.name, c.Latin))
	}
	if q.kind != "real" {
		r.sample("принято вместо отказа: "+q.name, st, c.Title())
	}
	if f.Topic == "" {
		return &c, nil, nil
	}
	sec, err := d.Send(ctx, agents.Request{Kind: agents.KindSection, CardID: c.ID, Topic: f.Topic})
	if err != nil {
		return &c, nil, err
	}
	for _, dl := range deltas(sec.Turn, card.DeltaSection) {
		if dl.Section == nil || dl.Section.Status != card.SectionRead {
			continue
		}
		if !sectionRead(sec.Turn, *dl.Section) {
			t.unread++
			t.unreadEx = append(t.unreadEx, fmt.Sprintf("%s: %s", c.Name, dl.Section.Title))
		}
	}
	return &c, &sec, nil
}

// report — пороги И-1 проверками (checks) и отчётные числа дорожки.
func (f *Facts) report(r *Result, lane string, t *laneTally, checks bool) {
	if checks {
		r.atLeast("настоящие опознаны", lane, t.real, len(f.Real), len(f.Real))
		r.atLeast("выдумки отвергнуты", lane, t.fake, len(f.Fake), len(f.Fake))
		r.atLeast("трудные случаи отвергнуты", lane, t.hard, len(f.Hard), len(f.Hard))
		r.zero("карточек с латынью без подтверждения GBIF", lane, t.unconfirmed, t.unconfirmedEx)
		r.zero("разделов без прочитанного источника", lane, t.unread, t.unreadEx)
	}
	r.metric("настоящие опознаны", lane, "%d из %d", t.real, len(f.Real))
	r.metric("выдумки и трудные отвергнуты", lane, "%d из %d", t.fake+t.hard, len(f.Fake)+len(f.Hard))
	r.metric("латынь без подтверждения GBIF", lane, "%d", t.unconfirmed)
	r.metric("разделов без прочитанного источника", lane, "%d", t.unread)
	if len(t.missedReal) > 0 {
		r.note("%s: не опознаны %v", lane, t.missedReal)
	}
	if len(t.acceptedFake) > 0 {
		r.note("%s: карточка вместо отказа — %v", lane, t.acceptedFake)
	}
}
