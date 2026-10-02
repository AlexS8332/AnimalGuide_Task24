package trivia

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
)

func verifyFacts() []Fact {
	return []Fact{
		{Text: "В помёте манула чаще всего 3–4 котёнка.", Sources: []string{"S4"}},
		{Text: "Манул прячется даже в траве высотой 5–10 см.", Sources: []string{"s3", "S3"}},
		{Text: "Манул весит 2–5 кг.", Sources: []string{"S2"}},
	}
}

// В запрос попадают только материалы, процитированные фактами, — у каждого
// факта свои; ни S1, ни S5 манула модель не видит.
func TestVerifyOnlyCitedMaterials(t *testing.T) {
	d := editorLoadDossier(t, "manul")
	fake := &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) {
		return llmtest.Text(`[{"n":1,"ok":true},{"n":2,"ok":true},{"n":3,"ok":true}]`), nil
	}}
	vs, spend, err := LLMVerifier{LLM: fake}.Verify(context.Background(), d, verifyFacts())
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 3 || !vs[0].OK || !vs[1].OK || !vs[2].OK || spend.Requests != 1 {
		t.Fatalf("вердикты %+v, spend %+v", vs, spend)
	}
	req := fake.Requests[0]
	if req.Messages[0].Content != VerifySystem || strings.Contains(req.Messages[0].Content, EditorSystem[:40]) {
		t.Fatal("проверяющий должен идти со своим промптом, без промпта редактора")
	}
	u := req.Messages[1].Content
	for _, id := range []string{"S1", "S5"} {
		if strings.Contains(u, `id="`+id+`"`) {
			t.Errorf("в запрос попал непроцитированный %s", id)
		}
	}
	// S3 процитирован дважды одним фактом — показан один раз.
	for id, want := range map[string]int{"S2": 1, "S3": 1, "S4": 1} {
		if got := strings.Count(u, `id="`+id+`"`); got != want {
			t.Errorf("%s показан %d раз, ждали %d", id, got, want)
		}
	}
	// Материал идёт после своего факта и до следующего.
	f1, s4, f2, s3 := strings.Index(u, "Факт 1:"), strings.Index(u, `id="S4"`), strings.Index(u, "Факт 2:"), strings.Index(u, `id="S3"`)
	if !(f1 < s4 && s4 < f2 && f2 < s3) {
		t.Errorf("материалы не у своих фактов: %d %d %d %d", f1, s4, f2, s3)
	}
}

// Пропущенный вердикт — неподтверждён с причиной; ok=false без причины —
// причина от кода; лишние и повторные номера отбрасываются; ровно
// len(facts) вердиктов.
func TestVerifyMissingAndExtraVerdicts(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) {
		return llmtest.Text("```json\n" + `[{"n":3,"ok":false},{"n":1,"ok":true,"reason":"всё верно"},{"n":1,"ok":false,"reason":"передумал"},{"n":7,"ok":true},{"n":0,"ok":true}]` + "\n```"), nil
	}}
	vs, _, err := LLMVerifier{LLM: fake}.Verify(context.Background(), editorLoadDossier(t, "manul"), verifyFacts())
	if err != nil {
		t.Fatal(err)
	}
	want := []Verdict{{OK: true}, {Reason: VerifyNoAnswer}, {Reason: VerifyNoReason}}
	if len(vs) != len(want) {
		t.Fatalf("вердиктов %d, ждали %d", len(vs), len(want))
	}
	for i := range want {
		if vs[i] != want[i] {
			t.Errorf("вердикт %d: %+v, ждали %+v", i+1, vs[i], want[i])
		}
	}
}

// Факт без материалов из досье модели не показывается; если таких все —
// запроса нет вовсе.
func TestVerifyFactsWithoutSources(t *testing.T) {
	d := editorLoadDossier(t, "akodon")
	facts := []Fact{
		{Text: "Выдумка.", Sources: []string{"S9"}},
		{Text: "Акодон живёт в Перу.", Sources: []string{"S2"}},
		{Text: "Без ссылок."},
	}
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llmtest.Text(`{"verdicts":[{"n":2,"ok":false,"reason":"в S2 сказано «endemic to Peru», это верно, но…"}]}`), nil
	}}
	vs, spend, err := LLMVerifier{LLM: fake}.Verify(context.Background(), d, facts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 3 || vs[0].Reason != VerifyNoSources || vs[2].Reason != VerifyNoSources || vs[1].OK || !strings.HasPrefix(vs[1].Reason, "в S2") {
		t.Fatalf("вердикты %+v", vs)
	}
	u := fake.Requests[0].Messages[1].Content
	if strings.Contains(u, "Факт 1:") || !strings.Contains(u, "Факт 2:") || strings.Contains(u, "Факт 3:") || spend.Requests != 1 {
		t.Fatalf("запрос:\n%s", u)
	}

	fake2 := &llmtest.Fake{}
	vs, spend, err = LLMVerifier{LLM: fake2}.Verify(context.Background(), d, []Fact{facts[0], facts[2]})
	if err != nil || len(vs) != 2 || fake2.Calls() != 0 || spend.Requests != 0 {
		t.Fatalf("без проверяемых фактов: %+v %+v %v, запросов %d", vs, spend, err, fake2.Calls())
	}
}

// Пустой список — пустой результат без запроса.
func TestVerifyEmpty(t *testing.T) {
	fake := &llmtest.Fake{}
	vs, spend, err := LLMVerifier{LLM: fake}.Verify(context.Background(), editorLoadDossier(t, "manul"), nil)
	if err != nil || vs == nil || len(vs) != 0 || fake.Calls() != 0 || spend.Requests != 0 || spend.Model != llm.DefaultModel {
		t.Fatalf("%v %+v %v %d", vs, spend, err, fake.Calls())
	}
}

// Мусор → повтор; две неудачи → ошибка; Spend по обоим запросам.
func TestVerifyRetryAndFail(t *testing.T) {
	d := editorLoadDossier(t, "manul")
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if len(req.Messages) == 2 {
			return llmtest.Text("Все факты верны."), nil
		}
		return llmtest.Text(`[{"n":1,"ok":true},{"n":2,"ok":false,"reason":"в S3 про траву 5–10 см, но про «даже» нет"},{"n":3,"ok":true}]`), nil
	}}
	at := time.Date(2026, 9, 24, 7, 0, 0, 0, time.UTC) // четверг, пик
	vs, spend, err := LLMVerifier{LLM: fake, Now: editorClock(at)}.Verify(context.Background(), d, verifyFacts())
	if err != nil || len(vs) != 3 || vs[1].OK || !vs[0].OK || spend.Requests != 2 || spend.Usage.Total != 30 {
		t.Fatalf("%+v %+v %v", vs, spend, err)
	}
	if spend.Cost.Tariff != llm.TariffPeak || !spend.Cost.Known || spend.Took != 3*time.Second {
		t.Fatalf("spend %+v", spend)
	}
	if last := fake.Requests[1].Messages[3].Content; !strings.Contains(last, `"n":1`) {
		t.Fatalf("напоминание формата: %q", last)
	}

	bad := &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) { return llmtest.Text("[не JSON]"), nil }}
	vs, spend, err = LLMVerifier{LLM: bad}.Verify(context.Background(), d, verifyFacts())
	if err == nil || vs != nil || spend.Requests != 2 {
		t.Fatalf("после двух неудач: %+v %+v %v", vs, spend, err)
	}
}

func TestVerifyParse(t *testing.T) {
	cases := map[string]int{
		`[{"n":1,"ok":true}]`:                          1,
		`Итог: [{"n":1,"ok":true},{"n":2,"ok":false}]`: 2,
		`{"verdicts":[{"n":2,"ok":true}]}`:             1,
		`[]`:                                           0,
	}
	for s, n := range cases {
		got, err := verifyParse(s)
		if err != nil || len(got) != n {
			t.Errorf("%q: %v %v", s, got, err)
		}
	}
	for _, s := range []string{"", "да", `{"ok":true}`} {
		if _, err := verifyParse(s); err == nil {
			t.Errorf("%q разобрался", s)
		}
	}
}
