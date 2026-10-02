// Package retrieve — второй этап поиска по базе знаний: переписывание
// запроса, кандидаты, реранкинг и фильтр релевантности.
//
//	вопрос ──rewrite──▶ запрос(ы) ──kb.Searcher──▶ K0 кандидатов (dense и BM25)
//	       ──rerank──▶ порядок ──filter──▶ K1 фрагментов (или ни одного)
//
// Переписанный запрос идёт ТОЛЬКО в поиск: модель отвечает на исходный
// вопрос. Каждый шаг пишет в Trace, почему кандидат остался или отсечён, —
// это видно в окне «База знаний» (вкладка «Поиск») и в журнале хода.
//
// Пороги — по косинусу dense (у e5 он сжат в 0,75–0,9, поэтому порог двойной:
// абсолютный пол MinScore и относительный «не хуже лучшего на Delta»).
// У BM25 своя шкала, и абсолютного порога по нему нет: при откате поиска на
// BM25 фильтр оставляет только относительный порог и отсев дублей и говорит
// об этом в Trace. Пороги подбираются на dev+out (Calibrate) и хранятся в
// индексе (kb.IndexInfo.MinScore), а на test проверяются, не подгоняются.
package retrieve

import (
	"errors"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

// ErrNotImplemented — заглушка контракта (оставлена для совместимости:
// реализация конвейера полная).
var ErrNotImplemented = errors.New("не реализовано")

// Rewrite — как переписывать запрос.
type Rewrite string

const (
	RewriteNone Rewrite = ""
	// RewriteCode — кодом, бесплатно: разговорные названия и синонимы из
	// корпуса (Species.Aliases, «или» вступления, латынь) дополняются
	// каноническим названием («кошачий медведь» → «… малая панда», латынь —
	// только в BM25); вопрос-продолжение без названия вида («а сколько она
	// весит?») получает прошлую реплику, где вид назван. Без переписывания
	// контекст в запрос не идёт.
	RewriteCode Rewrite = "code"
	// RewriteLLM — один запрос к модели: JSON {query, queries[1..3]} —
	// самодостаточный запрос и до трёх подзапросов для составного вопроса;
	// поверх — то же, что RewriteCode. Платно (+1 запрос).
	RewriteLLM Rewrite = "llm"
)

// Rerank — как переупорядочивать кандидатов.
type Rerank string

const (
	RerankNone Rerank = ""
	// RerankHybrid — бесплатно: RRF рангов dense и BM25 (k = 60) по всем
	// запросам; находит то, что пропускает один из поисков.
	RerankHybrid Rerank = "hybrid"
	// RerankLLM — один запрос к модели: оценка 0–3 каждому кандидату
	// (только начало текста); платно, только для сравнения в kb eval/qa.
	RerankLLM Rerank = "llm"
)

// Config — настройки конвейера.
type Config struct {
	Index   string  // пусто — rag.DefaultIndex ("structure")
	K0      int     // кандидатов; 0 → 20
	K1      int     // итог; 0 → 5
	Rewrite Rewrite `json:"rewrite,omitempty"`
	Rerank  Rerank  `json:"rerank,omitempty"`
	Filter  bool    `json:"filter,omitempty"`
	// MinScore — абсолютный пол косинуса dense; 0 — из индекса
	// (kb.IndexInfo.MinScore), а если и там 0 — DefaultMinScore.
	MinScore float64 `json:"min_score,omitempty"`
	// Delta — относительный порог: косинус ≥ лучший − Delta; 0 → DefaultDelta.
	Delta float64 `json:"delta,omitempty"`
	// Dedupe — отсев повторов: включается вместе с Filter. С v23 — повтор
	// ТЕКСТА (тот же text_sha или перекрытие ≥ половины фрагмента), а не
	// «один фрагмент на раздел»: почему — retrieve.duplicate.
}

// Умолчания до калибровки (по живому индексу e5-base v21: косинусы
// релевантных 0,82–0,89, нерелевантных 0,76–0,83).
const (
	DefaultK0       = 20
	DefaultK1       = 5
	DefaultMinScore = 0.80
	DefaultDelta    = 0.05
	RRFK            = 60
)

// Candidate — кандидат с баллами всех стадий и судьбой.
type Candidate struct {
	kb.Hit
	Dense     float64 `json:"dense"`      // косинус (0 — не было в dense-выдаче)
	BM25      float64 `json:"bm25"`       // нормированный BM25 (0 — не было)
	RankDense int     `json:"rank_dense"` // 0 — не было
	RankBM25  int     `json:"rank_bm25"`
	Rerank    float64 `json:"rerank"` // RRF или оценка модели; 0 — без реранкинга
	Final     int     `json:"final"`  // ранг после реранкинга, с 1
	Kept      bool    `json:"kept"`
	// Reason — почему отсечён: "порог 0.80", "хуже лучшего на 0.05",
	// "повтор текста", "за пределами K1"; пусто — остался.
	Reason string `json:"reason,omitempty"`

	// lead — косинус против лучшего косинуса своего запроса: max по
	// запросам (cos − лучший cos этого запроса); 0 — сам лучший. Для
	// относительного порога (у подзапросов RewriteLLM — свой лучший).
	// hasLead — посчитан (иначе — Dense − TopDense).
	lead    float64
	hasLead bool
}

// Trace — весь путь поиска.
type Trace struct {
	Original  string   `json:"original"`
	Rewritten string   `json:"rewritten"` // = Original, если не переписан
	Queries   []string `json:"queries"`   // что ушло в dense-поиск
	// QueriesBM25 — что ушло в BM25, по запросу на каждый из Queries: тот же
	// запрос плюс латынь названных видов (латынь — только в BM25, dense она
	// сбивает); пусто — те же Queries (добавление v23).
	QueriesBM25 []string `json:"queries_bm25,omitempty"`
	RewriteBy   string   `json:"rewrite_by,omitempty"`
	// Expanded — какие синонимы раскрыты кодом: "кошачий медведь → малая панда".
	Expanded []string `json:"expanded,omitempty"`
	// Anchored — виды корпуса, названные в САМОЙ реплике (каноном,
	// синонимом или латынью; добавление v23), а не унаследованные из
	// контекста. Названный вид — якорь: статья о нём в базе есть, и
	// абсолютный пол косинуса к такому запросу не применяется (см.
	// Pipeline.Search). У вопроса-продолжения вид из контекста пол не
	// снимает.
	Anchored []string `json:"anchored,omitempty"`
	Config   Config   `json:"config"`
	MinScore float64  `json:"min_score"` // фактический порог
	// MinScoreFrom — откуда порог: "настройки", "индекс" или "умолчание"
	// (MinScoreOf; добавление v23).
	MinScoreFrom string        `json:"min_score_from,omitempty"`
	Candidates   []Candidate   `json:"candidates"`
	Hits         []kb.Hit      `json:"hits"` // итог (Kept) в порядке Final
	Info         kb.SearchInfo `json:"info"`
	// Empty — фильтр отсёк всё: в базе ответа, вероятно, нет. TopDense —
	// лучший косинус кандидатов; Gap — отрыв лучшего от второго (top1 −
	// top2 по косинусу dense среди кандидатов; добавление v23).
	//
	// Для v24: по Anchored, TopDense, Gap и Empty отвечающий решает, говорить
	// ли «не знаю» — пусто без якоря значит «в базе об этом нет», а якорь с
	// низким TopDense и малым Gap — «вид есть, нужного аспекта, вероятно,
	// нет».
	Empty    bool      `json:"empty"`
	TopDense float64   `json:"top_dense"`
	Gap      float64   `json:"gap"`
	Usage    llm.Usage `json:"usage"`
	Cost     llm.Cost  `json:"cost"`
	Millis   int64     `json:"ms"`
	Note     string    `json:"note,omitempty"` // например, «BM25: абсолютного порога нет»

	// scores — оценки модели-реранкера по chunk_id; nil — их нет (другой
	// реранкинг или модель ответила неразборчиво).
	scores map[string]float64
}

// Query — что ищем. Context — предыдущие реплики человека (для
// вопросов-продолжений).
type Query struct {
	Text    string
	Context []string
}

// Pipeline — конвейер над базой знаний.
type Pipeline struct {
	Searcher *kb.Searcher
	// LLM, Model — для RewriteLLM и RerankLLM; nil — эти режимы — ошибка.
	LLM   llm.Chatter
	Model string
	// Aliases — словарь названий; nil — строится из базы при первом вызове.
	Aliases *Aliases
	// Now — часы для прайса; nil — time.Now.
	Now func() time.Time

	mu sync.Mutex // охраняет ленивую загрузку Aliases
}

// Aliases — названия видов корпуса: канон (русское название статьи) и
// синонимы (Species.Ru, Species.Aliases, латынь). Ключи нормализованы
// (corpus.Normalize), многословные синонимы ищутся по основам слов.
type Aliases struct {
	// Canon — синоним → канон («кошачий медведь» → «малая панда»).
	Canon map[string]string
	// Latin — канон → латынь.
	Latin map[string]string

	// Индекс сопоставления строится из Canon при первом вызове (aliases.go).
	once sync.Once
	ix   *matchIndex
}
