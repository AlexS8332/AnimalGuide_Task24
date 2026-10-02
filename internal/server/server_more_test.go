package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
)

// Разбор тела для расширений (профиль, память, подборка): пустое тело —
// можно, битое и слишком большое — 400 с объяснением.
func TestReadJSONForExtensions(t *testing.T) {
	var v struct{ Name string }
	rec := httptest.NewRecorder()
	if !ReadJSON(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("")), &v) {
		t.Fatal("пустое тело отклонено")
	}
	if !ReadJSON(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"Name":"рысь"}`)), &v) || v.Name != "рысь" {
		t.Fatalf("тело: %+v", v)
	}
	rec = httptest.NewRecorder()
	if ReadJSON(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"Name":`)), &v) || rec.Code != http.StatusBadRequest {
		t.Fatalf("битое тело: %d", rec.Code)
	}
	var out map[string]string
	json.NewDecoder(rec.Body).Decode(&out)
	if !strings.Contains(out["error"], "не разобралось") {
		t.Fatalf("ошибка: %v", out)
	}
	big := `{"Name":"` + strings.Repeat("я", maxRequestBody) + `"}`
	rec = httptest.NewRecorder()
	if ReadJSON(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(big)), &v) || rec.Code != http.StatusBadRequest {
		t.Fatalf("тело больше предела: %d", rec.Code)
	}
}

// Код ответа по ошибке: не найдено — 404, занят — 409, прочее — 400.
func TestStatusOf(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{runs.ErrNotFound, http.StatusNotFound},
		{fmt.Errorf("диалог: %w", runs.ErrNotFound), http.StatusNotFound},
		{os.ErrNotExist, http.StatusNotFound},
		{fmt.Errorf("обёртка: %w", runs.ErrBusy), http.StatusConflict},
		{runs.ErrEmpty, http.StatusBadRequest},
		{errors.New("что-то"), http.StatusBadRequest},
	}
	for _, c := range cases {
		if got := StatusOf(c.err); got != c.code {
			t.Errorf("StatusOf(%v) = %d, ждали %d", c.err, got, c.code)
		}
	}
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusTeapot, "чайник")
	if rec.Code != http.StatusTeapot || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || !strings.Contains(rec.Body.String(), "чайник") {
		t.Fatalf("WriteError: %d %q", rec.Code, rec.Body.String())
	}
}

// noFlush — ответ без http.Flusher: поток событий на нём невозможен.
type noFlush struct{ rec *httptest.ResponseRecorder }

func (n noFlush) Header() http.Header         { return n.rec.Header() }
func (n noFlush) Write(b []byte) (int, error) { return n.rec.Write(b) }
func (n noFlush) WriteHeader(code int)        { n.rec.WriteHeader(code) }

// Поток без поддержки сброса — честная ошибка 500, а не зависший ответ.
func TestTurnStreamNeedsFlusher(t *testing.T) {
	a := newApp(t)
	_, out := a.do(t, http.MethodPost, "/api/conversations", `{"text":"рысь"}`)
	a.stream(t, out["events"].(string))
	turnID := out["turn"].(map[string]any)["id"].(string)
	h := New(a.m, nil, nil)
	w := noFlush{rec: httptest.NewRecorder()}
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/turns/"+turnID+"/events", nil))
	if w.rec.Code != http.StatusInternalServerError {
		t.Fatalf("поток без Flusher: %d", w.rec.Code)
	}
}

// Поток завершённого хода: снимок и сразу done, без ожидания.
func TestStreamOfFinishedTurn(t *testing.T) {
	a := newApp(t)
	_, out := a.do(t, http.MethodPost, "/api/conversations", `{"text":"рысь"}`)
	a.stream(t, out["events"].(string))
	kinds := a.stream(t, out["events"].(string))
	if strings.Join(kinds, ",") != "snapshot,done" {
		t.Fatalf("повторный поток: %v", kinds)
	}
	code, turn := a.do(t, http.MethodGet, "/api/turns/"+out["turn"].(map[string]any)["id"].(string), "")
	if code != 200 || len(turn["events"].([]any)) == 0 {
		t.Fatalf("журнал хода: %d", code)
	}
}

// Клиент ушёл посреди хода — обработчик потока завершается, ход идёт
// дальше.
func TestStreamClientGoneMidTurn(t *testing.T) {
	a := newApp(t)
	release := make(chan struct{})
	a.brain.LeadScript = func(req llm.Request, step int) llm.Response {
		<-release
		return llmtest.Text("ок")
	}
	_, out := a.do(t, http.MethodPost, "/api/conversations", `{"text":"как дела?"}`)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.srv.URL+out["events"].(string), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if _, err := resp.Body.Read(buf); err != nil || !strings.HasPrefix(string(buf), "event: snapshot") {
		t.Fatalf("начало потока: %q %v", buf, err)
	}
	cancel()
	resp.Body.Close()
	close(release)
	// Ход дописывается и без слушателя.
	kinds := a.stream(t, out["events"].(string))
	if kinds[len(kinds)-1] != "done" {
		t.Fatalf("поток после ухода клиента: %v", kinds)
	}
	s, ok := a.m.Turn(out["turn"].(map[string]any)["id"].(string))
	if !ok || s.Wait(5*time.Second).Status != runs.StatusDone {
		t.Fatal("ход не завершился")
	}
}

// Пустой диалог с неверным собеседником — 400; выгрузка несуществующей
// карточки — 400, а не 500.
func TestCreateAndExportErrors(t *testing.T) {
	a := newApp(t)
	if code, out := a.do(t, http.MethodPost, "/api/conversations", `{"empty":true,"owners":["../x"]}`); code != 400 {
		t.Fatalf("опасный собеседник: %d %v", code, out)
	}
	if code, _ := a.do(t, http.MethodPost, "/api/conversations", `{"owners":["../x"],"text":"рысь"}`); code != 400 {
		t.Fatalf("ход с опасным собеседником: %d", code)
	}
	_, out := a.do(t, http.MethodPost, "/api/conversations", `{"empty":true,"title":"пустой"}`)
	id := out["conversation"].(map[string]any)["id"].(string)
	for _, q := range []string{"?kind=card&id=nope", "?kind=comparison&id=0", "?kind=what"} {
		if code, _ := a.do(t, http.MethodGet, "/api/conversations/"+id+"/export"+q, ""); code != 400 {
			t.Errorf("выгрузка %s: %d", q, code)
		}
	}
	for _, p := range []string{"/checkpoints", "/branches", "/switch", "/features", "/turns"} {
		if code, _ := a.do(t, http.MethodPost, "/api/conversations/"+id+p, `{bad`); code != 400 {
			t.Errorf("битое тело %s: %d", p, code)
		}
	}
	if code, _ := a.do(t, http.MethodPost, "/api/groups", `{bad`); code != 400 {
		t.Error("битое тело стенда")
	}
	if code, _ := a.do(t, http.MethodPost, "/api/groups/x/turns", `{bad`); code != 400 {
		t.Error("битое тело хода стенда")
	}
}

// Неизвестный механизм и ветка от несуществующей точки — 400 с текстом.
func TestFeatureAndBranchErrors(t *testing.T) {
	a := newApp(t)
	_, out := a.do(t, http.MethodPost, "/api/conversations", `{"empty":true}`)
	id := out["conversation"].(map[string]any)["id"].(string)
	code, body := a.do(t, http.MethodPost, "/api/conversations/"+id+"/features", `{"name":"нет-такого","on":true}`)
	if code != 400 || !strings.Contains(body["error"].(string), "неизвестный механизм") {
		t.Fatalf("неизвестный механизм: %d %v", code, body)
	}
	if code, _ := a.do(t, http.MethodPost, "/api/conversations/"+id+"/branches", `{"checkpoint":"nope"}`); code != 400 && code != 404 {
		t.Fatalf("ветка от несуществующей точки: %d", code)
	}
	if code, _ := a.do(t, http.MethodPost, "/api/conversations/"+id+"/switch", `{"branch":"nope"}`); code != 400 && code != 404 {
		t.Fatalf("несуществующая ветка: %d", code)
	}
}

// Сведения, которые передал main, дополняют и перекрывают базовые.
func TestMetaOverrides(t *testing.T) {
	a := newApp(t)
	h := New(a.m, nil, map[string]any{"pid": "скрыт", "profileForm": []string{"level"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/meta", nil))
	body, _ := io.ReadAll(rec.Body)
	var meta map[string]any
	json.Unmarshal(body, &meta)
	if meta["pid"] != "скрыт" || meta["profileForm"] == nil || meta["historyDir"] == "" || meta["serverStarted"] == nil {
		t.Fatalf("meta: %v", meta)
	}
}
