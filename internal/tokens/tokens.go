// Package tokens — оценка числа токенов до отправки запроса.
//
// Точное число знает только токенизатор модели, а он у DeepSeek лежит
// сборкой под Python. Поэтому здесь оценка по классам символов: латиница,
// кириллица, цифры, иероглифы, знаки. Документация DeepSeek даёт две
// опорные точки — английский символ ≈ 0.3 токена, китайский ≈ 0.6, — а
// вес кириллицы подобран по фактическому расходу из ответов API.
//
// Оценка нужна до запроса: чтобы показать, из чего складывается контекст —
// по блокам реестра механизмов, — и чтобы поймать переполнение раньше, чем
// его поймает API. Факт всегда берётся из usage ответа; калибровка сводит
// одно с другим, и несверенный оценщик — фантазия, а не оценка.
package tokens

import (
	"sync"
	"unicode"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

// Weights — сколько токенов приходится на символ каждого класса и какие
// накладные даёт структура запроса.
type Weights struct {
	Latin    float64 `json:"latin"`
	Cyrillic float64 `json:"cyrillic"`
	CJK      float64 `json:"cjk"`
	Digit    float64 `json:"digit"`
	Space    float64 `json:"space"`
	Other    float64 `json:"other"`

	// PerMessage — служебная обвязка одного сообщения: роль, разделители,
	// идентификатор вызова инструмента.
	PerMessage float64 `json:"perMessage"`
	// PerToolDef — обвязка одного описания инструмента поверх его текста.
	PerToolDef float64 `json:"perToolDef"`
	// PerRequest — обвязка всего запроса.
	PerRequest float64 `json:"perRequest"`
}

// Default — веса по умолчанию. Латиница и иероглифы взяты из документации
// DeepSeek, вес кириллицы и знаков подобран по фактическому расходу: первый
// прогон с весом кириллицы 0.50 завышал оценку на 19–36 %, после подгонки
// ошибка укладывается в единицы процентов.
var Default = Weights{
	Latin:      0.30,
	Cyrillic:   0.39,
	CJK:        0.60,
	Digit:      0.40,
	Space:      0.08,
	Other:      0.36,
	PerMessage: 3,
	PerToolDef: 6,
	PerRequest: 3,
}

// Estimate — оценка запроса по частям. Блоки — по механизмам реестра: так
// видно, во что обходится каждый механизм в каждом ходе, и выключенный
// механизм в разбивке просто отсутствует.
type Estimate struct {
	System  int                   `json:"system"`
	Blocks  map[features.Name]int `json:"blocks"`
	Tools   int                   `json:"tools"`
	History int                   `json:"history"`
	User    int                   `json:"user"`
	Total   int                   `json:"total"`
	// Constant — постоянная часть запроса: всё, кроме окна сообщений и
	// новой реплики. У неё свой бюджет (раздел 10 ТЗ: 6 тыс. токенов).
	Constant int `json:"constant"`
}

// Block — доля одного механизма.
func (e Estimate) Block(n features.Name) int { return e.Blocks[n] }

// Tokens — оценка до отправки против факта из ответа модели. Actual = 0
// означает, что ответа ещё нет.
type Tokens struct {
	Estimated int `json:"estimated"`
	Actual    int `json:"actual,omitempty"`
	// ErrorPct — на сколько процентов оценка разошлась с фактом;
	// отрицательное значение — оценка занизила.
	ErrorPct float64 `json:"errorPct,omitempty"`
}

// Text — оценка одной строки.
func (w Weights) Text(s string) float64 {
	var sum float64
	for _, r := range s {
		switch {
		case r >= 0x4E00 && r <= 0x9FFF, r >= 0x3040 && r <= 0x30FF:
			sum += w.CJK
		case unicode.IsSpace(r):
			sum += w.Space
		case unicode.IsDigit(r):
			sum += w.Digit
		case unicode.Is(unicode.Cyrillic, r):
			sum += w.Cyrillic
		case unicode.Is(unicode.Latin, r):
			sum += w.Latin
		default:
			sum += w.Other
		}
	}
	return sum
}

// Message — оценка одного сообщения вместе с обвязкой. Аргументы вызовов
// инструментов считаются как текст: модель платит и за них.
func (w Weights) Message(m llm.Message) float64 {
	sum := w.PerMessage + w.Text(m.Content)
	for _, c := range m.ToolCalls {
		sum += w.Text(c.Function.Name) + w.Text(c.Function.Arguments) + w.PerMessage
	}
	return sum
}

// Messages — оценка списка сообщений.
func (w Weights) Messages(ms []llm.Message) float64 {
	var sum float64
	for _, m := range ms {
		sum += w.Message(m)
	}
	return sum
}

// ToolDefs — оценка описаний инструментов. Они уходят модели в каждом
// запросе целиком, вместе со схемой аргументов.
func (w Weights) ToolDefs(defs []llm.ToolDef) float64 {
	var sum float64
	for _, d := range defs {
		sum += w.PerToolDef + w.Text(d.Function.Name) + w.Text(d.Function.Description) + w.Text(string(d.Function.Parameters))
	}
	return sum
}

// Parts — из чего складывается запрос: системный промпт, блоки механизмов в
// порядке реестра, описания инструментов, окно истории и новая реплика.
type Parts struct {
	System  string
	Blocks  []features.Block
	Tools   []llm.ToolDef
	History []llm.Message
	User    string
}

// Of — разбивка запроса. Total считается по сумме долей, чтобы части
// складывались в целое. Пустой блок даёт нулевую долю и в разбивку не
// попадает: выключенный механизм не отправляется вовсе.
func (w Weights) Of(p Parts) Estimate {
	e := Estimate{
		System:  round(w.PerMessage + w.Text(p.System)),
		Blocks:  map[features.Name]int{},
		Tools:   round(w.ToolDefs(p.Tools)),
		History: round(w.Messages(p.History)),
		User:    round(w.PerMessage + w.Text(p.User)),
	}
	blocks := 0
	for _, b := range p.Blocks {
		if b.Text == "" {
			continue
		}
		n := round(w.PerMessage + w.Text(b.Text))
		e.Blocks[b.Feature] += n
		blocks += n
	}
	e.Constant = e.System + blocks + e.Tools + round(w.PerRequest)
	e.Total = e.Constant + e.History + e.User
	return e
}

// OfMessages — оценка готового списка сообщений: им пользуется цикл хода,
// где история, ответы модели и результаты инструментов уже перемешаны.
func (w Weights) OfMessages(defs []llm.ToolDef, ms []llm.Message) int {
	return round(w.Messages(ms) + w.ToolDefs(defs) + w.PerRequest)
}

// Of — разбивка с весами по умолчанию.
func Of(p Parts) Estimate { return Default.Of(p) }

// OfMessages — оценка списка сообщений с весами по умолчанию.
func OfMessages(defs []llm.ToolDef, ms []llm.Message) int {
	return Default.OfMessages(defs, ms)
}

// Compare сводит оценку с фактом. Факт 0 (ответа нет) оставляет одну
// оценку: делить на ноль и рисовать стопроцентную ошибку нечестно.
func Compare(estimated, actual int) Tokens {
	t := Tokens{Estimated: estimated, Actual: actual}
	if actual > 0 {
		t.ErrorPct = float64(estimated-actual) / float64(actual) * 100
	}
	return t
}

func round(f float64) int {
	if f <= 0 {
		return 0
	}
	return int(f + 0.5)
}

// Calibration копит пары «оценка — факт» и считает поправочный множитель
// (ФТ-32). Оценщик по классам символов переносится с провайдера на
// провайдера плохо (Р-6: калибровка обязательна), а множитель по живым
// ответам сводит ошибку к единицам процентов. Безопасна для горутин.
type Calibration struct {
	mu     sync.Mutex
	est    float64
	actual float64
	n      int
}

// Observe записывает пару. Пары без факта не учитываются.
func (c *Calibration) Observe(estimated, actual int) {
	if estimated <= 0 || actual <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.est += float64(estimated)
	c.actual += float64(actual)
	c.n++
}

// Pairs — сколько пар учтено.
func (c *Calibration) Pairs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// Factor — во сколько раз надо умножить оценку, чтобы она совпала с фактом
// в среднем. Без данных — 1; множитель ограничен, чтобы одна странная пара
// не увела оценку в разы.
func (c *Calibration) Factor() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == 0 || c.est == 0 {
		return 1
	}
	k := c.actual / c.est
	if k < 0.7 {
		k = 0.7
	}
	if k > 1.3 {
		k = 1.3
	}
	return k
}

// Apply — оценка с поправкой.
func (c *Calibration) Apply(estimated int) int {
	if c == nil {
		return estimated
	}
	return round(float64(estimated) * c.Factor())
}

// ErrorPct — средняя ошибка оценки без поправки, в процентах от факта:
// ради неё калибровка и заведена, её показывает отчёт.
func (c *Calibration) ErrorPct() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.actual == 0 {
		return 0
	}
	return (c.est - c.actual) / c.actual * 100
}
