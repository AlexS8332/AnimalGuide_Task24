package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

const (
	identifierName     = "identifier"
	identifierMaxSteps = 10
)

const identifierSystem = `Ты агент-идентификатор справочника по животным. По названию, которое дал пользователь, найди статью русской Википедии именно об этом животном и подтверди латинское название по базе GBIF.

` + groundingRules + `

Порядок работы:
1. Поиск по названию программа уже сделала: ответ search_wikipedia есть в разговоре. Выбери статью строго по правилам проверки названия; если в выдаче её нет — поищи сам другим запросом.
2. read_wikipedia без section: вступление и список разделов. Латинское название обычно во вступлении в скобках после «лат.».
3. match_taxon с латинским названием. Подтверждено, только если found = true. В ответе сразу есть русские названия таксона по GBIF (vernacular_rus): по ним проверь, что русское название относится к этому таксону.
Если латынь видна уже в выдаче поиска, вызови read_wikipedia и match_taxon одним ответом: друг от друга они не зависят.
4. Сдай карточку: русское название, латынь ровно как её подтвердил match_taxon, заголовок статьи, краткое описание (2–4 предложения по вступлению) и русские названия таксонов из ответа match_taxon (царство, тип, класс, отряд, семейство, род), если уверен в них.

Дерево классификации и разделы статьи достроит программа — taxon_tree и чтение разделов не нужны.`

// identifierFinish — как закончить при включённом трекере.
const identifierFinish = `

Заверши работу вызовом submit_card, а если животного нет — report_not_found. Не отвечай текстом.`

// identifierPlain — контрольная дорожка без трекера (И-1): те же правила
// словами, результат — текстом. Код его не проверяет, а только разбирает.
const identifierPlain = `

Латинское название в ответе — только то, что подтвердил match_taxon (found = true); описание — только по прочитанной статье.
Ответь одним JSON-объектом без пояснений: {"name_ru": "...", "latin": "...", "wiki_title": "...", "summary": "..."} — или {"not_found": "почему сведений нет"}.`

// Identified — итог идентификатора: карточка или «сведений нет».
type Identified struct {
	Card     *card.Card     `json:"card,omitempty"`
	NotFound *card.NotFound `json:"notFound,omitempty"`
	Stats    agent.Stats    `json:"stats"`
}

var codeCalls atomic.Int64

// codeCall — вызов инструмента кодом координатора: с тем же журналом, что у
// вызова моделью, и со своим идентификатором — «почему так» у факта,
// добытого кодом, ведёт на это событие.
func codeCall(ctx context.Context, em agent.Emitter, t tools.Tool, args string) (string, string, error) {
	s := t.Spec()
	callID := fmt.Sprintf("code_%s_%d", s.Name, codeCalls.Add(1))
	via := ""
	if s.Via == tools.ViaMCP {
		via = " (через MCP)"
	}
	em.Log(agent.Event{Agent: coordinatorName, Kind: agent.EventToolCall, Tool: s.Name, CallID: callID, Via: s.Via,
		Title: "вызов " + s.Name + via + " кодом координатора", Detail: args})
	started := time.Now()
	out, err := t.Call(tools.WithCallID(ctx, callID), json.RawMessage(args))
	if err != nil {
		em.Log(agent.Event{Agent: coordinatorName, Kind: agent.EventToolError, Tool: s.Name, CallID: callID, Via: s.Via,
			Title: s.Name + ": ошибка", Detail: err.Error(), Seconds: time.Since(started).Seconds()})
		return "", callID, err
	}
	em.Log(agent.Event{Agent: coordinatorName, Kind: agent.EventToolResult, Tool: s.Name, CallID: callID, Via: s.Via,
		Title: fmt.Sprintf("%s: %d символов", s.Name, len([]rune(out))), Detail: tools.Truncate(out, 6000),
		Seconds: time.Since(started).Seconds()})
	return out, callID, nil
}

// Identify — идентификатор: статья, латынь, карточка. С трекером карточка
// принимается только по проверке submit_card; без него (контрольная
// дорожка) — как её написала модель, с пометкой, что латынь не сверялась.
func Identify(ctx context.Context, d Deps, reg *tools.Registry, query string, blocks []features.Block, fs features.Set, em agent.Emitter) (Identified, error) {
	tr := card.NewTracker()
	list, err := sourcePick(reg, "search_wikipedia", "read_wikipedia", "match_taxon", "taxon_tree", "vernacular_names")
	if err != nil {
		return Identified{}, err
	}
	list = identifierTools(list, em)
	spec := agent.Spec{Name: identifierName, System: identifierSystem, Tools: tr.ObserveAll(list), MaxSteps: identifierMaxSteps}
	user := fmt.Sprintf("Запрос пользователя: «%s».", strings.TrimSpace(query))
	// Первый шаг идентификатора — поиск по названию из запроса — выбора не
	// содержит: код делает его сам, и модель начинает с выбора статьи. Один
	// запрос к модели на каждое новое животное.
	preload := []agent.Preload{{Tool: "search_wikipedia", Args: jsonArgs(map[string]string{"query": strings.TrimSpace(query)})}}

	var accepted *card.Card
	var missing *card.NotFound
	if fs.On(features.Tracker) {
		spec.System += identifierFinish
		spec.Finish = []agent.Finisher{
			{
				Name:        "submit_card",
				Description: "Сдать карточку животного: русское и латинское название, заголовок прочитанной статьи, краткое описание. Код проверит латынь и статью по реальным вызовам инструментов.",
				Parameters:  json.RawMessage(card.CardSchema),
				Handle: func(_ context.Context, _ string, args json.RawMessage) (any, error) {
					var dr card.CardDraft
					if err := tools.ParseArgs(args, &dr); err != nil {
						return nil, err
					}
					c, err := card.CheckCard(tr, dr, query)
					if err != nil {
						return nil, err
					}
					accepted = &c
					return c, nil
				},
			},
			notFoundFinisher(query, &missing),
		}
	} else {
		spec.System += identifierPlain
		spec.AllowText = true
		mechanism(em, features.Tracker, "проверка по трекеру выключена — карточка принимается текстом",
			"Те же правила переданы словами в промпте; латынь и статья кодом не сверяются.")
	}

	reply, err := d.Runner.Run(ctx, spec, agent.Prepared{Blocks: headBlocks(blocks), User: user, Features: fs, Preload: preload}, em)
	out := Identified{Stats: reply.Stats}
	if err != nil {
		return out, fmt.Errorf("идентификатор: %w", err)
	}
	if !fs.On(features.Tracker) {
		accepted, missing = parsePlain(tr, reply.Text, query)
	}
	switch {
	case accepted != nil:
		c := *accepted
		if len(c.Tree) == 0 && c.TaxonKey > 0 {
			c = withTree(ctx, tr, reg, c, em)
		}
		out.Card = &c
		em.Publish(agent.Update{Kind: "card", Data: c})
	case missing != nil:
		out.NotFound = missing
		em.Publish(agent.Update{Kind: "notfound", Data: *missing})
	default:
		out.NotFound = &card.NotFound{Query: query, Reason: "идентификатор не сдал результат"}
	}
	return out, nil
}

// identifierTools — инструменты идентификатора: match_taxon сразу
// приносит русские названия таксона, а отдельного vernacular_names у
// агента нет. Модель звала его после каждой сверки «на всякий случай» —
// ещё один запрос к модели на каждое животное, хотя ключ таксона уже
// известен и второй вызов GBIF кодом ничего не решает за модель. Запрет
// держится отсутствием инструмента (ИП-7), а не словами промпта.
func identifierTools(list []tools.Tool, em agent.Emitter) []tools.Tool {
	var vern tools.Tool
	for _, t := range list {
		if t.Spec().Name == "vernacular_names" {
			vern = t
		}
	}
	if vern == nil {
		return list
	}
	out := make([]tools.Tool, 0, len(list))
	for _, t := range list {
		switch t.Spec().Name {
		case "vernacular_names":
			continue
		case "match_taxon":
			t = withVernacular(t, vern, em)
		}
		out = append(out, t)
	}
	return out
}

// withVernacular дописывает к найденному таксону его русские названия по
// GBIF (vernacular_rus). Вызов идёт кодом координатора, с журналом;
// неудача — пометка в ответе, а не ошибка сверки: латынь уже подтверждена.
func withVernacular(match, vern tools.Tool, em agent.Emitter) tools.Tool {
	return tools.Wrap(match, func(ctx context.Context, args json.RawMessage, next tools.CallFunc) (string, error) {
		out, err := next(ctx, args)
		if err != nil {
			return out, err
		}
		data, _ := tools.Unwrap(out)
		dec := json.NewDecoder(strings.NewReader(data))
		dec.UseNumber()
		var m map[string]any
		if dec.Decode(&m) != nil {
			return out, nil
		}
		found, _ := m["found"].(bool)
		key, _ := m["usage_key"].(json.Number)
		if !found || key == "" {
			return out, nil
		}
		vout, _, verr := codeCall(ctx, em, vern, `{"usage_key":`+key.String()+`}`)
		if verr != nil {
			m["vernacular_rus_error"] = verr.Error()
		} else {
			var v struct {
				Names []string `json:"names"`
			}
			vdata, _ := tools.Unwrap(vout)
			if json.Unmarshal([]byte(vdata), &v) == nil {
				m["vernacular_rus"] = append([]string{}, v.Names...)
			}
		}
		return tools.Result(m)
	})
}

// withTree достраивает дерево кодом: ключ таксона уже подтверждён, и звать
// ради дерева модель незачем. Неудача — оговорка в карточке, а не ошибка.
func withTree(ctx context.Context, tr *card.Tracker, reg *tools.Registry, c card.Card, em agent.Emitter) card.Card {
	t, ok := reg.Get("taxon_tree")
	if !ok {
		return c
	}
	observed := tr.Observe(t)
	if _, _, err := codeCall(ctx, em, observed, fmt.Sprintf(`{"usage_key":%d}`, c.TaxonKey)); err != nil {
		c.Notes = append(c.Notes, "дерево классификации не получено: "+err.Error())
		return c
	}
	if nodes, callID, ok := tr.Tree(c.TaxonKey); ok {
		c.SetTree(nodes, &card.Why{Tool: "taxon_tree", CallID: callID, Source: tools.TaxonURL(c.TaxonKey), SourceTitle: "GBIF"})
	}
	return c
}

func notFoundFinisher(query string, missing **card.NotFound) agent.Finisher {
	return agent.Finisher{
		Name: "report_not_found",
		Description: "Сообщить, что сведений о запрошенном животном нет: это не животное, название вымышлено, " +
			"искажено, или статьи именно о нём не нашлось. Похожее животное не подставлять.",
		Parameters: json.RawMessage(card.NotFoundSchema),
		Handle: func(_ context.Context, _ string, args json.RawMessage) (any, error) {
			var in struct {
				Reason string `json:"reason"`
			}
			if err := tools.ParseArgs(args, &in); err != nil {
				return nil, err
			}
			reason := strings.TrimSpace(in.Reason)
			if reason == "" {
				reason = "сведений о таком животном не найдено"
			}
			*missing = &card.NotFound{Query: query, Reason: reason}
			return **missing, nil
		},
	}
}

// parsePlain разбирает ответ контрольной дорожки. Проверки нет — есть
// только пометка: латынь, которую не подтвердил GBIF, отмечается в
// карточке, и стенд её считает.
func parsePlain(tr *card.Tracker, text, query string) (*card.Card, *card.NotFound) {
	var in struct {
		card.CardDraft
		NotFound string `json:"not_found"`
	}
	raw := text
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		raw = text[i : j+1]
	}
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil, &card.NotFound{Query: query, Reason: "ответ не разобрался: " + tools.Truncate(text, 200)}
	}
	if in.NotFound != "" || in.Latin == "" {
		reason := in.NotFound
		if reason == "" {
			reason = "сведений нет"
		}
		return nil, &card.NotFound{Query: query, Reason: reason}
	}
	c := card.Card{ID: "text-" + strings.ToLower(strings.ReplaceAll(strings.TrimSpace(in.Latin), " ", "-")),
		Name: in.Name, Latin: in.Latin, Query: query, Article: in.Article, Summary: in.Summary,
		Sections: card.EmptySections(), Sources: []card.Source{}}
	if m, ok := tr.Match(in.Latin); ok {
		c.ID, c.TaxonKey, c.Rank, c.Latin = card.IDOf(m.Key), m.Key, m.Rank, m.Canonical
		c.RankRu = tools.RankRu[m.Rank]
		c.AddSource(card.Source{Kind: "gbif", Title: "GBIF: " + m.Canonical, URL: tools.TaxonURL(m.Key)})
	} else {
		c.Unverified = true
		c.Notes = append(c.Notes, "латынь не сверена с GBIF: карточка принята без проверки")
	}
	if a, ok := tr.Article(in.Article); ok {
		c.Article, c.ArticleURL = a.Title, a.URL
		c.AddSource(card.Source{Kind: "wikipedia", Title: "Википедия: " + a.Title, URL: a.URL})
	} else {
		c.Unverified = true
		c.Notes = append(c.Notes, "статья «"+in.Article+"» в этом прогоне не читалась")
	}
	return &c, nil
}
