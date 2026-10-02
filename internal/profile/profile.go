// Package profile — анкета собеседника (ФТ-20): уровень изложения, длина,
// форма, обращение, латынь, единицы, эмодзи и ограничения. Рядом со
// значением — готовая строка промпта: профиль уходит модели директивами, а
// не пожеланием, и поэтому соблюдается, а не вспоминается.
//
// Профиль — не слой памяти: слой — про сведения, профиль — про манеру
// (ФТ-21). Он уходит первым блоком после свода при любом наборе слоёв, и
// выключить его можно только всем механизмом (контрольная дорожка), а не
// слоями памяти.
//
// memory и profile друг про друга не знают; про обоих знает extract.
package profile

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Поля анкеты.
const (
	FieldLevel   = "level"
	FieldLength  = "length"
	FieldForm    = "form"
	FieldAddress = "address"
	FieldLatin   = "latin"
	FieldUnits   = "units"
	FieldEmoji   = "emoji"
)

// Option — допустимое значение поля и строка промпта при нём.
type Option struct {
	Value     string `json:"value"`
	Title     string `json:"title"`
	Directive string `json:"directive"`
}

// Field — поле анкеты. Checked — соблюдение видно в тексте ответа и
// проверяется кодом (ФТ-23); для остальных честное «не проверяется» (Р-5).
type Field struct {
	Key     string   `json:"key"`
	Title   string   `json:"title"`
	Options []Option `json:"options"`
	Checked bool     `json:"checked"`
	// Words — как поле называют люди: эти ключи память не пишет (ИП-4).
	Words []string `json:"-"`
}

// Option — значение поля по его коду.
func (f Field) Option(v string) (Option, bool) {
	for _, o := range f.Options {
		if o.Value == v {
			return o, true
		}
	}
	return Option{}, false
}

var fields = []Field{
	{Key: FieldLevel, Title: "уровень изложения", Words: []string{"уровень", "возраст", "подготовка", "кто я"},
		Options: []Option{
			{"child", "ребёнок 7–10 лет", "Объясняй как ребёнку 7–10 лет: простые слова, короткие фразы, сравнения с тем, что ребёнок видел сам; никаких терминов без объяснения."},
			{"school", "школьник", "Объясняй как школьнику: обычные слова, термины — с коротким пояснением."},
			{"amateur", "любитель", "Объясняй как любителю природы: обычный уровень, термины можно, без упрощений до детских."},
			{"expert", "специалист", "Говори как со специалистом-зоологом: полные названия таксонов и ранги, никаких упрощений."},
		}},
	{Key: FieldLength, Title: "длина ответа", Checked: true, Words: []string{"длина", "длина ответа"},
		Options: []Option{
			{"tiny", "очень коротко", "Отвечай очень коротко: одно-два предложения."},
			{"short", "коротко", "Отвечай коротко: до четырёх предложений."},
			{"normal", "обычно", "Отвечай обычной длины: абзац-два по существу."},
			{"long", "подробно", "Отвечай подробно: разбирай тему полностью, с деталями из источника."},
		}},
	{Key: FieldForm, Title: "форма ответа", Checked: true, Words: []string{"форма", "форма ответа", "формат"},
		Options: []Option{
			{"prose", "сплошной текст", "Пиши сплошным текстом, без списков и заголовков."},
			{"list", "списком", "Оформляй ответ списком: по пункту на мысль."},
			{"table", "таблицей, где уместно", "Когда сравниваешь или перечисляешь признаки, давай таблицу в markdown."},
		}},
	{Key: FieldAddress, Title: "обращение", Checked: true, Words: []string{"обращение", "ты или вы"},
		Options: []Option{
			{"ty", "на «ты»", "Обращайся к собеседнику на «ты»."},
			{"vy", "на «вы»", "Обращайся к собеседнику на «вы»."},
		}},
	{Key: FieldLatin, Title: "латинские названия", Checked: true, Words: []string{"латынь в ответах", "латинские названия"},
		Options: []Option{
			{"hide", "скрывать", "Латинских названий в ответе не пиши совсем — только русские."},
			{"caption", "в подписи", "Латинское название давай один раз, в скобках после русского."},
			{"full", "везде с рангами", "Давай латинские названия таксонов везде, где они уместны, вместе с рангами."},
		}},
	{Key: FieldUnits, Title: "единицы", Words: []string{"единицы", "единицы измерения"},
		Options: []Option{
			{"metric", "метрические", "Размеры и массу давай в метрических единицах."},
			{"compare", "с пояснением («с кошку»)", "Размеры и массу давай в метрических единицах и поясняй сравнением с привычным: «размером с кошку»."},
		}},
	{Key: FieldEmoji, Title: "эмодзи", Checked: true, Words: []string{"эмодзи", "смайлики"},
		Options: []Option{
			{"no", "нет", "Не используй эмодзи."},
			{"some", "умеренно", "Можно одно-два уместных эмодзи на ответ."},
		}},
}

// Fields — анкета целиком для интерфейса: вторая копия в JavaScript
// разошлась бы с сервером.
func Fields() []Field { return fields }

// FieldOf — поле по коду или названию.
func FieldOf(key string) (Field, bool) {
	k := strings.ToLower(strings.TrimSpace(key))
	for _, f := range fields {
		if f.Key == k || f.Title == k {
			return f, true
		}
	}
	return Field{}, false
}

// Normalize — код значения по коду или по названию: модель иногда пишет
// «коротко» вместо «short».
func Normalize(field, value string) (string, bool) {
	f, ok := FieldOf(field)
	if !ok {
		return "", false
	}
	v := strings.ToLower(strings.TrimSpace(value))
	for _, o := range f.Options {
		if o.Value == v || strings.ToLower(o.Title) == v {
			return o.Value, true
		}
	}
	return "", false
}

// Reserved — чей это ключ памяти: «профиль», если им ведает анкета.
func Reserved(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	for _, f := range fields {
		if k == f.Key || k == f.Title {
			return true
		}
		for _, w := range f.Words {
			if k == w {
				return true
			}
		}
	}
	return k == "ограничения" || k == "обращаться" || k == "как отвечать"
}

// Источники значения.
const (
	SourceModel = "model"
	SourceHuman = "human"
	SourceRule  = "rule"
)

// Value — значение поля с происхождением: откуда взялось и по какой цитате.
type Value struct {
	Value  string `json:"value"`
	Source string `json:"source"`
	Turn   int    `json:"turn"`
	Quote  string `json:"quote,omitempty"`
}

// Limit — ограничение: чего не делать вообще.
type Limit struct {
	Text   string `json:"text"`
	Source string `json:"source"`
	Turn   int    `json:"turn"`
	Quote  string `json:"quote,omitempty"`
}

// MaxLimits — потолок ограничений: старое вытесняется новым.
const MaxLimits = 8

// Profile — анкета человека.
type Profile struct {
	ID     string           `json:"id"`
	Title  string           `json:"title,omitempty"`
	Values map[string]Value `json:"values"`
	Limits []Limit          `json:"limits"`
	// Asks — счётчик разовых просьб «поле=значение»: повторённая просьба
	// закрепляется (ФТ-22).
	Asks map[string]int `json:"asks,omitempty"`
	// Legacy — поля прежней анкеты, которым в анкете справочника нет места:
	// хранятся как были, в запрос не уходят.
	Legacy  map[string]json.RawMessage `json:"legacy,omitempty"`
	Version int                        `json:"version"`
	Created *time.Time                 `json:"created,omitempty"`
	Updated *time.Time                 `json:"updated,omitempty"`
}

// New — пустая анкета.
func New(id, title string) Profile {
	return Profile{ID: id, Title: title, Values: map[string]Value{}, Limits: []Limit{}}
}

// Empty — не заполнено ни одно поле и нет ограничений.
func (p Profile) Empty() bool { return len(p.Values) == 0 && len(p.Limits) == 0 }

// Val — код значения поля; пусто — не заполнено.
func (p Profile) Val(field string) string { return p.Values[field].Value }

// Get — значение поля.
func (p Profile) Get(field string) (Value, bool) {
	v, ok := p.Values[field]
	return v, ok
}

// Set ставит значение и говорит, изменилось ли оно.
func (p *Profile) Set(field string, v Value) bool {
	if p.Values == nil {
		p.Values = map[string]Value{}
	}
	if p.Values[field].Value == v.Value {
		return false
	}
	p.Values[field] = v
	p.touch()
	return true
}

// Clear очищает поле.
func (p *Profile) Clear(field string) bool {
	if _, ok := p.Values[field]; !ok {
		return false
	}
	delete(p.Values, field)
	p.touch()
	return true
}

// Ask засчитывает разовую просьбу и возвращает, сколько их было всего.
func (p *Profile) Ask(field, value string) int {
	if p.Asks == nil {
		p.Asks = map[string]int{}
	}
	p.Asks[field+"="+value]++
	p.touch()
	return p.Asks[field+"="+value]
}

// AddLimit добавляет ограничение без повторов; по потолку вытесняется самое
// старое. Возвращает, добавлено ли, и что вытеснено.
func (p *Profile) AddLimit(l Limit) (bool, string) {
	l.Text = strings.Join(strings.Fields(l.Text), " ")
	if l.Text == "" {
		return false, ""
	}
	for _, have := range p.Limits {
		if strings.EqualFold(have.Text, l.Text) {
			return false, ""
		}
	}
	p.Limits = append(p.Limits, l)
	dropped := ""
	if len(p.Limits) > MaxLimits {
		dropped = p.Limits[0].Text
		p.Limits = p.Limits[1:]
	}
	p.touch()
	return true, dropped
}

// DropLimit снимает ограничение.
func (p *Profile) DropLimit(text string) bool {
	for i, l := range p.Limits {
		if strings.EqualFold(l.Text, strings.TrimSpace(text)) {
			p.Limits = append(p.Limits[:i], p.Limits[i+1:]...)
			p.touch()
			return true
		}
	}
	return false
}

func (p *Profile) touch() {
	now := time.Now()
	p.Version++
	p.Updated = &now
	if p.Created == nil {
		p.Created = &now
	}
}

// Clone — глубокая копия.
func (p Profile) Clone() Profile {
	out := p
	out.Values = make(map[string]Value, len(p.Values))
	for k, v := range p.Values {
		out.Values[k] = v
	}
	out.Limits = append([]Limit{}, p.Limits...)
	if p.Asks != nil {
		out.Asks = make(map[string]int, len(p.Asks))
		for k, v := range p.Asks {
			out.Asks[k] = v
		}
	}
	return out
}

// With — анкета с разовыми правками этого хода: в файл они не пишутся, но в
// запрос уходят.
func (p Profile) With(once []SetOp) Profile {
	out := p.Clone()
	for _, op := range once {
		if v, ok := Normalize(op.Field, op.Value); ok {
			f, _ := FieldOf(op.Field)
			out.Values[f.Key] = Value{Value: v, Source: SourceModel, Quote: op.Quote}
		}
	}
	return out
}

// Filled — сколько полей заполнено.
func (p Profile) Filled() int { return len(p.Values) }

// Prompt — анкета блоком запроса: «как разговаривать с этим человеком»
// (П-3). Пустая анкета блока не даёт.
func (p Profile) Prompt() string {
	if p.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("Как разговаривать с этим человеком — профиль собеседника. Это правила формы ответа, а не тема разговора: соблюдай их молча, не объявляй и не обсуждай.\n")
	for _, f := range fields {
		v, ok := p.Values[f.Key]
		if !ok {
			continue
		}
		if o, ok := f.Option(v.Value); ok {
			b.WriteString("- " + o.Directive + "\n")
		}
	}
	if len(p.Limits) > 0 {
		b.WriteString("Ограничения (не делать никогда):\n")
		for _, l := range p.Limits {
			b.WriteString("- " + l.Text + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// Describe — анкета словами для извлекателя и журнала.
func (p Profile) Describe() string {
	var lines []string
	for _, f := range fields {
		label := "не задано"
		if v, ok := p.Values[f.Key]; ok {
			label = labelOf(f, v.Value)
		}
		lines = append(lines, fmt.Sprintf("- %s (%s): %s", f.Title, f.Key, label))
	}
	for _, l := range p.Limits {
		lines = append(lines, "- ограничение: "+l.Text)
	}
	return strings.Join(lines, "\n")
}

// Summary — анкета одной строкой: «специалист, коротко, на «вы»».
func (p Profile) Summary() string {
	var parts []string
	for _, f := range fields {
		if v, ok := p.Values[f.Key]; ok {
			parts = append(parts, labelOf(f, v.Value))
		}
	}
	if len(p.Limits) > 0 {
		parts = append(parts, fmt.Sprintf("ограничений: %d", len(p.Limits)))
	}
	if len(parts) == 0 {
		return "профиль пуст"
	}
	return strings.Join(parts, ", ")
}

func labelOf(f Field, value string) string {
	if o, ok := f.Option(value); ok {
		return o.Title
	}
	if value == "" {
		return "не задано"
	}
	return value
}

// Label — значение поля словами.
func Label(field, value string) string {
	f, ok := FieldOf(field)
	if !ok {
		return value
	}
	return labelOf(f, value)
}

// Preset — заготовка анкеты для роли (раздел 1 ТЗ) и для дорожек стенда И-3.
type Preset struct {
	ID     string            `json:"id"`
	Title  string            `json:"title"`
	Values map[string]string `json:"values"`
}

// Presets — заготовки ролей: ребёнок, любитель, специалист.
func Presets() []Preset {
	return []Preset{
		{ID: "child", Title: "ребёнок 7–10 лет", Values: map[string]string{FieldLevel: "child", FieldLength: "short", FieldForm: "prose",
			FieldAddress: "ty", FieldLatin: "hide", FieldUnits: "compare", FieldEmoji: "some"}},
		{ID: "amateur", Title: "любитель", Values: map[string]string{FieldLevel: "amateur", FieldLength: "normal", FieldLatin: "caption"}},
		{ID: "expert", Title: "специалист", Values: map[string]string{FieldLevel: "expert", FieldLength: "normal", FieldAddress: "vy",
			FieldLatin: "full", FieldUnits: "metric", FieldEmoji: "no"}},
	}
}

// PresetOf — заготовка по коду.
func PresetOf(id string) (Preset, bool) {
	for _, p := range Presets() {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// Build — анкета по заготовке.
func (pr Preset) Build(id, title string) Profile {
	p := New(id, title)
	keys := make([]string, 0, len(pr.Values))
	for k := range pr.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p.Set(k, Value{Value: pr.Values[k], Source: SourceHuman})
	}
	return p
}
