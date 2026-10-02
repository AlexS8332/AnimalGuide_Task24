package bench

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
)

// ragKB — база structure на настоящем корпусе и hash-эмбеддере.
func ragKB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	docs, m, err := corpus.Load("../../corpus")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kb.db")
	st, err := kb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.PutCorpus(ctx, docs, m); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Build(ctx, kb.NewStructure(0, 0), embed.Hash{}, nil); err != nil {
		t.Fatal(err)
	}
	return path
}

// ragModel — отвечающий агент и судья И-10 на подставной модели поверх
// модели стенда (ведущий части B идёт через неё же). answer решает, что
// ответить на вопрос в режиме и повторе (n — какой по счёту вызов этого
// режима на этот вопрос); судья судит правилом.
type ragModel struct {
	r      *rig
	qs     kb.QuestionSet
	answer func(q kb.Question, mode rag.Mode, n int) string

	mu    sync.Mutex
	calls map[string]int
}

func (m *ragModel) chat(req llm.Request) (llm.Response, error) {
	sys := req.Messages[0].Content
	user := req.Messages[len(req.Messages)-1].Content
	switch sys {
	case rag.JudgeSystem():
		for _, q := range m.qs.Questions {
			if strings.Contains(user, "Вопрос: "+q.Q+"\n") {
				_, text, _ := strings.Cut(user, "<<<\n")
				text = strings.TrimSuffix(text, "\n>>>")
				return llmtest.Text(`{"verdict":"` + string(rag.Rule(q, text).Verdict) + `","reason":"по эталону"}`), nil
			}
		}
	case rag.System(rag.NoRAG), rag.System(rag.RAG):
		mode := rag.NoRAG
		if sys == rag.System(rag.RAG) {
			mode = rag.RAG
		}
		// Вопрос — после «Вопрос: », если перед ним реплики, и до
		// фрагментов (после пустой строки).
		if _, after, ok := strings.Cut(user, "\n\nВопрос: "); ok && strings.HasPrefix(user, "Предыдущие реплики пользователя:") {
			user = after
		}
		text, _, _ := strings.Cut(user, "\n\n")
		for _, q := range m.qs.Questions {
			if q.Q == text {
				m.mu.Lock()
				if m.calls == nil {
					m.calls = map[string]int{}
				}
				m.calls[q.ID+string(mode)]++
				n := m.calls[q.ID+string(mode)]
				m.mu.Unlock()
				return llmtest.Text(m.answer(q, mode, n)), nil
			}
		}
	default:
		return m.r.chat(req)
	}
	return llm.Response{}, nil
}

// honest — rag отвечает эталоном и честно не знает того, чего в базе нет;
// norag не знает ничего и выдумывает на неотвечаемых.
func honest(q kb.Question, mode rag.Mode, _ int) string {
	switch {
	case mode == rag.RAG && q.Answerable:
		return q.Expect.Note
	case mode == rag.RAG:
		return "В базе знаний этого нет."
	case q.Answerable:
		return "Не знаю."
	}
	return "Примерно 10 кг."
}

func ragRig(t *testing.T, answer func(kb.Question, rag.Mode, int) string) (*rig, *RAG) {
	t.Helper()
	r := newRig(t)
	qs, err := kb.LoadQuestions("../../eval/questions.json")
	if err != nil {
		t.Fatal(err)
	}
	m := &ragModel{r: r, qs: qs, answer: answer}
	r.env.LLM = &llmtest.Fake{Fn: m.chat}
	return r, &RAG{KBPath: ragKB(t), Questions: "../../eval/questions.json", Embedder: embed.Hash{}}
}

func TestRAGTrial(t *testing.T) {
	r, tr := ragRig(t, honest)
	res := r.run(t, tr)
	if res.Mechanism != features.RAG || len(res.Lanes) != 4 || res.Lanes[3].Diff != "−rag" || res.Lanes[2].Name != laneChatRAG {
		t.Fatalf("дорожки: %s %+v", res.Mechanism, res.Lanes)
	}
	gain := find(t, res, "rag верен чаще norag (большинство повторов, test)", laneAnsRAG)
	if gain.Status != Pass || !strings.Contains(gain.Got, "rag 8, norag 0 из 10 (+8)") {
		t.Fatalf("разница: %+v", gain)
	}
	if c := find(t, res, "уверенных ошибок у rag на отвечаемых", ""); c.Status != Pass || c.Got != "0" {
		t.Fatalf("уверенные ошибки: %+v", c)
	}
	if c := find(t, res, "«не знаю» у rag на вопросах без ответа в базе", ""); c.Status != Pass || c.Got != "2 из 2" || c.Want != "2 из 2" {
		t.Fatalf("«не знаю»: %+v", c)
	}
	// Recall — поиском без модели на dev+test (hash-поиск); проверка
	// сходится со своим числом. Recall по выдаче отвечающего агента на test
	// — отчётный, с разбором промахов в заметке.
	rc := find(t, res, "доказательство в топ-5 поиска (dev+test, без модели)", "")
	if !strings.Contains(rc.Got, "из 27 вопросов; индекс structure, k = 5, поиск dense") || (rc.Status == Pass) != (rc.Got >= "0.80") ||
		(rc.Status == Fail) != strings.HasPrefix(rc.Note, "мимо топа: ") {
		t.Fatalf("recall: %+v", rc)
	}
	if m := metric(res, "доказательство в топ-k поиска без модели (dev+test)", laneAnsRAG); !strings.HasPrefix(rc.Got, strings.Fields(m)[0]) {
		t.Fatalf("recall без модели: %q / %q", m, rc.Got)
	}
	if metric(res, "доказательство в выдаче отвечающего агента (test, отчётно)", "rag") == "" ||
		!strings.Contains(strings.Join(res.Notes, "\n"), "Промахи выдачи rag на test (доказательства нет в топ-5, ранг первого релевантного в топ-20): ") {
		t.Fatalf("recall отвечающего агента: %+v / %v", res.Metrics, res.Notes)
	}
	if metric(res, "верно / частично / неверно / «не знаю» (большинство)", "rag") != "8 / 0 / 0 / 2 из 10" ||
		metric(res, "уверенных ошибок на отвечаемых", "norag") != "0" ||
		metric(res, "ответ по существу на вопросах без ответа в базе", "norag") != "2 из 2" ||
		metric(res, "верно на дискриминативных", "rag") == "" || metric(res, "смен вердикта между повторами (шум)", "rag") != "0 (0 % переходов)" ||
		metric(res, "согласие правила и судьи", "rag") != "100 % из 20" || !strings.HasPrefix(metric(res, "цена судьи", "судья"), "$") {
		t.Fatalf("метрики: %+v", res.Metrics)
	}
	// Часть B: вызов kb_search кодом — только на дорожке с rag.
	if metric(res, "kb_search кодом до первого запроса (по журналу)", laneChatRAG) != "4 из 4 ходов" ||
		metric(res, "kb_search кодом до первого запроса (по журналу)", laneChatNoRAG) != "0 из 4 ходов" ||
		metric(res, "запросов к модели на ход", laneChatRAG) == "" || metric(res, "доля кэша", laneChatNoRAG) == "" {
		t.Fatalf("чат: %+v", res.Metrics)
	}
	if len(res.Stats) != 2 || res.Stats[0].Turns != 4 || res.Stats[0].Brak != 0 {
		t.Fatalf("цена дорожек чата: %+v", res.Stats)
	}
	// Пары ответов части A (3 вопроса × 2 режима), затем чат.
	if len(res.Samples) != 10 || res.Samples[0].Lane != "norag" || res.Samples[1].Lane != "rag" ||
		res.Samples[0].Topic != res.Samples[1].Topic || res.Samples[6].Lane != laneChatRAG {
		t.Fatalf("ответы: %+v", res.Samples)
	}
	if len(res.Notes) < 3 || !strings.Contains(strings.Join(res.Notes, "\n"), "Верных ответов") {
		t.Fatalf("заметки: %v", res.Notes)
	}
}

// Провалы видны: база не помогает, rag выдумывает на неотвечаемых.
func TestRAGTrialFails(t *testing.T) {
	r, tr := ragRig(t, func(q kb.Question, mode rag.Mode, n int) string {
		if q.Answerable {
			return "Не знаю."
		}
		return "Примерно 10 кг."
	})
	tr.Dialog = []string{}
	res := r.run(t, tr)
	if c := find(t, res, "rag верен чаще norag (большинство повторов, test)", ""); c.Status != Fail {
		t.Fatalf("разница: %+v", c)
	}
	// Ответ по существу на неотвечаемых — не «уверенная ошибка на
	// отвечаемых», а провал проверки «не знаю» (нужно 2 из 2).
	if c := find(t, res, "уверенных ошибок у rag на отвечаемых", ""); c.Status != Pass {
		t.Fatalf("уверенные ошибки: %+v", c)
	}
	if c := find(t, res, "«не знаю» у rag на вопросах без ответа в базе", ""); c.Status != Fail || c.Note != "T09 wrong, T10 wrong" {
		t.Fatalf("«не знаю»: %+v", c)
	}
	if res.Verdict() != Fail || len(res.Lanes) != 2 || len(res.Stats) != 0 {
		t.Fatalf("без части B: %s %+v", res.Verdict(), res.Lanes)
	}
}

// Разница в пределах шума — «не определено», а не «принято».
func TestRAGTrialNoise(t *testing.T) {
	r, tr := ragRig(t, func(q kb.Question, mode rag.Mode, n int) string {
		if mode == rag.NoRAG && q.Answerable && n == 1 {
			return q.Expect.Note
		}
		return honest(q, mode, n)
	})
	tr.Dialog = []string{}
	res := r.run(t, tr)
	c := find(t, res, "rag верен чаще norag (большинство повторов, test)", "")
	if c.Status != Pending || !strings.Contains(c.Got, "rag 0, norag 8") || !strings.Contains(c.Note, "шума") {
		t.Fatalf("шум: %+v", c)
	}
}

// Базы нет — проверки «не определено» с подсказкой, стенд не сломан.
func TestRAGTrialNoKB(t *testing.T) {
	r, tr := ragRig(t, honest)
	tr.KBPath = filepath.Join(t.TempDir(), "nope.db")
	res := r.run(t, tr)
	if res.Count(Pending) != 4 || res.Count(Fail) != 0 || !strings.Contains(res.Checks[0].Note, "go run ./cmd/kb index") {
		t.Fatalf("без базы: %+v", res.Checks)
	}
	r.env.LLM = nil
	if res := RunOne(context.Background(), r.env, tr); !strings.Contains(res.Err, "Env.LLM") {
		t.Fatalf("без модели: %q", res.Err)
	}
}
