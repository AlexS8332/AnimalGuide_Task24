// Package trivia — «интересные факты» по расписанию: раз в час демон
// выбирает случайный вид из справочника MDD, собирает о нём выпуск фактов и
// сохраняет его; раз в сутки — сводка по выпускам.
//
// Этот файл — контракт этапа выбора вида. Случайный вид из 6904 чаще всего
// оказывается безымянным грызуном или летучей мышью без статьи и без
// наблюдений: выпуск о нём не соберёшь. Поэтому выбор двухступенчатый и с
// проверкой пригодности:
//
//  1. статус МСОП выбирается с весом «вес статуса × число видов с ним» —
//     редкие статусы выпадают чаще, чем их доля в справочнике;
//  2. внутри статуса — равновероятный вид (Search с случайным Offset);
//  3. вид, выбранный за последние NoRepeat, пропускается;
//  4. кандидат проверяется (Checker): есть статья в Википедии и не меньше
//     MinOccurrences наблюдений в GBIF. Итог проверки кэшируется на CheckTTL;
//  5. отвергнутые кандидаты с причиной попадают в Pick.Rejected — видно,
//     почему выбран именно этот вид. MaxAttempts ограничивает число
//     проверок (кэш или Checker); недавние виды проверку не тратят, поэтому
//     Pick.Attempts может быть больше MaxAttempts.
//
// Виды без кода МСОП (и с кодом вне известного списка) собираются в
// корзину OtherStatus. Все проверки упали сетью — ErrCheckFailed, а не
// ErrNoCandidate: «источник недоступен» и «пригодных нет» — разные беды.
package trivia

import (
	"context"
	"errors"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
)

// ErrNoCandidate — за MaxAttempts кандидатов пригодного не нашлось.
var ErrNoCandidate = errors.New("trivia: пригодный вид не найден")

// Причины отказа (Eligibility.Reason, Rejection.Reason) — коды, а не текст:
// по ним считается сводка «почему отбраковали».
const (
	ReasonOK          = ""
	ReasonDomestic    = "domestic"     // домашний вид — не отбирается
	ReasonRecent      = "recent"       // уже был за последние NoRepeat
	ReasonNoArticle   = "no_article"   // нет статьи ни в ru, ни в en Википедии
	ReasonNoGBIF      = "no_gbif"      // GBIF не знает названия
	ReasonFewRecords  = "few_records"  // наблюдений меньше MinOccurrences
	ReasonCheckFailed = "check_failed" // сетевая ошибка проверки; не кэшируется
)

// Eligibility — итог проверки пригодности вида. OK — пригоден.
type Eligibility struct {
	SpeciesID int       `json:"species_id"`
	SciName   string    `json:"sci_name"`
	CheckedAt time.Time `json:"checked_at"`
	OK        bool      `json:"ok"`
	Reason    string    `json:"reason,omitempty"` // код из Reason*; пусто при OK

	// Статья: ru, если есть, иначе en. Title — заголовок в той Википедии.
	WikiLang  string `json:"wiki_lang,omitempty"`
	WikiTitle string `json:"wiki_title,omitempty"`
	WikiURL   string `json:"wiki_url,omitempty"`
	// EnTitle — заголовок в английской Википедии (через неё ищется статья по
	// латинскому названию); пусто, если статьи нет.
	EnTitle string `json:"en_title,omitempty"`

	// GBIF: ключ таксона (species/match) и число наблюдений (occurrence/search).
	GBIFKey     int `json:"gbif_key,omitempty"`
	Occurrences int `json:"occurrences"`
}

// Checker проверяет пригодность вида. Сетевая ошибка — err (вид не
// отвергается навсегда, а пропускается в этом запуске); «статьи нет» или
// «наблюдений мало» — не ошибка, а Eligibility с OK=false и причиной.
// Вместе с err возвращается Eligibility с ReasonCheckFailed: такой итог
// PickStore.SaveCheck отвергает, случайно закэшировать сбой нельзя.
// minOccurrences — порог наблюдений.
type Checker interface {
	Check(ctx context.Context, sp mdd.Species, minOccurrences int) (Eligibility, error)
}

// Rejection — отвергнутый кандидат.
type Rejection struct {
	SpeciesID int    `json:"species_id"`
	SciName   string `json:"sci_name"`
	Reason    string `json:"reason"`
	Detail    string `json:"detail,omitempty"` // для людей: «12 наблюдений < 50», текст сетевой ошибки
}

// Pick — выбранный вид и как он выбирался.
type Pick struct {
	ID          int64         `json:"id"` // присваивает PickStore.SavePick
	SpeciesID   int           `json:"species_id"`
	SciName     string        `json:"sci_name"`
	IUCN        string        `json:"iucn,omitempty"`
	PickedAt    time.Time     `json:"picked_at"`
	Attempts    int           `json:"attempts"` // сколько кандидатов рассмотрено, включая выбранный
	Rejected    []Rejection   `json:"rejected,omitempty"`
	Eligibility Eligibility   `json:"eligibility"`
	Weights     WeightsReport `json:"weights"` // вероятности статусов в этом запуске
}

// WeightsReport — вероятность каждого статуса МСОП в запуске: код → доля
// (сумма 1). Нужна, чтобы по выпуску было видно, насколько «подкручен» выбор.
type WeightsReport map[string]float64

// PickStore — хранилище выборов и кэша проверок. Реализации: SQLite
// (продукт, компонент миграций "trivia") и Memory (тесты).
type PickStore interface {
	// SavePick сохраняет выбор и возвращает присвоенный ID (> 0).
	SavePick(ctx context.Context, p Pick) (int64, error)
	// RecentSpecies — id видов, выбранных не раньше since (без повторов).
	RecentSpecies(ctx context.Context, since time.Time) ([]int, error)
	// Picks — последние выборы, новые первыми; limit ≤ 0 — все.
	Picks(ctx context.Context, limit int) ([]Pick, error)
	// CachedCheck — сохранённая проверка вида, если она не старше notBefore
	// (CheckedAt >= notBefore); ok=false — нет или устарела.
	CachedCheck(ctx context.Context, speciesID int, notBefore time.Time) (e Eligibility, ok bool, err error)
	// SaveCheck сохраняет (заменяет) проверку вида. Проверки с
	// ReasonCheckFailed сохранять нельзя — вызывающий их не передаёт, а
	// реализация возвращает ошибку.
	SaveCheck(ctx context.Context, e Eligibility) error
}

// PickOptions — настройки выбора. Нулевые поля — значения по умолчанию
// (Defaults); отрицательные NoRepeat и CheckTTL отключают запрет повторов
// и кэш проверок.
type PickOptions struct {
	NoRepeat       time.Duration      // 30 дней
	CheckTTL       time.Duration      // 30 дней
	MaxAttempts    int                // 8
	MinOccurrences int                // 50
	Weights        map[string]float64 // вес статуса МСОП; нет в карте — 1
}

// DefaultWeights — редкие и исчезнувшие виды чаще: о них и фактов больше.
var DefaultWeights = map[string]float64{
	"LC": 1, "NT": 1.5, "VU": 2, "EN": 3, "CR": 4, "EW": 3, "EX": 2, "DD": 1, "NE": 1,
}

// Defaults — значения по умолчанию.
func Defaults() PickOptions {
	w := make(map[string]float64, len(DefaultWeights))
	for k, v := range DefaultWeights {
		w[k] = v
	}
	return PickOptions{NoRepeat: 30 * 24 * time.Hour, CheckTTL: 30 * 24 * time.Hour,
		MaxAttempts: 8, MinOccurrences: 50, Weights: w}
}
