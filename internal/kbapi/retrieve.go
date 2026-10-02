package kbapi

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
)

// v23: поиск через конвейер retrieve и последние матрица режимов и
// калибровка порога (файлы kb matrix и kb calibrate).

const (
	// maxK0 — предел кандидатов конвейера.
	maxK0 = 50
	// DefaultMatrixPath, DefaultCalibrationPath — куда пишут kb matrix и kb
	// calibrate (относительно каталога запуска приложения).
	DefaultMatrixPath      = "examples/rag/filter.json"
	DefaultCalibrationPath = "examples/rag/calibrate.json"
	// hintMatrix, hintCalibration — как собрать недостающий файл.
	hintMatrix      = "go run ./cmd/kb matrix"
	hintCalibration = "go run ./cmd/kb calibrate"
	// hintNoPipeline — конвейера нет: базы нет или приложение его не завело.
	hintNoPipeline = "второй этап поиска не подключён — обновите приложение; обычный поиск работает без параметров rewrite, rerank, filter и k0"
)

// pipelineParams — заданы ли параметры конвейера: хоть один — поиск идёт
// через него.
func pipelineParams(q url.Values) bool {
	for _, k := range []string{"rewrite", "rerank", "filter", "k0"} {
		if q.Has(k) {
			return true
		}
	}
	return false
}

// retrieveFn — чем искать через конвейер: Retrieve, иначе Pipeline.Search;
// nil — конвейера нет.
func (a *API) retrieveFn() RetrieveFunc {
	if a.Retrieve != nil {
		return a.Retrieve
	}
	if a.Pipeline == nil {
		return nil
	}
	return a.Pipeline.Search
}

// paidRetrieve — есть ли модель для платных режимов конвейера.
func (a *API) paidRetrieve() bool { return a.Pipeline != nil && a.Pipeline.LLM != nil }

// pipelineConfig — настройки конвейера из запроса; ошибка — 400.
func pipelineConfig(q url.Values, k int) (retrieve.Config, error) {
	c := retrieve.Config{K1: k, K0: retrieve.DefaultK0}
	switch rw := retrieve.Rewrite(strings.TrimSpace(q.Get("rewrite"))); rw {
	case retrieve.RewriteNone, retrieve.RewriteCode, retrieve.RewriteLLM:
		c.Rewrite = rw
	default:
		return c, errors.New("rewrite — пусто, code или llm: " + string(rw))
	}
	switch rr := retrieve.Rerank(strings.TrimSpace(q.Get("rerank"))); rr {
	case retrieve.RerankNone, retrieve.RerankHybrid, retrieve.RerankLLM:
		c.Rerank = rr
	default:
		return c, errors.New("rerank — пусто, hybrid или llm: " + string(rr))
	}
	switch f := strings.TrimSpace(q.Get("filter")); f {
	case "", "0", "false":
	case "1", "true":
		c.Filter = true
	default:
		return c, errors.New("filter — 0 или 1: " + f)
	}
	if s := strings.TrimSpace(q.Get("k0")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxK0 {
			return c, errors.New("k0 — целое от 1 до " + strconv.Itoa(maxK0) + ": " + s)
		}
		c.K0 = n
	}
	if c.K0 < c.K1 {
		return c, errors.New("k0 (" + strconv.Itoa(c.K0) + ") меньше k (" + strconv.Itoa(c.K1) + "): кандидатов должно быть не меньше итога")
	}
	return c, nil
}

// searchContext — предыдущие реплики (повторяемый параметр context).
func searchContext(q url.Values) ([]string, error) {
	var out []string
	for _, c := range q["context"] {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if utf8.RuneCountInString(c) > maxQ {
			return nil, errors.New("реплика контекста длиннее " + strconv.Itoa(maxQ) + " символов")
		}
		out = append(out, c)
	}
	if len(out) > maxContext {
		return nil, errors.New("реплик контекста больше " + strconv.Itoa(maxContext))
	}
	return out, nil
}

// searchPipeline — поиск через конвейер по одному индексу (пусто или all —
// rag.DefaultIndex). Ошибка конвейера — в Error выдачи (200), как у
// прямого поиска; платный режим без модели и конвейер без поиска — 503.
func (a *API) searchPipeline(w http.ResponseWriter, r *http.Request, query string, k int) {
	q := r.URL.Query()
	c, err := pipelineConfig(q, k)
	if err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctxLines, err := searchContext(q)
	if err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if (c.Rewrite == retrieve.RewriteLLM || c.Rerank == retrieve.RerankLLM) && !a.paidRetrieve() {
		why := "модели нет — rewrite=llm и rerank=llm выключены (бесплатные code и hybrid работают)"
		server.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": why, "why": why, "hint": HintNoModel})
		return
	}
	fn := a.retrieveFn()
	if fn == nil {
		why := "второй этап поиска не подключён"
		server.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": why, "why": why, "hint": hintNoPipeline})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	index := strings.TrimSpace(q.Get("index"))
	if index == "" || index == "all" {
		index = rag.DefaultIndex
	}
	if _, err := a.Searcher.Store.Index(ctx, index); err != nil {
		fail(w, err)
		return
	}
	c.Index = index
	tr, err := fn(ctx, retrieve.Query{Text: query, Context: ctxLines}, c)
	if r.Context().Err() != nil {
		return // клиент ушёл
	}
	res := SearchResult{Info: tr.Info, Hits: tr.Hits}
	if res.Info.Index == "" {
		res.Info.Index = index
	}
	if err != nil {
		res.Error = err.Error()
	} else {
		if tr.Config.Index == "" {
			tr.Config = c
		}
		if tr.Candidates == nil {
			tr.Candidates = []retrieve.Candidate{}
		}
		res.Trace = &tr
	}
	if res.Hits == nil {
		res.Hits = []kb.Hit{}
	}
	server.WriteJSON(w, http.StatusOK, SearchView{Query: query, Results: []SearchResult{res}})
}

// lastFile — GET matrix и calibration: JSON последнего прогона из файла,
// проверенный разбором в v; нет файла — 404 с командой, которая его
// соберёт.
func lastFile(w http.ResponseWriter, path, what, hint string, v any) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		server.WriteJSON(w, http.StatusNotFound, map[string]string{
			"error": what + " ещё нет (" + path + ") — соберите: " + hint, "hint": hint, "path": path})
		return
	}
	if err != nil {
		server.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := json.Unmarshal(b, v); err != nil {
		server.WriteError(w, http.StatusInternalServerError, path+" не разобрался: "+err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, v)
}

func (a *API) matrix(w http.ResponseWriter) {
	path := a.MatrixPath
	if path == "" {
		path = DefaultMatrixPath
	}
	var m retrieve.Matrix
	lastFile(w, path, "матрицы режимов", hintMatrix, &m)
}

func (a *API) calibration(w http.ResponseWriter) {
	path := a.CalibrationPath
	if path == "" {
		path = DefaultCalibrationPath
	}
	var c retrieve.Calibration
	lastFile(w, path, "калибровки порога", hintCalibration, &c)
}
