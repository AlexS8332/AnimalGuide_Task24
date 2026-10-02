package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/retrieve"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
)

// HookName — имя хука в журнале хода.
const HookName = "rag"

// leadRule — абзац системного промпта ведущего при механизме rag. Без него
// ведущий видит выдачу kb_search как свой первый шаг, но считает
// источниками только Википедию и GBIF и идёт туда по привычке: выдача
// оплачена токенами, а ответ строится мимо неё. Блоком правило быть не
// может: у механизма нет места в запросе (KindTool). Про вызов кодом
// сказано условно: правило получает и составитель подборки, а вызов кодом
// — только ведущий, и ход кнопкой его не делает. Правило — в каждом
// запросе ведущего (≈140 токенов), поэтому без повторов и без примеров
// сверх одного.
const leadRule = `База знаний (kb_search) — снимок статей справочника и MDD v2.5. Если сразу после реплики человека стоит выдача kb_search, это вызов кодом до твоего первого шага: опирайся на неё и называй фрагменты по chunk_id в квадратных скобках, например [manul/structure/004]. Если выдача не отвечает на вопрос — скажи, что в базе знаний этого нет, и при необходимости вызови kb_search сам другими словами или иди в Википедию или GBIF.`

// citeLeadRule — правило ведущего при механизме rag.cite (v24) вместо
// leadRule. Отличие от leadRule — запасного пути в Википедию и GBIF нет:
// ответ с источниками обещает человеку, что каждый факт стоит в цитате из
// базы, а статью Википедии kb_answer процитировать не даст (chunk_id
// только из выдачи kb_search). Условие «если среди твоих инструментов есть
// kb_answer» — потому что правило хода получает и составитель подборки, у
// которого завершающего инструмента нет. Правило постоянное (одно и то же
// каждый ход): решение кода «только не знаю» приходит схемой kb_answer и
// пометкой в выдаче, а не правкой системного промпта — иначе ход с
// «не знаю» обнулял бы кэш префикса окна.
const citeLeadRule = `База знаний (kb_search) — снимок статей справочника и MDD v2.5. Если сразу после реплики человека стоит выдача kb_search, это вызов кодом до твоего первого шага: опирайся на неё. Если среди твоих инструментов есть kb_answer — ход заканчивай только им, не текстом, и отвечай только по выдаче kb_search этого хода (Википедия и GBIF для ответа не годятся: их не процитировать):
- answer — кратко, без ссылок [chunk_id]; sources — chunk_id фрагментов из выдачи; quotes — куски текста фрагментов, скопированные ДОСЛОВНО (не короче 15 символов, пропуск — «…»); числа ответа — только из цитат;
- ответа в выдаче нет (можно сначала вызвать kb_search другими словами) — status unknown: в answer — чего нет в базе знаний, в clarify — уточняющий вопрос человеку;
- kb_answer вернул ошибку — исправь названное и вызови снова.`

// hintNoKB — что сделать, если базы нет.
const hintNoKB = "соберите базу: go run ./cmd/kb index -strategy all (путь к другой базе — флаг -kb или KB_DB)"

// Hook — механизм rag (features.RAG): ведущий получает kb_search, а код
// вызывает его до первого запроса с репликой человека (agents.Request.
// Preload), плюс абзац правил (Request.Rules): опирайся на найденные
// фрагменты и называй их [chunk_id]; если фрагменты не отвечают на
// вопрос — так и скажи и при необходимости иди в Википедию/GBIF.
//
// Базы нет (Searcher == nil) — механизм откатывается: ход идёт без
// kb_search, в журнале причина и подсказка, а в наборе механизмов хода rag
// выключен (как у trivia). Эмбеддер не отвечает — не откат механизма:
// поиск сам уходит в BM25 и называет причину в ответе инструмента.
//
// Голые названия («манул») и «сравни» идут мимо ведущего (agents.Classify) —
// база там не участвует; это предмет задания 25.
//
// Механизмы v23 rag.filter и rag.rewrite (требуют rag) переводят и вызов
// кодом, и сам инструмент kb_search на конвейер retrieve (PipelineTool):
// фильтр — ModeConfig(RAGFilter), переписывание — ModeConfig(RAGRewrite),
// оба — ModeConfig(RAGBoth). Контекст переписывания — прошлые реплики
// человека из окна хода (Request.Window).
//
// Механизм v24 rag.cite (требует rag): ведущий заканчивает ход только
// завершающим kb_answer (Request.Finish), выдача kb_search этого хода
// (вызов кодом и вызовы моделью) копится для проверки chunk_id и цитат, а
// «только не знаю» решает Gate по трассе вызова кодом (Hook.cite).
type Hook struct {
	Searcher *kb.Searcher
	Index    string // пусто — DefaultIndex
	K        int    // 0 — DefaultK
	Why      string // почему базы нет (для журнала)
	// Pipeline — конвейер механизмов rag.filter и rag.rewrite; nil — свой
	// поверх Searcher при первом ходе (словарь названий грузится один раз).
	Pipeline *retrieve.Pipeline

	mu sync.Mutex
}

// contextTurns — сколько прошлых реплик человека получает rewrite.
const contextTurns = 3

// pipeline — конвейер хука (ленивый).
func (h *Hook) pipeline() *retrieve.Pipeline {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Pipeline == nil {
		h.Pipeline = &retrieve.Pipeline{Searcher: h.Searcher}
	}
	return h.Pipeline
}

// mechanisms — механизмы v23 хода и настройки конвейера по ним; пусто —
// прямой поиск.
func mechanisms(t *runs.Turn) ([]string, retrieve.Config, bool) {
	filter, rewrite := t.Features.On(features.RAGFilter), t.Features.On(features.RAGRewrite)
	var names []string
	var c retrieve.Config
	switch {
	case filter && rewrite:
		names, c = []string{string(features.RAGFilter), string(features.RAGRewrite)}, ModeConfig(RAGBoth)
	case filter:
		names, c = []string{string(features.RAGFilter)}, ModeConfig(RAGFilter)
	case rewrite:
		names, c = []string{string(features.RAGRewrite)}, ModeConfig(RAGRewrite)
	default:
		return nil, c, false
	}
	return names, c, true
}

// humanTurns — прошлые реплики человека из окна хода (последние
// contextTurns), без текущей реплики.
func humanTurns(t *runs.Turn) []string {
	var out []string
	cur := strings.TrimSpace(t.Request.Text)
	for _, m := range t.Request.Window {
		if m.Role != llm.RoleUser {
			continue
		}
		if s := strings.TrimSpace(m.Content); s != "" {
			out = append(out, s)
		}
	}
	if n := len(out); n > 0 && out[n-1] == cur {
		out = out[:n-1]
	}
	if len(out) > contextTurns {
		out = out[len(out)-contextTurns:]
	}
	return out
}

func (h *Hook) Name() string { return HookName }

// Before — kb_search в инструменты хода, его вызов кодом с репликой и
// правило ведущему. Ход без реплики (кнопки карточки: раздел, узел дерева)
// получает инструмент и правило, но не вызов: искать нечем, а пустой
// запрос — это ошибка инструмента в журнале на каждом нажатии.
func (h *Hook) Before(ctx context.Context, t *runs.Turn) error {
	if !t.Features.On(features.RAG) {
		return nil
	}
	if h.Searcher == nil {
		h.off(t)
		return nil
	}
	index, k := orIndex(h.Index), orK(h.K)
	names, cfg, piped := mechanisms(t)
	// rag.cite: выдача хода копится (kb_answer проверяет по ней chunk_id и
	// цитаты), а правило ведущего — без запасного пути в Википедию.
	cite := t.Features.On(features.RAGCite)
	var obs *issued
	rule := leadRule
	if cite {
		obs, rule = &issued{}, citeLeadRule
	}
	via, detail := "", ""
	var history []string
	if piped {
		cfg.Index = index
		history = humanTurns(t)
		t.Request.Tools = append(t.Request.Tools, pipelineTool(h.pipeline(), cfg, history, k, obs))
		via = "; " + strings.Join(names, ", ")
		detail = "\nВторой этап поиска (" + strings.Join(names, ", ") + "): " + cfg.Describe() +
			". Переписанный запрос идёт только в поиск, ведущий видит исходную реплику; в ответе kb_search — переписанный запрос и сколько фрагментов отсёк фильтр."
	} else {
		t.Request.Tools = append(t.Request.Tools, searchTool(h.Searcher, index, k, obs))
	}
	t.Request.Rules = join(t.Request.Rules, rule)
	text := strings.TrimSpace(t.Request.Text)
	if text == "" {
		t.Em.Log(agent.Event{Agent: HookName, Kind: agent.EventMechanism, Mechanism: string(features.RAG),
			Title:  fmt.Sprintf("база знаний: kb_search выдан ведущему без вызова кодом — у хода нет реплики (индекс %s, k %d%s)", index, k, via),
			Detail: "Ход начат кнопкой, а не репликой: искать по базе нечем. Инструмент и правило у ведущего есть — вызвать kb_search он может сам." + detail})
		return nil
	}
	args, _ := json.Marshal(struct {
		Query string `json:"query"`
		K     int    `json:"k"`
	}{text, k})
	t.Request.Preload = append(t.Request.Preload, agent.Preload{Tool: ToolName, Args: string(args)})
	t.Em.Log(agent.Event{Agent: HookName, Kind: agent.EventMechanism, Mechanism: string(features.RAG), Tool: ToolName,
		Title: fmt.Sprintf("база знаний: заказан вызов kb_search кодом до первого запроса ведущего (индекс %s, k %d%s)", index, k, via),
		Detail: "Код поищет по базе знаний с репликой человека до первого запроса ведущего: выдача встанет после реплики, " +
			"а не блоком перед историей, и кэш префикса окна не сбрасывается. Ведущему выдано правило: опираться на выдачу " +
			"и называть [chunk_id]; нет ответа в выдаче — сказать об этом и при необходимости идти в Википедию или GBIF.\n" +
			"Оговорка: голое название животного («манул») и «сравни …» идут мимо ведущего (карточка, сравнение) — " +
			"запроса ведущего тогда нет, и вызова kb_search не будет. Сам вызов — отдельное событие журнала инструментов." + detail})
	if cite {
		h.cite(ctx, t, obs, piped, cfg, history, k, text)
	}
	return nil
}

// cite — механизм rag.cite: ведущий заканчивает ход только kb_answer
// (Request.Finish), проверка — по выдаче kb_search этого хода (obs). Gate
// — по трассе вызова кодом: хук считает её сам до хода (тот же запрос,
// что в Preload; инструмент потом отдаёт её, не ища второй раз), и если
// фильтр отсёк всё — схема kb_answer на этот ход разрешает только unknown,
// а к выдаче kb_search добавлена пометка. Без фильтра (rag без rag.filter и
// rag.rewrite) трассы нет: «только unknown» — по пустой выдаче в момент
// вызова kb_answer (Finisher). Принятый ответ — в итогах хода (Extras,
// ключ rag.cite) для окна.
func (h *Hook) cite(ctx context.Context, t *runs.Turn, obs *issued, piped bool, cfg retrieve.Config, history []string, k int, text string) {
	gated, why := false, ""
	gateLine := "трассы нет (без rag.filter и rag.rewrite) — «только не знаю» по пустой выдаче"
	if piped {
		cc := cfg
		cc.K1 = k
		if cc.Rewrite == retrieve.RewriteNone {
			history = nil
		}
		tr, err := h.pipeline().Search(ctx, retrieve.Query{Text: text, Context: history}, cc)
		switch {
		case err != nil:
			gateLine = "поиск до хода не удался (" + err.Error() + ") — «только не знаю» по пустой выдаче"
		default:
			obs.pre, obs.preQuery, obs.preK = &tr, text, cc.K1
			gated, why = Gate(&tr, tr.Hits)
			gateLine = "контекст есть — ответ по существу разрешён"
			if gated {
				obs.preNote = "kb_answer на этот ход примет только status unknown с уточняющим вопросом: " + why
				gateLine = "«только не знаю»: " + why
			}
		}
	}
	fin := Finisher(obs.Hits, gated, why)
	inner := fin.Handle
	fin.Handle = func(ctx context.Context, callID string, args json.RawMessage) (any, error) {
		res, err := inner(ctx, callID, args)
		if r, ok := res.(*CitedResult); ok && err == nil {
			t.Extra(string(features.RAGCite), ViewOf(r, gated, why))
		}
		return res, err
	}
	t.Request.Finish = append(t.Request.Finish, fin)
	t.Em.Log(agent.Event{Agent: HookName, Kind: agent.EventMechanism, Mechanism: string(features.RAGCite), Tool: FinishName,
		Title: "ответ с источниками: ведущий заканчивает ход только kb_answer; " + gateLine,
		Detail: "Код проверит ответ до человека: chunk_id — из выдачи kb_search этого хода, у ответа есть источник и цитата, " +
			"каждая цитата — дословный кусок своего фрагмента, числа ответа стоят в цитатах. Не прошло — отказ с подсказкой и ещё " +
			fmt.Sprintf("один запрос ведущего; после %d отказов ответ принимается с пометкой «не проверено».", MaxRejects)})
}

// CiteView — ответ kb_answer в итогах хода (Extras, ключ rag.cite): для
// окна — чипы источников «[1] Манул › Питание», цитаты, «не знаю» и
// уточнение, итог проверки кодом.
type CiteView struct {
	Text       string        `json:"text"`
	Cited      Cited         `json:"cited"`
	Check      CiteCheck     `json:"check"`
	Gated      bool          `json:"gated,omitempty"`
	GateReason string        `json:"gate_reason,omitempty"`
	Sources    []CiteViewSrc `json:"sources,omitempty"`
}

// CiteViewSrc — источник ответа с номером и заголовком из выдачи.
type CiteViewSrc struct {
	N       int    `json:"n"`
	ChunkID string `json:"chunk_id"`
	Title   string `json:"title,omitempty"`
	Path    string `json:"path,omitempty"`
	Issued  bool   `json:"issued"` // выдавался в этом ходе
}

// ViewOf — CiteView принятого ответа.
func ViewOf(r *CitedResult, gated bool, why string) CiteView {
	v := CiteView{Text: r.Text(), Cited: r.Cited, Check: r.Check, Gated: gated, GateReason: why}
	if r.Cited.Unknown() {
		return v
	}
	byID := map[string]kb.Hit{}
	for _, h := range r.Hits {
		byID[h.ID] = h
	}
	seen := map[string]bool{}
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		s := CiteViewSrc{N: len(v.Sources) + 1, ChunkID: id}
		if h, ok := byID[id]; ok {
			s.Title, s.Path, s.Issued = h.Title, pathOf(h.Chunk), true
		}
		v.Sources = append(v.Sources, s)
	}
	for _, s := range r.Cited.Sources {
		add(s.ChunkID)
	}
	for _, q := range r.Cited.Quotes {
		add(q.ChunkID)
	}
	return v
}

// off — базы нет: ход идёт без kb_search, в журнале причина и подсказка, в
// итоговом наборе хода механизм выключен.
func (h *Hook) off(t *runs.Turn) {
	t.Request.Features = t.Request.Features.With(features.RAG, false)
	if t.Request.Features.On(features.RAGCite) {
		// rag.cite требует rag: без базы цитировать нечего, ведущий отвечает
		// текстом.
		t.Request.Features = t.Request.Features.With(features.RAGCite, false)
	}
	why := strings.TrimSpace(h.Why)
	if why == "" {
		why = "базы знаний нет"
	}
	t.Em.Log(agent.Event{Agent: HookName, Kind: agent.EventMechanism, Mechanism: string(features.RAG),
		Title:  "база знаний: " + why + " — ход идёт без kb_search",
		Detail: "Ход идёт без kb_search: ведущий отвечает по источникам хода (Википедия, GBIF).\nЧто сделать: " + hintNoKB + "."})
}

// After — ничего: ответ rag.cite проверяет kb_answer во время хода, а
// принятый результат пишет в Extras сам Finisher (Hook.cite).
func (h *Hook) After(ctx context.Context, t *runs.Turn) error { return nil }

func join(a, b string) string {
	switch {
	case strings.TrimSpace(a) == "":
		return b
	case strings.TrimSpace(b) == "":
		return a
	}
	return a + "\n\n" + b
}

var _ runs.Hook = (*Hook)(nil)
