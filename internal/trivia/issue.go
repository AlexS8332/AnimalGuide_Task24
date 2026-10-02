package trivia

import (
	"context"
	"errors"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
)

// Контракт этапа сборки выпуска. Выпуск собирается в три шага:
//
//  1. Collector (код, без модели) собирает досье: карточку MDD, статью
//     Википедии (ru, если есть, иначе en), наблюдения GBIF и сверку
//     наблюдений с ареалом MDD. Каждый кусок — Material со своим ID
//     («S1», «S2», …): это тот же принцип, что трекер v17, — модель может
//     сослаться только на то, что реально получено из источника.
//  2. Editor (модель) пишет черновик: заголовок, вступление и 3–5 фактов по-
//     русски, у каждого факта — ссылки на Material.ID.
//  3. Verifier (модель, отдельный запрос без промпта редактора) проверяет
//     каждый факт по процитированным материалам. Неподтверждённые факты
//     уходят в Issue.Dropped с причиной; в выпуск попадают только
//     подтверждённые.
//
// Конвейер карточки v17 (привратник → идентификатор → специалисты) здесь
// не используется: он читает только русскую Википедию, а у заметной доли
// пригодных видов статья есть лишь на английском, и он втрое дороже.

// Виды материалов.
const (
	KindMDD       = "mdd"
	KindWikipedia = "wikipedia"
	KindGBIF      = "gbif"
)

// Состояния выпуска.
const (
	IssueOK     = "ok"     // есть хотя бы MinFacts подтверждённых фактов
	IssueThin   = "thin"   // подтверждённых фактов меньше MinFacts, но ≥ 1
	IssueFailed = "failed" // выпуск не собран: ошибка на любом шаге или 0 фактов
)

// ErrIssueNotFound — выпуска (или выбора) с таким ID нет.
var ErrIssueNotFound = errors.New("trivia: выпуск не найден")

// MinFacts — сколько подтверждённых фактов нужно выпуску, чтобы быть «ok».
const MinFacts = 3

// Material — кусок исходных данных досье. Text видит модель; Title и URL —
// люди (ссылка «источник» у факта в интерфейсе).
type Material struct {
	ID    string `json:"id"`   // «S1», «S2», … в порядке досье
	Kind  string `json:"kind"` // KindMDD | KindWikipedia | KindGBIF
	Title string `json:"title"`
	URL   string `json:"url,omitempty"`
	Text  string `json:"text,omitempty"`
	// Tool — какой вызов источника его дал (read_wikipedia, gbif occurrence
	// facet, mdd_get…): для журнала и отладки.
	Tool string `json:"tool,omitempty"`
}

// Статус страны наблюдений относительно ареала MDD.
const (
	RangeIn        = "in"        // страна есть в Species.Countries
	RangeUncertain = "uncertain" // страна в Species.CountriesUncertain
	RangeOut       = "out"       // страны нет в ареале MDD
	RangeUnknown   = "unknown"   // код страны GBIF не сопоставился с именем MDD
)

// CountryCount — наблюдения в одной стране.
type CountryCount struct {
	Code  string `json:"code"` // ISO 3166-1 alpha-2, как у GBIF
	Name  string `json:"name"` // имя страны так, как пишет MDD («Russia», «Iran»)
	Count int    `json:"count"`
	Range string `json:"range"` // Range*
}

// Observations — наблюдения GBIF по виду. Считает код.
type Observations struct {
	GBIFKey int `json:"gbif_key"`
	Total   int `json:"total"` // за всё время
	// Recent — за последние WindowDays дней (по eventDate).
	WindowDays int            `json:"window_days"`
	Recent     int            `json:"recent"`
	ByCountry  []CountryCount `json:"by_country,omitempty"` // за всё время, по убыванию, не больше 20
	// RecentByCountry — за окно, по убыванию.
	RecentByCountry []CountryCount `json:"recent_by_country,omitempty"`
	// OutOfRange — страны за всё время с Range == RangeOut: «наблюдён вне
	// ареала MDD». Чаще всего это зоопарки, интродукция или ошибки
	// определения — поэтому это отметка, а не факт.
	OutOfRange []CountryCount `json:"out_of_range,omitempty"`
}

// Dossier — всё, что собрано о виде до модели.
type Dossier struct {
	Pick         Pick          `json:"pick"`
	Species      mdd.Species   `json:"species"`
	NameRu       string        `json:"name_ru,omitempty"` // заголовок ru-статьи или русское народное название GBIF
	Materials    []Material    `json:"materials"`
	Observations Observations  `json:"observations"`
	CollectedAt  time.Time     `json:"collected_at"`
	Took         time.Duration `json:"took"`
}

// Collector собирает досье. Ошибка — если нет ни одного материала, кроме
// MDD, или упал обязательный источник; частичный сбой (например, не
// получились наблюдения за окно) — не ошибка, а меньше материалов.
type Collector interface {
	Collect(ctx context.Context, p Pick, sp mdd.Species) (Dossier, error)
}

// Fact — факт выпуска.
type Fact struct {
	Text    string   `json:"text"`    // по-русски, одно–два предложения
	Sources []string `json:"sources"` // Material.ID, минимум один
	// Verdict — вывод проверяющего: пусто у подтверждённого, иначе причина.
	Verdict string `json:"verdict,omitempty"`
}

// Draft — черновик редактора.
type Draft struct {
	Title string `json:"title"` // заголовок выпуска, по-русски
	Lead  string `json:"lead"`  // вступление, 1–2 предложения
	Facts []Fact `json:"facts"`
}

// Spend — расход на модель одного шага.
type Spend struct {
	Model    string        `json:"model"`
	Requests int           `json:"requests"`
	Usage    llm.Usage     `json:"usage"`
	Cost     llm.Cost      `json:"cost"`
	Took     time.Duration `json:"took"`
}

// Editor пишет черновик по досье. Факты со ссылками на несуществующие
// Material.ID, без ссылок или пустые код отбрасывает до проверки (это
// делает Builder, не Editor).
type Editor interface {
	Write(ctx context.Context, d Dossier) (Draft, Spend, error)
}

// Verdict — вывод проверяющего по одному факту (в порядке фактов).
type Verdict struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"` // почему не подтверждён
}

// Verifier проверяет факты по процитированным материалам досье. Возвращает
// ровно len(facts) вердиктов.
type Verifier interface {
	Verify(ctx context.Context, d Dossier, facts []Fact) ([]Verdict, Spend, error)
}

// Issue — выпуск: то, что видит человек и отдают инструменты MCP.
type Issue struct {
	ID        int64     `json:"id"` // присваивает IssueStore.SaveIssue
	PickID    int64     `json:"pick_id"`
	SpeciesID int       `json:"species_id"`
	SciName   string    `json:"sci_name"`
	NameRu    string    `json:"name_ru,omitempty"`
	IUCN      string    `json:"iucn,omitempty"`
	Order     string    `json:"order,omitempty"`
	Family    string    `json:"family,omitempty"`
	Realms    []string  `json:"realms,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Status    string    `json:"status"`          // Issue*
	Error     string    `json:"error,omitempty"` // при IssueFailed

	Title   string `json:"title,omitempty"`
	Lead    string `json:"lead,omitempty"`
	Facts   []Fact `json:"facts,omitempty"`   // подтверждённые
	Dropped []Fact `json:"dropped,omitempty"` // отброшенные кодом или проверяющим, с Verdict
	// Sources — материалы досье без Text (Title, URL, Kind, ID): ссылки у
	// фактов ведут сюда.
	Sources      []Material   `json:"sources,omitempty"`
	Observations Observations `json:"observations"`

	Spend []Spend       `json:"spend,omitempty"` // по шагам: редактор, проверяющий
	Cost  llm.Cost      `json:"cost"`            // сумма Spend
	Took  time.Duration `json:"took"`            // вся сборка, с досье
}

// IssueQuery — выборка выпусков. Пустые поля не ограничивают.
type IssueQuery struct {
	Text      string    // подстрока в SciName, NameRu, Title, Lead или тексте факта; без учёта регистра (в т.ч. кириллица)
	SpeciesID int       // только этот вид
	Status    []string  // любой из
	Since     time.Time // CreatedAt ≥ Since
	Until     time.Time // CreatedAt < Until (нулевое — без верхней границы)
	Limit     int       // 0 → 20, предел 100
	Offset    int
}

// IssueStore — хранилище выпусков (компонент миграций "trivia", следующий
// шаг после выборов). Реализации: SQLite и Memory.
type IssueStore interface {
	// SaveIssue сохраняет выпуск и возвращает ID (> 0); входной ID
	// игнорируется. Выпуск без PickID или SpeciesID — ошибка.
	SaveIssue(ctx context.Context, is Issue) (int64, error)
	// Issue — выпуск по ID; ErrIssueNotFound, если нет.
	Issue(ctx context.Context, id int64) (Issue, error)
	// Issues — выборка, новые первыми (CreatedAt, затем ID убыв.); total —
	// без Limit/Offset.
	Issues(ctx context.Context, q IssueQuery) (list []Issue, total int, err error)
	// Pick — выбор по ID (выпуск ссылается на выбор); ErrIssueNotFound, если нет.
	Pick(ctx context.Context, id int64) (Pick, error)
}

// Builder собирает выпуск по выбору: досье → черновик → проверка →
// сохранение. Реализация — в builder.go.
type IssueBuilder interface {
	Build(ctx context.Context, p Pick, sp mdd.Species) (Issue, error)
}
