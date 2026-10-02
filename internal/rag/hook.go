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
	via, detail := "", ""
	if piped {
		cfg.Index = index
		t.Request.Tools = append(t.Request.Tools, PipelineTool(h.pipeline(), cfg, humanTurns(t), k))
		via = "; " + strings.Join(names, ", ")
		detail = "\nВторой этап поиска (" + strings.Join(names, ", ") + "): " + cfg.Describe() +
			". Переписанный запрос идёт только в поиск, ведущий видит исходную реплику; в ответе kb_search — переписанный запрос и сколько фрагментов отсёк фильтр."
	} else {
		t.Request.Tools = append(t.Request.Tools, SearchTool(h.Searcher, index, k))
	}
	t.Request.Rules = join(t.Request.Rules, leadRule)
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
	return nil
}

// off — базы нет: ход идёт без kb_search, в журнале причина и подсказка, в
// итоговом наборе хода механизм выключен.
func (h *Hook) off(t *runs.Turn) {
	t.Request.Features = t.Request.Features.With(features.RAG, false)
	why := strings.TrimSpace(h.Why)
	if why == "" {
		why = "базы знаний нет"
	}
	t.Em.Log(agent.Event{Agent: HookName, Kind: agent.EventMechanism, Mechanism: string(features.RAG),
		Title:  "база знаний: " + why + " — ход идёт без kb_search",
		Detail: "Ход идёт без kb_search: ведущий отвечает по источникам хода (Википедия, GBIF).\nЧто сделать: " + hintNoKB + "."})
}

// After — ничего: выдача не проверяется и не пишется ходом (цитаты — v24).
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
