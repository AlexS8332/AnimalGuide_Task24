package feed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
)

const (
	// Prefix — раздел API «Интересных фактов».
	Prefix = "/api/facts"
	// maxBody — предел тела запроса: у обоих POST тело — одно поле.
	maxBody = 64 << 10
	// readTimeout — предел GET: чтение готового из базы демона.
	readTimeout = 30 * time.Second
	// runTimeout — предел POST: выпуск собирается секунды, сводка — до
	// пары минут; ждём с запасом, но не вечно.
	runTimeout = 3 * time.Minute
)

// jobs — задания run_now, которые принимает раздел.
var jobs = map[string]bool{"issue": true, "summary": true, "mdd": true}

// Extension — REST раздела «Интересные факты» (контракт — feed.go). Ответы
// инструментов демона отдаются как есть; приложение ничего не хранит и не
// пересчитывает.
func (h *Hook) Extension() []server.Extension {
	return []server.Extension{{Prefix: Prefix + "/", Handler: http.HandlerFunc(h.handle)}}
}

// route — маршрут: метод, инструмент и разбор аргументов.
type route struct {
	method string
	tool   string
	args   func(r *http.Request) (any, error)
}

var routes = map[string]route{
	"latest":        {http.MethodGet, "facts_latest", latestArgs},
	"issue":         {http.MethodGet, "facts_get", issueArgs},
	"search":        {http.MethodGet, "facts_search", searchArgs},
	"summary":       {http.MethodGet, "summary_get", summaryArgs},
	"summaries":     {http.MethodGet, "summary_get", func(*http.Request) (any, error) { return map[string]any{"list": true}, nil }},
	"run":           {http.MethodPost, toolRunNow, runArgs},
	"summary/build": {http.MethodPost, toolSummaryBuild, buildArgs},
}

// badRequest — ошибка разбора запроса (400).
type badRequest string

func (e badRequest) Error() string { return string(e) }

func (h *Hook) handle(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, Prefix), "/")
	if path == "status" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
		defer cancel()
		// Состояние — 200 всегда: «демон не подключён» — это и есть ответ.
		server.WriteJSON(w, http.StatusOK, h.remote().Status(ctx))
		return
	}
	rt, ok := routes[path]
	if !ok {
		server.WriteError(w, http.StatusNotFound, "нет такого раздела: "+Prefix+"/"+path)
		return
	}
	if r.Method != rt.method {
		methodNotAllowed(w, rt.method)
		return
	}
	args, err := rt.args(r)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			server.WriteError(w, http.StatusBadRequest, "тело запроса больше 64 КБ")
			return
		}
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	timeout := readTimeout
	if rt.method == http.MethodPost {
		timeout = runTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	rm := h.remote()
	raw, err := rm.Call(ctx, rt.tool, args)
	if err != nil {
		var te *ToolError
		if errors.As(err, &te) {
			server.WriteError(w, http.StatusUnprocessableEntity, te.Text)
			return
		}
		// Демон не ответил: окно покажет причину и подсказку, а следующий
		// опрос состояния спросит демон заново, не дожидаясь кэша.
		rm.Forget()
		hint := rm.Hint(err)
		msg := err.Error()
		if hint != "" {
			msg += " — " + hint
		}
		server.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msg, "hint": hint})
		return
	}
	if rt.method == http.MethodPost {
		// Запуск меняет расписание и расход: состояние в окне должно
		// обновиться сразу.
		rm.Forget()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
}

func (h *Hook) remote() *Remote {
	if h.Remote == nil {
		return NewRemote("", "", nil)
	}
	return h.Remote
}

func methodNotAllowed(w http.ResponseWriter, method string) {
	w.Header().Set("Allow", method)
	server.WriteError(w, http.StatusMethodNotAllowed, "нужен "+method)
}

// ------------------------------------------------------------ аргументы

// intParam — целое из строки запроса; пусто — не задано.
func intParam(r *http.Request, name string, min int64) (int64, bool, error) {
	s := strings.TrimSpace(r.URL.Query().Get(name))
	if s == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < min {
		return 0, false, badRequest(name + " — целое не меньше " + strconv.FormatInt(min, 10) + ", а не «" + s + "»")
	}
	return n, true, nil
}

func latestArgs(r *http.Request) (any, error) {
	args := map[string]any{}
	n, ok, err := intParam(r, "limit", 1)
	if err != nil {
		return nil, err
	}
	if ok {
		args["limit"] = n
	}
	return args, nil
}

func issueArgs(r *http.Request) (any, error) {
	id, hasID, err := intParam(r, "id", 1)
	if err != nil {
		return nil, err
	}
	species := strings.TrimSpace(r.URL.Query().Get("species"))
	switch {
	case hasID && species != "":
		return nil, badRequest("нужен один из id и species, а не оба")
	case hasID:
		return map[string]any{"id": id}, nil
	case species != "":
		return map[string]any{"species": species}, nil
	}
	return nil, badRequest("нужен id выпуска или species — вид")
}

func searchArgs(r *http.Request) (any, error) {
	args := map[string]any{}
	q := r.URL.Query()
	for _, name := range []string{"text", "since", "until"} {
		if v := strings.TrimSpace(q.Get(name)); v != "" {
			args[name] = v
		}
	}
	n, ok, err := intParam(r, "offset", 0)
	if err != nil {
		return nil, err
	}
	if ok {
		args["offset"] = n
	}
	return args, nil
}

func summaryArgs(r *http.Request) (any, error) {
	id, ok, err := intParam(r, "id", 1)
	if err != nil {
		return nil, err
	}
	if ok {
		return map[string]any{"id": id}, nil
	}
	return map[string]any{}, nil
}

// readBody — тело POST не больше maxBody; пустое тело — пустой объект.
func readBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return err
		}
		return badRequest("тело запроса не разобралось: " + err.Error())
	}
	return nil
}

func runArgs(r *http.Request) (any, error) {
	var in struct {
		Job string `json:"job"`
	}
	if err := readBody(r, &in); err != nil {
		return nil, err
	}
	if !jobs[in.Job] {
		return nil, badRequest("job — issue, summary или mdd, а не «" + in.Job + "»")
	}
	return map[string]any{"job": in.Job}, nil
}

func buildArgs(r *http.Request) (any, error) {
	var in struct {
		Hours *int `json:"hours"`
	}
	if err := readBody(r, &in); err != nil {
		return nil, err
	}
	if in.Hours == nil {
		return map[string]any{}, nil // по умолчанию демона — сутки
	}
	if *in.Hours < 1 {
		return nil, badRequest("hours — целое не меньше 1")
	}
	return map[string]any{"hours": *in.Hours}, nil
}
