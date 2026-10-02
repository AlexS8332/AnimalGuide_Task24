package kbapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
)

const (
	// askTimeout — предел одного ask: запросы режимов к модели идут
	// параллельно.
	askTimeout = 3 * time.Minute
	// maxAskModes — предел режимов одного ask: колонки ответов рядом.
	maxAskModes = 3
	// evalTimeout — предел прогона контрольных вопросов.
	evalTimeout = 60 * time.Minute
	// maxQ — предел длины вопроса (и реплики контекста) в символах.
	maxQ = 500
	// maxContext — предел реплик контекста вопроса-продолжения.
	maxContext = 10
	// maxRepeats — предел повторов прогона: каждый повтор — полный проход
	// по набору в обоих режимах.
	maxRepeats = 3
	// keepEvals — сколько последних прогонов помнит раздел.
	keepEvals = 10
	// maxBody — предел тела POST.
	maxBody = 64 << 10

	// HintNoModel — что делать, если модели нет.
	HintNoModel = "нужен DEEPSEEK_API_KEY: впишите ключ в .env.local (см. .env.example) и перезапустите приложение"
	// hintNoQuestions — что делать, если нет файла контрольных вопросов.
	hintNoQuestions = "положите набор в eval/questions.json (или укажите путь в KB_QUESTIONS) и перезапустите приложение"
)

// Состояния прогона.
const (
	StateRunning   = "running"
	StateDone      = "done"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// evalRun — прогон evals. Поля меняются под API.mu.
type evalRun struct {
	view EvalView
	rows map[string]int // id вопроса → индекс в view.Rows
	// cancel — отмена контекста горутины прогона (DELETE evals/{id}).
	cancel context.CancelFunc
}

// method — метод из allowed; иначе 405 с Allow и false.
func method(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	server.WriteError(w, http.StatusMethodNotAllowed, "нужен "+strings.Join(allowed, " или "))
	return false
}

// readBody — JSON тела не больше maxBody; пустое тело — нули.
func readBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errors.New("тело запроса больше 64 КБ")
		}
		return errors.New("тело запроса не разобралось: " + err.Error())
	}
	return nil
}

// ------------------------------------------------------------ функции rag

// askFn — чем отвечать: Ask, иначе Answerer; nil — модели нет.
func (a *API) askFn() AskFunc {
	if a.Ask != nil {
		return a.Ask
	}
	if a.Answerer == nil {
		return nil
	}
	return a.Answerer.Answer
}

func (a *API) ruleFn() RuleFunc {
	if a.Rule != nil {
		return a.Rule
	}
	return rag.Rule
}

// evalFn — чем прогонять: Eval, иначе rag.Eval над Answerer; nil — модели
// нет.
func (a *API) evalFn() EvalFunc {
	if a.Eval != nil {
		return a.Eval
	}
	if a.Answerer == nil {
		return nil
	}
	ans := a.Answerer
	return func(ctx context.Context, qs kb.QuestionSet, o rag.EvalOptions) (rag.Report, error) {
		return rag.Eval(ctx, ans, qs, o)
	}
}

// noModel — 503: отвечать нечем. Базы нет — причина в ней (без базы
// приложение отвечающего агента не заводит), иначе — нет ключа модели.
func (a *API) noModel(w http.ResponseWriter) {
	if a.Searcher == nil {
		a.unavailable(w)
		return
	}
	why := "модели нет — ответ по базе и контрольные вопросы выключены"
	server.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": why, "why": why, "hint": HintNoModel})
}

// allModes — режимы ответа (v23: плюс три режима с конвейером retrieve).
var allModes = []rag.Mode{rag.NoRAG, rag.RAG, rag.RAGFilter, rag.RAGRewrite, rag.RAGBoth}

// validMode — режим из allModes.
func validMode(m rag.Mode) bool {
	for _, x := range allModes {
		if x == m {
			return true
		}
	}
	return false
}

// modeNames — режимы через запятую для сообщений об ошибке.
func modeNames() string {
	s := make([]string, len(allModes))
	for i, m := range allModes {
		s[i] = string(m)
	}
	return strings.Join(s, ", ")
}

// modes — режимы запроса: пусто — norag и rag; неизвестный или повтор —
// ошибка.
func modes(in []rag.Mode) ([]rag.Mode, error) {
	if len(in) == 0 {
		return []rag.Mode{rag.NoRAG, rag.RAG}, nil
	}
	var out []rag.Mode
	for _, m := range in {
		if !validMode(m) {
			return nil, errors.New("режим — один из " + modeNames() + ": " + string(m))
		}
		for _, x := range out {
			if x == m {
				return nil, errors.New("режим дважды: " + string(m))
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// loadQuestions — набор вопросов; нет файла — 404 с подсказкой, битый — 500.
func (a *API) loadQuestions(w http.ResponseWriter) (kb.QuestionSet, bool) {
	if a.Questions == "" {
		server.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "файл контрольных вопросов не задан", "hint": hintNoQuestions})
		return kb.QuestionSet{}, false
	}
	qs, err := kb.LoadQuestions(a.Questions)
	if errors.Is(err, fs.ErrNotExist) {
		server.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "нет файла контрольных вопросов: " + a.Questions, "hint": hintNoQuestions})
		return kb.QuestionSet{}, false
	}
	if err != nil {
		server.WriteError(w, http.StatusInternalServerError, err.Error())
		return kb.QuestionSet{}, false
	}
	return qs, true
}

// ------------------------------------------------------------ questions

func (a *API) questions(w http.ResponseWriter) {
	qs, ok := a.loadQuestions(w)
	if !ok {
		return
	}
	if qs.Questions == nil {
		qs.Questions = []kb.Question{}
	}
	server.WriteJSON(w, http.StatusOK, qs.Questions)
}

// ------------------------------------------------------------ ask

// ask — вопрос в режимах рядом: запросы к модели идут параллельно, ответы —
// в порядке Modes. Вопрос из набора (QuestionID) оценивается только
// правилом: судья — второй платный запрос на ответ, ему место в прогоне.
// Ошибка одного режима не роняет другой: она — в Error, ответ режима —
// пустой; ошибки всех режимов — 502.
func (a *API) ask(w http.ResponseWriter, r *http.Request) {
	var in AskRequest
	if err := readBody(r, &in); err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Q = strings.TrimSpace(in.Q)
	if in.Q == "" {
		server.WriteError(w, http.StatusBadRequest, "нужен вопрос: q")
		return
	}
	if utf8.RuneCountInString(in.Q) > maxQ {
		server.WriteError(w, http.StatusBadRequest, "вопрос длиннее "+strconv.Itoa(maxQ)+" символов")
		return
	}
	if len(in.Context) > maxContext {
		server.WriteError(w, http.StatusBadRequest, "реплик контекста больше "+strconv.Itoa(maxContext))
		return
	}
	for _, c := range in.Context {
		if utf8.RuneCountInString(c) > maxQ {
			server.WriteError(w, http.StatusBadRequest, "реплика контекста длиннее "+strconv.Itoa(maxQ)+" символов")
			return
		}
	}
	ms, err := modes(in.Modes)
	if err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(ms) > maxAskModes {
		server.WriteError(w, http.StatusBadRequest, "режимов больше "+strconv.Itoa(maxAskModes)+": рядом помещается не больше трёх ответов")
		return
	}
	var question *kb.Question
	note := ""
	if id := strings.TrimSpace(in.QuestionID); id != "" {
		qs, ok := a.loadQuestions(w)
		if !ok {
			return
		}
		for i := range qs.Questions {
			if qs.Questions[i].ID == id {
				question = &qs.Questions[i]
				break
			}
		}
		if question == nil {
			server.WriteError(w, http.StatusBadRequest, "нет вопроса "+id+" в наборе")
			return
		}
		if len(in.Context) == 0 {
			in.Context = question.Context
		}
		// Текст изменён — это уже другой вопрос: ожидание набора к нему не
		// относится, и оценка правилом вводила бы в заблуждение.
		if !sameText(in.Q, question.Q) {
			note = "вопрос изменён — текст не совпадает с " + question.ID + " набора, поэтому ответы не оцениваются правилом"
			question = nil
		}
	}
	fn := a.askFn()
	if fn == nil {
		a.noModel(w)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), askTimeout)
	defer cancel()
	q := rag.Question{Text: in.Q, Context: in.Context}
	answers := make([]rag.Answer, len(ms))
	errs := make([]error, len(ms))
	var wg sync.WaitGroup
	for i, m := range ms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i], errs[i] = fn(ctx, q, m)
			answers[i].Mode = m
		}()
	}
	wg.Wait()
	if r.Context().Err() != nil {
		return // клиент ушёл
	}

	v := AskView{Q: in.Q, Answers: answers, Note: note}
	var msgs []string
	for i, m := range ms {
		if errs[i] != nil {
			msgs = append(msgs, string(m)+": "+errs[i].Error())
			continue
		}
		if question != nil {
			res := a.ruleFn()(*question, answers[i].Text)
			v.Rows = append(v.Rows, rag.Run{Repeat: 1, Answer: answers[i], Rule: res, Final: res.Verdict,
				Recall: m.UsesBase() && a.recall(ctx, *question, answers[i].Hits)})
		}
	}
	v.Error = strings.Join(msgs, "; ")
	if len(msgs) == len(ms) {
		server.WriteJSON(w, http.StatusBadGateway, v)
		return
	}
	server.WriteJSON(w, http.StatusOK, v)
}

// sameText — тексты совпадают с точностью до пробелов.
func sameText(a, b string) bool {
	return strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
}

// recall — нашёлся ли в выдаче фрагмент, покрывающий хотя бы одно
// доказательство вопроса (≥ kb.EvidenceCover), как в kb.Compare.
func (a *API) recall(ctx context.Context, q kb.Question, hits []kb.Hit) bool {
	if a.Searcher == nil || len(hits) == 0 {
		return false
	}
	texts := map[string]string{}
	for _, e := range q.Evidence {
		t, ok := texts[e.DocID]
		if !ok {
			d, err := a.Searcher.Store.Doc(ctx, e.DocID)
			if err != nil {
				continue
			}
			t = d.Text()
			texts[e.DocID] = t
		}
		start, n := corpus.Find(t, e.Quote)
		if start < 0 {
			continue
		}
		for _, h := range hits {
			if h.DocID == e.DocID && kb.Covers(h.Start, h.End, start, start+n) >= kb.EvidenceCover {
				return true
			}
		}
	}
	return false
}

// ------------------------------------------------------------ evals

// startEval — POST evals: прогон в горутине со своим контекстом (окно
// получает id и опрашивает; закрытая вкладка прогон не обрывает).
func (a *API) startEval(w http.ResponseWriter, r *http.Request) {
	var in EvalRequest
	if err := readBody(r, &in); err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Repeats == 0 {
		in.Repeats = 1
	}
	if in.Repeats < 1 || in.Repeats > maxRepeats {
		server.WriteError(w, http.StatusBadRequest, "repeats — от 1 до "+strconv.Itoa(maxRepeats)+": "+strconv.Itoa(in.Repeats))
		return
	}
	ms, err := modes(in.Modes)
	if err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Modes = ms
	if len(in.Splits) == 0 {
		in.Splits = []string{kb.SplitTest}
	}
	for _, s := range in.Splits {
		if s != kb.SplitTest && s != kb.SplitDev && s != kb.SplitOut {
			server.WriteError(w, http.StatusBadRequest, "набор — test, dev или out: "+s)
			return
		}
	}
	fn := a.evalFn()
	if fn == nil {
		a.noModel(w)
		return
	}
	qs, ok := a.loadQuestions(w)
	if !ok {
		return
	}
	n := 0
	for _, s := range in.Splits {
		n += len(qs.Split(s))
	}
	if n == 0 {
		server.WriteError(w, http.StatusBadRequest, "в наборах "+strings.Join(in.Splits, ", ")+" нет вопросов")
		return
	}
	// Судья — если попросили и он есть; иначе прогон честно идёт правилом.
	in.Judge = in.Judge && a.Judge != nil
	opts := rag.EvalOptions{Splits: in.Splits, Modes: ms, Repeats: in.Repeats, Seed: time.Now().UnixNano()}
	if in.Judge {
		opts.Judge = a.Judge
	}
	// Контекст — до заведения прогона: отмена (DELETE) должна застать его
	// уже в прогоне.
	bg, stop := context.WithTimeout(context.WithoutCancel(r.Context()), evalTimeout)
	run, busy := a.addEval(in, n*len(ms)*in.Repeats, stop)
	if run == nil {
		stop()
		server.WriteJSON(w, http.StatusConflict, map[string]string{"id": busy,
			"error": "прогон уже идёт (" + busy + ") — дождитесь его конца: одновременно идёт один прогон"})
		return
	}
	id := run.view.ID
	opts.Progress = func(row rag.Row) { a.progress(id, row) }
	go func() {
		defer stop()
		rep, err := fn(bg, qs, opts)
		a.finishEval(id, rep, err)
	}()
	server.WriteJSON(w, http.StatusAccepted, a.snapshot(run, true))
}

// addEval заводит прогон, если ни один не идёт; иначе nil и id идущего.
func (a *API) addEval(req EvalRequest, total int, cancel context.CancelFunc) (*evalRun, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, x := range a.evals {
		if x.view.State == StateRunning {
			return nil, x.view.ID
		}
	}
	a.seq++
	run := &evalRun{view: EvalView{ID: "e" + strconv.Itoa(a.seq), State: StateRunning,
		Started: time.Now().Format(time.RFC3339), Request: req, Total: total}, rows: map[string]int{}, cancel: cancel}
	a.evals = append(a.evals, run)
	if len(a.evals) > keepEvals {
		a.evals = append([]*evalRun(nil), a.evals[len(a.evals)-keepEvals:]...)
	}
	return run, ""
}

func (a *API) findEval(id string) *evalRun {
	for _, x := range a.evals {
		if x.view.ID == id {
			return x
		}
	}
	return nil
}

// progress — строка вопроса по мере готовности: встаёт на место своего
// вопроса (повторный приход — та же строка с новыми прогонами), Done —
// число готовых ответов во всех строках.
func (a *API) progress(id string, row rag.Row) {
	a.mu.Lock()
	defer a.mu.Unlock()
	run := a.findEval(id)
	if run == nil || run.view.State != StateRunning {
		return
	}
	row = copyRow(row)
	if i, ok := run.rows[row.Question.ID]; ok {
		run.view.Rows[i] = row
	} else {
		run.rows[row.Question.ID] = len(run.view.Rows)
		run.view.Rows = append(run.view.Rows, row)
	}
	run.view.Done = doneRuns(run.view.Rows)
}

// copyRow — своя копия карт строки: прогон может дописывать их после
// Progress, а снимок кодируется в JSON вне мьютекса.
func copyRow(r rag.Row) rag.Row {
	runs := make(map[rag.Mode][]rag.Run, len(r.Runs))
	for m, rs := range r.Runs {
		runs[m] = append([]rag.Run(nil), rs...)
	}
	maj := make(map[rag.Mode]rag.Verdict, len(r.Majority))
	for m, v := range r.Majority {
		maj[m] = v
	}
	flips := make(map[rag.Mode]int, len(r.Flips))
	for m, n := range r.Flips {
		flips[m] = n
	}
	r.Runs, r.Majority, r.Flips = runs, maj, flips
	return r
}

func doneRuns(rows []rag.Row) int {
	n := 0
	for _, r := range rows {
		for _, rs := range r.Runs {
			n += len(rs)
		}
	}
	return n
}

// finishEval — итог прогона: отчёт (строки — из него) или ошибка.
func (a *API) finishEval(id string, rep rag.Report, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	run := a.findEval(id)
	if run == nil {
		return
	}
	if run.view.State == StateCancelled {
		return // остановлен: строки — какие успели, состояние не меняется
	}
	if err != nil {
		run.view.State = StateFailed
		run.view.Error = err.Error()
		return
	}
	run.view.State = StateDone
	if len(rep.Rows) > 0 {
		run.view.Rows = rep.Rows
	}
	rep.Rows = nil // строки — в Rows прогона, второй раз не нужны
	run.view.Report = &rep
	run.view.Done = run.view.Total
}

// snapshot — копия вида прогона под мьютексом (rows — со строками).
func (a *API) snapshot(run *evalRun, rows bool) EvalView {
	a.mu.Lock()
	defer a.mu.Unlock()
	return copyView(run, rows)
}

func copyView(run *evalRun, rows bool) EvalView {
	v := run.view
	v.Request.Splits = append([]string(nil), v.Request.Splits...)
	v.Request.Modes = append([]rag.Mode(nil), v.Request.Modes...)
	if rows {
		v.Rows = append([]rag.Row(nil), v.Rows...)
	} else {
		v.Rows = nil
	}
	return v
}

func (a *API) getEval(w http.ResponseWriter, id string) {
	a.mu.Lock()
	run := a.findEval(id)
	var v EvalView
	if run != nil {
		v = copyView(run, true)
	}
	a.mu.Unlock()
	if run == nil {
		server.WriteError(w, http.StatusNotFound, "нет такого прогона: "+id)
		return
	}
	server.WriteJSON(w, http.StatusOK, v)
}

// cancelEval — DELETE evals/{id}: отмена идущего прогона. Контекст
// горутины отменяется, состояние сразу cancelled (новый прогон можно
// запускать, не дожидаясь, пока текущий запрос к модели вернётся); строки —
// те, что успели. Закончившийся прогон — 409, неизвестный — 404.
func (a *API) cancelEval(w http.ResponseWriter, id string) {
	a.mu.Lock()
	run := a.findEval(id)
	if run == nil {
		a.mu.Unlock()
		server.WriteError(w, http.StatusNotFound, "нет такого прогона: "+id)
		return
	}
	if run.view.State != StateRunning {
		state := run.view.State
		a.mu.Unlock()
		server.WriteError(w, http.StatusConflict, "прогон "+id+" уже не идёт ("+state+")")
		return
	}
	run.view.State = StateCancelled
	run.view.Error = "остановлен по запросу: готово " + strconv.Itoa(run.view.Done) + " из " + strconv.Itoa(run.view.Total) + " ответов"
	if run.cancel != nil {
		run.cancel()
	}
	v := copyView(run, true)
	a.mu.Unlock()
	server.WriteJSON(w, http.StatusOK, v)
}

// listEvals — последние прогоны, новые первыми, без строк.
func (a *API) listEvals(w http.ResponseWriter) {
	a.mu.Lock()
	out := make([]EvalView, 0, len(a.evals))
	for i := len(a.evals) - 1; i >= 0; i-- {
		out = append(out, copyView(a.evals[i], false))
	}
	a.mu.Unlock()
	server.WriteJSON(w, http.StatusOK, out)
}
