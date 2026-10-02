package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
)

// answerSystem — системный промпт отвечающего агента, ОБЩИЙ для обоих
// режимов. Режимы отличаются только правилом ragRule в конце и блоком
// фрагментов в сообщении пользователя: всё остальное побайтно одинаково,
// иначе у разницы в ответах была бы вторая причина. Правило «не знаешь —
// скажи» стоит в общей части намеренно: без него norag выдумывал бы чаще,
// чем модель умеет на самом деле, и сравнение мерило бы промпт, а не базу.
const answerSystem = `Ты — справочник о животных. Отвечай по-русски, кратко и по существу: сначала прямой ответ на вопрос, затем, если нужно, одно-два уточнения. Числа называй с единицами измерения.
Если не знаешь ответа или не уверен в нём — так и скажи прямо, не выдумывай факты, числа и названия.`

// ragRule — правило режима RAG. Идёт последним абзацем системного
// промпта: общий префикс обоих режимов кэшируется у провайдера, а правило
// и фрагменты — хвост запроса.
const ragRule = `К вопросу приложены фрагменты базы знаний справочника. Опирайся только на них, а не на свою память: если фрагменты расходятся с тем, что ты помнишь, верны фрагменты. Называй источники — chunk_id фрагментов в квадратных скобках, например [manul/structure/004]. Если во фрагментах ответа нет — так и скажи: «в базе знаний этого нет», и не дополняй ответ по памяти.`

// citeRule — правило kb_answer (режим rag+cite, v24): идёт после ragRule.
// ragRule велит называть [chunk_id] в тексте — здесь это уточнено: ссылки
// уходят в sources, а не в answer. Слово «ДОСЛОВНО» и запрет пересказа —
// главное: цитата, которую модель «улучшила» (сократила, поправила падеж,
// переставила слова), не проходит проверку кодом и стоит отказа и ещё
// одного запроса. «Числа — только из цитат» — та же проверка с другой
// стороны: число ответа без цитаты — это число без опоры. Как говорить
// «не знаю» — подробно: answer «в базе знаний нет …» (его читает человек
// после «Не знаю:») и уточнение по ближайшему найденному, без фактов в
// нём — иначе уточняющий вопрос становился бы ответом по памяти.
const citeRule = `Ответ отдавай только вызовом инструмента kb_answer, не текстом:
- status: answered — ответ есть во фрагментах; unknown — ответа во фрагментах нет (и тогда, когда о животном фрагменты есть, а нужного факта в них нет).
- answer: прямой ответ кратко, одно-три предложения, без ссылок [chunk_id]. Каждое число ответа должно стоять в одной из цитат.
- sources: chunk_id фрагментов, на которых держится ответ, — только из приложенных.
- quotes: одна-три цитаты, подтверждающие ответ: кусок текста фрагмента с этим chunk_id, скопированный ДОСЛОВНО — без пересказа, сокращений и правки слов, чисел и знаков, не короче 15 символов; пропуск внутри цитаты отмечай «…». Факты и числа ответа — только из цитат.
- unknown: answer — чего именно нет («в базе знаний нет данных о …»), clarify — уточняющий вопрос человеку по ближайшему, что нашлось во фрагментах (без фактов в нём), sources и quotes — пустые списки.
Если kb_answer вернул ошибку — исправь ровно то, что в ней названо, и вызови kb_answer снова.`

// gateNote — пометка в сообщении пользователя, когда «не знаю» решил код
// (Gate): модель знает, почему answered недоступен, и формулирует
// уточнение, а не ищет, как ответить.
const gateNote = "Пометка кода: релевантных фрагментов нет — %s. Ответ на этот вопрос — только kb_answer со status unknown и уточняющим вопросом в clarify."

// System — системный промпт режима (для вкладки и тестов).
// Режимы v23 (rag+filter, rag+rewrite, rag+both) — тот же промпт, что у
// rag: отличаются только фрагменты. rag+cite (v24) — тот же промпт и
// правило kb_answer последним абзацем.
func System(mode Mode) string {
	switch {
	case mode == RAGCite:
		return answerSystem + "\n\n" + ragRule + "\n\n" + citeRule
	case mode.UsesBase():
		return answerSystem + "\n\n" + ragRule
	}
	return answerSystem
}

// CiteRule — правило kb_answer (для отчёта и тестов).
func CiteRule() string { return citeRule }

// Query — строка поиска по вопросу: предыдущие реплики и сам вопрос через
// пробел (как в kb.Compare). Вопрос-продолжение «а сколько она весит?» без
// предыдущей реплики искать нечем.
func (q Question) Query() string {
	parts := make([]string, 0, len(q.Context)+1)
	for _, c := range q.Context {
		if c = strings.TrimSpace(c); c != "" {
			parts = append(parts, c)
		}
	}
	return strings.Join(append(parts, strings.TrimSpace(q.Text)), " ")
}

// UserText — сообщение пользователя без фрагментов. Без контекста — сам
// вопрос; с контекстом — «Предыдущие реплики пользователя:» списком, пустая
// строка и «Вопрос: …».
func (q Question) UserText() string {
	text := strings.TrimSpace(q.Text)
	var prev []string
	for _, c := range q.Context {
		if c = strings.TrimSpace(c); c != "" {
			prev = append(prev, "- "+c)
		}
	}
	if len(prev) == 0 {
		return text
	}
	return "Предыдущие реплики пользователя:\n" + strings.Join(prev, "\n") + "\n\nВопрос: " + text
}

// QuestionOf — вопрос набора для отвечающего агента.
func QuestionOf(q kb.Question) Question {
	return Question{Text: q.Q, Context: q.Context}
}

// Answer отвечает в режиме. RAG без базы (Searcher == nil) — ошибка, а не
// тихий NoRAG.
//
// Один запрос к модели, без инструментов, температура 0: системный промпт
// и ОДНО сообщение пользователя. Контекст вопроса (предыдущие реплики
// человека) — в том же сообщении перед вопросом (UserText): ответов
// справочника на них у отвечающего агента нет, а несколько сообщений
// пользователя подряд без ответов между ними модель читает как
// оборванный диалог. Фрагменты базы — в том же сообщении, после вопроса.
func (a *Answerer) Answer(ctx context.Context, q Question, mode Mode) (Answer, error) {
	if strings.TrimSpace(q.Text) == "" {
		return Answer{}, errors.New("пустой вопрос")
	}
	if !mode.Known() {
		return Answer{}, fmt.Errorf("неизвестный режим %q (%s)", mode, modeList())
	}
	if a == nil || a.LLM == nil {
		return Answer{}, errors.New("отвечающему агенту не передана модель")
	}
	out := Answer{Mode: mode, System: System(mode), User: q.UserText()}
	switch {
	case mode.Pipelined():
		if a.Pipeline == nil {
			return Answer{}, fmt.Errorf("режим %s без конвейера поиска (Answerer.Pipeline)", mode)
		}
		t, err := a.Pipeline.Search(ctx, retrieve.Query{Text: q.Text, Context: q.Context}, a.Config(mode))
		if err != nil {
			return Answer{}, fmt.Errorf("поиск по базе знаний (%s): %w", mode, err)
		}
		out.Trace = &t
		out.Hits, out.Search = a.plant(q, t.Hits), t.Info
		// Пустой итог фильтра — та же явная строка «ничего не найдено»,
		// что у пустой выдачи: модель должна сказать «в базе этого нет».
		out.User += "\n\n" + Compose(out.Hits)
	case mode == RAG:
		if a.Searcher == nil {
			return Answer{}, errors.New("режим rag без базы знаний: соберите её командой kb index")
		}
		hits, info, err := a.Searcher.Search(ctx, q.Query(), kb.SearchOptions{Index: a.index(), K: a.k()})
		if err != nil {
			return Answer{}, fmt.Errorf("поиск по базе знаний: %w", err)
		}
		out.Hits, out.Search = a.plant(q, hits), info
		out.User += "\n\n" + Compose(out.Hits)
	}
	if mode == RAGCite {
		return a.cite(ctx, out)
	}
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: out.System}, {Role: llm.RoleUser, Content: out.User}}

	started := time.Now()
	resp, err := a.LLM.Chat(ctx, llm.Request{Model: a.model(), Messages: msgs, Temperature: 0})
	out.Millis = time.Since(started).Milliseconds()
	if err != nil {
		return Answer{}, fmt.Errorf("модель (%s): %w", mode, err)
	}
	out.Text = strings.TrimSpace(resp.Message.Content)
	out.Usage = resp.Usage
	out.Cost = llm.PriceOf(a.model(), resp.Usage, a.now())
	if out.Trace != nil && out.Trace.Usage.Total > 0 {
		// Платные шаги конвейера (переписывание, реранкинг моделью) — в цене
		// ответа: режим платит за них, и сравнение цены режимов честное.
		out.Usage = out.Usage.Add(out.Trace.Usage)
		out.Cost = out.Cost.Add(out.Trace.Cost)
	}
	return out, nil
}

// plant — выдача с подставными фрагментами (Plant).
func (a *Answerer) plant(q Question, hits []kb.Hit) []kb.Hit {
	if a.Plant == nil {
		return hits
	}
	extra := a.Plant(q)
	if len(extra) == 0 {
		return hits
	}
	out := append(append([]kb.Hit(nil), hits...), extra...)
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

// citeSteps — предел запросов rag+cite: первый ответ, по запросу на каждый
// отказ проверки и на каждое напоминание «ответь инструментом».
const citeSteps = 1 + MaxRejects + agent.MaxReminders

// cite — ответ rag+cite: Gate по трассе (выдача пуста или фильтр отсёк
// всё → схема kb_answer только с unknown и пометка в сообщении), затем
// agent.Runner с одним завершающим инструментом kb_answer без права
// закончить текстом. Текст ответа — CitedResult.Text, оценка — по
// Cited (Eval). Цена — сумма шагов и платных шагов конвейера.
func (a *Answerer) cite(ctx context.Context, out Answer) (Answer, error) {
	gated, why := Gate(out.Trace, out.Hits)
	if gated {
		out.User += "\n\n" + fmt.Sprintf(gateNote, why)
	}
	hits := out.Hits
	fin := Finisher(func() []kb.Hit { return hits }, gated, why)
	spec := agent.Spec{Name: "answerer", System: out.System, Finish: []agent.Finisher{fin}, AllowText: false, MaxSteps: citeSteps}
	r := agent.Runner{LLM: a.LLM, Model: a.model(), Temperature: 0}
	started := time.Now()
	reply, err := r.Run(ctx, spec, agent.Prepared{User: out.User}, nil)
	out.Millis = time.Since(started).Milliseconds()
	out.Usage, out.Cost = reply.Stats.Usage, reply.Stats.Cost
	if out.Trace != nil && out.Trace.Usage.Total > 0 {
		out.Usage = out.Usage.Add(out.Trace.Usage)
		out.Cost = out.Cost.Add(out.Trace.Cost)
	}
	if err != nil {
		return out, fmt.Errorf("модель (%s): %w", RAGCite, err)
	}
	res, ok := reply.Final.(*CitedResult)
	if !ok {
		return out, fmt.Errorf("модель (%s): kb_answer не принят", RAGCite)
	}
	out.Cited = res
	out.Text = res.Text()
	return out, nil
}

// Config — настройки конвейера режима: ModeConfig, поверх — заданные
// в Configs поля (ненулевые; Filter включается, но не выключается), индекс
// и k отвечающего агента, если их не задали.
func (a *Answerer) Config(m Mode) retrieve.Config {
	c := ModeConfig(m)
	if o, ok := a.Configs[m]; ok {
		if o.Index != "" {
			c.Index = o.Index
		}
		if o.K0 > 0 {
			c.K0 = o.K0
		}
		if o.K1 > 0 {
			c.K1 = o.K1
		}
		if o.Rewrite != "" {
			c.Rewrite = o.Rewrite
		}
		if o.Rerank != "" {
			c.Rerank = o.Rerank
		}
		if o.MinScore > 0 {
			c.MinScore = o.MinScore
		}
		if o.Delta > 0 {
			c.Delta = o.Delta
		}
		c.Filter = c.Filter || o.Filter
		c.Scope = c.Scope || o.Scope
	}
	if c.Index == "" {
		c.Index = a.index()
	}
	if c.K1 == 0 {
		c.K1 = a.k()
	}
	return c
}

// modeList — «norag, rag, rag+filter, …» для сообщений об ошибке.
func modeList() string {
	parts := make([]string, len(Modes))
	for i, m := range Modes {
		parts[i] = string(m)
	}
	return strings.Join(parts, ", ")
}

func (a *Answerer) index() string { return orIndex(a.Index) }
func (a *Answerer) k() int        { return orK(a.K) }

func (a *Answerer) model() string {
	if strings.TrimSpace(a.Model) == "" {
		return llm.DefaultModel
	}
	return a.Model
}

func (a *Answerer) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
