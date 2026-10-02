package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

const (
	sectionMaxSteps = 6
	compareMaxSteps = 12
)

const sectionSystemTemplate = `Ты агент-специалист по разделу «%s» карточки животного. Получаешь статью Википедии и её оглавление. Найди в статье сведения по теме и перескажи их для этого человека.

` + groundingRules + `

Правила:
- Работай только по тексту, который вернул read_wikipedia. Ничего не добавляй из памяти.
- Начни с раздела, название которого ближе всего к теме (например: %s). Если в оглавлении такого нет, проверь разделы, где тема может быть внутри («Образ жизни», «Биология», «Описание»).
- Если подходящих разделов нет, но сведения по теме есть во вступлении (read_wikipedia без section), перескажи их и укажи section = «вступление».
- В поле section передавай название раздела так, как его вернул read_wikipedia.
- Длину, форму и уровень пересказа задаёт профиль собеседника, если он есть; иначе — 3–6 предложений по существу.
- Если сведений по теме в статье нет, сдай found = false с коротким объяснением.
- Если программа уже прочитала подходящий раздел (его ответ read_wikipedia есть в разговоре) и сведений в нём хватает, не читай его снова — сразу сдавай результат.`

const sectionFinish = `

Заверши работу вызовом submit_section, а не текстом.`

const sectionPlain = `

Ответь одним JSON-объектом без пояснений: {"found": true, "section": "раздел статьи", "text": "пересказ"} — или {"found": false, "section": "", "text": "почему сведений нет"}. Пересказ — только по прочитанному через read_wikipedia разделу.`

// ReadSection — читатель раздела: пересказ темы по статье под профиль
// собеседника (С-1, ФТ-13). С трекером пересказ принимается, только если
// раздел действительно читали; без него — как написала модель.
func ReadSection(ctx context.Context, d Deps, reg *tools.Registry, c card.Card, topic card.Topic, blocks []features.Block, fs features.Set, em agent.Emitter) (card.Section, agent.Stats, error) {
	name := "section." + topic.Key
	tr := card.NewTracker()
	list, err := sourcePick(reg, "read_wikipedia")
	if err != nil {
		return card.Section{}, agent.Stats{}, err
	}
	spec := agent.Spec{Name: name, System: fmt.Sprintf(sectionSystemTemplate, topic.Title, strings.Join(topic.Candidates, ", ")),
		Tools: tr.ObserveAll(list), MaxSteps: sectionMaxSteps}
	user := fmt.Sprintf("Животное: %s.\nСтатья: «%s».\nОглавление статьи:\n- %s\nТема: %s.",
		c.Title(), c.Article, strings.Join(headingsOr(c.Headings), "\n- "), topic.Title)

	var got *card.Section
	if fs.On(features.Tracker) {
		spec.System += sectionFinish
		spec.Finish = []agent.Finisher{{
			Name:        "submit_section",
			Description: "Сдать пересказ раздела или сообщить, что сведений в статье нет. Код проверит, что раздел читали.",
			Parameters:  json.RawMessage(card.SectionSchema),
			Handle: func(_ context.Context, _ string, args json.RawMessage) (any, error) {
				var dr card.SectionDraft
				if err := tools.ParseArgs(args, &dr); err != nil {
					return nil, err
				}
				s, err := card.CheckSection(tr, &c, topic, dr)
				if err != nil {
					return nil, err
				}
				got = &s
				return s, nil
			},
		}}
	} else {
		spec.System += sectionPlain
		spec.AllowText = true
	}
	reply, err := d.Runner.Run(ctx, spec, agent.Prepared{Blocks: headBlocks(blocks), User: user, Features: fs,
		Preload: sectionPreload(c, topic)}, em)
	if err != nil {
		return card.Section{}, reply.Stats, fmt.Errorf("раздел «%s»: %w", topic.Title, err)
	}
	if !fs.On(features.Tracker) {
		got = plainSection(tr, c, topic, reply.Text)
	}
	if got == nil {
		return card.Section{}, reply.Stats, fmt.Errorf("раздел «%s»: специалист не сдал результат", topic.Title)
	}
	return *got, reply.Stats, nil
}

// sectionPreloadMax — сколько разделов читать кодом до специалиста. Два —
// как он сам чаще всего и читает («Распространение и численность» плюс
// «Среда обитания»); больше — лишние токены в запросе, а не сбережённый
// запрос.
const sectionPreloadMax = 2

// sectionPreload — разделы, которые код читает сам до первого запроса к
// специалисту. Шаг без выбора: промпт велит начать с раздела, чьё название
// ближе всего к теме, а кандидаты темы и оглавление статьи известны коду.
// Специалист получает прочитанное как свой первый шаг и чаще всего сразу
// сдаёт пересказ — один запрос к модели вместо двух (клик по разделу —
// ФТ-13, бюджет $0.001). Совпадения нет — не читается ничего, и специалист
// ищет раздел сам, как раньше. Проверка по трекеру не ослабевает: вызов
// кодом идёт через тот же наблюдаемый инструмент, что и вызов моделью.
func sectionPreload(c card.Card, topic card.Topic) []agent.Preload {
	if strings.TrimSpace(c.Article) == "" {
		return nil
	}
	var out []agent.Preload
	for _, h := range TopicHeadings(c.Headings, topic) {
		out = append(out, agent.Preload{Tool: "read_wikipedia", Args: jsonArgs(map[string]string{"title": c.Article, "section": h})})
	}
	return out
}

// TopicHeadings — разделы оглавления, подходящие теме: сначала точные
// совпадения с кандидатами темы в их порядке, потом заголовки, в которых
// кандидат содержится («Охота и питание» для «Питание»). Не больше
// sectionPreloadMax.
func TopicHeadings(headings []string, topic card.Topic) []string {
	low := func(s string) string { return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "ё", "е") }
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		if k := low(h); k != "" && !seen[k] && len(out) < sectionPreloadMax {
			seen[k] = true
			out = append(out, strings.TrimSpace(h))
		}
	}
	for _, cand := range topic.Candidates {
		for _, h := range headings {
			if low(h) == low(cand) {
				add(h)
			}
		}
	}
	for _, cand := range topic.Candidates {
		for _, h := range headings {
			if strings.Contains(low(h), low(cand)) {
				add(h)
			}
		}
	}
	return out
}

func jsonArgs(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func headingsOr(h []string) []string {
	if len(h) == 0 {
		return []string{"(оглавление неизвестно — открой статью read_wikipedia без section)"}
	}
	return h
}

// plainSection — пересказ контрольной дорожки: без проверки, с пометкой,
// если раздел не читался.
func plainSection(tr *card.Tracker, c card.Card, topic card.Topic, text string) *card.Section {
	var in card.SectionDraft
	raw := text
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		raw = text[i : j+1]
	}
	s := card.Section{Key: topic.Key, Title: topic.Title}
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		s.Status, s.Text = card.SectionRead, strings.TrimSpace(text)
		s.Reason = "ответ не по форме: раздел не указан"
		return &s
	}
	if !in.Found {
		s.Status, s.Reason = card.SectionNone, in.Text
		return &s
	}
	s.Status, s.Heading, s.Text = card.SectionRead, in.Heading, in.Text
	if sec, ok := tr.SectionRead(c.Article, in.Heading); ok {
		s.Why = &card.Why{Tool: "read_wikipedia", CallID: sec.CallID, Source: c.ArticleURL}
	} else {
		s.Reason = "раздел не читался: пересказ не подтверждён источником"
	}
	return &s
}

const compareSystem = `Ты агент сравнения справочника по животным. Сравни двух животных по строкам: %s.

` + groundingRules + `

Правила:
- Для каждого животного читай разделы его статьи read_wikipedia (title — статья этого животного, section — раздел из её оглавления или без section для вступления).
- В ячейку — факт из прочитанного раздела и название этого раздела (a_section / b_section) так, как его вернул read_wikipedia.
- Если сведений о животном по строке в статье нет — «сведений нет». Не догадка и не сведения о похожем животном.
- Факт о первом животном бери только из его статьи, о втором — только из его.

Заверши работу вызовом submit_comparison, а не текстом.`

// Compare — сравнивающий: таблица по общим разделам (С-4). Ячейка без
// прочитанного раздела становится «сведений нет» кодом, а не догадкой.
func Compare(ctx context.Context, d Deps, reg *tools.Registry, a, b card.Card, blocks []features.Block, fs features.Set, em agent.Emitter) (card.Comparison, agent.Stats, error) {
	tr := card.NewTracker()
	list, err := sourcePick(reg, "read_wikipedia")
	if err != nil {
		return card.Comparison{}, agent.Stats{}, err
	}
	spec := agent.Spec{Name: "comparer", System: fmt.Sprintf(compareSystem, strings.Join(card.Aspects, ", ")),
		Tools: tr.ObserveAll(list), MaxSteps: compareMaxSteps}
	user := fmt.Sprintf("Первое животное: %s. Статья «%s». Оглавление: %s.\nВторое животное: %s. Статья «%s». Оглавление: %s.",
		a.Title(), a.Article, strings.Join(headingsOr(a.Headings), "; "),
		b.Title(), b.Article, strings.Join(headingsOr(b.Headings), "; "))

	var got *card.Comparison
	spec.Finish = []agent.Finisher{{
		Name:        "submit_comparison",
		Description: "Сдать таблицу сравнения. Код оставит в ячейке только факты из прочитанных разделов, остальное станет «сведений нет».",
		Parameters:  json.RawMessage(card.ComparisonSchema),
		Handle: func(_ context.Context, _ string, args json.RawMessage) (any, error) {
			var dr card.ComparisonDraft
			if err := tools.ParseArgs(args, &dr); err != nil {
				return nil, err
			}
			cmp, err := card.CheckComparison(tr, a, b, dr)
			if err != nil {
				return nil, err
			}
			got = &cmp
			return cmp, nil
		},
	}}
	reply, err := d.Runner.Run(ctx, spec, agent.Prepared{Blocks: headBlocks(blocks), User: user, Features: fs}, em)
	if err != nil {
		return card.Comparison{}, reply.Stats, fmt.Errorf("сравнение: %w", err)
	}
	if got == nil {
		return card.Comparison{}, reply.Stats, fmt.Errorf("сравнение: результат не сдан")
	}
	return *got, reply.Stats, nil
}
