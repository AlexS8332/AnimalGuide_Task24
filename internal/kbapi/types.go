// Package kbapi — REST окна «База знаний»: корпус, индексы, чанки документа,
// поиск и последний отчёт сравнения стратегий.
//
//	GET /api/kb/info                      InfoView
//	GET /api/kb/docs                      []kb.DocInfo
//	GET /api/kb/docs/{id}?index=structure DocView: текст документа и границы чанков
//	GET /api/kb/search?q=&index=&k=&mode= SearchView (index пусто — оба индекса рядом)
//	GET /api/kb/report                    kb.Report (404 — отчёта ещё нет)
//
// v22 — ответ по базе и сравнение с ответом без базы:
//
//	POST /api/kb/ask        AskRequest → AskView: один вопрос в режимах norag и rag рядом
//	GET  /api/kb/questions  []kb.Question набора (eval/questions.json)
//	POST /api/kb/evals      EvalRequest → EvalView (202): прогон контрольных вопросов, асинхронно
//	GET  /api/kb/evals/{id} EvalView: строки по мере готовности, итог rag.Report
//	DELETE /api/kb/evals/{id} EvalView: остановить идущий прогон (state cancelled; 409 — уже не идёт)
//	GET  /api/kb/evals      []EvalView без строк — последние прогоны
//
// ask и evals платные (модель): 503, если у приложения нет модели (Answerer
// == nil); 409 — прогон уже идёт (в теле id идущего), одновременно один.
//
// v23 — второй этап поиска (internal/retrieve):
//
//	GET /api/kb/search?…&rewrite=&rerank=&filter=&k0=&context= — поиск через
//	    конвейер: один индекс (пусто — structure), в SearchResult.Trace —
//	    кандидаты K0 с баллами всех стадий и причинами отсечения
//	GET /api/kb/matrix       retrieve.Matrix — последняя матрица режимов (файл; 404 — нет)
//	GET /api/kb/calibration  retrieve.Calibration — последняя калибровка порога (файл; 404 — нет)
//
// Режимы ask и evals — norag, rag, rag+filter, rag+rewrite, rag+both; ask —
// до трёх режимов рядом. rewrite=llm и rerank=llm платные: 503 без модели.
//
// v24 — ответ с источниками и цитатами:
//
//	режим rag+cite в ask и evals: в rag.Answer — cited (ответ kb_answer и
//	    проверка кодом) и support (судья смысла, если был)
//	GET /api/kb/chunk/{chunk_id} ChunkView: текст чанка, путь раздела и
//	    документ — открыть цитату без загрузки всего документа (404 — нет чанка)
//
// Коды: 400 — неверный запрос; 404 — нет документа, индекса или отчёта;
// 405 — не тот метод; 503 — базы нет (в теле why и hint: как её собрать).
// Всё, что пришло из корпуса, интерфейс выводит как данные (esc), а не
// разметку.
package kbapi

import (
	"context"
	"net/http"
	"sync"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
)

// Prefix — раздел API.
const Prefix = "/api/kb/"

// HintNoBase — что делать, если базы нет.
const HintNoBase = "соберите базу: go run ./cmd/kb index -strategy all (корпус — каталог corpus/, " +
	"эмбеддер — uv run embedder/server.py; без него индекс соберётся только для BM25)"

// API — раздел. Searcher == nil — базы нет (kb.db не найдена или не
// открылась; причина — Why).
type API struct {
	Searcher *kb.Searcher
	// Embedder — для статуса в InfoView; может быть nil.
	Embedder *embed.HTTP
	Path     string // путь к kb.db, для подсказок
	Why      string
	// Answerer — отвечающий агент (v22); nil — ask и evals выключены.
	Answerer *rag.Answerer
	// Judge — судья прогона; nil — оценка только правилом.
	Judge *rag.Judge
	// Questions — путь к eval/questions.json.
	Questions string

	// Ask, Rule, Eval — чем отвечать и оценивать; nil — rag по умолчанию
	// (Answerer.Answer, rag.Rule, rag.Eval). Подменяются в тестах и на
	// стенде проверок интерфейса, где модели нет. Ask != nil включает ask и
	// evals и без Answerer.
	Ask  AskFunc
	Rule RuleFunc
	Eval EvalFunc

	// Pipeline — второй этап поиска (v23) для search с параметрами
	// конвейера; nil — такой поиск выключен (503), если не задан Retrieve.
	// Платные режимы (rewrite=llm, rerank=llm) — только при Pipeline.LLM.
	Pipeline *retrieve.Pipeline
	// Retrieve — чем искать через конвейер; nil — Pipeline.Search.
	// Подменяется в тестах и на стенде проверок интерфейса.
	Retrieve RetrieveFunc
	// MatrixPath, CalibrationPath — файлы последней матрицы режимов и
	// калибровки (kb matrix, kb calibrate); пусто — DefaultMatrixPath и
	// DefaultCalibrationPath.
	MatrixPath      string
	CalibrationPath string

	// mu охраняет прогоны evals: Progress приходит из горутины прогона,
	// GET — из обработчиков.
	mu    sync.Mutex
	seq   int
	evals []*evalRun // старые первыми
}

// AskFunc — ответ одного режима (как Answerer.Answer).
type AskFunc func(ctx context.Context, q rag.Question, mode rag.Mode) (rag.Answer, error)

// RuleFunc — оценка ответа правилом (как rag.Rule).
type RuleFunc func(q kb.Question, answer string) rag.RuleResult

// EvalFunc — прогон контрольных вопросов (как rag.Eval над Answerer).
type EvalFunc func(ctx context.Context, qs kb.QuestionSet, o rag.EvalOptions) (rag.Report, error)

// RetrieveFunc — поиск через конвейер (как Pipeline.Search).
type RetrieveFunc func(ctx context.Context, q retrieve.Query, c retrieve.Config) (retrieve.Trace, error)

// InfoView — GET info.
type InfoView struct {
	OK       bool            `json:"ok"`
	Path     string          `json:"path"`
	Why      string          `json:"why,omitempty"`
	Hint     string          `json:"hint,omitempty"`
	Manifest corpus.Manifest `json:"manifest"`
	Docs     int             `json:"docs"`
	Pages    float64         `json:"pages"`
	Indexes  []kb.IndexInfo  `json:"indexes"`
	Embedder embed.Status    `json:"embedder"`
	Report   bool            `json:"report"` // есть сохранённый отчёт
}

// DocView — GET docs/{id}: документ, его канонический текст и чанки
// индекса (без текста — границы по Start/End в Text).
type DocView struct {
	Doc    kb.DocInfo `json:"doc"`
	Text   string     `json:"text"`
	Index  string     `json:"index"`
	Chunks []kb.Chunk `json:"chunks"`
}

// ChunkView — GET chunk/{id}: чанк с текстом и его документ.
type ChunkView struct {
	Chunk kb.Chunk   `json:"chunk"`
	Doc   kb.DocInfo `json:"doc"`
}

// SearchView — GET search: по результату на каждый запрошенный индекс.
type SearchView struct {
	Query   string         `json:"query"`
	Results []SearchResult `json:"results"`
}

// SearchResult — выдача одного индекса.
type SearchResult struct {
	Info  kb.SearchInfo `json:"info"`
	Hits  []kb.Hit      `json:"hits"`
	Error string        `json:"error,omitempty"`
	// Trace — путь поиска через конвейер (v23): кандидаты до фильтра и
	// после; nil — прямой поиск.
	Trace *retrieve.Trace `json:"trace,omitempty"`
}

// Extension — раздел для server.New.
func (a *API) Extension() []server.Extension {
	return []server.Extension{{Prefix: Prefix, Handler: http.HandlerFunc(a.handle)}}
}

// AskRequest — POST ask. Modes пусто — norag и rag; не больше maxAskModes.
type AskRequest struct {
	Q       string     `json:"q"`
	Context []string   `json:"context,omitempty"`
	Modes   []rag.Mode `json:"modes,omitempty"`
	// QuestionID — если вопрос из набора: ответы оцениваются правилом. Если
	// Q не совпадает с текстом вопроса набора (с точностью до пробелов), это
	// другой вопрос — оценки нет, причина — в AskView.Note.
	QuestionID string `json:"question_id,omitempty"`
}

// AskView — ответы режимов рядом (в порядке Modes) и оценки, если вопрос из
// набора.
type AskView struct {
	Q       string       `json:"q"`
	Answers []rag.Answer `json:"answers"`
	Rows    []rag.Run    `json:"runs,omitempty"`
	// Note — почему ответы не оценены, хотя question_id передан.
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
}

// EvalRequest — POST evals.
type EvalRequest struct {
	Splits  []string   `json:"splits,omitempty"` // пусто — test
	Modes   []rag.Mode `json:"modes,omitempty"`
	Repeats int        `json:"repeats,omitempty"` // 0 → 1, предел 3
	Judge   bool       `json:"judge"`
}

// EvalView — прогон: состояние, строки по мере готовности, итог.
type EvalView struct {
	ID      string      `json:"id"`
	State   string      `json:"state"` // running | done | failed | cancelled
	Started string      `json:"started"`
	Request EvalRequest `json:"request"`
	Total   int         `json:"total"` // ответов всего (вопросы × режимы × повторы)
	Done    int         `json:"done"`
	Rows    []rag.Row   `json:"rows,omitempty"`
	Report  *rag.Report `json:"report,omitempty"`
	Error   string      `json:"error,omitempty"`
}
