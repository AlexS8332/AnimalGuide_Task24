package rag

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
)

// scenario — подставная модель прогона контрольных вопросов.
//
// rag отвечает эталоном (Expect.Note) и честным «в базе знаний этого нет»
// на неотвечаемые. norag верно знает только T01; на остальные отвечаемые
// при noisy первый повтор — «не знаю», второй — неверное число (шум), без
// noisy — всегда неверное число; на неотвечаемые уверенно выдумывает.
// Судья судит правилом, кроме T02, где он строже (partial) — расхождение
// голосов. judged — что и в каком порядке видел судья.
type scenario struct {
	qs    kb.QuestionSet
	noisy bool
	fail  string // id вопроса, на котором norag падает

	mu     sync.Mutex
	calls  map[string]int
	judged []string
}

var judgeQ = regexp.MustCompile(`(?m)^Вопрос: (.*)$`)

func (s *scenario) chat(req llm.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	if isJudge(req) {
		m := judgeQ.FindStringSubmatch(lastUser(req))
		var q kb.Question
		for _, x := range s.qs.Questions {
			if m != nil && x.Q == m[1] {
				q = x
			}
		}
		text := judgedText(req)
		s.judged = append(s.judged, q.ID+": "+text)
		v := Rule(q, text).Verdict
		if q.ID == "T02" && v == Correct {
			return verdictJSON(Partial, "не сказано, что это наблюдения в неволе"), nil
		}
		return verdictJSON(v, "по эталону"), nil
	}
	q, ok := questionOf(s.qs, req)
	if !ok {
		return llm.Response{}, fmt.Errorf("неизвестный вопрос: %.80s", lastUser(req))
	}
	if req.Messages[0].Content == System(RAG) {
		if !q.Answerable {
			return llmtest.Text("В базе знаний этого нет."), nil
		}
		return llmtest.Text(q.Expect.Note), nil
	}
	if q.ID == s.fail {
		return llm.Response{}, errors.New("модель недоступна")
	}
	s.calls[q.ID]++
	switch {
	case !q.Answerable:
		return llmtest.Text("Примерно 10 кг."), nil
	case q.ID == "T01":
		return llmtest.Text(q.Expect.Note), nil
	case s.noisy && s.calls[q.ID] == 1:
		return llmtest.Text("Точно не знаю."), nil
	}
	return llmtest.Text("Примерно 100."), nil
}

func runEval(t *testing.T, sc *scenario, o EvalOptions) Report {
	t.Helper()
	sc.qs = questions(t)
	a := &Answerer{LLM: &llmtest.Fake{Fn: sc.chat}, Searcher: searcher(t)}
	rep, err := Eval(context.Background(), a, sc.qs, o)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestEvalNoise(t *testing.T) {
	sc := &scenario{noisy: true}
	judge := &Judge{LLM: &llmtest.Fake{Fn: sc.chat}}
	var progress []string
	rep := runEval(t, sc, EvalOptions{Repeats: 2, Judge: judge, Seed: 7, Progress: func(r Row) { progress = append(progress, r.Question.ID) }})

	if strings.Join(progress, ",") != "T01,T02,T03,T04,T11,T06,T07,T08,T09,T10" || len(rep.Rows) != 10 {
		t.Fatalf("строки по мере готовности: %v", progress)
	}
	ra, _ := rep.statOf(RAG)
	no, _ := rep.statOf(NoRAG)
	if ra.Questions != 10 || ra.Correct != 7 || ra.Partial != 1 || ra.Abstain != 2 || ra.RightAbstain != 2 ||
		ra.ConfidentWrong != 0 || ra.AnsweredUnanswerable != 0 || ra.Flips != 0 || ra.Judged != 20 ||
		ra.Discriminative != 8 || no.Discriminative != 8 || no.DiscriminativeCorrect != 1 {
		t.Fatalf("rag: %+v", ra)
	}
	// norag: «не знаю» и неверное число — ничья решается в худшую сторону.
	if no.Correct != 1 || no.Wrong != 9 || no.Abstain != 0 || no.ConfidentWrong != 7 || no.AnsweredUnanswerable != 2 || no.RightAbstain != 0 ||
		no.Flips != 7 || no.FlipRate != 0.7 || no.Recall != 0 {
		t.Fatalf("norag: %+v", no)
	}
	if ra.Agreement != 0.9 || no.Agreement != 1 || len(rep.Disagreements) != 2 ||
		!strings.HasPrefix(rep.Disagreements[0], "T02 rag #1: правило correct, судья partial") {
		t.Fatalf("согласие: %v %v %v", ra.Agreement, no.Agreement, rep.Disagreements)
	}
	if rep.Rows[1].Runs[RAG][0].Final != Partial || rep.Rows[1].Runs[RAG][0].Rule.Verdict != Correct {
		t.Fatal("итог прогона — судья")
	}
	if ra.Usage.Total != 20*15 || !ra.Cost.Known || !rep.JudgeCost.Known || rep.JudgeCost.USD <= 0 {
		t.Fatalf("расход: %+v %+v", ra.Usage, rep.JudgeCost)
	}
	// +6 не больше шума 7 — не засчитано.
	c := strings.Join(rep.Conclusion, "\n")
	for _, want := range []string{"rag 7 из 10, norag 1 из 10 — разница +6", "не больше шума", "rag 0, norag 7", "не засчитана",
		"Уверенных ошибок на отвечаемых вопросах (wrong): norag 7, rag 0",
		"Вопросы без ответа в базе (2): «не знаю» — norag 0, rag 2; ответ по существу — norag 2, rag 0",
		"Дискриминативные вопросы (без базы модель их не знает, 8): верно rag 7, norag 1 — разница +6",
		"Согласие правила и судьи: 95 % из 40 прогонов (расхождений 2)", "судья $"} {
		if !strings.Contains(c, want) {
			t.Errorf("вывод без %q:\n%s", want, c)
		}
	}
	if len(rep.Conclusion) != 7 {
		t.Fatalf("строк вывода %d", len(rep.Conclusion))
	}
	if rep.Model != llm.DefaultModel || rep.Index != DefaultIndex || rep.K != DefaultK || rep.Embedder != "hash-256" ||
		rep.CorpusSHA == "" || rep.Repeats != 2 {
		t.Fatalf("шапка: %+v", rep)
	}

	// Recall — то же правило покрытия, что в kb.Compare.
	st := searcher(t).Store
	hit, n := 0, 0
	for _, row := range rep.Rows {
		q := row.Question
		for _, r := range row.Runs[RAG] {
			want := false
			for _, e := range q.Evidence {
				d, _ := st.Doc(context.Background(), e.DocID)
				s, l := corpus.Find(d.Text(), e.Quote)
				for _, h := range r.Answer.Hits {
					want = want || (h.DocID == e.DocID && kb.Covers(h.Start, h.End, s, s+l) >= kb.EvidenceCover)
				}
			}
			if r.Recall != want {
				t.Errorf("%s: recall %v, ждали %v", q.ID, r.Recall, want)
			}
			if q.Answerable {
				n++
				if want {
					hit++
				}
			}
		}
		for _, r := range row.Runs[NoRAG] {
			if r.Recall || len(r.Answer.Hits) > 0 {
				t.Fatal("у norag нет выдачи")
			}
		}
	}
	if ra.Recall != float64(hit)/float64(n) || hit == 0 {
		t.Fatalf("recall %v, ждали %d из %d", ra.Recall, hit, n)
	}

	// Судья: ответы вопроса — вперемешку и по одному, порядок задаёт Seed.
	if len(sc.judged) != 40 {
		t.Fatalf("оценок %d", len(sc.judged))
	}
	natural := 0
	for i := 0; i < 40; i += 4 {
		// Естественный порядок: norag#1, rag#1, norag#2, rag#2.
		row := rep.Rows[i/4]
		want := []string{row.Runs[NoRAG][0].Answer.Text, row.Runs[RAG][0].Answer.Text, row.Runs[NoRAG][1].Answer.Text, row.Runs[RAG][1].Answer.Text}
		same := true
		for k := range want {
			same = same && strings.HasSuffix(sc.judged[i+k], ": "+want[k])
		}
		if same {
			natural++
		}
	}
	if natural == 10 {
		t.Fatal("судья видел ответы в естественном порядке")
	}
	again := &scenario{noisy: true}
	runEval(t, again, EvalOptions{Repeats: 2, Judge: &Judge{LLM: &llmtest.Fake{Fn: again.chat}}, Seed: 7})
	if strings.Join(again.judged, "\n") != strings.Join(sc.judged, "\n") {
		t.Fatal("тот же Seed — другой порядок")
	}

	md := rep.Markdown()
	for _, want := range []string{"# Ответ с базой знаний и без", "## Вывод", "## Сводка режимов", "| `rag` | 10 | 7 | 1 | 0 | 2 | 0 | 2 из 2 |",
		"| T01 | fact |", "✓ correct (correct, correct)", "✗ wrong (abstain, wrong)", "ожидалось: marbled-polecat › Интересные факты",
		"<details><summary>T08 — Где в России водится харза? → А сколько она весит?</summary>", "**norag #2** — **wrong**",
		"## Расхождения правила и судьи", "- T02 rag #1: правило correct, судья partial", "corpus_sha `" + rep.CorpusSHA + "`",
		"эмбеддер: hash-256", "> В базе знаний этого нет."} {
		if !strings.Contains(md, want) {
			t.Errorf("отчёт без %q", want)
		}
	}
}

// Без шума разница засчитана; один повтор — оговорка; без судьи — итог по
// правилу и согласие не определено.
func TestEvalConclusion(t *testing.T) {
	rep := runEval(t, &scenario{}, EvalOptions{Repeats: 2})
	c := strings.Join(rep.Conclusion, "\n")
	if !strings.Contains(c, "Разница +7 больше шума (смен вердикта между повторами: rag 0, norag 0) — засчитана в пользу rag") ||
		!strings.Contains(c, "Судьи не было") {
		t.Fatalf("вывод:\n%s", c)
	}
	ra, _ := rep.statOf(RAG)
	if ra.Judged != 0 || ra.Agreement != 0 || ra.Correct != 8 || rep.Disagreements == nil || len(rep.Disagreements) != 0 {
		t.Fatalf("без судьи: %+v %v", ra, rep.Disagreements)
	}
	if md := rep.Markdown(); !strings.Contains(md, "Судьи не было.") {
		t.Fatal("отчёт без судьи")
	}
	rep = runEval(t, &scenario{}, EvalOptions{Modes: []Mode{RAG, NoRAG}})
	if c := strings.Join(rep.Conclusion, "\n"); !strings.Contains(c, "Повтор один — без замера шума: разница +7") {
		t.Fatalf("один повтор:\n%s", c)
	}
	if rep.Stats[0].Mode != RAG {
		t.Fatal("порядок режимов")
	}
	// Один режим — сводка по нему, без разницы.
	rep = runEval(t, &scenario{}, EvalOptions{Modes: []Mode{NoRAG}, Splits: []string{kb.SplitDev}})
	if len(rep.Rows) != 19 || len(rep.Stats) != 1 || !strings.HasPrefix(rep.Conclusion[0], "norag: верно 0 из 19") {
		t.Fatalf("один режим: %d %v", len(rep.Rows), rep.Conclusion)
	}
}

// Ошибка ответа не обрывает прогон: прогон с ошибкой вне большинства.
func TestEvalAnswerError(t *testing.T) {
	rep := runEval(t, &scenario{fail: "T11"}, EvalOptions{Modes: []Mode{NoRAG}})
	row := rep.Rows[4]
	if row.Question.ID != "T11" || row.Runs[NoRAG][0].Error == "" || row.Majority[NoRAG] != "" {
		t.Fatalf("строка с ошибкой: %+v", row)
	}
	if st := rep.Stats[0]; st.Questions != 10 || st.Correct+st.Wrong+st.Partial+st.Abstain != 9 {
		t.Fatalf("сводка: %+v", st)
	}
	if !strings.Contains(rep.Markdown(), "> ошибка: модель (norag): модель недоступна") {
		t.Fatal("ошибка в отчёте")
	}
}

func TestEvalBadOptions(t *testing.T) {
	qs := questions(t)
	fake := &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) { return llmtest.Text("x"), nil }}
	if _, err := Eval(context.Background(), &Answerer{LLM: fake}, qs, EvalOptions{}); err == nil {
		t.Fatal("rag без базы")
	}
	if _, err := Eval(context.Background(), &Answerer{LLM: fake}, qs, EvalOptions{Modes: []Mode{NoRAG}, Splits: []string{"nope"}}); err == nil {
		t.Fatal("пустой набор")
	}
	if _, err := Eval(context.Background(), &Answerer{LLM: fake}, qs, EvalOptions{Modes: []Mode{"x"}}); err == nil {
		t.Fatal("неизвестный режим")
	}
	if _, err := Eval(context.Background(), nil, qs, EvalOptions{}); err == nil {
		t.Fatal("без агента")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Eval(ctx, &Answerer{LLM: fake}, qs, EvalOptions{Modes: []Mode{NoRAG}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена: %v", err)
	}
}

func TestMajority(t *testing.T) {
	ans := kb.Question{Answerable: true}
	un := kb.Question{}
	runs := func(vs ...Verdict) []Run {
		var out []Run
		for _, v := range vs {
			out = append(out, Run{Final: v})
		}
		return out
	}
	for _, c := range []struct {
		q     kb.Question
		runs  []Run
		want  Verdict
		flips int
	}{
		{ans, runs(Correct, Correct, Wrong), Correct, 1},
		{ans, runs(Correct, Partial), Partial, 1},
		{ans, runs(Partial, Correct, Partial), Partial, 2},
		{ans, runs(Abstain, Wrong), Wrong, 1},
		{un, runs(Abstain, Wrong), Wrong, 1},
		{un, runs(Abstain, Abstain), Abstain, 0},
		{ans, append(runs(Correct), Run{Error: "x"}, Run{Final: Correct}), Correct, 0},
		{ans, nil, "", 0},
	} {
		v, f := majority(c.q, c.runs)
		if v != c.want || f != c.flips {
			t.Errorf("%v: %s/%d, ждали %s/%d", c.runs, v, f, c.want, c.flips)
		}
	}
}

// Ranks — поиск без модели тем же запросом, что у отвечающего агента: ранг
// ≤ k ровно тогда, когда в выдаче k фрагментов есть доказательство.
func TestRanks(t *testing.T) {
	s, qs := searcher(t), questions(t)
	ctx := context.Background()
	rows, info, err := Ranks(ctx, s, qs, []string{kb.SplitDev, kb.SplitTest}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 27 || info.Mode != kb.Dense || info.Index != DefaultIndex {
		t.Fatalf("строк %d, поиск %+v", len(rows), info)
	}
	ev := &evidenceIndex{s: s, texts: map[string]string{}}
	byID := map[string]kb.Question{}
	for _, q := range qs.Questions {
		byID[q.ID] = q
	}
	for _, r := range rows {
		q := byID[r.ID]
		hits, _, err := s.Search(ctx, QuestionOf(q).Query(), kb.SearchOptions{Index: DefaultIndex, K: DefaultK})
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Rank > 0 && r.Rank <= DefaultK; got != ev.covered(ctx, q, hits) || r.Split != q.Split {
			t.Errorf("%s: ранг %d, в выдаче k=%d — %v", r.ID, r.Rank, DefaultK, !got)
		}
	}
	rec, hit := RecallAt(rows, DefaultK)
	if hit > len(rows) || rec != float64(hit)/float64(len(rows)) {
		t.Fatalf("recall %v %d", rec, hit)
	}
	if _, _, err := Ranks(ctx, nil, qs, []string{kb.SplitTest}, ""); err == nil {
		t.Fatal("без базы")
	}
}
