// Package rag — ответ по базе знаний: вопрос → поиск фрагментов →
// объединение с вопросом → запрос к модели, и сравнение такого ответа с
// ответом модели без базы.
//
// Два места, где это работает:
//
//   - Answerer — отдельный отвечающий агент без инструментов. Режимы
//     отличаются РОВНО контекстом: NoRAG — системный промпт и вопрос, RAG —
//     тот же системный промпт, вопрос и найденные фрагменты. На нём стоит
//     сравнение «с RAG / без RAG» (kb qa, И-10, вкладка «Контрольные
//     вопросы»): у ведущего справочника есть Википедия и GBIF, и «без RAG» с
//     ними было бы «модель с живыми источниками», а не «модель без базы».
//   - Hook — механизм rag в чате: ведущий получает инструмент kb_search, а
//     код вызывает его ДО первого запроса (agent.Preload) с репликой
//     человека. Выдача встаёт после реплики, а не блоком перед историей:
//     блоки идут системными сообщениями перед окном, и блок, меняющийся
//     каждый ход, обнулял бы кэш префикса всего окна.
//
// Оценка ответа — два голоса: правило кодом (Expect вопроса: группы must,
// must_not, числа с допуском) и судья-модель, которая не знает режима
// (ответы перемешаны). Согласие голосов — отдельная метрика, расхождения —
// списком для ручного просмотра.
package rag

import (
	"errors"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
)

// ErrNotImplemented — заглушка контракта.
var ErrNotImplemented = errors.New("не реализовано")

// Mode — режим ответа.
type Mode string

const (
	NoRAG Mode = "norag"
	RAG   Mode = "rag"
	// Режимы v23 — RAG с конвейером retrieve (второй этап поиска). Модель и
	// промпт те же, что у RAG: отличается только, какие фрагменты дошли.
	RAGFilter  Mode = "rag+filter"  // фильтр релевантности и отсев дублей
	RAGRewrite Mode = "rag+rewrite" // переписывание запроса кодом (синонимы, контекст)
	RAGBoth    Mode = "rag+both"    // переписывание кодом + фильтр
)

// Pipelined — режим идёт через конвейер retrieve (а не прямой поиск).
func (m Mode) Pipelined() bool { return m == RAGFilter || m == RAGRewrite || m == RAGBoth }

// UsesBase — режим с базой знаний.
func (m Mode) UsesBase() bool { return m != NoRAG }

// Умолчания поиска для ответа.
const (
	DefaultIndex = "structure"
	DefaultK     = 5
)

// ToolName — инструмент поиска по базе знаний.
const ToolName = "kb_search"

// Question — вопрос к отвечающему агенту. Context — предыдущие реплики
// пользователя (вопросы-продолжения): уходят модели в том же сообщении
// перед вопросом (UserText), а в поиск — склейкой с вопросом (как в
// kb.Compare).
type Question struct {
	Text    string
	Context []string
}

// Answer — ответ одного режима.
type Answer struct {
	Mode   Mode          `json:"mode"`
	Text   string        `json:"text"`
	Hits   []kb.Hit      `json:"hits,omitempty"`
	Search kb.SearchInfo `json:"search,omitempty"`
	Usage  llm.Usage     `json:"usage"`
	Cost   llm.Cost      `json:"cost"`
	Millis int64         `json:"ms"`
	// Trace — путь поиска через конвейер retrieve (режимы v23); nil у
	// прямого поиска и norag.
	Trace *retrieve.Trace `json:"trace,omitempty"`
	// Prompt — что ушло модели (для вкладки и отчёта): system и user.
	System string `json:"system"`
	User   string `json:"user"`
}

// Answerer — отвечающий агент без инструментов, один запрос к модели.
type Answerer struct {
	LLM      llm.Chatter
	Model    string
	Searcher *kb.Searcher
	Index    string // пусто — DefaultIndex
	K        int    // 0 — DefaultK
	// Now — часы для прайса (пик/не пик); nil — time.Now.
	Now func() time.Time
	// Pipeline — второй этап поиска (v23) для режимов Pipelined; nil —
	// эти режимы — ошибка. Configs — настройки режимов поверх
	// умолчаний ModeConfig.
	Pipeline *retrieve.Pipeline
	Configs  map[Mode]retrieve.Config
}

// ModeConfig — настройки конвейера по умолчанию для режима v23: filter —
// только фильтр, rewrite — только переписывание кодом, both — переписывание
// кодом и фильтр. У остальных режимов — пустые. Порог (MinScore) здесь не
// задаётся: конвейер берёт его из индекса (kb calibrate -write), иначе —
// retrieve.DefaultMinScore (retrieve.MinScoreOf).
func ModeConfig(m Mode) retrieve.Config {
	switch m {
	case RAGFilter:
		return retrieve.Config{Filter: true}
	case RAGRewrite:
		return retrieve.Config{Rewrite: retrieve.RewriteCode}
	case RAGBoth:
		// Без гибридного реранкинга: на dev гибрид в both дал только −D16
		// (ранг 5 → 12) и MRR 0.63 → 0.53 (матрица v23,
		// examples/rag/filter.md). Гибрид — отдельная строка матрицы
		// (retrieve.Presets «hybrid»).
		return retrieve.Config{Rewrite: retrieve.RewriteCode, Filter: true}
	}
	return retrieve.Config{}
}

// Modes — все режимы ответа по порядку (для разбора флагов).
var Modes = []Mode{NoRAG, RAG, RAGFilter, RAGRewrite, RAGBoth}

// Known — режим известен.
func (m Mode) Known() bool {
	for _, x := range Modes {
		if x == m {
			return true
		}
	}
	return false
}

// Verdict — оценка ответа.
type Verdict string

const (
	Correct Verdict = "correct"
	Partial Verdict = "partial"
	Wrong   Verdict = "wrong"
	// Abstain — модель честно сказала, что не знает. Для неотвечаемых
	// вопросов (answerable=false) это правильный исход.
	Abstain Verdict = "abstain"
)

// RuleResult — оценка правилом по Expect вопроса.
type RuleResult struct {
	Verdict Verdict `json:"verdict"`
	// Hit, Miss — группы must, найденные и не найденные (первая форма группы).
	Hit  []string `json:"hit,omitempty"`
	Miss []string `json:"miss,omitempty"`
	// Bad — найденные must_not; Numbers — найденные/не найденные числа.
	Bad     []string `json:"bad,omitempty"`
	Numbers []string `json:"numbers,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// Judge — судья-модель. Видит вопрос, ожидание словами (Expect.Note и
// группы must), фрагменты-доказательства и ОДИН ответ без пометки режима.
type Judge struct {
	LLM   llm.Chatter
	Model string
}

// JudgeResult — вердикт судьи с причиной.
type JudgeResult struct {
	Verdict Verdict   `json:"verdict"`
	Reason  string    `json:"reason"`
	Usage   llm.Usage `json:"usage"`
	Cost    llm.Cost  `json:"cost"`
}

// EvalOptions — прогон контрольных вопросов.
type EvalOptions struct {
	Splits  []string // пусто — test
	Modes   []Mode   // пусто — NoRAG и RAG
	Repeats int      // 0 → 1; повторы — для замера шума модели
	// Judge — nil: только правило.
	Judge *Judge
	// Seed — порядок оценки судьёй (перемешивание режимов).
	Seed int64
	// Progress — по мере готовности ответа: для вкладки и CLI.
	Progress func(Row)
}

// Run — один ответ одного режима на вопрос: оценки двух голосов.
type Run struct {
	Repeat int          `json:"repeat"`
	Answer Answer       `json:"answer"`
	Rule   RuleResult   `json:"rule"`
	Judge  *JudgeResult `json:"judge,omitempty"`
	// Final — итоговый вердикт прогона: судья, если он есть, иначе правило.
	Final Verdict `json:"final"`
	// Recall — нашёлся ли в выдаче фрагмент, покрывающий доказательство
	// (≥ kb.EvidenceCover); у NoRAG — false.
	Recall bool   `json:"recall"`
	Error  string `json:"error,omitempty"`
	// JudgeError — судья не ответил или ответил неразборчиво: Final тогда
	// по правилу, а прогон не входит в согласие голосов (v22, добавление).
	JudgeError string `json:"judge_error,omitempty"`
}

// Row — вопрос в отчёте: прогоны по режимам и вердикт большинства.
type Row struct {
	Question kb.Question      `json:"question"`
	Runs     map[Mode][]Run   `json:"runs"`
	Majority map[Mode]Verdict `json:"majority"`
	// Flips — сколько раз вердикт менялся между повторами (шум).
	Flips map[Mode]int `json:"flips"`
}

// ModeStats — сводка режима.
type ModeStats struct {
	Mode      Mode `json:"mode"`
	Questions int  `json:"questions"`
	Correct   int  `json:"correct"`
	Partial   int  `json:"partial"`
	Wrong     int  `json:"wrong"`
	Abstain   int  `json:"abstain"`
	// ConfidentWrong — уверенные ошибки на отвечаемых: вердикт wrong (не
	// «не знаю» и не частичный ответ).
	ConfidentWrong int `json:"confident_wrong"`
	// RightAbstain — «не знаю» на неотвечаемых.
	RightAbstain int `json:"right_abstain"`
	// AnsweredUnanswerable — ответ по существу на неотвечаемом: у norag это
	// ответ из памяти модели (он может быть и верным — база его просто не
	// проверяет), у rag — нарушение правила «нет во фрагментах — так и
	// скажи». Считается отдельно от ConfidentWrong (v22, добавление).
	AnsweredUnanswerable int `json:"answered_unanswerable"`
	// Discriminative, DiscriminativeCorrect — вопросов с discriminative=true
	// (модель без базы их не знает) и верных среди них: разница режимов
	// здесь — вклад базы, а не общие знания модели (v22, добавление).
	Discriminative        int     `json:"discriminative"`
	DiscriminativeCorrect int     `json:"discriminative_correct"`
	Recall                float64 `json:"recall"`
	// Agreement — доля прогонов, где правило и судья согласны.
	Agreement float64 `json:"agreement"`
	FlipRate  float64 `json:"flip_rate"`
	// Judged — сколько прогонов оценил судья: знаменатель Agreement; 0 —
	// судьи не было, и согласие не определено (v22, добавление).
	Judged int `json:"judged"`
	// Flips — смен вердикта между повторами по всем вопросам режима.
	Flips     int       `json:"flips"`
	Usage     llm.Usage `json:"usage"`
	Cost      llm.Cost  `json:"cost"`
	AvgMillis int64     `json:"avg_ms"`
}

// Report — сравнение режимов.
type Report struct {
	Created   time.Time   `json:"created"`
	Model     string      `json:"model"`
	Index     string      `json:"index"`
	K         int         `json:"k"`
	Embedder  string      `json:"embedder"`
	CorpusSHA string      `json:"corpus_sha"`
	Repeats   int         `json:"repeats"`
	Rows      []Row       `json:"rows"`
	Stats     []ModeStats `json:"stats"`
	// Disagreements — где правило и судья разошлись: id вопроса, режим,
	// повтор, оба вердикта.
	Disagreements []string `json:"disagreements"`
	Conclusion    []string `json:"conclusion"`
	JudgeCost     llm.Cost `json:"judge_cost"`
}

// ProbeRow — проба дискриминативности одного вопроса.
type ProbeRow struct {
	ID       string    `json:"id"`
	Q        string    `json:"q"`
	Verdicts []Verdict `json:"verdicts"`
	// Discriminative — модель без базы отвечает верно меньше чем в 2 из 3.
	Discriminative bool `json:"discriminative"`
	// Answers — ответы модели без базы по повторам (для probe.md); Cost —
	// цена ответов и судьи по вопросу (v22, добавление).
	Answers []string `json:"answers,omitempty"`
	Cost    llm.Cost `json:"cost"`
}
