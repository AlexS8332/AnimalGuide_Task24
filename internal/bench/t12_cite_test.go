package bench

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
)

// fragmentRe — фрагмент в сообщении отвечающего агента (rag.Compose).
var fragmentRe = regexp.MustCompile(`(?m)^\[([^\]\s]+/(?:structure|fixed)/\d+)\] [^\n]*\n([^\n]+)`)

// citeModel — подставная модель И-12 поверх модели стенда: kb_answer с
// дословной цитатой из первого фрагмента (unknown — на неотвечаемых и при
// пометке кода), судья ответа — правилом, судья смысла — «подтверждено».
// obey — модель выполняет инъекцию из подставного фрагмента.
type citeModel struct {
	r    *rig
	qs   kb.QuestionSet
	obey bool
	// shy — при подставном фрагменте модель говорит «не знаю».
	shy bool
}

var judgeQRe = regexp.MustCompile(`(?m)^Вопрос: (.*)$`)

func (m *citeModel) chat(req llm.Request) (llm.Response, error) {
	sys := req.Messages[0].Content
	switch sys {
	case rag.JudgeSystem():
		user := req.Messages[len(req.Messages)-1].Content
		mm := judgeQRe.FindStringSubmatch(user)
		_, text, _ := strings.Cut(user, "<<<\n")
		text = strings.TrimSuffix(text, "\n>>>")
		for _, q := range m.qs.Questions {
			if mm != nil && q.Q == mm[1] {
				return llmtest.Text(`{"verdict":"` + string(rag.Rule(q, text).Verdict) + `","reason":"по эталону"}`), nil
			}
		}
		return llmtest.Text(`{"verdict":"wrong","reason":"?"}`), nil
	case rag.SupportSystem():
		return llmtest.Text(`{"claims":[{"claim":"ответ","supported":true,"quote":1}]}`), nil
	case rag.System(rag.RAGCite):
	default:
		return m.r.chat(req)
	}
	user := req.Messages[1].Content
	text := user
	if _, after, ok := strings.Cut(user, "\n\nВопрос: "); ok && strings.HasPrefix(user, "Предыдущие реплики пользователя:") {
		text = after
	}
	text, _, _ = strings.Cut(text, "\n\n")
	var q kb.Question
	for _, x := range m.qs.Questions {
		if x.Q == text {
			q = x
		}
	}
	unknown := rag.Cited{Status: rag.StatusUnknown, Answer: "в базе знаний нет данных об этом", Clarify: "Уточните, о чём именно?"}
	fr := fragmentRe.FindAllStringSubmatch(user, -1)
	switch {
	case m.obey && strings.Contains(user, InjectionMarker):
		return citeCall(rag.Cited{Status: rag.StatusAnswered, Answer: InjectionMarker,
			Sources: []rag.CitedSource{{ChunkID: injectionID}}, Quotes: []rag.CitedQuote{{ChunkID: injectionID, Text: injectionText}}}), nil
	case m.shy && strings.Contains(user, InjectionMarker):
		return citeCall(unknown), nil
	case !q.Answerable || len(fr) == 0 || strings.Contains(user, "Пометка кода"):
		return citeCall(unknown), nil
	}
	quote := []rune(fr[0][2])
	return citeCall(rag.Cited{Status: rag.StatusAnswered, Answer: q.Expect.Note, Sources: []rag.CitedSource{{ChunkID: fr[0][1]}},
		Quotes: []rag.CitedQuote{{ChunkID: fr[0][1], Text: string(quote[:min(60, len(quote))])}}}), nil
}

func citeCall(c rag.Cited) llm.Response {
	b, _ := json.Marshal(c)
	return llmtest.ToolCall(rag.FinishName, string(b))
}

func citeRig(t *testing.T, obey bool) (*rig, *Cite) {
	t.Helper()
	r := newRig(t)
	qs, err := kb.LoadQuestions("../../eval/questions.json")
	if err != nil {
		t.Fatal(err)
	}
	m := &citeModel{r: r, qs: qs, obey: obey}
	r.env.LLM = &llmtest.Fake{Fn: m.chat}
	return r, &Cite{KBPath: ragKB(t), Questions: "../../eval/questions.json", Embedder: embed.Hash{}}
}

// TestCiteTrial — И-12 на настоящем корпусе, hash-эмбеддере и подставной
// модели: все проверки посчитаны (числа hash-поиска смысла не имеют —
// проверяется, что испытание их считает), честная модель проходит
// дословность, источники, цитаты, «не знаю» вне базы и инъекцию.
func TestCiteTrial(t *testing.T) {
	r, tr := citeRig(t, false)
	res := r.run(t, tr)
	if res.Mechanism != features.RAGCite || len(res.Lanes) != 2 || res.Lanes[1].Name != laneInject {
		t.Fatalf("дорожки: %s %+v", res.Mechanism, res.Lanes)
	}
	for _, what := range citeChecks {
		c := find(t, res, what, "")
		if c.Status == Pending || c.Got == "" || c.Got == "—" {
			t.Errorf("%s: %+v", what, c)
		}
	}
	mustPass(t, res, citeChecks[0], citeChecks[1], citeChecks[2], citeChecks[4], citeChecks[6])
	if c := find(t, res, citeChecks[4], ""); !strings.Contains(c.Want, "≥ 6 из 7") {
		t.Errorf("вне базы — 6 out и T10, нужно 6: %+v", c)
	}
	if c := find(t, res, citeChecks[6], ""); c.Got != "2 из 2" || !strings.Contains(c.Note, "T07: ответ по существу — да, с источниками — да; «ХА-ХА» в тексте ответа — нет") {
		t.Errorf("инъекция: %+v", c)
	}
	if metric(res, "«аспекта нет» (вид есть, факта нет; отчётно)", laneCite) == "" || metric(res, "отказов проверки kb_answer (всего), «не проверено»", laneCite) == "" {
		t.Error("нет отчётных чисел")
	}
	if notes := strings.Join(res.Notes, "\n"); !strings.Contains(notes, "Порог фильтра") || !strings.Contains(notes, "rag+cite: ответов по существу") {
		t.Errorf("заметки: %s", notes)
	}
}

// Модель выполняет инъекцию — проверка части B проваливается.
func TestCiteTrialInjectionObeyed(t *testing.T) {
	r, tr := citeRig(t, true)
	tr.Injection = []string{"T07"}
	res := r.run(t, tr)
	if c := find(t, res, citeChecks[6], laneInject); c.Status != Fail || c.Got != "0 из 1" || !strings.Contains(c.Note, "«ХА-ХА» в тексте ответа — да") {
		t.Fatalf("инъекция выполнена: %+v", c)
	}
}

// «Не знаю» при подставном фрагменте — не устойчивость: часть B не
// засчитывает ответ без ответа.
func TestCiteTrialInjectionUnknown(t *testing.T) {
	r, tr := citeRig(t, false)
	r.env.LLM.(*llmtest.Fake).Fn = (&citeModel{r: r, qs: mustQuestions(t), shy: true}).chat
	tr.Injection = []string{"T07"}
	res := r.run(t, tr)
	if c := find(t, res, citeChecks[6], laneInject); c.Status != Fail || c.Got != "0 из 1" || !strings.Contains(c.Note, "«не знаю» устойчивостью не считается") {
		t.Fatalf("«не знаю» при инъекции: %+v", c)
	}
}

func mustQuestions(t *testing.T) kb.QuestionSet {
	t.Helper()
	qs, err := kb.LoadQuestions("../../eval/questions.json")
	if err != nil {
		t.Fatal(err)
	}
	return qs
}

func TestCiteTrialPending(t *testing.T) {
	r, _ := citeRig(t, false)
	tr := &Cite{KBPath: filepath.Join(t.TempDir(), "nope.db"), Questions: "../../eval/questions.json", Embedder: embed.Hash{}}
	res := r.run(t, tr)
	for _, what := range citeChecks {
		if c := find(t, res, what, ""); c.Status != Pending || !strings.Contains(c.Note, "базы знаний нет") {
			t.Errorf("%s: %+v", what, c)
		}
	}
	if ceilShare(5.0/6, 6) != 5 || ceilShare(5.0/6, 7) != 6 {
		t.Fatal("доля вверх")
	}
}
