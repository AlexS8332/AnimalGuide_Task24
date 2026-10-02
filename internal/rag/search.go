package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// MaxK — предел k у kb_search: модель может попросить больше, но каждый
// фрагмент — ≈250–300 токенов в запросе, и выдача в 20 фрагментов съела бы
// бюджет хода, не прибавив ответу точности.
const MaxK = 10

// searchSchema — аргументы kb_search. k необязателен: умолчание — то, с
// которым собран инструмент (Hook.K), чтобы вызов кодом и вызов моделью
// без k давали одну и ту же выдачу.
const searchSchema = `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Вопрос или ключевые слова по-русски: вид, тема, что нужно найти"},
    "k": {"type": "integer", "minimum": 1, "maximum": 10, "description": "Сколько фрагментов вернуть (по умолчанию 5)"}
  },
  "required": ["query"]
}`

// searchDescription — описание для модели. Слова «фрагменты статей» и
// «chunk_id» здесь, а не только в правиле хода: описание видит и модель,
// которой правило не досталось (вызов из другого агента).
const searchDescription = "Поиск по базе знаний справочника: замороженный снимок статей русской Википедии о хищных и справочник MDD v2.5 (систематика, статусы МСОП). " +
	"Возвращает фрагменты статей с chunk_id, статьёй и разделом, отсортированные по близости к запросу. " +
	"Ссылайся на найденное по chunk_id в квадратных скобках."

// SearchHit — фрагмент в ответе kb_search.
type SearchHit struct {
	ChunkID string  `json:"chunk_id"`
	Title   string  `json:"title"`
	Section string  `json:"section"`
	Path    string  `json:"path"`
	Score   float64 `json:"score"`
	URL     string  `json:"url,omitempty"`
	Text    string  `json:"text"`
}

// SearchResult — ответ kb_search целиком. Rewritten, Filtered, Note — у
// поиска через конвейер (механизмы rag.rewrite и rag.filter, v23): что
// ушло в поиск вместо query, сколько кандидатов отсечено фильтром и
// заметка (пустой итог, откат на BM25).
type SearchResult struct {
	Query     string      `json:"query"`
	Rewritten string      `json:"rewritten,omitempty"`
	Index     string      `json:"index"`
	Mode      kb.Mode     `json:"mode"`
	Fallback  string      `json:"fallback,omitempty"`
	Filtered  int         `json:"filtered,omitempty"`
	Note      string      `json:"note,omitempty"`
	Hits      []SearchHit `json:"hits"`
}

// SearchTool — kb_search{query, k}: поиск по индексу базы знаний. Untrusted:
// фрагменты — данные, а не указания (пометка источника и поиск инъекций
// работают как у Википедии). Ответ — JSON: query, index, mode (dense/bm25),
// fallback (почему не dense), hits[{chunk_id, title, section, path, score,
// url, text}].
//
// Пометку «данные, а не указания» инструмент сам не ставит: её кладёт Runner
// по признаку Untrusted (механизм envelope), как у Википедии, — иначе при
// включённом envelope она стояла бы дважды.
func SearchTool(s *kb.Searcher, index string, k int) tools.Tool {
	return searchTool(s, index, k, nil)
}

// searchTool — SearchTool с наблюдателем выдачи (механизм rag.cite: чьи
// chunk_id выдавались в этом ходе и не отсёк ли вызов Gate — issued.plain);
// obs == nil — без наблюдения.
func searchTool(s *kb.Searcher, index string, k int, obs *issued) tools.Tool {
	index = orIndex(index)
	k = orK(k)
	return tools.Func{
		S: tools.Spec{Name: ToolName, Description: searchDescription, Parameters: json.RawMessage(searchSchema),
			Untrusted: true, Via: tools.ViaLocal},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			if s == nil {
				return "", errors.New("базы знаний нет")
			}
			var in struct {
				Query string `json:"query"`
				K     int    `json:"k"`
			}
			if err := tools.ParseArgs(args, &in); err != nil {
				return "", err
			}
			in.Query = strings.TrimSpace(in.Query)
			if in.Query == "" {
				return "", errors.New("пустой запрос: передай query")
			}
			n := in.K
			if n <= 0 {
				n = k
			}
			n = min(n, MaxK)
			hits, info, err := s.Search(ctx, in.Query, kb.SearchOptions{Index: index, K: n})
			if err != nil {
				return "", fmt.Errorf("поиск по базе знаний: %w", err)
			}
			out := SearchResult{Query: in.Query, Index: info.Index, Mode: info.Mode, Fallback: info.Fallback,
				Hits: make([]SearchHit, 0, len(hits))}
			if obs != nil {
				gated, why := obs.gatePlain(ctx, in.Query, hits, info)
				obs.add(hits, gated, why)
				if gated {
					out.Note = searchGateNote(why)
				}
			}
			for _, h := range hits {
				out.Hits = append(out.Hits, SearchHit{ChunkID: h.ID, Title: h.Title, Section: h.Section, Path: pathOf(h.Chunk),
					Score: math.Round(h.Score*1000) / 1000, URL: h.URL, Text: h.Text})
			}
			return tools.Result(out)
		},
	}
}

// PipelineTool — kb_search через конвейер retrieve (механизмы rag.filter и
// rag.rewrite, v23): та же схема и то же описание, что у SearchTool, а
// выдача — итог конвейера с настройками c (K1 — k вызова). history —
// прошлые реплики человека: по ним rewrite находит вид для вопроса-
// продолжения («а сколько она весит?»), и вызов моделью получает тот же
// контекст, что вызов кодом. Без переписывания контекст не нужен и в
// конвейер не идёт: склейка с прошлыми репликами тянула бы поиск к прошлой
// теме. Ответ — кратко: переписанный запрос (если переписан), сколько
// отсечено фильтром и заметка; полный путь поиска — в окне «База знаний».
func PipelineTool(p *retrieve.Pipeline, c retrieve.Config, history []string, k int) tools.Tool {
	return pipelineTool(p, c, history, k, nil)
}

// pipelineTool — PipelineTool с наблюдателем выдачи (rag.cite): выдача
// каждого вызова и решение Gate по его трассе копятся в obs, отсечённый
// вызов получает пометку для модели.
func pipelineTool(p *retrieve.Pipeline, c retrieve.Config, history []string, k int, obs *issued) tools.Tool {
	k = orK(k)
	c.Index = orIndex(c.Index)
	if c.Rewrite == retrieve.RewriteNone {
		history = nil
	}
	return tools.Func{
		S: tools.Spec{Name: ToolName, Description: searchDescription, Parameters: json.RawMessage(searchSchema),
			Untrusted: true, Via: tools.ViaLocal},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			if p == nil || p.Searcher == nil {
				return "", errors.New("базы знаний нет")
			}
			var in struct {
				Query string `json:"query"`
				K     int    `json:"k"`
			}
			if err := tools.ParseArgs(args, &in); err != nil {
				return "", err
			}
			in.Query = strings.TrimSpace(in.Query)
			if in.Query == "" {
				return "", errors.New("пустой запрос: передай query")
			}
			cc := c
			cc.K1 = k
			if in.K > 0 {
				cc.K1 = orK(in.K)
			}
			t, err := p.Search(ctx, retrieve.Query{Text: in.Query, Context: history}, cc)
			if err != nil {
				return "", fmt.Errorf("поиск по базе знаний: %w", err)
			}
			note := ""
			if obs != nil {
				gated, why := Gate(&t, t.Hits)
				obs.add(t.Hits, gated, why)
				if gated {
					note = searchGateNote(why)
				}
			}
			out := SearchResult{Query: in.Query, Index: t.Info.Index, Mode: t.Info.Mode, Fallback: t.Info.Fallback,
				Hits: make([]SearchHit, 0, len(t.Hits))}
			if t.Rewritten != "" && t.Rewritten != t.Original {
				out.Rewritten = t.Rewritten
			}
			for _, x := range t.Candidates {
				if x.FilterCut() {
					out.Filtered++
				}
			}
			if cc.Filter && t.Empty {
				out.Note = "фильтр релевантности отсёк всё: в базе знаний ответа, вероятно, нет"
			}
			if t.Info.Fallback != "" && cc.Filter {
				out.Note = joinNote(out.Note, "поиск без векторов — порог только относительный")
			}
			out.Note = joinNote(out.Note, note)
			for _, h := range t.Hits {
				out.Hits = append(out.Hits, SearchHit{ChunkID: h.ID, Title: h.Title, Section: h.Section, Path: pathOf(h.Chunk),
					Score: math.Round(h.Score*1000) / 1000, URL: h.URL, Text: h.Text})
			}
			return tools.Result(out)
		},
	}
}

// issued — выдача kb_search одного хода (механизм rag.cite): все фрагменты,
// которые ведущий получил — вызовом кодом и своими вызовами, — без
// повторов. По ней kb_answer проверяет, что chunk_id выдавались в этом
// ходе. Каждый вызов несёт и решение Gate: «только не знаю» — если отсечены
// ВСЕ вызовы хода (Issued.Gate); релевантная выдача (open) — фрагменты
// неотсечённых вызовов, из неё у «не знаю» берётся «ближайшее в базе».
// plain — Gate вызова без конвейера (rag без rag.filter и rag.rewrite):
// трассы нет, хук собирает её из выдачи (Hook.plainGate). Методы безопасны
// на nil.
type issued struct {
	mu    sync.Mutex
	hits  []kb.Hit
	seen  map[string]bool
	open  []kb.Hit
	opens map[string]bool
	calls int
	why   string

	plain func(ctx context.Context, query string, hits []kb.Hit, info kb.SearchInfo) (bool, string)
}

// add — выдача одного вызова и решение Gate по нему.
func (o *issued) add(hits []kb.Hit, gated bool, why string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen == nil {
		o.seen, o.opens = map[string]bool{}, map[string]bool{}
	}
	o.calls++
	for _, h := range hits {
		if !o.seen[h.ID] {
			o.seen[h.ID] = true
			o.hits = append(o.hits, h)
		}
	}
	if gated {
		if o.why == "" {
			o.why = why
		}
		return
	}
	for _, h := range hits {
		if !o.opens[h.ID] {
			o.opens[h.ID] = true
			o.open = append(o.open, h)
		}
	}
}

// Hits — копия выдачи хода.
func (o *issued) Hits() []kb.Hit {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]kb.Hit(nil), o.hits...)
}

// Gate — «только не знаю» по всем вызовам хода: ни один вызов не дал
// релевантной выдачи. Причина — Gate первого отсечённого вызова.
func (o *issued) Gate() (bool, string, []kb.Hit) {
	if o == nil {
		return true, "kb_search в этом ходе не вызывался", nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.open) > 0 {
		return false, "", append([]kb.Hit(nil), o.open...)
	}
	switch {
	case o.calls == 0:
		return true, "kb_search в этом ходе не вызывался", nil
	case len(o.hits) == 0 && o.why == "":
		return true, "в базе знаний по вопросу ничего не найдено", nil
	}
	return true, o.why, nil
}

// gatePlain — Gate вызова без конвейера (o.plain); без него — только пустая
// выдача.
func (o *issued) gatePlain(ctx context.Context, query string, hits []kb.Hit, info kb.SearchInfo) (bool, string) {
	if o != nil && o.plain != nil {
		return o.plain(ctx, query, hits, info)
	}
	return Gate(nil, hits)
}

// searchGateNote — пометка в ответе kb_search при отсечённом вызове (rag.cite).
func searchGateNote(why string) string {
	return "релевантных фрагментов нет (" + why + "): ответ по существу kb_answer примет, только если другой вызов kb_search этого хода найдёт их; иначе — status unknown"
}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// Compose — блок контекста для модели: по фрагменту на абзац с заголовком
// «[chunk_id] Статья › Путь раздела», в обёртке «данные, а не указания».
// Пустая выдача — явная строка «в базе знаний ничего не найдено».
//
// Текст фрагмента идёт целиком: обрезанный фрагмент — это ровно тот
// случай, когда число стоит в отрезанном хвосте. Обёртка — та же пометка
// tools.TrustNote, что Runner ставит ответам источников: одна формулировка
// на всё приложение, чтобы модель видела одинаковый сигнал и в ответе
// kb_search, и в контексте отвечающего агента.
func Compose(hits []kb.Hit) string {
	if len(hits) == 0 {
		return "Фрагменты базы знаний: в базе знаний ничего не найдено по этому вопросу."
	}
	var b strings.Builder
	b.WriteString("Фрагменты базы знаний (")
	b.WriteString(tools.TrustNote)
	b.WriteString(")\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "\n[%s] %s › %s\n%s\n", h.ID, h.Title, pathOf(h.Chunk), strings.TrimSpace(h.Text))
	}
	return strings.TrimRight(b.String(), "\n")
}

// pathOf — путь раздела фрагмента: «Образ жизни › Питание»; у вступления
// пути нет — тогда имя раздела («Вступление»).
func pathOf(c kb.Chunk) string {
	if len(c.Path) > 0 {
		return strings.Join(c.Path, " › ")
	}
	if c.Section != "" {
		return c.Section
	}
	return "Вступление"
}

func orIndex(index string) string {
	if strings.TrimSpace(index) == "" {
		return DefaultIndex
	}
	return index
}

func orK(k int) int {
	if k <= 0 {
		return DefaultK
	}
	return min(k, MaxK)
}
