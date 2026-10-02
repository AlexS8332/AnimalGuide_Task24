package retrieve

import (
	"time"
)

// Named — конфигурация с именем для матрицы режимов.
type Named struct {
	Name   string `json:"name"` // "base", "filter", "rewrite", "both", "hybrid", "llm-rerank", "llm-rewrite"
	Config Config `json:"config"`
}

// MatrixRow — метрики поиска одной конфигурации на наборе.
type MatrixRow struct {
	Name  string `json:"name"`
	Split string `json:"split"`
	K1    int    `json:"k1"`
	N     int    `json:"n"`
	// RecallBefore — доказательство в dense-выдаче K0 (как у base: то, из
	// чего выбирает фильтр без переписывания); RecallUnion — среди всех
	// кандидатов (dense ∪ BM25 всех запросов; добавление v23); RecallAfter —
	// среди итоговых K1 (то, что увидит модель).
	RecallBefore float64 `json:"recall_before"`
	RecallUnion  float64 `json:"recall_union"`
	RecallAfter  float64 `json:"recall_after"`
	MRR          float64 `json:"mrr"`
	// Precision — среднее по отвечаемым вопросам доли итоговых фрагментов,
	// покрывающих доказательство (≥ kb.EvidenceCover); пустой итог на
	// отвечаемом вопросе — 0.
	Precision float64 `json:"precision"`
	// CutShare — доля кандидатов K0, отсечённых фильтром; WrongCut — доля
	// релевантных (покрывающих доказательство) среди отсечённых фильтром
	// кандидатов отвечаемых вопросов; LostQ — доля вопросов, где фильтр снял
	// доказательство: оно было среди кандидатов, а в итог не попало
	// (добавление v23; до исправления так считался WrongCut).
	CutShare float64 `json:"cut_share"`
	WrongCut float64 `json:"wrong_cut"`
	LostQ    float64 `json:"lost_q"`
	// OutEmpty — на неотвечаемых (out и answerable=false): доля вопросов,
	// где после фильтра не осталось ничего.
	OutEmpty float64 `json:"out_empty"`
	OutN     int     `json:"out_n"`
	Tokens   float64 `json:"tokens"` // среднее токенов итоговых фрагментов
	Millis   float64 `json:"ms"`
	CostUSD  float64 `json:"cost_usd"`
	// Rows — по вопросу: ранг доказательства до и после, оставлено, пусто ли.
	Rows []MatrixQ `json:"rows"`
}

// MatrixQ — вопрос в строке матрицы.
type MatrixQ struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Rewritten string `json:"rewritten,omitempty"`
	RankBefor int    `json:"rank_before"` // в кандидатах; 0 — нет
	RankAfter int    `json:"rank_after"`  // в итоге; 0 — нет
	Kept      int    `json:"kept"`
	Empty     bool   `json:"empty"`
	// Answerable — у вопроса есть ответ в базе и доказательство найдено в
	// тексте документа (входит в N); иначе вопрос — в OutN, если он
	// неотвечаемый (добавление v23).
	Answerable bool `json:"answerable"`
	// Unanswerable — вопрос без ответа в базе (out и answerable=false).
	Unanswerable bool `json:"unanswerable,omitempty"`
	// Cut — доказательство было среди кандидатов, а в итог не попало,
	// потому что релевантный кандидат отсечён фильтром (не K1).
	Cut bool `json:"cut,omitempty"`
	// InDense — доказательство в dense-выдаче K0; TopDense — лучший косинус
	// кандидатов; Anchored — в реплике назван вид (пол не применялся).
	InDense  bool    `json:"in_dense,omitempty"`
	TopDense float64 `json:"top_dense,omitempty"`
	Anchored bool    `json:"anchored,omitempty"`
}

// Matrix — сравнение конфигураций на наборах и при разных K1 (без модели,
// кроме платных строк).
type Matrix struct {
	Created   time.Time   `json:"created"`
	CorpusSHA string      `json:"corpus_sha"`
	Embedder  string      `json:"embedder"`
	Index     string      `json:"index"`
	MinScore  float64     `json:"min_score"`
	Delta     float64     `json:"delta"`
	Rows      []MatrixRow `json:"rows"`
	// Conclusion — вывод кодом, числами, с вопросами вместо долей там, где
	// выборка мала.
	Conclusion []string `json:"conclusion"`
	// K0 — кандидатов; Fallback — почему поиск шёл не по векторам (пусто —
	// по векторам); Configs — сравниваемые конфигурации (добавления v23).
	K0       int     `json:"k0"`
	Fallback string  `json:"fallback,omitempty"`
	Configs  []Named `json:"configs"`
}

// CalibRow — порог и его последствия на dev+out.
type CalibRow struct {
	MinScore float64 `json:"min_score"`
	// DevRecall — доказательство в итоге на dev (answerable); OutEmpty — доля
	// out, где всё отсечено; LostDev — dev-вопросы, у которых фильтр отсёк
	// доказательство.
	DevRecall float64  `json:"dev_recall"`
	OutEmpty  float64  `json:"out_empty"`
	LostDev   []string `json:"lost_dev,omitempty"`
}

// Calibration — подбор абсолютного порога на dev+out: середина зазора
// между лучшим косинусом вопросов вне базы и косинусом доказательства
// неякорных dev; без зазора — наибольший порог из перебора с шагом 0.005,
// при котором recall на dev падает не больше чем на MaxDrop против «без
// фильтра» (см. Calibrate). Test не используется.
type Calibration struct {
	Index    string     `json:"index"`
	Embedder string     `json:"embedder"`
	Delta    float64    `json:"delta"`
	MaxDrop  float64    `json:"max_drop"`
	Base     float64    `json:"base_recall"` // dev без фильтра
	Chosen   float64    `json:"chosen"`
	Table    []CalibRow `json:"table"`
	// Hist — косинусы лучшего кандидата: у отвечаемых dev и у out (для
	// гистограммы в отчёте и окне).
	DevTop []float64 `json:"dev_top"`
	OutTop []float64 `json:"out_top"`
	// Created, DevN, OutN, Written, Note — когда, сколько вопросов, записан
	// ли порог в индекс и оговорки (добавления v23).
	Created time.Time `json:"created"`
	DevN    int       `json:"dev_n"`
	OutN    int       `json:"out_n"`
	Written bool      `json:"written"`
	// Anchored — вопросы, в реплике которых назван вид корпуса: пол к ним
	// не применяется (Trace.Anchored).
	Anchored []string `json:"anchored,omitempty"`
	Note     string   `json:"note,omitempty"`

	// Правило выбора (добавления v23, исправление): Rule — "gap" (середина
	// зазора) или "max-drop" (прежнее правило, зазора нет). OutMax — лучший
	// косинус вопросов вне базы (out-of-base без якоря), EvidenceMin —
	// наименьший косинус доказательства у неякорных dev, чьё доказательство
	// доходит до итога без пола (−1 — таких вопросов нет); Gap = EvidenceMin − OutMax; MarginOut и MarginDev —
	// запас порога до них. FloorDev, FloorOut, FloorTest — вопросы,
	// чувствительные к полу (вид в реплике не назван). At — строка таблицы
	// с выбранным порогом (середина зазора может не совпасть с шагом).
	Rule          string   `json:"rule,omitempty"`
	OutMax        float64  `json:"out_max"`
	OutMaxID      string   `json:"out_max_id,omitempty"`
	EvidenceMin   float64  `json:"evidence_min"`
	EvidenceMinID string   `json:"evidence_min_id,omitempty"`
	Gap           float64  `json:"gap"`
	MarginOut     float64  `json:"margin_out"`
	MarginDev     float64  `json:"margin_dev"`
	FloorDev      []string `json:"floor_dev,omitempty"`
	FloorOut      []string `json:"floor_out,omitempty"`
	FloorTest     []string `json:"floor_test,omitempty"`
	// FloorMissed — неякорные dev, чьё доказательство не доходит до итога и
	// без пола: в зазор они не входят (пол их не теряет).
	FloorMissed []string `json:"floor_missed,omitempty"`
	At          CalibRow `json:"at"`
}
