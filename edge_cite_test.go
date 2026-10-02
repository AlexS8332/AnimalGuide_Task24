//go:build edge

package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/rag"
)

// Заготовки режима rag+cite (v24) для окна «База знаний» и чата: ответ
// kb_answer с источниками и цитатами и итог проверки кодом — без модели.
// Фрагменты и цитаты — из настоящих чанков временной kb.db (корпус
// corpus/), так что клик по цитате находит её в тексте чанка.
//
//   - «Чем питается манул?» — ответ с двумя источниками и двумя цитатами,
//     проверка пройдена, судья смысла подтвердил утверждения;
//   - вопрос, у которого фильтр отсёк всё (фосса), — «не знаю» решил код
//     (Forced) с уточняющим вопросом;
//   - «Сколько весит манул?» — две попытки отклонены, ответ принят с
//     пометкой «не проверено»: вторая цитата не дословная, числа «6» в
//     цитатах нет;
//   - вопросы набора — ответ по первым двум фрагментам выдачи, T09 —
//     «не знаю» решила модель, T04 — «не проверено».

// edgeLine — предложение чанка со словом want (пусто — первое): строки
// заголовков «## …» пропускаются, предложение не выходит за строку, так что
// цитата — дословная подстрока текста чанка.
func edgeLine(text, want string) string {
	sentence := regexp.MustCompile(`[^.!?]+[.!?]`)
	var first string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "## ") {
			continue
		}
		for _, s := range sentence.FindAllString(line, -1) {
			s = strings.TrimSpace(s)
			if len([]rune(s)) < 20 {
				continue
			}
			if first == "" {
				first = s
			}
			if want == "" || strings.Contains(strings.ToLower(s), want) {
				return s
			}
		}
	}
	return first
}

// edgeChunk — чанк структурного индекса документа doc с разделом section.
func edgeChunk(ctx context.Context, s *kb.Searcher, doc, section string) (kb.Hit, error) {
	cs, err := s.Store.Chunks(ctx, rag.DefaultIndex, doc)
	if err != nil {
		return kb.Hit{}, err
	}
	for _, c := range cs {
		if c.Section == section {
			return kb.Hit{Chunk: c}, nil
		}
	}
	return kb.Hit{}, fmt.Errorf("нет чанка %s › %s", doc, section)
}

// edgeHits — чанки как выдача хода: ранги и правдоподобные баллы.
func edgeHits(ctx context.Context, s *kb.Searcher, doc string, sections ...string) ([]kb.Hit, error) {
	var out []kb.Hit
	for i, sec := range sections {
		h, err := edgeChunk(ctx, s, doc, sec)
		if err != nil {
			return nil, err
		}
		h.Rank, h.Score = i+1, 0.71-0.06*float64(i)
		out = append(out, h)
	}
	return out, nil
}

// edgeCitedText — текст ответа человеку в том виде, в каком его собирает
// rag.CitedResult.Text: ответ, «**Источники:** [n] Статья › Раздел
// (`chunk_id`)», «**Цитаты:**» — «> «…» [n]»; у unknown — «Не знаю: …» и
// «Уточните: …». По нему лента чата рисует чипы и плашку.
func edgeCitedText(c rag.Cited, hits []kb.Hit) string {
	if c.Status == rag.StatusUnknown {
		s := "Не знаю: " + c.Answer
		if c.Clarify != "" {
			s += "\n\nУточните: " + c.Clarify
		}
		return s
	}
	where := map[string]string{}
	for _, h := range hits {
		where[h.ID] = h.Title + " › " + h.Section
	}
	num := map[string]int{}
	var src []string
	for i, x := range c.Sources {
		num[x.ChunkID] = i + 1
		w := where[x.ChunkID]
		if w == "" {
			w = x.ChunkID
		}
		src = append(src, fmt.Sprintf("[%d] %s (`%s`)", i+1, w, x.ChunkID))
	}
	var b strings.Builder
	b.WriteString(c.Answer)
	b.WriteString("\n\n**Источники:** " + strings.Join(src, "; "))
	if len(c.Quotes) > 0 {
		b.WriteString("\n\n**Цитаты:**")
		for _, q := range c.Quotes {
			fmt.Fprintf(&b, "\n> «%s» [%d]", q.Text, num[q.ChunkID])
		}
	}
	return b.String()
}

// edgeAnswered — ответ по фрагментам: источники — все, цитаты — по
// предложению со словом из want (пусто — первое).
func edgeAnswered(answer string, hits []kb.Hit, want ...string) rag.Cited {
	c := rag.Cited{Status: rag.StatusAnswered, Answer: answer}
	for i, h := range hits {
		c.Sources = append(c.Sources, rag.CitedSource{ChunkID: h.ID})
		w := ""
		if i < len(want) {
			w = want[i]
		}
		c.Quotes = append(c.Quotes, rag.CitedQuote{ChunkID: h.ID, Text: edgeLine(h.Text, w)})
	}
	return c
}

func okCheck(c rag.Cited) rag.CiteCheck {
	return rag.CiteCheck{OK: true, HasSources: len(c.Sources) > 0, HasQuotes: len(c.Quotes) > 0, Verbatim: 1}
}

// edgeSupport — судья смысла: утверждение — предложение ответа; bad —
// номер неподтверждённого (−1 — все подтверждены).
func edgeSupport(c rag.Cited, bad int) *rag.Support {
	if c.Status == rag.StatusUnknown {
		return &rag.Support{OK: true}
	}
	sp := &rag.Support{OK: true, Usage: llm.Usage{Prompt: 380, Completion: 60, Total: 440}, Cost: llm.Cost{USD: 0.00014, Known: true}}
	refs := regexp.MustCompile(`\s*\[\d+\]`)
	for i, s := range regexp.MustCompile(`[^.!?]+[.!?]`).FindAllString(c.Answer, -1) {
		cl := rag.SupportClaim{Claim: strings.TrimSpace(refs.ReplaceAllString(s, "")), Supported: true, Quote: i % max(1, len(c.Quotes))}
		if i == bad {
			cl.Supported, cl.Quote, cl.Reason = false, -1, "в цитатах этого нет — модель добавила от себя"
			sp.OK = false
			sp.Unsupported++
		} else {
			sp.Supported++
		}
		sp.Claims = append(sp.Claims, cl)
	}
	return sp
}

// cite — ответ rag+cite заготовкой поверх ответа режима (выдача — уже в
// a.Hits). Судья смысла — только у своего вопроса о питании манула: в
// «Спросить» его нет, его добавляет прогон с судьёй (eval).
func (e *edgeRAG) cite(ctx context.Context, q rag.Question, known *kb.Question, a *rag.Answer) error {
	low := strings.ToLower(q.Text)
	var c rag.Cited
	var ch rag.CiteCheck
	switch {
	case strings.Contains(low, "манул") && (strings.Contains(low, "пита") || strings.Contains(low, "корм")):
		hits, err := edgeHits(ctx, e.s, "manul", "Охота и питание", "Поведение")
		if err != nil {
			return err
		}
		a.Hits, a.Trace = hits, nil
		c = edgeAnswered("Манул кормится почти исключительно мелкими грызунами и пищухами [1]. Активен он в основном в сумерках, а днём спит в укрытии [2].",
			hits, "грызун", "сумерк")
		ch = okCheck(c)
		a.Support = edgeSupport(c, -1)
	case strings.Contains(low, "весит манул") || strings.Contains(low, "масса манула"):
		hits, err := edgeHits(ctx, e.s, "manul", "Описание и внешний вид")
		if err != nil {
			return err
		}
		a.Hits, a.Trace = hits, nil
		c = edgeAnswered("Манул весит 2—5 кг, к зиме — до 6 кг; самцы заметно крупнее самок [1].", hits, "масса")
		c.Quotes = append(c.Quotes, rag.CitedQuote{ChunkID: hits[0].ID, Text: "Самцы заметно крупнее самок"})
		ch = rag.CiteCheck{OK: false, Unverified: true, HasSources: true, HasQuotes: true, NotVerbatim: []int{1}, Verbatim: 0.5,
			NumbersMissing: []string{"6"}, Rejects: 2,
			Problems: []string{"цитата 2 не найдена дословно во фрагменте " + hits[0].ID, "число 6 есть в ответе, но нет ни в одной цитате"}}
	case len(a.Hits) == 0:
		c = rag.Cited{Status: rag.StatusUnknown, Answer: "в базе знаний нет фрагментов об этом — фильтр релевантности отсёк всех кандидатов.",
			Clarify: "Какой вид из базы вас интересует — например, манул или харза? В базе статьи о хищных России и сопредельных стран."}
		ch = rag.CiteCheck{OK: true, Forced: true}
	case known != nil && known.ID == "T09":
		c = rag.Cited{Status: rag.StatusUnknown, Answer: "во фрагментах о камышовом коте нет продолжительности жизни.",
			Clarify: "Подойдёт ли то, что в базе есть о его размножении и взрослении?"}
		ch = rag.CiteCheck{OK: true}
	default:
		hits := a.Hits
		if len(hits) > 2 {
			hits = hits[:2]
		}
		answer := "По фрагментам базы: " + kbCutRunes(hitText(hits), 120) + " [1]."
		if known != nil && edgeRAGAnswers[known.ID] != [2]string{} {
			answer = strings.ReplaceAll(edgeRAGAnswers[known.ID][1], " %s", " [1]")
		}
		c = edgeAnswered(answer, hits)
		ch = okCheck(c)
		if known != nil && known.ID == "T04" && len(c.Quotes) > 1 {
			c.Quotes[1].Text = "погоня длится около полукилометра"
			ch = rag.CiteCheck{Unverified: true, HasSources: true, HasQuotes: true, NotVerbatim: []int{1}, Verbatim: 0.5,
				NumbersMissing: []string{"50"}, Rejects: 2, Problems: []string{"цитата 2 не дословная", "число 50 — не в цитатах"}}
		}
	}
	a.Cited = &rag.CitedResult{Cited: c, Check: ch, Hits: a.Hits}
	a.Text = edgeCitedText(c, a.Hits)
	return nil
}

// edgeCiteSupport — судья смысла прогона: у T02 одно утверждение не
// подтверждено цитатами.
func edgeCiteSupport(q kb.Question, a rag.Answer) *rag.Support {
	if a.Cited == nil {
		return nil
	}
	bad := -1
	if q.ID == "T02" {
		bad = 0
	}
	return edgeSupport(a.Cited.Cited, bad)
}

// edgeLeadCite — ответы ведущего чата с механизмом rag.cite: реплика →
// текст, как его собирает CitedResult.Text. Ксенофоб — разметка в
// заголовке, разделе и цитате должна остаться буквами.
func edgeLeadCite(t *testing.T, r *edgeRAG) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	var a rag.Answer
	if err := r.cite(ctx, rag.Question{Text: "Чем питается манул?"}, nil, &a); err != nil {
		t.Fatal(err)
	}
	out["питается манул"] = a.Text
	a = rag.Answer{}
	if err := r.cite(ctx, rag.Question{Text: "Сколько весит фосса?"}, nil, &a); err != nil {
		t.Fatal(err)
	}
	out["фосса"] = a.Text
	h, err := edgeChunk(ctx, r.s, "xss-test", "<i>Питание</i>")
	if err != nil {
		t.Fatal(err)
	}
	c := edgeAnswered("Ксенофоб ест теги и запивает их амперсандами [1].", []kb.Hit{h})
	c.Quotes[0].Text = `Ксенофоб ест теги <img src=x onerror="window.__xss=43"> и запивает их амперсандами &amp;.`
	out["ксенофоб"] = edgeCitedText(c, []kb.Hit{h})
	return out
}
