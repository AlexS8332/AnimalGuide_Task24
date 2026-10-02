package rag

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

func TestCompose(t *testing.T) {
	if got := Compose(nil); !strings.Contains(got, "ничего не найдено") {
		t.Fatalf("пустая выдача: %q", got)
	}
	hits := []kb.Hit{
		{Chunk: kb.Chunk{ID: "manul/structure/004", Title: "Манул", Path: []string{"Образ жизни", "Питание"}, Text: "Питается пищухами.\nИногда птицами."}},
		{Chunk: kb.Chunk{ID: "manul/structure/000", Title: "Манул", Section: "Вступление", Text: "Манул — дикая кошка."}},
	}
	got := Compose(hits)
	for _, want := range []string{tools.TrustNote, "[manul/structure/004] Манул › Образ жизни › Питание\nПитается пищухами.\nИногда птицами.",
		"[manul/structure/000] Манул › Вступление\nМанул — дикая кошка."} {
		if !strings.Contains(got, want) {
			t.Fatalf("нет %q в\n%s", want, got)
		}
	}
	if strings.Count(got, tools.TrustNote) != 1 {
		t.Fatal("пометка не одна")
	}
}

func TestSearchTool(t *testing.T) {
	s := searcher(t)
	tl := SearchTool(s, "", 0)
	sp := tl.Spec()
	if sp.Name != ToolName || !sp.Untrusted || !json.Valid(sp.Parameters) {
		t.Fatalf("описание: %+v", sp)
	}
	out, err := tl.Call(context.Background(), json.RawMessage(`{"query":"сколько часов малая панда тратит на еду","k":3}`))
	if err != nil {
		t.Fatal(err)
	}
	var res SearchResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Index != DefaultIndex || res.Mode != kb.Dense || len(res.Hits) != 3 || res.Hits[0].ChunkID == "" ||
		res.Hits[0].Text == "" || res.Hits[0].Path == "" || res.Query == "" {
		t.Fatalf("выдача: %+v", res)
	}
	// k по умолчанию — с которым собран инструмент; больше MaxK — MaxK.
	out, _ = SearchTool(s, "structure", 2).Call(context.Background(), json.RawMessage(`{"query":"манул"}`))
	json.Unmarshal([]byte(out), &res)
	if len(res.Hits) != 2 {
		t.Fatalf("k по умолчанию: %d", len(res.Hits))
	}
	out, _ = tl.Call(context.Background(), json.RawMessage(`{"query":"манул","k":50}`))
	json.Unmarshal([]byte(out), &res)
	if len(res.Hits) != MaxK {
		t.Fatalf("предел k: %d", len(res.Hits))
	}
	// Без эмбеддера — BM25 с причиной, а не ошибка.
	bm := &kb.Searcher{Store: s.Store}
	out, err = SearchTool(bm, "", 0).Call(context.Background(), json.RawMessage(`{"query":"манул"}`))
	if err != nil || !strings.Contains(out, `"mode":"bm25"`) || !strings.Contains(out, `"fallback":"эмбеддер не задан"`) {
		t.Fatalf("откат: %v %s", err, out)
	}
	if _, err := tl.Call(context.Background(), json.RawMessage(`{"query":"  "}`)); err == nil {
		t.Fatal("пустой запрос")
	}
	if _, err := tl.Call(context.Background(), json.RawMessage(`{"query":`)); err == nil {
		t.Fatal("битые аргументы")
	}
	if _, err := SearchTool(nil, "", 0).Call(context.Background(), json.RawMessage(`{"query":"манул"}`)); err == nil {
		t.Fatal("без базы")
	}
	if _, err := SearchTool(s, "nope", 0).Call(context.Background(), json.RawMessage(`{"query":"манул"}`)); err == nil {
		t.Fatal("нет индекса")
	}
}

// Режимы отличаются ровно правилом в конце системного промпта и
// фрагментами после вопроса; инструментов нет ни в одном.
func TestAnswerModes(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		r := llmtest.Text("Ответ.")
		r.Usage = llm.Usage{Prompt: 1000, Completion: 50, Total: 1050, CacheHit: 600, CacheMiss: 400}
		return r, nil
	}}
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	a := &Answerer{LLM: fake, Searcher: searcher(t), K: 3, Now: func() time.Time { return at }}
	q := Question{Text: "Сколько часов в день кошачий медведь тратит на еду?"}

	no, err := a.Answer(context.Background(), q, NoRAG)
	if err != nil {
		t.Fatal(err)
	}
	with, err := a.Answer(context.Background(), q, RAG)
	if err != nil {
		t.Fatal(err)
	}
	r0, r1 := fake.Requests[0], fake.Requests[1]
	if len(r0.Tools) != 0 || len(r1.Tools) != 0 || r0.Temperature != 0 || r0.Model != llm.DefaultModel {
		t.Fatalf("запрос: %+v", r0)
	}
	if len(r0.Messages) != 2 || r0.Messages[0].Content != System(NoRAG) || r0.Messages[1].Content != q.Text {
		t.Fatalf("norag: %+v", r0.Messages)
	}
	if !strings.HasPrefix(System(RAG), System(NoRAG)+"\n\n") || !strings.Contains(System(RAG), "chunk_id") ||
		r1.Messages[0].Content != System(RAG) {
		t.Fatal("системный промпт rag — общий префикс и правило")
	}
	user := r1.Messages[1].Content
	if !strings.HasPrefix(user, q.Text+"\n\nФрагменты базы знаний") || len(with.Hits) != 3 ||
		!strings.Contains(user, "["+with.Hits[0].ID+"]") || !strings.Contains(user, with.Hits[2].Text) {
		t.Fatalf("rag: %s", user)
	}
	if no.Mode != NoRAG || with.Mode != RAG || no.Hits != nil || with.Search.Mode != kb.Dense || with.Search.Index != DefaultIndex {
		t.Fatalf("ответы: %+v / %+v", no, with.Search)
	}
	if no.Text != "Ответ." || no.System != System(NoRAG) || no.User != q.Text || with.User != user || no.Usage.Prompt != 1000 {
		t.Fatalf("поля ответа: %+v", no)
	}
	if !no.Cost.Known || no.Cost.USD <= 0 || no.Cost != llm.PriceOf(llm.DefaultModel, no.Usage, at) {
		t.Fatalf("цена: %+v", no.Cost)
	}
}

// Вопрос-продолжение: предыдущая реплика — в том же сообщении перед
// вопросом (одно сообщение пользователя), а в поиск — вместе с вопросом.
func TestAnswerContext(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) { return llmtest.Text("2,5–5,8 кг."), nil }}
	// BM25: по слову «харза» из предыдущей реплики, без которой искать нечем.
	a := &Answerer{LLM: fake, Model: "m", Searcher: &kb.Searcher{Store: searcher(t).Store}}
	q := QuestionOf(question(t, questions(t), "T08"))
	if q.Query() != "Где в России водится харза? А сколько она весит?" {
		t.Fatalf("запрос поиска: %q", q.Query())
	}
	ans, err := a.Answer(context.Background(), q, RAG)
	if err != nil {
		t.Fatal(err)
	}
	m := fake.Requests[0].Messages
	want := "Предыдущие реплики пользователя:\n- Где в России водится харза?\n\nВопрос: А сколько она весит?\n\nФрагменты базы знаний"
	if len(m) != 2 || m[0].Role != llm.RoleSystem || m[1].Role != llm.RoleUser || !strings.HasPrefix(m[1].Content, want) ||
		ans.User != m[1].Content || fake.Requests[0].Model != "m" {
		t.Fatalf("сообщения: %+v", m)
	}
	if askedText(m[1].Content) != q.Text {
		t.Fatalf("вопрос в сообщении: %q", askedText(m[1].Content))
	}
	// norag: то же одно сообщение без фрагментов; без контекста — сам вопрос.
	no, err := a.Answer(context.Background(), q, NoRAG)
	if err != nil {
		t.Fatal(err)
	}
	if m := fake.Requests[1].Messages; len(m) != 2 || no.User != m[1].Content ||
		m[1].Content != "Предыдущие реплики пользователя:\n- Где в России водится харза?\n\nВопрос: А сколько она весит?" {
		t.Fatalf("norag: %+v", m)
	}
	if (Question{Text: " Вопрос? ", Context: []string{" ", ""}}).UserText() != "Вопрос?" {
		t.Fatal("пустой контекст")
	}
	found := false
	for _, h := range ans.Hits {
		found = found || h.DocID == "yellow-throated-marten"
	}
	if !found || ans.Cost.Known {
		t.Fatalf("выдача без харзы или цена неизвестной модели посчитана: %+v", ans.Cost)
	}
}

func TestAnswerErrors(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) { return llm.Response{}, context.DeadlineExceeded }}
	a := &Answerer{LLM: fake}
	ctx := context.Background()
	if _, err := a.Answer(ctx, Question{Text: "?"}, RAG); err == nil || !strings.Contains(err.Error(), "без базы") {
		t.Fatalf("rag без базы: %v", err)
	}
	if _, err := a.Answer(ctx, Question{Text: "?"}, "both"); err == nil {
		t.Fatal("неизвестный режим")
	}
	if _, err := a.Answer(ctx, Question{Text: " "}, NoRAG); err == nil {
		t.Fatal("пустой вопрос")
	}
	if _, err := a.Answer(ctx, Question{Text: "?"}, NoRAG); err == nil {
		t.Fatal("ошибка модели")
	}
	if _, err := (&Answerer{}).Answer(ctx, Question{Text: "?"}, NoRAG); err == nil {
		t.Fatal("без модели")
	}
	if fake.Calls() != 1 {
		t.Fatalf("запросов к модели %d", fake.Calls())
	}
}

func TestParseVerdict(t *testing.T) {
	for in, want := range map[string]Verdict{
		`{"verdict":"correct","reason":"всё на месте"}`:                        Correct,
		"```json\n{\"verdict\": \"partial\", \"reason\": \"нет тиража\"}\n```": Partial,
		"Оценка {см. ниже}:\n{\"verdict\":\"WRONG\",\"reason\":\"x\"} — так.":  Wrong,
		`  {"reason":"нет данных","verdict":" abstain "}`:                      Abstain,
	} {
		v, reason, err := ParseVerdict(in)
		if err != nil || v != want || reason == "" {
			t.Errorf("%q: %s %q %v", in, v, reason, err)
		}
	}
	for _, in := range []string{"", "верно", `{"verdict":"maybe"}`, `{"reason":"нет вердикта"}`, `{"verdict":`} {
		if _, _, err := ParseVerdict(in); err == nil {
			t.Errorf("%q разобран", in)
		}
	}
}

// Судья видит вопрос, эталон словами и группами, доказательства и один
// ответ — и ничего о режиме.
func TestJudgeGrade(t *testing.T) {
	qs := questions(t)
	var got llm.Request
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		got = req
		return llmtest.Text("```json\n{\"verdict\":\"correct\",\"reason\":\"совпало\"}\n```"), nil
	}}
	j := &Judge{LLM: fake}
	answer := "Два вида: A. fulgens и A. styani [mdd-carnivora/structure/003]."
	res, err := j.Grade(context.Background(), question(t, qs, "T03"), answer)
	if err != nil || res.Verdict != Correct || res.Reason != "совпало" || res.Usage.Total == 0 || !res.Cost.Known {
		t.Fatalf("оценка: %+v %v", res, err)
	}
	user := got.Messages[1].Content
	for _, want := range []string{"Вопрос: Сколько видов малых (красных) панд признаёт MDD v2.5?", "Эталон: Два вида рода Ailurus",
		"- 2 вида | два вида | двух видов", "Числа: 2", "Доказательства:", "(mdd-carnivora)", "<<<\nДва вида: A. fulgens и A. styani.\n>>>"} {
		if !strings.Contains(user, want) {
			t.Errorf("нет %q во входе судьи:\n%s", want, user)
		}
	}
	if got.Messages[0].Content != JudgeSystem() || got.Temperature != 0 || len(got.Tools) != 0 {
		t.Fatal("системный промпт судьи")
	}
	low := strings.ToLower(user + got.Messages[0].Content)
	for _, leak := range []string{"norag", "режим rag", "без базы", "с базой", "structure/"} {
		if strings.Contains(low, leak) {
			t.Errorf("судья узнаёт режим: %q", leak)
		}
	}
	// Неотвечаемый вопрос и вопрос-продолжение.
	j.Grade(context.Background(), question(t, qs, "T10"), "Не знаю.")
	if !strings.Contains(got.Messages[1].Content, "Ответа в базе нет") {
		t.Fatalf("неотвечаемый: %s", got.Messages[1].Content)
	}
	j.Grade(context.Background(), question(t, qs, "T08"), "2,5 кг")
	if !strings.Contains(got.Messages[1].Content, "Предыдущие реплики пользователя:\n- Где в России водится харза?") {
		t.Fatalf("контекст: %s", got.Messages[1].Content)
	}
	// Неразборчиво — ошибка, а не wrong; расход при этом известен.
	fake.Fn = func(llm.Request) (llm.Response, error) { return llmtest.Text("Ответ верный."), nil }
	res, err = j.Grade(context.Background(), question(t, qs, "T03"), answer)
	if err == nil || res.Verdict != "" || res.Usage.Total == 0 {
		t.Fatalf("неразборчивый ответ: %+v %v", res, err)
	}
	if _, err := (&Judge{}).Grade(context.Background(), kb.Question{}, ""); err == nil {
		t.Fatal("судья без модели")
	}
}

// Судья слеп к приметам режима: ссылки [chunk_id] и «в базе знаний» /
// «во фрагментах» до него не доходят — один и тот же ответ с ними и без
// них он получает одинаковым текстом.
func TestJudgeBlind(t *testing.T) {
	qs := questions(t)
	var users []string
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		users = append(users, req.Messages[1].Content)
		return verdictJSON(Correct, "совпало"), nil
	}}
	j := &Judge{LLM: fake}
	q := question(t, qs, "T07")
	for _, pair := range [][2]string{
		{"Малая панда ест по 13 часов [red-panda/structure/013].", "Малая панда ест по 13 часов."},
		{"Малая панда ест по 13 часов [red-panda/structure/013, red-panda/fixed/020].", "Малая панда ест по 13 часов."},
		{"Во фрагментах этого нет [jungle-cat/structure/001].", "У меня этого нет."},
		{"В приведённых фрагментах ничего не сказано; в базе знаний этого нет.", "У меня ничего не сказано; у меня этого нет."},
	} {
		users = nil
		for _, a := range pair {
			if _, err := j.Grade(context.Background(), q, a); err != nil {
				t.Fatal(err)
			}
		}
		if users[0] != users[1] {
			t.Errorf("судья различает %q и %q:\n%s\n---\n%s", pair[0], pair[1], users[0], users[1])
		}
	}
	if strings.Contains(JudgeSystem(), "structure/") {
		t.Fatal("в промпте судьи остались ссылки на фрагменты")
	}
	if got := Blind("Около 13 часов red-panda/structure/013."); got != "Около 13 часов ." {
		t.Fatalf("голая ссылка: %q", got)
	}
}
