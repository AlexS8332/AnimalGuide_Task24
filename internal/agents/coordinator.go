package agents

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// turn — общее состояние хода для координатора: правки карточек и расход
// всех агентов. Специалисты работают параллельно, поэтому под замком.
type turn struct {
	d      Deps
	reg    *tools.Registry
	fs     features.Set
	blocks []features.Block
	em     agent.Emitter

	mu     sync.Mutex
	base   []card.Delta // правки пути ветки до этого хода
	deltas []card.Delta // правки этого хода
	stats  agent.Stats
	runs   int
}

func (t *turn) add(s agent.Stats) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.runs == 0 {
		t.stats = s
	} else {
		t.stats = t.stats.Add(s)
	}
	t.runs++
}

func (t *turn) delta(d card.Delta) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.deltas = append(t.deltas, d)
}

// state — карточки пути вместе с правками этого хода.
func (t *turn) state() card.State {
	t.mu.Lock()
	defer t.mu.Unlock()
	all := make([]card.Delta, 0, len(t.base)+len(t.deltas))
	all = append(all, t.base...)
	return card.Fold(append(all, t.deltas...))
}

// OpenCard — «рысь»: привратник, идентификатор, карточка (С-1). Уже
// открытое животное не открывается заново — ни одного запроса к модели.
func (t *turn) openCard(ctx context.Context, name string) (*card.Card, *card.NotFound, error) {
	name = strings.TrimSpace(name)
	st := t.state()
	if c := st.Find(name); c != nil {
		t.delta(card.Delta{Kind: card.DeltaCard, Card: c})
		note(t.em, "карточка «"+c.Name+"» уже открыта — повторно не ищу", "")
		cc := *c
		return &cc, nil, nil
	}
	// Название, о котором в этой ветке уже решали, привратник второй раз не
	// судит: его ответ при температуре 0 тот же, а запрос — лишний. Отказ
	// привратника повторяется без единого запроса к модели; если привратник
	// название пропустил, а сведений не нашёл идентификатор, — идентификатор
	// пробует снова (его неудача могла быть сбоем источника), но без
	// привратника.
	prior := st.Refused(name)
	if prior != nil && prior.Gate {
		nf := *prior
		note(t.em, "«"+name+"» привратник в этой ветке уже отклонил — повторно не спрашиваю", nf.Reason)
		t.em.Publish(agent.Update{Kind: "notfound", Data: nf})
		return nil, &nf, nil
	}
	started := time.Now()
	t.em.Log(agent.Event{Agent: coordinatorName, Kind: agent.EventAgentStart,
		Title: "открываю карточку «" + name + "»: привратник → идентификатор"})

	var v Verdict
	if prior != nil {
		v = Verdict{OK: true, Skipped: true}
		note(t.em, "«"+name+"» привратник в этой ветке уже пропустил — сразу к идентификатору", "")
	} else {
		var gs agent.Stats
		v, gs = Gate(ctx, t.d, name, t.fs, t.em)
		t.add(gs)
	}
	if !v.OK {
		nf := card.NotFound{Query: name, Reason: "похожее название не означает то же животное: такого таксона привратник не знает", Gate: true}
		t.delta(card.Delta{Kind: card.DeltaNotFound, NotFound: &nf})
		t.em.Publish(agent.Update{Kind: "notfound", Data: nf})
		t.em.Log(agent.Event{Agent: coordinatorName, Kind: agent.EventAgentDone, Title: "привратник отказал — дорогие шаги не делались",
			Seconds: time.Since(started).Seconds()})
		return nil, &nf, nil
	}
	res, err := Identify(ctx, t.d, t.reg, name, t.blocks, t.fs, t.em)
	t.add(res.Stats)
	if err != nil {
		return nil, nil, err
	}
	if res.NotFound != nil {
		t.delta(card.Delta{Kind: card.DeltaNotFound, NotFound: res.NotFound})
		t.em.Log(agent.Event{Agent: coordinatorName, Kind: agent.EventAgentDone, Title: "сведений нет: " + res.NotFound.Reason,
			Seconds: time.Since(started).Seconds()})
		return nil, res.NotFound, nil
	}
	t.delta(card.Delta{Kind: card.DeltaCard, Card: res.Card})
	t.em.Log(agent.Event{Agent: coordinatorName, Kind: agent.EventAgentDone, Title: "карточка: " + res.Card.Title(),
		Seconds: time.Since(started).Seconds()})
	return res.Card, nil, nil
}

// readSections — специалисты по разделам работают параллельно: ход длится
// столько, сколько самый долгий. Ошибка одного — оговорка в карточке, а не
// падение хода (ФТ-7). Прочитанное не перечитывается (ФТ-13).
func (t *turn) readSections(ctx context.Context, c card.Card, keys []string) []card.Section {
	var topics []card.Topic
	seen := map[string]bool{}
	for _, k := range keys {
		topic, ok := card.TopicOf(k)
		if !ok || seen[topic.Key] {
			continue
		}
		seen[topic.Key] = true
		st := t.state()
		if cur := st.Card(c.ID); cur != nil && cur.Done(topic.Key) {
			note(t.em, "раздел «"+topic.Title+"» уже прочитан — не перечитываю", "")
			continue
		}
		topics = append(topics, topic)
	}
	out := make([]card.Section, len(topics))
	var wg sync.WaitGroup
	for i, topic := range topics {
		wg.Add(1)
		go func(i int, topic card.Topic) {
			defer wg.Done()
			reading := card.Section{Key: topic.Key, Title: topic.Title, Status: card.SectionReading}
			t.em.Publish(agent.Update{Kind: "section", Data: map[string]any{"cardId": c.ID, "section": reading}})
			s, st, err := ReadSection(ctx, t.d, t.reg, c, topic, t.blocks, t.fs, t.em)
			t.add(st)
			if err != nil {
				s = card.Section{Key: topic.Key, Title: topic.Title, Status: card.SectionUnread,
					Reason: "специалист не справился: " + err.Error()}
				t.em.Log(agent.Event{Agent: "section." + topic.Key, Kind: agent.EventAgentError, Title: err.Error()})
			} else {
				t.delta(card.Delta{Kind: card.DeltaSection, CardID: c.ID, Section: &s})
			}
			t.em.Publish(agent.Update{Kind: "section", Data: map[string]any{"cardId": c.ID, "section": s}})
			out[i] = s
		}(i, topic)
	}
	wg.Wait()
	return out
}

// openNode — клик по узлу дерева (С-3): соседи узла из GBIF кодом, без
// модели.
func (t *turn) openNode(ctx context.Context, c card.Card, key int, name string) (card.Neighbors, error) {
	for _, n := range c.Neighbors {
		if n.NodeKey == key {
			return n, nil
		}
	}
	tool, ok := t.reg.Get("taxon_children")
	if !ok {
		return card.Neighbors{}, fmt.Errorf("инструмента taxon_children нет в наборе источников")
	}
	tr := card.NewTracker()
	if _, _, err := codeCall(ctx, t.em, tr.Observe(tool), fmt.Sprintf(`{"usage_key":%d,"limit":24}`, key)); err != nil {
		return card.Neighbors{}, err
	}
	n, _ := card.NeighborsOf(tr, key, name)
	t.delta(card.Delta{Kind: card.DeltaNeighbors, CardID: c.ID, Neighbors: &n})
	t.em.Publish(agent.Update{Kind: "neighbors", Data: map[string]any{"cardId": c.ID, "neighbors": n}})
	return n, nil
}

// compare — сравнение двух животных (С-4): недостающие карточки
// открываются тем же координатором, потом работает сравнивающий.
func (t *turn) compare(ctx context.Context, a, b string) (*card.Comparison, []string, error) {
	var cards [2]*card.Card
	var missing []string
	for i, name := range []string{a, b} {
		c, nf, err := t.openCard(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		if nf != nil {
			missing = append(missing, fmt.Sprintf("«%s»: %s", name, nf.Reason))
			continue
		}
		cards[i] = c
	}
	if cards[0] == nil || cards[1] == nil {
		return nil, missing, nil
	}
	if cards[0].ID == cards[1].ID {
		return nil, []string{"это одно и то же животное: " + cards[0].Title()}, nil
	}
	cmp, st, err := Compare(ctx, t.d, t.reg, *cards[0], *cards[1], t.blocks, t.fs, t.em)
	t.add(st)
	if err != nil {
		return nil, nil, err
	}
	t.delta(card.Delta{Kind: card.DeltaComparison, Comparison: &cmp})
	t.em.Publish(agent.Update{Kind: "comparison", Data: cmp})
	return &cmp, nil, nil
}

// topicKeys — разделы, о которых спрашивают словами (С-2): состав
// специалистов следует из того, что запросил пользователь, и решает это
// код, а не модель.
var topicWords = map[string][]string{
	"habitat":   {"ареал", "обитает", "обитания", "живёт", "живет", "водится", "распростран", "где жив"},
	"diet":      {"питает", "питани", "ест ", "ест?", "едят", "корм", "рацион", "пищ", "охотит"},
	"lifestyle": {"образ жизни", "повадк", "поведени", "ведёт себя", "ведет себя"},
	"breeding":  {"размнож", "потомств", "детёныш", "детеныш", "котят", "щенк", "брачн", "гнезд"},
	"status":    {"охран", "красн", "исчеза", "угроз", "численност", "редк"},
}

// TopicsIn — ключи разделов, названных в тексте.
func TopicsIn(text string) []string {
	low := " " + strings.ToLower(text) + " "
	var out []string
	for key, words := range topicWords {
		for _, w := range words {
			if strings.Contains(low, w) {
				out = append(out, key)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
