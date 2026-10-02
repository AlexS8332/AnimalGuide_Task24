package kb

import "time"

// Контрольные вопросы базы знаний — eval/questions.json. Разметка не зависит
// от стратегии чанкинга: релевантность задаёт дословный фрагмент-
// доказательство (Evidence.Quote) из документа, а не chunk_id. Чанк
// релевантен вопросу, если покрывает не меньше EvidenceCover фрагмента
// (по смещениям в Doc.Text) хотя бы одного доказательства.

// QuestionsSchema — версия формата файла вопросов.
const QuestionsSchema = 1

// EvidenceCover — какую долю фрагмента должен покрыть чанк.
const EvidenceCover = 0.8

// Наборы.
const (
	SplitTest = "test" // 10 контрольных вопросов заданий; формулировки не меняются
	SplitDev  = "dev"  // подбор порогов и параметров
	SplitOut  = "out"  // вне корпуса: ответа в базе нет (калибровка «не знаю»)
)

// Number — число, которое должно быть в ответе, с допуском.
type Number struct {
	V   float64 `json:"v"`
	Tol float64 `json:"tol,omitempty"`
}

// Expect — что должно быть в ответе (проверяется с v22). Must — группы
// синонимов: в ответе должна быть хотя бы одна форма из каждой группы.
type Expect struct {
	Must    [][]string `json:"must,omitempty"`
	MustNot []string   `json:"must_not,omitempty"`
	Numbers []Number   `json:"numbers,omitempty"`
	// Note — ожидание словами, для людей.
	Note string `json:"note,omitempty"`
}

// SourceRef — какой источник должен использоваться (для людей и отчёта).
type SourceRef struct {
	DocID   string `json:"doc_id"`
	Section string `json:"section,omitempty"`
}

// Evidence — дословный фрагмент документа, на котором держится ответ.
type Evidence struct {
	DocID string `json:"doc_id"`
	Quote string `json:"quote"`
}

// Question — контрольный вопрос.
type Question struct {
	ID    string `json:"id"`    // "T01", "D07", "O03"
	Split string `json:"split"` // test | dev | out
	// Type — fact | number | conflict | section | compare | multihop |
	// synonym | followup | aspect-missing | out-of-base.
	Type        string   `json:"type"`
	Q           string   `json:"q"`
	Paraphrases []string `json:"paraphrases,omitempty"`
	// Context — предыдущие реплики пользователя (по порядку) для вопроса-
	// продолжения (type followup): Q без них не понять — «а сколько он
	// весит?». Поиск и rewrite получают Context вместе с Q.
	Context    []string    `json:"context,omitempty"`
	Answerable bool        `json:"answerable"`
	Expect     *Expect     `json:"expect,omitempty"`
	Sources    []SourceRef `json:"sources,omitempty"`
	Evidence   []Evidence  `json:"evidence,omitempty"`
	// Discriminative — модель без базы отвечает неверно (проба v22); nil —
	// ещё не проверялось.
	Discriminative *bool  `json:"discriminative,omitempty"`
	Note           string `json:"note,omitempty"`
}

// QuestionSet — файл вопросов.
type QuestionSet struct {
	Schema    int        `json:"schema"`
	Version   int        `json:"version"`
	Questions []Question `json:"questions"`
}

// LoadQuestions, Verify и Split — в questions.go.

// CompareOptions — параметры сравнения стратегий.
type CompareOptions struct {
	Indexes []string // id индексов; пусто — все
	Splits  []string // пусто — dev и test
	K       []int    // 0 → {1, 3, 5}
	// Budget — бюджет контекста в токенах для recall при одинаковом объёме
	// (берутся чанки топа, пока влезают); 0 → 1500.
	Budget int
	Mode   Mode // dense по умолчанию; bm25 — справочная строка
}

// IndexStats — структурные метрики индекса (без вопросов).
type IndexStats struct {
	Index    string   `json:"index"`
	Strategy Strategy `json:"strategy"`
	Params   Params   `json:"params"`
	Embedder string   `json:"embedder"`
	Chunks   int      `json:"chunks"`
	P50      int      `json:"p50_tokens"`
	P95      int      `json:"p95_tokens"`
	Tokens   int      `json:"tokens"`
	// MixedShare — доля чанков, захвативших два и более раздела.
	MixedShare float64 `json:"mixed_share"`
	// SplitSections — доля разделов (с собственным текстом), разрезанных на
	// несколько чанков.
	SplitSections float64 `json:"split_sections"`
	// OverlapShare — доля символов, попавших в индекс повторно (перекрытие).
	OverlapShare float64 `json:"overlap_share"`
	// MidSentence — доля чанков, оборванных посреди предложения.
	MidSentence float64 `json:"mid_sentence"`
	// WholeEvidence — доля доказательств (вопросов сравниваемых наборов),
	// которые целиком (100 %) лежат в одном чанке. «Разорвано» (покрыто
	// меньше EvidenceCover) при коротких цитатах почти всегда 0 у обеих
	// стратегий; эта метрика строже и различает их.
	WholeEvidence float64 `json:"whole_evidence"`
	// Evidence — сколько доказательств в знаменателе WholeEvidence.
	Evidence     int     `json:"evidence"`
	BuildSeconds float64 `json:"build_seconds"`
	Bytes        int64   `json:"bytes"`
}

// Retrieval — метрики поиска индекса на наборе вопросов.
type Retrieval struct {
	Index string `json:"index"`
	Mode  Mode   `json:"mode"`
	Split string `json:"split"`
	N     int    `json:"n"`
	// BrokenEvidence — доля доказательств, которые не покрыты ни одним
	// чанком индекса (≥ EvidenceCover): их нельзя найти никаким поиском.
	BrokenEvidence float64         `json:"broken_evidence"`
	Recall         map[int]float64 `json:"recall"` // k → доля вопросов с релевантным чанком в топ-k
	MRR            float64         `json:"mrr"`
	// RecallBudget — то же при одинаковом бюджете токенов топа.
	RecallBudget float64 `json:"recall_budget"`
	// RecallAll5 — доля вопросов, у которых в топ-5 нашлись ВСЕ
	// доказательства (каждое покрыто каким-нибудь чанком топа): для
	// многофактных вопросов (сравнение, multihop) одного найденного факта
	// мало. У вопроса с одним доказательством совпадает с recall@5.
	RecallAll5 float64 `json:"recall_all@5"`
	// RecallAllBudget — то же в пределах бюджета токенов.
	RecallAllBudget float64 `json:"recall_all_budget"`
	// Multi — сколько вопросов набора с двумя и более доказательствами.
	Multi int `json:"multi"`
	// Fallback — почему режим не dense, хотя просили dense (по первому
	// вопросу, где был откат); пусто — отката не было.
	Fallback string `json:"fallback,omitempty"`
	// Rows — по вопросу: ранг первого релевантного (0 — не найден).
	Rows []RetrievalRow `json:"rows"`
}

// RetrievalRow — вопрос в отчёте.
type RetrievalRow struct {
	ID   string `json:"id"`
	Q    string `json:"q"`
	Rank int    `json:"rank"`
	// All5 — все доказательства вопроса в топ-5.
	All5  bool     `json:"all5"`
	Top   []string `json:"top"` // chunk_id топ-5
	Score float64  `json:"score"`
}

// Report — сравнение стратегий.
type Report struct {
	Created   time.Time `json:"created"`
	CorpusSHA string    `json:"corpus_sha"`
	Docs      int       `json:"docs"`
	Pages     float64   `json:"pages"`
	Embedder  string    `json:"embedder"`
	// Budget — бюджет токенов для RecallBudget.
	Budget    int          `json:"budget"`
	Stats     []IndexStats `json:"stats"`
	Retrieval []Retrieval  `json:"retrieval"`
	// Conclusion — вывод числами, собранный кодом из метрик.
	Conclusion []string `json:"conclusion"`
}

// Compare, LastReport и Report.Markdown — в compare.go.

// Covers — какую долю фрагмента [qs, qe) покрывает отрезок [cs, ce).
func Covers(cs, ce, qs, qe int) float64 {
	if qe <= qs {
		return 0
	}
	lo, hi := cs, ce
	if qs > lo {
		lo = qs
	}
	if qe < hi {
		hi = qe
	}
	if hi <= lo {
		return 0
	}
	return float64(hi-lo) / float64(qe-qs)
}
