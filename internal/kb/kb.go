// Package kb — база знаний справочника: чанки документов корпуса, индекс с
// эмбеддингами в SQLite и поиск по нему (вектором или BM25).
//
// Индекс — пересобираемый артефакт: он живёт в отдельном файле kb.db
// (компонент миграций "kb" общего internal/db), а не в trivia.db демона и не
// в git. Источник истины — корпус (internal/corpus).
//
// Поиск — полный перебор косинуса в памяти: тысячи чанков × 768 измерений —
// миллисекунды, FAISS не нужен (и требует cgo). Если эмбеддер недоступен или
// индекс построен другой моделью, поиск откатывается на BM25 (FTS5,
// токенизатор trigram) и честно говорит об этом в SearchInfo.
package kb

import (
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
)

// ErrNotImplemented — заглушка контракта.
var ErrNotImplemented = errors.New("не реализовано")

// ErrNoIndex — индекса с таким id в базе нет.
var ErrNoIndex = errors.New("индекса нет")

// ErrNoDoc и ErrNoChunk — документа или чанка с таким id в базе нет (окно
// отвечает на них 404, а не 500).
var (
	ErrNoDoc   = errors.New("документа нет")
	ErrNoChunk = errors.New("чанка нет")
)

// Strategy — стратегия чанкинга; она же id индекса в базе (один индекс на
// стратегию: пересборка заменяет прежний).
type Strategy string

const (
	// Fixed — окно фиксированного размера с перекрытием по сплошному тексту
	// документа (Doc.Text), без оглядки на разделы; раздел чанка — раздел
	// большинства: тот, которому принадлежит большая часть текста чанка.
	Fixed Strategy = "fixed"
	// Structure — граница по заголовкам: собственное тело раздела — чанк;
	// длиннее Max — режется по абзацам (затем по предложениям), короче Min —
	// склеивается с соседом того же родителя.
	Structure Strategy = "structure"
)

// Params — параметры стратегии, символы (руны).
type Params struct {
	Size    int `json:"size,omitempty"`    // fixed
	Overlap int `json:"overlap,omitempty"` // fixed
	Max     int `json:"max,omitempty"`     // structure
	Min     int `json:"min,omitempty"`     // structure
}

// Стартовые параметры. У fixed размер по умолчанию — медиана длины
// структурных чанков (kb index -strategy all считает её сам), чтобы
// сравнивались границы, а не размер; перекрытие — 15 % размера.
const (
	DefaultMax        = 1200
	DefaultMin        = 200
	DefaultSize       = 800
	DefaultOverlapPct = 15
)

// Chunk — фрагмент документа с метаданными.
type Chunk struct {
	// ID — "<doc_id>/<strategy>/<ord:03>": "manul/structure/004".
	ID       string   `json:"chunk_id"`
	DocID    string   `json:"doc_id"`
	Source   string   `json:"source"`
	Title    string   `json:"title"`
	Section  string   `json:"section"`      // последний элемент Path или corpus.IntroTitle
	Path     []string `json:"section_path"` // ["Образ жизни", "Питание"]
	Strategy Strategy `json:"strategy"`
	Ord      int      `json:"ord"`
	// Start, End — смещения в рунах в corpus.Doc.Text().
	Start int `json:"start"`
	End   int `json:"end"`
	// Text — сам фрагмент (то, что видит модель и цитирует ответ).
	Text string `json:"text"`
	// Mixed — чанк захватил текст двух и более разделов: внутри него есть
	// строка заголовка «## …» другого раздела. У fixed бывает часто, у
	// structure — только при склейке короткого раздела с соседом того же
	// родителя (оба раздела целиком в чанке, текст не дублируется).
	Mixed  bool   `json:"mixed,omitempty"`
	Tokens int    `json:"tokens"`
	SHA    string `json:"text_sha"`
	URL    string `json:"url,omitempty"`
	RevID  int64  `json:"revid,omitempty"`
}

// EmbedText — что уходит эмбеддеру: «Заголовок › Путь раздела» и текст.
// Заголовок в тексте эмбеддинга помогает найти «питание манула», когда в
// самом абзаце слово «манул» не встречается. Реализация — в chunk.go.

// Chunker — стратегия чанкинга.
type Chunker interface {
	Strategy() Strategy
	Params() Params
	Split(d corpus.Doc) []Chunk
}

// NewFixed и NewStructure — стратегии с параметрами (0 → умолчания; у
// перекрытия fixed умолчание — отрицательное значение, 0 — без перекрытия);
// реализация — в chunk.go.

// IndexInfo — индекс в базе.
type IndexInfo struct {
	ID        string    `json:"index_id"` // = string(Strategy)
	Strategy  Strategy  `json:"strategy"`
	Params    Params    `json:"params"`
	Embedder  string    `json:"embedder"` // embed.Embedder.Model()
	Dims      int       `json:"dims"`
	CorpusSHA string    `json:"corpus_sha"`
	Chunks    int       `json:"chunks"`
	Tokens    int       `json:"tokens"`
	BuiltAt   time.Time `json:"built_at"`
	Seconds   float64   `json:"seconds"`
	// MinScore — порог релевантности (калибруется в v23); 0 — не задан.
	MinScore float64 `json:"min_score,omitempty"`
}

// DocInfo — документ в базе (без текста).
type DocInfo struct {
	ID      string  `json:"doc_id"`
	Source  string  `json:"source"`
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	RevID   int64   `json:"revid,omitempty"`
	License string  `json:"license"`
	Chars   int     `json:"chars"`
	Pages   float64 `json:"pages"`
	SHA256  string  `json:"sha256"`
}

// Store — kb.db. Таблицы: kb_meta (corpus_sha, манифест), kb_docs (doc
// JSON целиком), kb_indexes, kb_chunks (метаданные, текст, vec BLOB float32
// LE), kb_embed_cache, kb_fts_<index_id> (FTS5, trigram, своя на индекс —
// шаг миграции 2), kb_reports (последний отчёт сравнения, JSON).
type Store struct {
	db *sql.DB
}

// Progress — ход индексации: сколько чанков закодировано из скольких.
type Progress func(done, total int)

// Методы Store (Open, PutCorpus, Manifest, Docs, Doc, Build, Indexes, Index,
// Chunks, Chunk, GetVec, PutVec) — в store.go.

// Mode — как найдено.
type Mode string

const (
	Dense Mode = "dense"
	BM25  Mode = "bm25"
)

// Hit — найденный чанк. Score — косинус (dense) или нормированный BM25
// (bm25: 1 у лучшего, доли у остальных; у BM25 своя шкала, порог по ней
// отдельный).
type Hit struct {
	Chunk
	Score float64 `json:"score"`
	Rank  int     `json:"rank"` // с 1
}

// SearchInfo — как прошёл поиск. Fallback — почему не dense (эмбеддер
// недоступен, индекс другой модели, индекс без векторов); пусто — dense.
type SearchInfo struct {
	Index    string  `json:"index"`
	Mode     Mode    `json:"mode"`
	Fallback string  `json:"fallback,omitempty"`
	Embedder string  `json:"embedder,omitempty"`
	Millis   float64 `json:"ms"`
}

// SearchOptions — параметры поиска. Mode пусто — dense с откатом на BM25.
type SearchOptions struct {
	Index string
	K     int // 0 → 5
	Mode  Mode
}

// Searcher — поиск по индексам базы. Векторы индекса грузятся в память при
// первом поиске по нему и перечитываются, если индекс пересобран (BuiltAt).
type Searcher struct {
	Store *Store
	// Embedder — для векторов вопроса; nil — только BM25.
	Embedder embed.Embedder

	// mu охраняет dense: Searcher делят обработчики HTTP из разных горутин.
	mu    sync.Mutex
	dense map[string]*denseIndex
}
