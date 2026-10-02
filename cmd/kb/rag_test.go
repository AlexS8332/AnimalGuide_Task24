package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
)

// Команды ask, qa и probe на подставной модели: OpenAI-совместимый сервер
// в процессе (DEEPSEEK_BASE_URL), база — настоящий корпус на hash-эмбеддере.

// fakeDeepSeek — подставной /chat/completions. rag отвечает «по базе» со
// ссылкой, norag — «не знаю», кроме вопросов о MDD (их модель «знает»);
// судья: «ВЕРНО» и ссылка на фрагмент — correct, «не знаю» — abstain.
type fakeDeepSeek struct {
	mu   sync.Mutex
	reqs []llm.Request
}

func (f *fakeDeepSeek) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, `{"error":{"message":"не тот запрос"}}`, http.StatusBadRequest)
			return
		}
		var req llm.Request
		var body struct {
			Model    string        `json:"model"`
			Messages []llm.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Model, req.Messages = body.Model, body.Messages
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		f.mu.Unlock()
		sys, user := body.Messages[0].Content, body.Messages[len(body.Messages)-1].Content
		var text string
		var calls []any
		switch {
		case sys == rag.System(rag.RAGCite):
			calls = citeCall(body.Messages[1].Content)
		case sys == rag.SupportSystem():
			text = `{"claims":[{"claim":"ответ","supported":true,"quote":1}]}`
		case sys == rag.JudgeSystem():
			text = `{"verdict":"abstain","reason":"сказано, что не знает"}`
			if strings.Contains(user, "ВЕРНО") || strings.Contains(user, "По базе: ответ.") {
				text = "```json\n{\"verdict\":\"correct\",\"reason\":\"совпало\"}\n```"
			}
		case sys == rag.System(rag.RAG):
			text = "По базе: ответ [red-panda/structure/013]."
		case strings.Contains(user, "MDD"):
			text = "Знаю: ВЕРНО."
		default:
			text = "Не знаю."
		}
		msg, finish := map[string]any{"role": "assistant", "content": text}, "stop"
		if calls != nil {
			msg, finish = map[string]any{"role": "assistant", "content": "", "tool_calls": calls}, "tool_calls"
		}
		json.NewEncoder(w).Encode(map[string]any{
			"model":   body.Model,
			"choices": []any{map[string]any{"message": msg, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110, "prompt_cache_hit_tokens": 60, "prompt_cache_miss_tokens": 40},
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	t.Setenv("DEEPSEEK_BASE_URL", srv.URL)
	t.Setenv("DEEPSEEK_MODEL", "")
}

// fragmentRe — фрагмент в сообщении отвечающего агента (rag.Compose).
var fragmentRe = regexp.MustCompile(`(?m)^\[([^\]\s]+/(?:structure|fixed)/\d+)\] [^\n]*\n([^\n]+)`)

// citeCall — kb_answer подставной модели: дословная цитата из первого
// фрагмента, а при пометке кода «релевантных фрагментов нет» — unknown.
func citeCall(user string) []any {
	c := rag.Cited{Status: rag.StatusUnknown, Answer: "в базе знаний нет данных об этом", Clarify: "О каком животном речь?"}
	if m := fragmentRe.FindStringSubmatch(user); m != nil && !strings.Contains(user, "Пометка кода") {
		r := []rune(m[2])
		c = rag.Cited{Status: rag.StatusAnswered, Answer: "По базе: ответ.", Sources: []rag.CitedSource{{ChunkID: m[1]}},
			Quotes: []rag.CitedQuote{{ChunkID: m[1], Text: string(r[:min(60, len(r))])}}}
	}
	args, _ := json.Marshal(c)
	return []any{map[string]any{"id": "call_1", "type": "function",
		"function": map[string]any{"name": rag.FinishName, "arguments": string(args)}}}
}

func (f *fakeDeepSeek) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// ragDB — база structure во временном каталоге.
func ragDB(t *testing.T) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "kb.db")
	if code, out, errOut := runKB("index", "-db", db, "-corpus", repoCorpus, "-embedder", "hash", "-strategy", "structure"); code != exitOK {
		t.Fatalf("index: %d\n%s\n%s", code, out, errOut)
	}
	return db
}

func TestAsk(t *testing.T) {
	f := &fakeDeepSeek{}
	f.serve(t)
	db := ragDB(t)
	code, out, errOut := runKB("ask", "-db", db, "-embedder", "hash", "-k", "3", "-context", "Где в России водится харза?", "А сколько она весит?")
	if code != exitOK {
		t.Fatalf("ask: %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"== norag ==\nНе знаю.", "== rag ==\nНайдено (structure, dense hash-256", " 1. ", " 3. ",
		"По базе: ответ [red-panda/structure/013].", "(deepseek-v4-flash; токены 100 → 10, из кэша 60; $", "Итого: $"} {
		if !strings.Contains(out, want) {
			t.Errorf("ask: нет %q в\n%s", want, out)
		}
	}
	if strings.Contains(out, " 4. ") || f.count() != 2 {
		t.Fatalf("k или число запросов: %d\n%s", f.count(), out)
	}
	// Предыдущая реплика — в том же сообщении перед вопросом.
	rq := f.reqs[1]
	if len(rq.Messages) != 2 || !strings.HasPrefix(rq.Messages[1].Content, "Предыдущие реплики пользователя:\n- Где в России водится харза?\n\nВопрос: А сколько она весит?") || rq.Model != llm.DefaultModel {
		t.Fatalf("запрос rag: %+v", rq.Messages)
	}
	// norag без базы: kb.db не нужна.
	code, out, _ = runKB("ask", "-mode", "norag", "-db", filepath.Join(t.TempDir(), "nope.db"), "Сколько весит фосса?")
	if code != exitOK || !strings.Contains(out, "== norag ==") || strings.Contains(out, "== rag ==") || strings.Contains(out, "Итого") {
		t.Fatalf("norag: %d\n%s", code, out)
	}
	// rag без базы — ошибка, а не тихий norag.
	if code, _, errOut := runKB("ask", "-mode", "rag", "-db", filepath.Join(t.TempDir(), "nope.db"), "?"); code != exitFailed ||
		!strings.Contains(errOut, "kb index") {
		t.Fatalf("rag без базы: %d %s", code, errOut)
	}
	for _, args := range [][]string{{"ask"}, {"ask", "-mode", "x", "?"}, {"ask", "-k", "20", "?"}, {"ask", "-mode", "rag", "-db", db, "-index", "fixed", "?"}} {
		if code, _, _ := runKB(args...); code == exitOK {
			t.Errorf("%v: принято", args)
		}
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	if code, _, errOut := runKB("ask", "?"); code != exitUsage || !strings.Contains(errOut, "DEEPSEEK_API_KEY") {
		t.Fatalf("без ключа: %d %s", code, errOut)
	}
}

func TestQA(t *testing.T) {
	f := &fakeDeepSeek{}
	f.serve(t)
	db := ragDB(t)
	dir := t.TempDir()
	md := filepath.Join(dir, "rag", "compare.md")
	code, out, errOut := runKB("qa", "-db", db, "-embedder", "hash", "-questions", "../../eval/questions.json", "-out", md)
	if code != exitOK {
		t.Fatalf("qa: %d\n%s\n%s", code, out, errOut)
	}
	// 10 вопросов × 2 режима и столько же оценок судьи.
	if f.count() != 40 {
		t.Fatalf("запросов %d", f.count())
	}
	if !strings.Contains(out, "- Верных ответов (большинство повторов): rag 10 из 10, norag 2 из 10") ||
		!strings.Contains(out, "Повтор один — без замера шума") || !strings.Contains(out, "JSON: "+filepath.Join(dir, "rag", "compare.json")) {
		t.Fatalf("qa:\n%s", out)
	}
	if !strings.Contains(errOut, "  T01 fact: norag abstain, rag correct") || !strings.Contains(errOut, "  T03 conflict: norag correct, rag correct") {
		t.Fatalf("ход прогона:\n%s", errOut)
	}
	raw, err := os.ReadFile(md)
	if err != nil || !strings.Contains(string(raw), "# Ответ с базой знаний и без") {
		t.Fatalf("отчёт: %v", err)
	}
	var rep rag.Report
	data, _ := os.ReadFile(filepath.Join(dir, "rag", "compare.json"))
	if err := json.Unmarshal(data, &rep); err != nil || len(rep.Rows) != 10 || rep.Embedder != "hash-256" {
		t.Fatalf("JSON: %v %d %q", err, len(rep.Rows), rep.Embedder)
	}
	// Без судьи, один режим, dev.
	n := f.count()
	code, out, _ = runKB("qa", "-modes", "norag", "-splits", "dev", "-judge=false", "-questions", "../../eval/questions.json",
		"-db", filepath.Join(dir, "nope.db"), "-out", filepath.Join(dir, "dev.md"))
	if code != exitOK || f.count()-n != 19 || !strings.Contains(out, "norag: верно 0 из 19") {
		t.Fatalf("norag dev: %d %d\n%s", code, f.count()-n, out)
	}
	// Поиск откатился на BM25 — закоммиченный отчёт по умолчанию не
	// перетирается.
	questions, _ := filepath.Abs("../../eval/questions.json")
	t.Chdir(t.TempDir())
	code, _, errOut = runKB("qa", "-db", db, "-embedder", "none", "-judge=false", "-questions", questions)
	if code != exitFailed || !strings.Contains(errOut, "векторный поиск не состоялся") {
		t.Fatalf("откат: %d %s", code, errOut)
	}
	if _, err := os.Stat(defaultCompare); err == nil {
		t.Fatal("отчёт по BM25 записан")
	}
	for _, args := range [][]string{{"qa", "-modes", "x"}, {"qa", "-repeat", "0"}, {"qa", "-questions", "nope.json"}} {
		if code, _, _ := runKB(args...); code != exitUsage {
			t.Errorf("%v: %d", args, code)
		}
	}
}

func TestProbe(t *testing.T) {
	f := &fakeDeepSeek{}
	f.serve(t)
	dir := t.TempDir()
	src, _ := os.ReadFile("../../eval/questions.json")
	qpath := filepath.Join(dir, "questions.json")
	os.WriteFile(qpath, src, 0o644)
	md := filepath.Join(dir, "probe.md")
	code, out, errOut := runKB("probe", "-questions", qpath, "-repeat", "1", "-out", md, "-write")
	if code != exitOK {
		t.Fatalf("probe: %d\n%s\n%s", code, out, errOut)
	}
	// Модель «знает» вопросы о MDD: T03, T06, D07, D08.
	if !strings.Contains(out, "Вопросов 27, недискриминативных 4: T03, T06, D07, D08") || !strings.Contains(out, "Поле discriminative записано") ||
		f.count() != 27*2 {
		t.Fatalf("probe: %d\n%s", f.count(), out)
	}
	if raw, err := os.ReadFile(md); err != nil || !strings.Contains(string(raw), "недискриминативных 4 (T03, T06, D07, D08)") {
		t.Fatalf("probe.md: %v", err)
	}
	qs, err := kb.LoadQuestions(qpath)
	if err != nil {
		t.Fatal(err)
	}
	// Запись атомарная: временного файла рядом не осталось.
	if left, _ := filepath.Glob(filepath.Join(dir, ".questions.json.*")); len(left) != 0 {
		t.Fatalf("временные файлы: %v", left)
	}
	for _, q := range qs.Questions {
		switch {
		case !q.Answerable || q.Split == kb.SplitOut:
			if q.Discriminative != nil {
				t.Errorf("%s: поле у неотвечаемого", q.ID)
			}
		case q.Discriminative == nil:
			t.Errorf("%s: поля нет", q.ID)
		case *q.Discriminative == strings.Contains(q.Q, "MDD"):
			t.Errorf("%s: %v", q.ID, *q.Discriminative)
		}
	}
	// Без -write файл не трогается.
	os.WriteFile(qpath, src, 0o644)
	if code, _, _ := runKB("probe", "-questions", qpath, "-repeat", "1", "-judge=false", "-out", ""); code != exitOK {
		t.Fatal("probe без -write")
	}
	if now, _ := os.ReadFile(qpath); string(now) != string(src) {
		t.Fatal("файл вопросов изменён без -write")
	}
	if code, _, _ := runKB("probe", "-repeat", "0"); code != exitUsage {
		t.Fatal("-repeat 0")
	}
}
