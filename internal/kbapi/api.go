package kbapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
)

const (
	// healthTimeout — предел опроса эмбеддера для InfoView: окно не должно
	// ждать сайдкар, которого нет.
	healthTimeout = 2 * time.Second
	// searchTimeout — предел поиска по всем запрошенным индексам (dense
	// ходит в эмбеддер за вектором вопроса).
	searchTimeout = 30 * time.Second
	// maxK — предел k поиска.
	maxK = 20
	// hintNoReport — что делать, если отчёта сравнения ещё нет.
	hintNoReport = "отчёта сравнения ещё нет — соберите его: go run ./cmd/kb eval"
)

// unavailableView — 503 GET info: InfoView без базы и ошибка, как у
// остальных маршрутов (error, why, hint).
type unavailableView struct {
	InfoView
	Error string `json:"error"`
}

func (a *API) handle(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(Prefix, "/")), "/")
	// v22: ответ и прогоны — свои методы и своя доступность (модель, а не
	// только база).
	switch {
	case path == "ask":
		if method(w, r, http.MethodPost) {
			a.ask(w, r)
		}
		return
	case path == "questions":
		if method(w, r, http.MethodGet) {
			a.questions(w)
		}
		return
	case path == "evals":
		if r.Method == http.MethodPost {
			a.startEval(w, r)
		} else if method(w, r, http.MethodGet, http.MethodPost) {
			a.listEvals(w)
		}
		return
	case path == "matrix", path == "calibration":
		// v23: файлы последних прогонов kb matrix и kb calibrate — видны и
		// без базы.
		if method(w, r, http.MethodGet) {
			if path == "matrix" {
				a.matrix(w)
			} else {
				a.calibration(w)
			}
		}
		return
	case strings.HasPrefix(path, "evals/"):
		id := strings.TrimPrefix(path, "evals/")
		if r.Method == http.MethodDelete {
			a.cancelEval(w, id)
		} else if method(w, r, http.MethodGet, http.MethodDelete) {
			a.getEval(w, id)
		}
		return
	}
	switch {
	case path == "info", path == "docs", path == "search", path == "report", strings.HasPrefix(path, "docs/"):
	default:
		server.WriteError(w, http.StatusNotFound, "нет такого раздела: "+Prefix+path)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		server.WriteError(w, http.StatusMethodNotAllowed, "нужен GET")
		return
	}
	if path == "info" {
		a.info(w, r)
		return
	}
	if a.Searcher == nil {
		a.unavailable(w)
		return
	}
	switch {
	case path == "docs":
		a.docs(w, r)
	case path == "search":
		a.search(w, r)
	case path == "report":
		a.report(w, r)
	default:
		a.doc(w, r, strings.TrimPrefix(path, "docs/"))
	}
}

// why — почему базы нет: Why или слова по умолчанию.
func (a *API) why() string {
	if a.Why != "" {
		return a.Why
	}
	if a.Path != "" {
		return "базы знаний нет: " + a.Path
	}
	return "базы знаний нет"
}

// unavailable — 503 с причиной и подсказкой, как собрать базу.
func (a *API) unavailable(w http.ResponseWriter) {
	why := a.why()
	server.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": why, "why": why, "hint": HintNoBase})
}

// fail — ошибка базы: нет документа или индекса — 404, иначе 500.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, kb.ErrNoDoc), errors.Is(err, kb.ErrNoIndex), errors.Is(err, kb.ErrNoChunk):
		server.WriteError(w, http.StatusNotFound, err.Error())
	default:
		server.WriteError(w, http.StatusInternalServerError, err.Error())
	}
}

// health — статус эмбеддера коротким таймаутом; эмбеддера нет — так и
// сказано.
func (a *API) health(ctx context.Context) embed.Status {
	if a.Embedder == nil {
		return embed.Status{Why: "эмбеддер не задан", Hint: embed.Hint}
	}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	return a.Embedder.Health(ctx)
}

func (a *API) info(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := InfoView{Path: a.Path, Indexes: []kb.IndexInfo{}}
	if a.Searcher == nil {
		v.Why, v.Hint = a.why(), HintNoBase
		v.Embedder = a.health(ctx)
		server.WriteJSON(w, http.StatusServiceUnavailable, unavailableView{InfoView: v, Error: v.Why})
		return
	}
	st := a.Searcher.Store
	m, err := st.Manifest(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	docs, err := st.Docs(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	idx, err := st.Indexes(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	_, has, err := st.LastReport(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	v.OK = true
	v.Manifest = m
	v.Docs = len(docs)
	v.Pages = m.Pages
	if v.Pages == 0 {
		for _, d := range docs {
			v.Pages += d.Pages
		}
	}
	if idx != nil {
		v.Indexes = idx
	}
	v.Report = has
	if len(idx) == 0 {
		v.Hint = HintNoBase
	}
	v.Embedder = a.health(ctx)
	server.WriteJSON(w, http.StatusOK, v)
}

func (a *API) docs(w http.ResponseWriter, r *http.Request) {
	docs, err := a.Searcher.Store.Docs(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if docs == nil {
		docs = []kb.DocInfo{}
	}
	server.WriteJSON(w, http.StatusOK, docs)
}

// doc — документ, его текст и чанки индекса ?index= (пусто — первый
// индекс базы). Текст чанков не отдаётся: границы — Start/End в Text.
func (a *API) doc(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	st := a.Searcher.Store
	if id == "" || strings.Contains(id, "/") {
		server.WriteError(w, http.StatusNotFound, "нет такого документа: "+id)
		return
	}
	index := strings.TrimSpace(r.URL.Query().Get("index"))
	if index == "" {
		idx, err := st.Indexes(ctx)
		if err != nil {
			fail(w, err)
			return
		}
		if len(idx) == 0 {
			server.WriteError(w, http.StatusNotFound, "в базе нет ни одного индекса — "+HintNoBase)
			return
		}
		index = idx[0].ID
	}
	d, err := st.Doc(ctx, id)
	if err != nil {
		fail(w, err)
		return
	}
	infos, err := st.Docs(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	v := DocView{Text: d.Text(), Index: index, Chunks: []kb.Chunk{}}
	for _, x := range infos {
		if x.ID == id {
			v.Doc = x
			break
		}
	}
	chunks, err := st.Chunks(ctx, index, id)
	if err != nil {
		fail(w, err)
		return
	}
	for _, c := range chunks {
		c.Text = ""
		v.Chunks = append(v.Chunks, c)
	}
	server.WriteJSON(w, http.StatusOK, v)
}

// search — поиск по одному индексу или по всем рядом (index пусто или
// all). Ошибка одного индекса не роняет остальные: она — в Error его
// выдачи.
func (a *API) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	if query == "" {
		server.WriteError(w, http.StatusBadRequest, "нужен запрос: q")
		return
	}
	k := kb.DefaultK
	if s := strings.TrimSpace(q.Get("k")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxK {
			server.WriteError(w, http.StatusBadRequest, "k — целое от 1 до "+strconv.Itoa(maxK)+": "+s)
			return
		}
		k = n
	}
	mode := kb.Mode(strings.TrimSpace(q.Get("mode")))
	if mode != "" && mode != kb.Dense && mode != kb.BM25 {
		server.WriteError(w, http.StatusBadRequest, "режим поиска — dense или bm25: "+string(mode))
		return
	}
	// v23: параметры конвейера — поиск в два этапа по одному индексу.
	if pipelineParams(q) {
		a.searchPipeline(w, r, query, k)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	st := a.Searcher.Store
	var ids []string
	switch index := strings.TrimSpace(q.Get("index")); index {
	case "", "all":
		idx, err := st.Indexes(ctx)
		if err != nil {
			fail(w, err)
			return
		}
		for _, x := range idx {
			ids = append(ids, x.ID)
		}
		if len(ids) == 0 {
			server.WriteError(w, http.StatusNotFound, "в базе нет ни одного индекса — "+HintNoBase)
			return
		}
	default:
		if _, err := st.Index(ctx, index); err != nil {
			fail(w, err)
			return
		}
		ids = []string{index}
	}
	v := SearchView{Query: query, Results: []SearchResult{}}
	for _, id := range ids {
		hits, info, err := a.Searcher.Search(ctx, query, kb.SearchOptions{Index: id, K: k, Mode: mode})
		res := SearchResult{Info: info, Hits: hits}
		if res.Info.Index == "" {
			res.Info.Index = id
		}
		if err != nil {
			if r.Context().Err() != nil {
				return // клиент ушёл
			}
			res.Error = err.Error()
		}
		if res.Hits == nil {
			res.Hits = []kb.Hit{}
		}
		v.Results = append(v.Results, res)
	}
	server.WriteJSON(w, http.StatusOK, v)
}

func (a *API) report(w http.ResponseWriter, r *http.Request) {
	rep, ok, err := a.Searcher.Store.LastReport(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if !ok {
		server.WriteJSON(w, http.StatusNotFound, map[string]string{"error": hintNoReport, "hint": "go run ./cmd/kb eval"})
		return
	}
	server.WriteJSON(w, http.StatusOK, rep)
}
