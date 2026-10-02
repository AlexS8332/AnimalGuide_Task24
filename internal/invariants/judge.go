package invariants

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

// Судья: вторая ступень стража (ФТ-30).
//
// Страж ищет слова и не понимает смысла: «таблетки коту не давайте, это
// решает ветеринар» и «дайте коту полтаблетки» словарём не различить. По
// отобранным фрагментам задаётся отдельный короткий вопрос модели.
//
// Почему отдельный запрос, а не проверка в том же ходу: агент, которого
// просят проверить самого себя, проверяет благосклонно. Судья не видит ни
// системного промпта агента, ни истории — только правило, реплику и
// фрагменты; у него нет задачи быть полезным и памяти о том, что он это
// написал.

// JudgeSystem — промпт судьи. Начало строки — признак, по которому
// подставная модель тестов узнаёт судью.
const JudgeSystem = `Ты — судья свода справочника по животным. Ты проверяешь черновик ответа справочника на нарушение правил свода. Работай строго по тексту, ничего не додумывай и ничего не исправляй.

Нарушение — когда ответ сам делает то, что правило запрещает: даёт совет или инструкцию, оценивает, подставляет похожее животное, выдаёт сведения не из источников. Не нарушение: ответ называет запрещённое, чтобы от него отказаться; объясняет, почему так нельзя; отсылает к специалисту; пересказывает сведения источника без совета.

Ответь только JSON вида {"verdicts":[{"invariant":"ид","violates":true|false,"why":"одна фраза"}]} — по одной записи на каждое правило из списка. Никакого текста вокруг.`

// Verdict — решение судьи по одному инварианту.
type Verdict struct {
	Invariant string `json:"invariant"`
	Violates  bool   `json:"violates"`
	Fragment  string `json:"fragment,omitempty"`
	Why       string `json:"why,omitempty"`
}

// Review — итог проверки ответа.
type Review struct {
	Screened []Hit     `json:"screened,omitempty"`
	Verdicts []Verdict `json:"verdicts,omitempty"`
	Broken   []Verdict `json:"broken,omitempty"`
	// Called — судья звался; Checked — вердикт получен.
	Called  bool      `json:"called"`
	Checked bool      `json:"checked"`
	Err     string    `json:"error,omitempty"`
	Usage   llm.Usage `json:"usage"`
	Cost    llm.Cost  `json:"cost"`
	Seconds float64   `json:"seconds,omitempty"`
}

// OK — ответ можно отдавать.
func (r Review) OK() bool { return len(r.Broken) == 0 }

// Judge — проверяющий: короткий запрос без инструментов.
type Judge struct {
	LLM   llm.Chatter
	Model string
}

// MaxTokens судьи: ответ — JSON в пару строк.
const judgeMaxTokens = 400

// Check — проверить ответ по отобранным фрагментам. Нет подозрений — нет
// запроса: ответ без единого маркера проверять нечем и незачем.
func (j Judge) Check(ctx context.Context, c Charter, s Screening, user, answer string) Review {
	rev := Review{Screened: s.Hits}
	suspect := s.Suspect()
	if len(suspect) == 0 {
		rev.Checked = true
		return rev
	}
	if j.LLM == nil {
		rev.Err = "судья не подключён"
		return rev
	}
	byInv := map[string][]Hit{}
	var order []string
	for _, h := range suspect {
		if _, ok := byInv[h.Invariant]; !ok {
			order = append(order, h.Invariant)
		}
		byInv[h.Invariant] = append(byInv[h.Invariant], h)
	}
	known := map[string]string{}
	var b strings.Builder
	b.WriteString("Правила, которые надо проверить:\n")
	for _, id := range order {
		inv, ok := c.Item(id)
		if !ok {
			continue
		}
		known[strings.ToLower(inv.ID)] = inv.ID
		fmt.Fprintf(&b, "\n[%s] %s\nПравило: %s\n", inv.ID, inv.Title, inv.Rule)
		if len(inv.Except) > 0 {
			fmt.Fprintf(&b, "Не считается нарушением: %s\n", strings.Join(inv.Except, "; "))
		}
		b.WriteString("Подозрительные места ответа:\n")
		for _, h := range byInv[id] {
			fmt.Fprintf(&b, "- «%s»\n", h.Fragment)
		}
	}
	fmt.Fprintf(&b, "\nО чём спрашивал человек: «%s»\n", trim(user, 400))
	fmt.Fprintf(&b, "\nЧерновик ответа целиком:\n%s\n", trim(answer, 4000))

	started := time.Now()
	rev.Called = true
	resp, err := j.LLM.Chat(ctx, llm.Request{
		Model: j.Model,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: JudgeSystem},
			{Role: llm.RoleUser, Content: b.String()},
		},
		Temperature: 0,
		MaxTokens:   judgeMaxTokens,
	})
	rev.Usage, rev.Seconds = resp.Usage, time.Since(started).Seconds()
	rev.Cost = llm.PriceOf(j.Model, resp.Usage, started)
	if err != nil {
		// Сбой судьи не становится отказом человеку: наказывать за чужую
		// аварию нельзя. Зато видно, что ход остался непроверенным.
		rev.Err = err.Error()
		return rev
	}
	verdicts, err := ParseVerdicts(resp.Message.Content)
	if err != nil {
		rev.Err = err.Error()
		return rev
	}
	rev.Checked = true
	for _, v := range verdicts {
		id, ok := known[strings.ToLower(strings.TrimSpace(v.Invariant))]
		if !ok {
			continue // судья назвал то, чего ему не показывали
		}
		v.Invariant = id
		if v.Fragment == "" && len(byInv[id]) > 0 {
			v.Fragment = byInv[id][0].Fragment
		}
		rev.Verdicts = append(rev.Verdicts, v)
		if v.Violates {
			rev.Broken = append(rev.Broken, v)
		}
	}
	return rev
}

// ParseVerdicts достаёт JSON из ответа судьи: обёртку ```json дешевле
// простить, чем переспрашивать.
func ParseVerdicts(text string) ([]Verdict, error) {
	text = strings.TrimSpace(text)
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j <= i {
		return nil, fmt.Errorf("ответ судьи не разобрался: нет JSON")
	}
	var out struct {
		Verdicts []Verdict `json:"verdicts"`
	}
	if err := json.Unmarshal([]byte(text[i:j+1]), &out); err != nil {
		return nil, fmt.Errorf("ответ судьи не разобрался: %w", err)
	}
	return out.Verdicts, nil
}

// Refusal — ответ, который код выдаёт вместо нарушающего. Последнее слово
// за кодом: инвариант, который держится только на послушности модели, — не
// инвариант. Отказ сделан из полей свода, а не из фантазии, и учит (ИП-8):
// что нельзя, какое правило, почему, что можно вместо этого.
func Refusal(c Charter, broken []Verdict) string {
	var b strings.Builder
	b.WriteString("Так ответить не могу: ответ нарушил правила справочника.\n")
	seen := map[string]bool{}
	for _, v := range broken {
		inv, ok := c.Item(v.Invariant)
		if !ok || seen[inv.ID] {
			continue
		}
		seen[inv.ID] = true
		fmt.Fprintf(&b, "\n**%s. %s**\n\n> %s\n\n", inv.ID, inv.Title, inv.Rule)
		if inv.Because != "" {
			fmt.Fprintf(&b, "Почему: %s\n\n", inv.Because)
		}
		if inv.Instead != "" {
			fmt.Fprintf(&b, "Что можно вместо этого: %s\n\n", inv.Instead)
		}
	}
	b.WriteString("Если правило пора пересмотреть, скажите об этом прямо — справочник подготовит поправку к своду, а решение останется за вами.\n")
	b.WriteString("\n*Ответ заменён проверкой свода.*")
	return b.String()
}
