package rag

import (
	"context"
	"encoding/json"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
)

// Ответ с обязательными источниками и цитатами (v24).
//
// Модель отвечает не текстом, а завершающим инструментом kb_answer
// (FinishName): ответ, источники (chunk_id) и дословные цитаты. Код
// проверяет его ДО того, как ответ дойдёт до человека (ИП-1): каждый chunk_id
// выдавался в этом ходе, у ответа есть хотя бы один источник и одна цитата,
// каждая цитата — дословная подстрока своего фрагмента после нормализации
// (corpus.Normalize: регистр, ё, кавычки, тире, пробелы). Не прошло —
// отказ уходит модели с подсказкой (ИП-8), она исправляется; после
// MaxRejects отказов ответ принимается с пометкой «не проверено» (иначе ход
// крутился бы до предела шагов и тратил деньги).
//
// «Не знаю» решает КОД, а не модель: если релевантность ниже порога
// (Gate: фильтр конвейера отсёк всё, выдача пуста), схема kb_answer на этот
// ход разрешает только status = unknown с непустым уточняющим вопросом —
// ответить по существу модели нечем (запрет — отсутствием варианта, ИП-7).
// Модель и сама может сказать unknown, если в фрагментах ответа нет.

// RAGCite — режим ответа v24: конвейер как у RAGBoth (переписывание +
// фильтр) и kb_answer с проверкой.
const RAGCite Mode = "rag+cite"

// FinishName — завершающий инструмент ответа с источниками.
const FinishName = "kb_answer"

// MaxRejects — сколько отказов проверки до приёма «не проверено».
const MaxRejects = 2

// Статусы ответа.
const (
	StatusAnswered = "answered"
	StatusUnknown  = "unknown"
)

// CitedSource — источник ответа.
type CitedSource struct {
	ChunkID string `json:"chunk_id"`
}

// CitedQuote — цитата: дословный фрагмент текста чанка.
type CitedQuote struct {
	ChunkID string `json:"chunk_id"`
	Text    string `json:"text"`
}

// Cited — аргументы kb_answer.
type Cited struct {
	Status  string        `json:"status"`
	Answer  string        `json:"answer"`
	Sources []CitedSource `json:"sources"`
	Quotes  []CitedQuote  `json:"quotes"`
	// Clarify — уточняющий вопрос человеку (обязателен при unknown).
	Clarify string `json:"clarify,omitempty"`
}

// Text — ответ человеку: текст, «Источники: [1] Статья › Раздел
// (chunk_id)…», цитаты; у unknown — «Не знаю: …» и уточняющий вопрос.
// Заголовки разделов берутся из hits (их передаёт CiteCheck).
func (c Cited) Text() string { return "" }

// CiteCheck — итог проверки ответа кодом.
type CiteCheck struct {
	OK bool `json:"ok"`
	// Unverified — принят после MaxRejects отказов, проверку не прошёл.
	Unverified bool `json:"unverified,omitempty"`
	// Forced — «не знаю» потребовал код (Gate), а не модель.
	Forced bool `json:"forced,omitempty"`
	// HasSources, HasQuotes — есть ли хотя бы по одному (у answered).
	HasSources bool `json:"has_sources"`
	HasQuotes  bool `json:"has_quotes"`
	// UnknownIDs — chunk_id, которых не было в выдаче хода.
	UnknownIDs []string `json:"unknown_ids,omitempty"`
	// NotVerbatim — номера цитат (с 0), которых нет в своём фрагменте.
	NotVerbatim []int `json:"not_verbatim,omitempty"`
	// Verbatim — доля дословных цитат.
	Verbatim float64 `json:"verbatim"`
	// NumbersMissing — числа ответа, которых нет ни в одной цитате (мягкая
	// проверка: первый раз — отказ с подсказкой, второй — только метрика).
	NumbersMissing []string `json:"numbers_missing,omitempty"`
	// Problems — что не так словами (для отказа модели и для окна).
	Problems []string `json:"problems,omitempty"`
	Rejects  int      `json:"rejects"`
}

// Check проверяет ответ против выдачи хода. strictNumbers — числа без
// цитаты дают отказ (первая попытка) или только пометку (повторные).
func Check(c Cited, hits []kb.Hit, strictNumbers bool) CiteCheck { return CiteCheck{} }

// Gate — решение «не знаю» кодом по трассе конвейера: true, если выдача
// пуста (фильтр отсёк всё) или трасса говорит о слабом контексте. Причина —
// для журнала и окна.
func Gate(tr *retrieve.Trace, hits []kb.Hit) (unknown bool, reason string) { return false, "" }

// FinishSchema — JSON Schema аргументов kb_answer; при onlyUnknown — status
// ограничен значением unknown, а clarify обязателен.
func FinishSchema(onlyUnknown bool) json.RawMessage { return nil }

// Finisher — завершающий инструмент kb_answer для agent.Runner (чат и
// Answerer): Handle проверяет Check, считает отказы и на MaxRejects
// принимает с Unverified. Результат Handle — *CitedResult (Texter для
// agents.lead).
func Finisher(hits func() []kb.Hit, onlyUnknown bool, gateReason string) agent.Finisher {
	return agent.Finisher{}
}

// CitedResult — принятый ответ: аргументы, проверка, выдача (для заголовков
// разделов в Text).
type CitedResult struct {
	Cited Cited     `json:"cited"`
	Check CiteCheck `json:"check"`
	Hits  []kb.Hit  `json:"-"`
}

// Text — ответ человеку.
func (r *CitedResult) Text() string { return "" }

// Support — судья смысла: раскладывает ответ на утверждения и для каждого
// решает, подтверждено ли оно цитатами (только цитатами, не памятью).
type Support struct {
	Claims      []SupportClaim `json:"claims"`
	Supported   int            `json:"supported"`
	Unsupported int            `json:"unsupported"`
	// OK — все утверждения подтверждены (или ответ — «не знаю»).
	OK    bool      `json:"ok"`
	Usage llm.Usage `json:"usage"`
	Cost  llm.Cost  `json:"cost"`
}

// SupportClaim — утверждение ответа и вердикт.
type SupportClaim struct {
	Claim     string `json:"claim"`
	Supported bool   `json:"supported"`
	Quote     int    `json:"quote"` // номер подтверждающей цитаты, -1 — нет
	Reason    string `json:"reason,omitempty"`
}

// CheckSupport — один запрос к судье: ответ + цитаты → Support (строгий
// JSON). Судья не видит режима и вопроса не оценивает — только «сказано ли
// это в цитатах».
func (j *Judge) CheckSupport(ctx context.Context, c Cited) (Support, error) {
	return Support{}, ErrNotImplemented
}
