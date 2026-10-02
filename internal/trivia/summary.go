package trivia

import (
	"context"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

// Контракт сводки. Сводка — агрегат по выпускам за период плюс короткий
// текст модели. Агрегат считает код (из IssueStore, PickStore и журнала
// запусков), модель только пересказывает его: цифры сводки не зависят от
// модели и проверяются тестами.

// RunInfo — запуск планировщика глазами сводки. Отдельный тип, а не
// schedule.Run: trivia не зависит от планировщика.
type RunInfo struct {
	Job     string    `json:"job"`
	Status  string    `json:"status"` // ok | failed | budget | skipped
	Started time.Time `json:"started"`
	CostUSD float64   `json:"cost_usd"`
	Ref     string    `json:"ref,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// RunSource — журнал запусков за период [from, to). Реализацию-обёртку над
// schedule.RunStore делает демон.
type RunSource interface {
	RunsBetween(ctx context.Context, from, to time.Time) ([]RunInfo, error)
}

// Count — строка разбивки: ключ и число.
type Count struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// SpeciesLine — вид в сводке.
type SpeciesLine struct {
	IssueID   int64  `json:"issue_id"`
	SpeciesID int    `json:"species_id"`
	SciName   string `json:"sci_name"`
	NameRu    string `json:"name_ru,omitempty"`
	IUCN      string `json:"iucn,omitempty"`
	Order     string `json:"order,omitempty"`
	Status    string `json:"status"`
	Title     string `json:"title,omitempty"`
	Facts     int    `json:"facts"`
	// Highlight — первый подтверждённый факт: из него модель берёт
	// «самое интересное за сутки», не выдумывая.
	Highlight  string   `json:"highlight,omitempty"`
	Recent     int      `json:"recent_observations"` // наблюдения GBIF за окно
	OutOfRange []string `json:"out_of_range,omitempty"`
}

// Aggregate — агрегат за период. Все разбивки — по убыванию Count, затем
// по Key.
type Aggregate struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	Issues   int           `json:"issues"`    // всего выпусков за период
	ByStatus []Count       `json:"by_status"` // ok / thin / failed
	Species  []SpeciesLine `json:"species"`   // по времени выпуска
	ByOrder  []Count       `json:"by_order"`
	ByIUCN   []Count       `json:"by_iucn"`
	ByRealm  []Count       `json:"by_realm"` // вид с двумя областями считается в обеих

	Facts        int     `json:"facts"`         // подтверждённых
	Dropped      int     `json:"dropped"`       // отброшенных (кодом и проверяющим)
	DroppedShare float64 `json:"dropped_share"` // Dropped / (Facts+Dropped); 0 при пустом

	// Выбор видов за период (по PickStore): сколько кандидатов отвергнуто и
	// почему — видно, во что обходится «случайный» вид.
	Picks            int     `json:"picks"`
	Rejected         int     `json:"rejected"`
	RejectedByReason []Count `json:"rejected_by_reason"`

	RecentObservations int      `json:"recent_observations"`            // сумма по выпускам
	OutOfRangeSpecies  []string `json:"out_of_range_species,omitempty"` // «Otocolobus manul: Germany, Japan»

	// Журнал: запуски по заданиям и статусам («issue/ok», «issue/budget»,
	// «mdd/ok»), сбои с текстом, события релиза MDD (Detail запусков mdd с
	// Ref, отличным от прошлого).
	Runs       []Count  `json:"runs"`
	Failures   []string `json:"failures,omitempty"`    // «12:00 issue: досье: …»
	MDDRelease []string `json:"mdd_release,omitempty"` // «вышел релиз v2.6 (было v2.5)»

	CostUSD     float64 `json:"cost_usd"`     // сумма CostUSD запусков
	BudgetSkips int     `json:"budget_skips"` // запусков со статусом budget
}

// Aggregator считает агрегат за период [from, to). Реализация в
// aggregate.go: `type Aggregator struct { Issues IssueStore; Picks
// PickStore; Runs RunSource }` (Runs может быть nil — тогда разделы журнала
// пустые):
//
//	func (a *Aggregator) Aggregate(ctx context.Context, from, to time.Time) (Aggregate, error)

// Summary — сводка: агрегат + текст.
type Summary struct {
	ID        int64     `json:"id"` // присваивает SummaryStore
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	CreatedAt time.Time `json:"created_at"`
	Trigger   string    `json:"trigger"` // schedule | manual
	Aggregate Aggregate `json:"aggregate"`
	// Text — пересказ агрегата моделью по-русски (5–10 предложений); пусто,
	// если модель не ответила — сводка всё равно сохраняется с агрегатом.
	Text  string   `json:"text,omitempty"`
	Error string   `json:"error,omitempty"`
	Spend Spend    `json:"spend"`
	Cost  llm.Cost `json:"cost"`
}

// Summarizer пишет текст сводки по агрегату. Пустой агрегат (0 выпусков,
// 0 запусков) — текст пишется кодом без модели («за период выпусков не
// было»), Spend нулевой.
type Summarizer interface {
	Summarize(ctx context.Context, a Aggregate) (text string, s Spend, err error)
}

// SummaryStore — хранилище сводок (компонент "trivia", шаг миграции после
// выпусков). Реализации: SQLite и Memory.
type SummaryStore interface {
	// SaveSummary сохраняет и возвращает ID (> 0).
	SaveSummary(ctx context.Context, s Summary) (int64, error)
	// Summary — по ID; ErrIssueNotFound, если нет.
	Summary(ctx context.Context, id int64) (Summary, error)
	// Summaries — новые первыми (CreatedAt, ID убыв.); limit ≤ 0 → 20, предел 100.
	Summaries(ctx context.Context, limit int) ([]Summary, error)
	// LatestSummary — последняя; ok=false — не было.
	LatestSummary(ctx context.Context) (s Summary, ok bool, err error)
}
