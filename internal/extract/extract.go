// Package extract — извлекатель: один запрос на ход решает, что из новой
// реплики запомнить и куда положить (ФТ-18). Адресатов четыре, и они
// разной природы:
//
//	профиль         — как разговаривать с этим человеком (манера);
//	долговременная  — что известно о человеке вообще, между разговорами;
//	рабочая         — что собрано по текущей подборке;
//	карточка фактов — что сказано и решено в этой ветке разговора.
//
// Главное в промпте — граница между ними (П-6). Запрос один: по запросу на
// адресата сломал бы бюджет в четыре запроса на ход.
//
// Извлекатель читает только реплики пользователя и ответы модели, но не
// содержимое источников (ФТ-42): ничто из текста статьи не может стать
// правкой профиля, памяти или карточки фактов.
//
// memory и profile друг про друга не знают; про обоих знает этот пакет.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/facts"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/memory"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/profile"
)

// recentMessages — сколько последних сообщений показать вместе с репликой:
// без них «да, второй вариант» не во что превратить.
const recentMessages = 6

// DefaultMaxTokens — потолок ответа: это JSON с правками, а не текст.
const DefaultMaxTokens = 700

// Targets — какие адресаты включены на этом ходе (механизмы диалога).
type Targets struct {
	Profile bool
	Long    bool
	Work    bool
	Facts   bool
}

// Any — есть ли куда писать вообще.
func (t Targets) Any() bool { return t.Profile || t.Long || t.Work || t.Facts }

// Input — с чем извлекатель входит в ход.
type Input struct {
	Targets Targets
	Profile profile.Profile
	Long    memory.Card
	Work    memory.Card
	Facts   facts.State
	// History — путь ветки до реплики; содержимое инструментов из него
	// выбрасывается.
	History []llm.Message
	User    string
	Turn    int
	// Reserved — чьи ключи не пишет никто, кроме хозяина: анкета, карточка
	// животного, код.
	Reserved func(key string) string
}

// Reply — ответ извлекателя.
type Reply struct {
	Memory  memory.Patch  `json:"memory"`
	Profile profile.Patch `json:"profile"`
	Facts   FactsPatch    `json:"facts"`
}

// FactsPatch — правки карточки фактов.
type FactsPatch struct {
	Set []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"set"`
	Delete []string `json:"delete"`
}

// FactChange — правка карточки фактов.
type FactChange struct {
	Op     string `json:"op"`
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Update — итог: адресаты после правок, сами правки и цена запроса.
type Update struct {
	Profile        profile.Profile `json:"-"`
	ProfileChanges profile.Changes `json:"profile,omitempty"`
	Long           memory.Card     `json:"-"`
	Work           memory.Card     `json:"-"`
	MemoryChanges  memory.Changes  `json:"memory,omitempty"`
	Facts          facts.State     `json:"-"`
	FactChanges    []FactChange    `json:"facts,omitempty"`
	Called         bool            `json:"called"`
	Usage          llm.Usage       `json:"usage"`
	Cost           llm.Cost        `json:"cost"`
	Seconds        float64         `json:"seconds"`
}

// Extractor — один запрос к модели на ход. Он часть обвязки хода, а не
// инструмент агента (ИП-2): модель его не видит и вызвать не может.
type Extractor struct {
	LLM       llm.Chatter
	Model     string
	MaxTokens int
}

// Run — правки по реплике. Правки накладываются на копии адресатов из
// Input; записать их — дело вызывающего. Ошибка — не ошибка хода: ход идёт
// с прежними памятью и профилем.
func (e Extractor) Run(ctx context.Context, in Input) (Update, error) {
	upd := Update{Profile: in.Profile.Clone(), Long: in.Long.Clone(), Work: in.Work.Clone(), Facts: in.Facts.Clone()}
	if !in.Targets.Any() {
		return upd, nil
	}
	max := e.MaxTokens
	if max <= 0 {
		max = DefaultMaxTokens
	}
	started := time.Now()
	resp, err := e.LLM.Chat(ctx, llm.Request{
		Model: e.Model,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: System(in.Targets)},
			{Role: llm.RoleUser, Content: request(in)},
		},
		Temperature: 0,
		MaxTokens:   max,
	})
	upd.Called = true
	upd.Usage, upd.Seconds = resp.Usage, time.Since(started).Seconds()
	upd.Cost = llm.PriceOf(e.Model, resp.Usage, started)
	if err != nil {
		return upd, fmt.Errorf("извлекатель: %w", err)
	}
	reply, err := ParseReply(resp.Message.Content)
	if err != nil {
		return upd, fmt.Errorf("извлекатель: %w", err)
	}
	upd.apply(in, reply)
	return upd, nil
}

// apply раскладывает ответ по адресатам по правилам приложения.
func (u *Update) apply(in Input, r Reply) {
	if in.Targets.Profile {
		u.ProfileChanges = profile.Apply(&u.Profile, r.Profile, in.User, in.Turn)
	}
	if in.Targets.Long || in.Targets.Work {
		var long, work *memory.Card
		if in.Targets.Long && u.Long.ID != "" {
			long = &u.Long
		}
		if in.Targets.Work && u.Work.ID != "" {
			work = &u.Work
		}
		u.MemoryChanges = memory.Apply(long, work, r.Memory, memory.Rules{
			UseLong: long != nil, UseWork: work != nil, Reserved: in.Reserved,
			MaxLong: memory.DefaultMaxLong, MaxWork: memory.DefaultMaxWork,
		}, in.Turn)
	}
	if in.Targets.Facts {
		for _, f := range r.Facts.Set {
			key := strings.ToLower(strings.TrimSpace(f.Key))
			if key == "" || strings.TrimSpace(f.Value) == "" {
				continue
			}
			if in.Reserved != nil {
				if owner := in.Reserved(key); owner != "" {
					u.FactChanges = append(u.FactChanges, FactChange{Op: memory.OpSkip, Key: key, Value: f.Value,
						Reason: "ключ ведёт " + owner + ": один ключ живёт в одном месте"})
					continue
				}
			}
			// Ключ уже живёт в слое памяти — в карточку фактов он не дублируется
			// (ИП-4 при ветках: иначе у модели два ответа на один вопрос).
			if _, ok := u.Long.Get(key); ok {
				u.FactChanges = append(u.FactChanges, FactChange{Op: memory.OpSkip, Key: key, Value: f.Value, Reason: "ключ уже в долговременной памяти"})
				continue
			}
			if _, ok := u.Work.Get(key); ok {
				u.FactChanges = append(u.FactChanges, FactChange{Op: memory.OpSkip, Key: key, Value: f.Value, Reason: "ключ уже в рабочей памяти"})
				continue
			}
			if u.Facts.Set(key, f.Value, in.Turn) {
				u.FactChanges = append(u.FactChanges, FactChange{Op: memory.OpSet, Key: key, Value: f.Value})
			}
		}
		for _, k := range r.Facts.Delete {
			if u.Facts.Delete(k) {
				u.FactChanges = append(u.FactChanges, FactChange{Op: memory.OpDelete, Key: k})
			}
		}
	}
}

// Changed — изменилось ли хоть что-нибудь.
func (u Update) Changed() bool {
	return len(u.ProfileChanges.Applied()) > 0 || len(u.MemoryChanges.Applied()) > 0 || len(u.FactChanges) > 0
}

// ParseReply разбирает ответ: берётся самый внешний объект — модель то и
// дело заворачивает JSON в ограду или предваряет фразой.
func ParseReply(s string) (Reply, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return Reply{}, fmt.Errorf("пустой ответ")
	}
	if i := strings.Index(text, "{"); i >= 0 {
		if j := strings.LastIndex(text, "}"); j > i {
			text = text[i : j+1]
		}
	}
	var r Reply
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return Reply{}, fmt.Errorf("ответ не разобрался как JSON: %w", err)
	}
	return r, nil
}

// Render — реплики диалога для извлекателя. Ответы инструментов не
// показываются вовсе: это содержимое источников (ФТ-42); вызовы — одной
// пометкой, без аргументов.
func Render(ms []llm.Message) string {
	var b strings.Builder
	for _, m := range ms {
		switch m.Role {
		case llm.RoleUser:
			b.WriteString("Пользователь: " + strings.TrimSpace(m.Content) + "\n")
		case llm.RoleAssistant:
			if t := strings.TrimSpace(m.Content); t != "" {
				b.WriteString("Справочник: " + clip(t, 600) + "\n")
			}
		}
	}
	return b.String()
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
