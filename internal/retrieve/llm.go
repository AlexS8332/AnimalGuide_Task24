package retrieve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

// Платные шаги конвейера: переписывание и реранкинг моделью. По одному
// запросу на поиск, температура 0, ответ — JSON. Неразборчивый ответ — не
// ошибка поиска: конвейер откатывается на бесплатный шаг и пишет об этом в
// Note (поиск без переписывания хуже, чем поиск с кодовым, но лучше, чем
// никакой). Ошибка сети — ошибка: платили за ответ, которого нет, и
// вызывающий должен это увидеть.

// rewriteSystem — системный промпт переписывания.
const rewriteSystem = `Ты готовишь запрос к поиску по базе знаний о хищных млекопитающих: статьи русской Википедии о видах и справочник MDD v2.5 (латинские названия видов, статусы МСОП).
Перепиши вопрос пользователя в самодостаточный поисковый запрос по-русски:
- назови вид его обычным русским названием и, если знаешь, латинским; разговорное название, описание («кошка, которую называют …») или местоимение («она», «он») замени видом — местоимение восстанови по предыдущим репликам;
- оставь то, что спрашивают (вес, питание, статус, число видов), без вежливых слов;
- если вопрос составной (сравнение двух видов, цепочка «кто это → какой у него статус»), добавь подзапросы — по одному на каждую часть, не больше трёх.
Не отвечай на вопрос. Ответ — только JSON без пояснений: {"query": "…", "queries": ["…"]}; queries может быть пустым.`

// rewriteUser — сообщение переписыванию: реплики контекста и вопрос.
func rewriteUser(q Query) string {
	var prev []string
	for _, c := range q.Context {
		if c = strings.TrimSpace(c); c != "" {
			prev = append(prev, "- "+c)
		}
	}
	text := strings.TrimSpace(q.Text)
	if len(prev) == 0 {
		return "Вопрос: " + text
	}
	return "Предыдущие реплики пользователя:\n" + strings.Join(prev, "\n") + "\n\nВопрос: " + text
}

// maxSubqueries — подзапросов модели сверх основного.
const maxSubqueries = 3

// rewriteLLM — переписывание моделью; поверх — синонимы кодом (Queries) по
// каждому запросу. Неразборчивый ответ — откат на RewriteCode с заметкой.
func (p *Pipeline) rewriteLLM(ctx context.Context, al *Aliases, q Query, t *Trace) error {
	resp, err := p.LLM.Chat(ctx, llm.Request{Model: p.model(), Temperature: 0, Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: rewriteSystem}, {Role: llm.RoleUser, Content: rewriteUser(q)}}})
	if err != nil {
		return fmt.Errorf("переписывание моделью: %w", err)
	}
	p.addCost(t, resp.Usage)
	var out struct {
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
	}
	if err := parseJSON(resp.Message.Content, &out); err != nil || strings.TrimSpace(out.Query) == "" {
		why := "пустой query"
		if err != nil {
			why = err.Error()
		}
		t.RewriteBy = string(RewriteCode)
		var note, bm25 string
		t.Rewritten, bm25, t.Expanded, note = rewriteCode(al, q)
		t.Queries = []string{t.Rewritten}
		t.QueriesBM25 = []string{bm25}
		t.Note = joinNote(note, "модель ответила неразборчиво ("+clip(why, 120)+") — переписано кодом")
		return nil
	}
	t.RewriteBy = string(RewriteLLM)
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[normQuery(s)] {
			return
		}
		seen[normQuery(s)] = true
		dense, bm25, list := al.Queries(s)
		for _, x := range list {
			if !contains(t.Expanded, x) {
				t.Expanded = append(t.Expanded, x)
			}
		}
		t.Queries = append(t.Queries, dense)
		t.QueriesBM25 = append(t.QueriesBM25, bm25)
	}
	add(out.Query)
	for _, s := range out.Queries {
		if len(t.Queries) > maxSubqueries {
			break
		}
		add(s)
	}
	t.Rewritten = t.Queries[0]
	return nil
}

// rerankSystem — системный промпт реранкинга.
const rerankSystem = `Ты оцениваешь, помогают ли фрагменты базы знаний ответить на вопрос. Шкала:
3 — во фрагменте есть ответ на вопрос или его часть;
2 — фрагмент о том же виде и той же теме, ответ вероятен рядом;
1 — тот же вид или та же тема, но ответа нет;
0 — не относится к вопросу.
Оцени каждый фрагмент по его id. Ответ — только JSON без пояснений: {"scores": {"<id>": <0–3>, …}}.`

// rerankChars — сколько начальных символов фрагмента видит реранкер.
const rerankChars = 300

// noteRerankFailed — заметка о неразборчивом ответе реранкера.
const noteRerankFailed = "модель-реранкер ответила неразборчиво — порядок RRF"

// rerankLLM — оценки модели всем кандидатам одним запросом. Кандидат без
// оценки в ответе получает 0 (модель его пропустила — значит, не нашла
// пользы); неразборчивый ответ — порядок RRF и заметка, оценок нет.
func (p *Pipeline) rerankLLM(ctx context.Context, t *Trace) error {
	if len(t.Candidates) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Вопрос: %s\n", t.Original)
	if t.Rewritten != "" && t.Rewritten != t.Original {
		fmt.Fprintf(&b, "Поисковый запрос: %s\n", t.Rewritten)
	}
	b.WriteString("\nФрагменты:\n")
	for _, x := range t.Candidates {
		path := x.Section
		if len(x.Path) > 0 {
			path = strings.Join(x.Path, " › ")
		}
		fmt.Fprintf(&b, "\n[%s] %s › %s\n%s\n", x.ID, x.Title, path, clip(strings.Join(strings.Fields(x.Text), " "), rerankChars))
	}
	resp, err := p.LLM.Chat(ctx, llm.Request{Model: p.model(), Temperature: 0, Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: rerankSystem}, {Role: llm.RoleUser, Content: b.String()}}})
	if err != nil {
		return fmt.Errorf("реранкинг моделью: %w", err)
	}
	p.addCost(t, resp.Usage)
	scores, err := parseScores(resp.Message.Content)
	if err != nil {
		t.Note = joinNote(t.Note, noteRerankFailed+" ("+clip(err.Error(), 120)+")")
		return nil
	}
	t.scores = map[string]float64{}
	missing := 0
	for _, x := range t.Candidates {
		v, ok := scores[x.ID]
		if !ok {
			missing++
		}
		t.scores[x.ID] = min(max(v, 0), 3)
	}
	if missing > 0 {
		t.Note = joinNote(t.Note, fmt.Sprintf("реранкер не оценил %d из %d кандидатов — им 0", missing, len(t.Candidates)))
	}
	return nil
}

// parseScores — {"scores": {id: n}} или {"scores": [{"id":…, "score":…}]}.
func parseScores(s string) (map[string]float64, error) {
	var raw struct {
		Scores json.RawMessage `json:"scores"`
	}
	if err := parseJSON(s, &raw); err != nil {
		return nil, err
	}
	if len(raw.Scores) == 0 {
		return nil, errors.New("нет поля scores")
	}
	var m map[string]float64
	if err := json.Unmarshal(raw.Scores, &m); err == nil {
		return m, nil
	}
	var list []struct {
		ID    string  `json:"id"`
		Score float64 `json:"score"`
	}
	if err := json.Unmarshal(raw.Scores, &list); err != nil {
		return nil, fmt.Errorf("scores: %w", err)
	}
	m = map[string]float64{}
	for _, x := range list {
		m[x.ID] = x.Score
	}
	return m, nil
}

// parseJSON — JSON-объект из ответа модели: без ограды ``` и текста вокруг.
func parseJSON(s string, v any) error {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return errors.New("в ответе нет JSON-объекта")
	}
	return json.Unmarshal([]byte(s[i:j+1]), v)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
