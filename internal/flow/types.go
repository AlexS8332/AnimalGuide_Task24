// Package flow — длинный флоу агента через несколько MCP-серверов: модель
// получает инструменты всех серверов реестра (hub), сама выбирает, какие
// вызывать и в каком порядке, а код после неё проверяет выбор, маршрут и
// порядок вызовов (ИП-1: результат проверяет код, а не слова модели).
//
// Агент — обычный agent.Runner: инструменты реестра плюс завершающий
// инструмент flow_done, который код принимает, только если блокнот закрыт.
// Каждый вызов пишется в трассу (Call) с сервером, который его обслужил.
//
// Проверка (Verify) не требует полного порядка — модель вправе переставлять
// независимые шаги. Порядок доказывается зависимостями данных: значение из
// ответа одного вызова должно прийти в аргументы более позднего (Flow), —
// а там, где данных нет, частичным порядком (Before). Сервер каждого вызова
// сверяется с маршрутом реестра и со счётчиками самих серверов (Evidence:
// server_info до и после).
package flow

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/hub"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

// FinishTool — завершающий инструмент флоу.
const FinishTool = "flow_done"

// Call — один вызов инструмента моделью. N — номер по порядку (с 1), Turn —
// номер ответа модели (вызовы одного ответа делят Turn). Server — куда
// вызов ушёл по маршруту реестра; пусто — инструмента нет у реестра
// (модель выдумала имя). Result — ответ инструмента (JSON), Error — текст
// ошибки. Summary — короткий итог для журнала и интерфейса.
type Call struct {
	N       int             `json:"n"`
	Turn    int             `json:"turn"`
	CallID  string          `json:"callId"`
	Server  string          `json:"server"`
	Tool    string          `json:"tool"`
	Args    json.RawMessage `json:"args"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
	OK      bool            `json:"ok"`
	Summary string          `json:"summary,omitempty"`
	Bytes   int             `json:"bytes"`
	Took    time.Duration   `json:"took"`
	// From — номера вызовов, из ответов которых пришли аргументы этого
	// (заполняет Verify по совпавшим Flow): для стрелок «← из №k».
	From []int `json:"from,omitempty"`
}

// Match — условие на аргумент: равенство или регулярное выражение (Go
// regexp; для нечувствительности к регистру — (?i)). Пустое — любое.
type Match struct {
	Eq     string `json:"eq,omitempty"`
	Regexp string `json:"regexp,omitempty"`
}

// StepSpec — обязательный шаг флоу: инструмент на сервере с условиями на
// аргументы. Min/Max — сколько успешных вызовов допустимо (0 у Max — без
// предела). AllowError — ответ-ошибка тоже засчитывается (например,
// facts_get: выпуска о виде может не быть — это законный ответ).
type StepSpec struct {
	ID         string           `json:"id"`
	Server     string           `json:"server"`
	Tool       string           `json:"tool"`
	Args       map[string]Match `json:"args,omitempty"`
	Min        int              `json:"min"`
	Max        int              `json:"max,omitempty"`
	AllowError bool             `json:"allowError,omitempty"`
}

// Режимы Flow.
const (
	FlowEq       = "eq"       // значения равны (строки после TrimSpace; числа — как числа)
	FlowEqFold   = "eq_fold"  // равны без учёта регистра
	FlowContains = "contains" // аргумент содержит значение (или значение — один из элементов массива)
)

// Flow — зависимость данных: значение по пути Path в ответе шага From
// должно прийти в аргумент Arg шага To. Path — через точку, индексы
// массивов числом, «*» — любой элемент («results.*.title»). Выполненная
// зависимость означает и порядок: вызов To позже вызова From.
type Flow struct {
	From string `json:"from"`
	Path string `json:"path"`
	To   string `json:"to"`
	Arg  string `json:"arg"`
	Mode string `json:"mode"`
}

// Order — частичный порядок без зависимости данных: все успешные вызовы
// шага A раньше первого успешного вызова шага B.
type Order struct {
	A string `json:"a"`
	B string `json:"b"`
}

// Spec — что проверяет Verify. Forbidden — инструменты, вызов которых —
// провал (шаблон с «*» в конце). MaxCalls — сверх него предупреждение.
// MinServers — сколько разных серверов должно обслужить успешные вызовы.
// CiteTool и CiteArg — где модель перечисляет источники (nb_add, cites):
// каждый названный инструмент должен быть успешно вызван раньше, а среди
// их серверов — все CiteServers.
type Spec struct {
	Steps       []StepSpec `json:"steps"`
	Flows       []Flow     `json:"flows"`
	Before      []Order    `json:"before"`
	Forbidden   []string   `json:"forbidden"`
	MaxCalls    int        `json:"maxCalls"`
	MinServers  int        `json:"minServers"`
	CiteTool    string     `json:"citeTool,omitempty"`
	CiteArg     string     `json:"citeArg,omitempty"`
	CiteServers []string   `json:"citeServers,omitempty"`
}

// Level — итог одной проверки.
type Level string

const (
	LevelOK   Level = "ok"
	LevelFail Level = "fail"
	LevelWarn Level = "warn"
)

// Check — одна проверка: имя («выбор: mdd_get», «маршрут», «данные:
// mdd_search → mdd_get.id», «порядок: nb_open → nb_add», «серверы
// подтвердили»…), итог и пояснение словами.
type Check struct {
	Name  string `json:"name"`
	Level Level  `json:"level"`
	Note  string `json:"note,omitempty"`
}

// Verdict — итог Verify. OK — нет ни одного fail. Match — какие вызовы (N)
// сопоставлены каждому шагу Spec.
type Verdict struct {
	OK     bool             `json:"ok"`
	Checks []Check          `json:"checks"`
	Match  map[string][]int `json:"match"`
}

// Evidence — счётчики серверов до и после флоу. nil — не снимались
// (проверка «серверы подтвердили» пропускается с предупреждением).
type Evidence struct {
	Before hub.Snapshot `json:"before"`
	After  hub.Snapshot `json:"after"`
}

// Preset — заготовка флоу: задача модели (Task, с %s на место вида) и
// проверки.
type Preset struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Task    string `json:"task"`
	Species string `json:"species"` // вид по умолчанию
	Spec    Spec   `json:"spec"`
}

// ServerDelta — сколько вызовов каждого инструмента насчитал сам сервер
// за флоу и сколько их в трассе.
type ServerDelta struct {
	Server string         `json:"server"`
	Served map[string]int `json:"served"` // по server_info
	Traced map[string]int `json:"traced"` // по трассе
	PID    int            `json:"pid,omitempty"`
}

// Trace — прогон целиком: для CLI (-json), REST и интерфейса.
type Trace struct {
	Preset  string        `json:"preset"`
	Species string        `json:"species"`
	Task    string        `json:"task"`
	Calls   []Call        `json:"calls"`
	Verdict Verdict       `json:"verdict"`
	Servers []ServerDelta `json:"servers"`
	Answer  string        `json:"answer"`         // итог модели из flow_done
	File    string        `json:"file,omitempty"` // путь из nb_close
	Preview string        `json:"preview,omitempty"`
	CostUSD float64       `json:"costUsd"`
	Usage   llm.Usage     `json:"usage"`
	Turns   int           `json:"turns"`
	Started time.Time     `json:"started"`
	Took    time.Duration `json:"took"`
	OK      bool          `json:"ok"`
	Error   string        `json:"error,omitempty"`
}

// Config — из чего собрать прогон. Runner — модель (LLM, Model,
// Temperature 0); Router — реестр серверов. MaxSteps — предел ответов
// модели, 0 — 24. Emitter — журнал агента (необязателен).
type Config struct {
	Runner   agent.Runner
	Router   hub.Router
	MaxSteps int
	Emitter  agent.Emitter
}

// ErrNoPreset — неизвестная заготовка.
var ErrNoPreset = errors.New("flow: нет такой заготовки")
