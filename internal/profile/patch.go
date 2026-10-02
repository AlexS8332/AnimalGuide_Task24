package profile

import (
	"fmt"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/words"
)

// Самопополнение профиля (ФТ-22). Правило, купленное опытом: цитата
// доказывает, что слова были сказаны, поэтому её слова обязаны найтись в
// текущей реплике — не в статье, не в ответе модели, не в прошлых ходах.
// Это заодно и защита от содержимого источников (ФТ-42): правка, которую
// «подсказал» текст статьи, цитаты в реплике не найдёт.

// Области действия правки.
const (
	ScopeAlways = "always"
	ScopeOnce   = "once"
)

// SetOp — правка поля, предложенная извлекателем.
type SetOp struct {
	Field string `json:"field"`
	Value string `json:"value"`
	Scope string `json:"scope"`
	Quote string `json:"quote"`
}

// LimitOp — ограничение.
type LimitOp struct {
	Text  string `json:"text"`
	Drop  bool   `json:"drop,omitempty"`
	Scope string `json:"scope"`
	Quote string `json:"quote"`
}

// Patch — ответ извлекателя про профиль.
type Patch struct {
	Set    []SetOp   `json:"set"`
	Limits []LimitOp `json:"limits"`
}

// Виды правок профиля.
const (
	OpSet     = "set"
	OpOnce    = "once"
	OpLimit   = "limit"
	OpUnlimit = "unlimit"
	OpDrop    = "drop"
	OpReject  = "reject"
)

// Change — правка профиля или отказ: чипом под ответом с цитатой (ФТ-22).
type Change struct {
	Op     string `json:"op"`
	Field  string `json:"field,omitempty"`
	Title  string `json:"title,omitempty"`
	From   string `json:"from,omitempty"`
	Value  string `json:"value,omitempty"`
	Label  string `json:"label,omitempty"`
	Source string `json:"source,omitempty"`
	Reason string `json:"reason,omitempty"`
	Quote  string `json:"quote,omitempty"`
}

// String — правка словами.
func (c Change) String() string {
	switch c.Op {
	case OpOnce:
		return fmt.Sprintf("разово, только для этого ответа: %s — %s", c.Title, c.Label)
	case OpLimit:
		return fmt.Sprintf("в ограничения: «%s»", c.Value)
	case OpUnlimit:
		return fmt.Sprintf("снято ограничение: «%s»", c.Value)
	case OpDrop:
		return fmt.Sprintf("вытеснено из ограничений: «%s»", c.Value)
	case OpReject:
		what := c.Value
		if c.Title != "" {
			what = c.Title + " = " + c.Value
		}
		return fmt.Sprintf("отклонено: «%s» — %s", what, c.Reason)
	}
	if c.From != "" && c.From != "не задано" {
		return fmt.Sprintf("в профиле «%s»: %s вместо «%s»", c.Title, c.Label, c.From)
	}
	return fmt.Sprintf("в профиле «%s»: %s", c.Title, c.Label)
}

// Changes — правки хода.
type Changes []Change

// Applied — правки, изменившие файл профиля.
func (cs Changes) Applied() Changes {
	var out Changes
	for _, c := range cs {
		switch c.Op {
		case OpSet, OpLimit, OpUnlimit, OpDrop:
			out = append(out, c)
		}
	}
	return out
}

// Once — разовые правки хода.
func (cs Changes) Once() []SetOp {
	var out []SetOp
	for _, c := range cs {
		if c.Op == OpOnce {
			out = append(out, SetOp{Field: c.Field, Value: c.Value, Scope: ScopeOnce, Quote: c.Quote})
		}
	}
	return out
}

// Summary — правки одной строкой.
func (cs Changes) Summary() string {
	if len(cs) == 0 {
		return "профиль не изменился"
	}
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.String()
	}
	return strings.Join(parts, "; ")
}

// promoteAfter — после стольких разовых просьб об одном и том же просьба
// закрепляется в профиле: повторённая — это уже предпочтение.
const promoteAfter = 2

// Apply накладывает правки на анкету по правилам ФТ-22.
func Apply(p *Profile, patch Patch, user string, turn int) Changes {
	var out Changes
	for _, op := range patch.Set {
		out = append(out, applySet(p, op, user, turn)...)
	}
	for _, op := range patch.Limits {
		out = append(out, applyLimit(p, op, user, turn)...)
	}
	return out
}

func applySet(p *Profile, op SetOp, user string, turn int) Changes {
	f, ok := FieldOf(op.Field)
	if !ok {
		return Changes{{Op: OpReject, Field: op.Field, Value: op.Value, Source: SourceRule, Reason: "такого поля в анкете нет", Quote: op.Quote}}
	}
	value, ok := Normalize(f.Key, op.Value)
	if !ok {
		return Changes{{Op: OpReject, Field: f.Key, Title: f.Title, Value: op.Value, Source: SourceRule,
			Reason: "значение не из перечня: свободный текст в анкету не кладётся", Quote: op.Quote}}
	}
	label := labelOf(f, value)
	if reason := checkQuote(op.Quote, user); reason != "" {
		return Changes{{Op: OpReject, Field: f.Key, Title: f.Title, Value: value, Label: label, Source: SourceRule, Reason: reason, Quote: op.Quote}}
	}
	if strings.EqualFold(strings.TrimSpace(op.Scope), ScopeOnce) {
		if n := p.Ask(f.Key, value); n >= promoteAfter && p.Val(f.Key) != value {
			from := labelOf(f, p.Val(f.Key))
			p.Set(f.Key, Value{Value: value, Source: SourceRule, Turn: turn, Quote: op.Quote})
			return Changes{{Op: OpSet, Field: f.Key, Title: f.Title, From: from, Value: value, Label: label, Source: SourceRule, Quote: op.Quote,
				Reason: fmt.Sprintf("о том же просили разово уже %d раза — это предпочтение, закреплено", n)}}
		}
		return Changes{{Op: OpOnce, Field: f.Key, Title: f.Title, Value: value, Label: label, Source: SourceModel, Quote: op.Quote,
			Reason: "просьба про этот ответ — профиль не меняется"}}
	}
	from := labelOf(f, p.Val(f.Key))
	if !p.Set(f.Key, Value{Value: value, Source: SourceModel, Turn: turn, Quote: op.Quote}) {
		return nil
	}
	return Changes{{Op: OpSet, Field: f.Key, Title: f.Title, From: from, Value: value, Label: label, Source: SourceModel, Quote: op.Quote}}
}

func applyLimit(p *Profile, op LimitOp, user string, turn int) Changes {
	text := strings.Join(strings.Fields(op.Text), " ")
	if text == "" {
		return nil
	}
	if reason := checkQuote(op.Quote, user); reason != "" {
		return Changes{{Op: OpReject, Value: text, Source: SourceRule, Reason: reason, Quote: op.Quote}}
	}
	if op.Drop {
		if p.DropLimit(text) {
			return Changes{{Op: OpUnlimit, Value: text, Source: SourceModel, Quote: op.Quote}}
		}
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(op.Scope), ScopeOnce) {
		return Changes{{Op: OpReject, Value: text, Source: SourceRule, Reason: "разовая просьба в ограничения не идёт", Quote: op.Quote}}
	}
	added, dropped := p.AddLimit(Limit{Text: text, Source: SourceModel, Turn: turn, Quote: op.Quote})
	if !added {
		return nil
	}
	out := Changes{{Op: OpLimit, Value: text, Source: SourceModel, Quote: op.Quote}}
	if dropped != "" {
		out = append(out, Change{Op: OpDrop, Value: dropped, Source: SourceRule, Reason: fmt.Sprintf("потолок — %d ограничений", MaxLimits)})
	}
	return out
}

// checkQuote — цитата обязательна, и её значимые слова должны найтись в
// текущей реплике пользователя. Пустая строка — всё в порядке.
func checkQuote(quote, user string) string {
	if strings.TrimSpace(quote) == "" {
		return "нет цитаты: правка без слов пользователя не принимается"
	}
	if ok, _ := words.InReply(quote, user, true); !ok {
		return "слов цитаты нет в текущей реплике пользователя"
	}
	return ""
}
