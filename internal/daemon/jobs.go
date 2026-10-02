// Package daemon — задания демона для планировщика: выпуск фактов (раз в
// час), проверка релиза справочника MDD (раз в сутки) и сводка (раз в
// сутки). Здесь только склейка готовых частей (mdd, trivia, schedule):
// задание собирает зависимости на запуск, зовёт конвейер и переводит его
// итог в schedule.Outcome — расход, ссылку и строку для людей.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/schedule"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// Имена заданий — они же ключи в журнале запусков и в разбивке сводки
// («issue/ok», «mdd/ok»).
const (
	JobIssue   = "issue"
	JobMDD     = "mdd"
	JobSummary = "summary"
)

// Приставки Ref: «issue:42», «mdd:v2.5», «summary:7».
const (
	RefIssue   = "issue:"
	RefMDD     = "mdd:"
	RefSummary = "summary:"
)

// MDDNewRelease — начало Detail запуска mdd, когда сменилась версия
// релиза. Сводка распознаёт событие по нему (или по смене Ref между
// запусками mdd); trivia не может импортировать daemon, поэтому договор —
// строка, а не функция.
const MDDNewRelease = "новый релиз"

// DetailMDDEmpty — Detail пропущенного выпуска, пока справочник не загружен.
const DetailMDDEmpty = "справочник MDD ещё не загружен"

// ---------------------------------------------------------------- выпуск

// IssueDeps — зависимости задания «выпуск». Фабрики источников подменяются
// в тестах, чтобы собрать выпуск без сети.
type IssueDeps struct {
	Species mdd.Store
	Picks   trivia.PickStore
	Issues  trivia.IssueStore
	LLM     llm.Chatter
	Model   string // "" → llm.DefaultModel
	Options trivia.PickOptions

	// NewFetcher — HTTP-клиент с кэшем на один запуск; nil → tools.NewFetcher.
	NewFetcher func() *tools.Fetcher
	// Checker — проверка пригодности вида поверх Fetcher; nil → WebChecker.
	Checker func(*tools.Fetcher) trivia.Checker
	// Collector — сборщик досье поверх Fetcher; nil → WebCollector.
	Collector func(*tools.Fetcher) trivia.Collector
	Log       *slog.Logger // nil — без журнала
	// Now — часы выбора, проверки, досье и выпуска; nil — time.Now. Демон
	// передаёт часы планировщика: на подставных часах испытания выпуск
	// обязан жить в том же времени, что журнал запусков и сводка.
	Now func() time.Time
}

// IssueJob — выпуск фактов каждые every.
//
// Ошибки и расход:
//   - справочник не загружен → schedule.ErrSkip (Detail DetailMDDEmpty):
//     это не сбой, а «ещё рано» — демон только что поставлен, и загрузка
//     MDD идёт своим заданием;
//   - не нашлось пригодного вида (ErrNoCandidate) или проверки упали сетью
//     (ErrCheckFailed) → ошибка без расхода: модель не вызывалась;
//   - выпуск сохранён со статусом failed → Outcome (с Ref и расходом) И
//     ошибка: запуск failed, но потраченное на модель учитывается в лимите.
func IssueJob(d IssueDeps, every time.Duration) schedule.Job {
	return schedule.Job{
		Name:  JobIssue,
		Every: every,
		Paid:  true,
		Run:   func(ctx context.Context) (schedule.Outcome, error) { return runIssue(ctx, d) },
	}
}

func runIssue(ctx context.Context, d IssueDeps) (schedule.Outcome, error) {
	if d.Species == nil || d.Picks == nil || d.Issues == nil || d.LLM == nil {
		return schedule.Outcome{}, errors.New("выпуск: не заданы справочник, хранилища или модель")
	}
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	// Fetcher — на каждый запуск свой: его кэш ответов вечный, и общий на
	// весь демон через месяц отдал бы повторной проверке вида старое число
	// наблюдений, а сборщику — старую статью. Внутри запуска кэш нужен:
	// проверка и досье читают одну и ту же статью.
	newFetcher := d.NewFetcher
	if newFetcher == nil {
		newFetcher = tools.NewFetcher
	}
	f := newFetcher()

	var checker trivia.Checker
	if d.Checker != nil {
		checker = d.Checker(f)
	} else {
		wc := trivia.NewWebChecker(f)
		wc.Now = d.Now
		checker = wc
	}
	var collector trivia.Collector
	if d.Collector != nil {
		collector = d.Collector(f)
	} else {
		wc := trivia.NewWebCollector(f)
		wc.Now = d.Now
		collector = wc
	}

	picker := &trivia.Picker{Species: d.Species, Checker: checker, Store: d.Picks, Options: d.Options, Now: d.Now}
	p, err := picker.Pick(ctx)
	if err != nil {
		if errors.Is(err, mdd.ErrNotFound) && ctx.Err() == nil {
			return schedule.Outcome{Detail: DetailMDDEmpty}, fmt.Errorf("%w: %s", schedule.ErrSkip, DetailMDDEmpty)
		}
		return schedule.Outcome{}, fmt.Errorf("выбор вида: %w", err)
	}
	log.Info("выбран вид", "sci_name", p.SciName, "pick_id", p.ID,
		"attempts", p.Attempts, "rejected", len(p.Rejected))

	sp, err := d.Species.Get(ctx, p.SpeciesID)
	if err != nil {
		// Справочник заменили между выбором и сборкой, и вида в новом релизе
		// нет — редкость, но выпуск о пустом виде хуже пропуска.
		return schedule.Outcome{}, fmt.Errorf("вид %s (%d) из справочника: %w", p.SciName, p.SpeciesID, err)
	}

	b := &trivia.Builder{
		Collector: collector,
		Editor:    trivia.LLMEditor{LLM: d.LLM, Model: d.Model, Now: d.Now},
		Verifier:  trivia.LLMVerifier{LLM: d.LLM, Model: d.Model, Now: d.Now},
		Store:     d.Issues,
		Now:       d.Now,
	}
	is, buildErr := b.Build(ctx, p, sp)
	if buildErr != nil && ctx.Err() != nil && is.ID == 0 && is.SciName == "" {
		// Демон гасится: Builder ничего не сохранил, отчитываться не о чем.
		return schedule.Outcome{}, buildErr
	}

	out := schedule.Outcome{CostUSD: is.Cost.USD, Detail: issueDetail(is, sp)}
	if is.ID > 0 {
		out.Ref = fmt.Sprintf("%s%d", RefIssue, is.ID)
	}
	log.Info("выпуск", "issue_id", is.ID, "status", is.Status, "facts", len(is.Facts),
		"dropped", len(is.Dropped), "cost_usd", is.Cost.USD)

	switch {
	case buildErr != nil:
		// Builder уже приписал вид к ошибке.
		return out, buildErr
	case is.Status == trivia.IssueFailed:
		// Конвейер отработал, но фактов не осталось: Build не считает это
		// ошибкой, а для журнала запусков это сбой — выпуска нет.
		return out, fmt.Errorf("выпуск о %s не собран: %s", is.SciName, is.Error)
	}
	return out, nil
}

// issueDetail — «Манул (Otocolobus manul): 5 фактов» или «…: выпуск не
// собран — <ошибка>».
func issueDetail(is trivia.Issue, sp mdd.Species) string {
	sci := is.SciName
	if sci == "" {
		sci = sp.SciName
	}
	name := sci
	if ru := strings.TrimSpace(is.NameRu); ru != "" {
		name = ru + " (" + sci + ")"
	}
	if is.Status == trivia.IssueFailed || is.Status == "" {
		reason := strings.TrimSpace(is.Error)
		if reason == "" {
			reason = "неизвестная ошибка"
		}
		return name + ": выпуск не собран — " + reason
	}
	return name + ": " + plural(len(is.Facts), "факт", "факта", "фактов")
}

// plural — «1 факт», «2 факта», «5 фактов».
func plural(n int, one, few, many string) string {
	w := many
	switch m10, m100 := n%10, n%100; {
	case m10 == 1 && m100 != 11:
		w = one
	case m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14):
		w = few
	}
	return fmt.Sprintf("%d %s", n, w)
}

// ------------------------------------------------------------ релиз MDD

// MDDJob — проверка релиза MDD раз в сутки в daily («HH:MM»). Бесплатное:
// модель не вызывается. Проверка стоит одного запроса с If-None-Match.
//
// Форматы Outcome (Ref всегда «mdd:<версия, которая теперь в базе>»):
//   - «релиз v2.5 не менялся» — архив не изменился (304);
//   - «загружен релиз v2.5 (6904 вида)» — базы не было;
//   - «релиз v2.5 перезагружен: архив поправлен без смены версии (6904
//     вида)» — сменился ETag, версия прежняя;
//   - «новый релиз v2.6 (было v2.5): 6910 видов (+6), изменений в
//     систематике 12» — сменилась версия. Detail начинается с MDDNewRelease
//     только в этом случае, и только в этом случае Ref отличается от Ref
//     прошлого успешного запуска.
func MDDJob(st mdd.Store, o mdd.SyncOptions, daily string) schedule.Job {
	return schedule.Job{
		Name:  JobMDD,
		Daily: daily,
		Paid:  false,
		Run: func(ctx context.Context) (schedule.Outcome, error) {
			if st == nil {
				return schedule.Outcome{}, errors.New("релиз MDD: хранилище не задано")
			}
			res, err := mdd.Sync(ctx, st, o)
			if err != nil {
				return schedule.Outcome{}, fmt.Errorf("релиз MDD: %w", err)
			}
			return mddOutcome(ctx, st, res), nil
		},
	}
}

func mddOutcome(ctx context.Context, st mdd.Store, res mdd.SyncResult) schedule.Outcome {
	rel := res.Release
	out := schedule.Outcome{Ref: RefMDD + rel.Version}
	species := plural(rel.Species, "вид", "вида", "видов")
	switch {
	case !res.Updated:
		out.Detail = "релиз " + rel.Version + " не менялся"
	case res.Prev == nil:
		out.Detail = fmt.Sprintf("загружен релиз %s (%s)", rel.Version, species)
	case res.Prev.Version == rel.Version:
		out.Detail = fmt.Sprintf("релиз %s перезагружен: архив поправлен без смены версии (%s)", rel.Version, species)
	default:
		out.Detail = fmt.Sprintf("%s %s (было %s): %s (%+d)", MDDNewRelease, rel.Version,
			res.Prev.Version, species, rel.Species-res.Prev.Species)
		// Число изменений — для сводки; не получилось прочитать — событие
		// всё равно главное, без числа.
		if ch, err := st.Changes(ctx, "", 0); err == nil && len(ch) > 0 {
			out.Detail += fmt.Sprintf(", изменений в систематике %d", len(ch))
		}
	}
	return out
}

// ----------------------------------------------------------------- сводка

// SummaryDeps — зависимости сводки. Aggregate — функция, а не
// trivia.Aggregator: так демон и тесты подставляют что угодно.
type SummaryDeps struct {
	Aggregate  func(ctx context.Context, from, to time.Time) (trivia.Aggregate, error)
	Summarizer trivia.Summarizer
	Store      trivia.SummaryStore
	Now        func() time.Time // nil → time.Now
	Period     time.Duration    // 0 → 24 часа
}

// SummaryJob — сводка раз в сутки в daily за период [now−Period, now).
//
// Конец периода округляется вниз до минуты: запуск по расписанию стартует
// на секунды позже слота, и без округления соседние сводки пересекались бы
// или оставляли щель в несколько секунд — выпуск на стыке попал бы в обе
// или ни в одну.
//
// Модель упала — сводка всё равно сохраняется (агрегат + Error), Outcome
// несёт Ref и расход, а запуск failed.
func SummaryJob(d SummaryDeps, daily string) schedule.Job {
	return schedule.Job{
		Name:  JobSummary,
		Daily: daily,
		Paid:  true,
		Run: func(ctx context.Context) (schedule.Outcome, error) {
			now := time.Now
			if d.Now != nil {
				now = d.Now
			}
			period := d.Period
			if period <= 0 {
				period = 24 * time.Hour
			}
			// Ручная сводка — «до этой минуты включительно»: округление
			// вниз выбросило бы выпуск, собранный секунды назад. Её период
			// с суточными не стыкуется, и это не нужно.
			to, trigger := now().Truncate(time.Minute), schedule.TriggerSchedule
			if r, ok := schedule.CurrentRun(ctx); ok && r.Trigger == schedule.TriggerManual {
				to, trigger = now(), schedule.TriggerManual
			}
			s, err := BuildSummary(ctx, d, to.Add(-period), to, trigger)
			if s.ID == 0 && s.Cost.USD == 0 && err != nil {
				return schedule.Outcome{}, err
			}
			out := schedule.Outcome{CostUSD: s.Cost.USD, Detail: summaryDetail(s)}
			if s.ID > 0 {
				out.Ref = fmt.Sprintf("%s%d", RefSummary, s.ID)
			}
			return out, err
		},
	}
}

// BuildSummary — сводка за [from, to) вне планировщика (инструмент
// summary_get, кнопка в интерфейсе): агрегат → текст модели → сохранение.
//
// Не посчитался агрегат — ошибка без сохранения: сводка без цифр ничего не
// стоит. Модель не ответила — сводка сохраняется с агрегатом и Error, и
// возвращается вместе с ошибкой (ID уже присвоен). Отменённый ctx — ошибка
// без сохранения: демон гасится, это не сбой сводки.
func BuildSummary(ctx context.Context, d SummaryDeps, from, to time.Time, trigger string) (trivia.Summary, error) {
	if d.Aggregate == nil || d.Summarizer == nil || d.Store == nil {
		return trivia.Summary{}, errors.New("сводка: не заданы агрегатор, модель или хранилище")
	}
	if !from.Before(to) {
		return trivia.Summary{}, fmt.Errorf("сводка: пустой период %s — %s", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}

	agg, err := d.Aggregate(ctx, from, to)
	if err != nil {
		return trivia.Summary{}, fmt.Errorf("сводка: агрегат: %w", err)
	}
	text, spend, sumErr := d.Summarizer.Summarize(ctx, agg)
	s := trivia.Summary{
		From:      from,
		To:        to,
		CreatedAt: now().Round(0),
		Trigger:   trigger,
		Aggregate: agg,
		Text:      strings.TrimSpace(text),
		Spend:     spend,
		Cost:      spend.Cost,
	}
	if sumErr != nil {
		sumErr = fmt.Errorf("сводка: текст: %w", sumErr)
		s.Error = sumErr.Error()
	}
	if err := ctx.Err(); err != nil {
		// Деньги могли уйти, но запись о них — в журнале запусков через
		// Outcome; сводку с оборванным текстом не сохраняем.
		return s, err
	}
	id, err := d.Store.SaveSummary(ctx, s)
	if err != nil {
		return s, errors.Join(sumErr, fmt.Errorf("сводка не сохранена: %w", err))
	}
	s.ID = id
	return s, sumErr
}

// summaryDetailRunes — сколько знаков текста сводки идёт в Detail запуска.
const summaryDetailRunes = 120

// summaryDetail — начало текста в одну строку или «сводка без текста: …».
func summaryDetail(s trivia.Summary) string {
	text := strings.Join(strings.Fields(s.Text), " ")
	if text == "" {
		reason := s.Error
		if reason == "" {
			reason = "модель вернула пустой ответ"
		}
		return "сводка без текста: " + reason
	}
	if utf8.RuneCountInString(text) <= summaryDetailRunes {
		return text
	}
	r := []rune(text)
	return strings.TrimRight(string(r[:summaryDetailRunes]), " ,.;:—-") + "…"
}

// ------------------------------------------------------ журнал для сводки

// runSourceLimit — предел RunQuery.Limit у RunStore.
const runSourceLimit = 500

// RunSourceOf — журнал запусков глазами сводки.
//
// Запуски отдаются от старых к новым: сводка ищет смену Ref между
// соседними запусками mdd, и ей естественнее идти по времени. Незавершённые
// (RunRunning) не отдаются: у них нет итога, а среди них — сам запуск
// сводки, который сейчас этот журнал и читает.
//
// RunQuery не умеет Offset, а Limit ограничен 500. Поэтому журнал читается
// окнами назад по времени: следующее окно — до Started самой старой записи
// предыдущего (Until исключающий). Записи с тем же Started, что у самой
// старой, могли не поместиться в окно — они дочитываются отдельным запросом
// [Started, Started+1нс) и склеиваются по ID. Предел остаётся один: больше
// 500 запусков с одинаковым Started (на практике невозможно — трёх заданий
// и так хватает на ~30 запусков в сутки).
func RunSourceOf(st schedule.RunStore) trivia.RunSource { return runSource{st: st} }

type runSource struct{ st schedule.RunStore }

func (r runSource) RunsBetween(ctx context.Context, from, to time.Time) ([]trivia.RunInfo, error) {
	if r.st == nil {
		return nil, errors.New("журнал запусков не задан")
	}
	var (
		all  []schedule.Run
		seen = map[int64]bool{}
	)
	add := func(list []schedule.Run) {
		for _, run := range list {
			if !seen[run.ID] {
				seen[run.ID] = true
				all = append(all, run)
			}
		}
	}
	until := to
	for {
		page, err := r.st.Runs(ctx, schedule.RunQuery{Since: from, Until: until, Limit: runSourceLimit})
		if err != nil {
			return nil, fmt.Errorf("журнал запусков: %w", err)
		}
		add(page)
		if len(page) < runSourceLimit {
			break
		}
		oldest := page[len(page)-1].Started
		same, err := r.st.Runs(ctx, schedule.RunQuery{Since: oldest, Until: oldest.Add(time.Nanosecond), Limit: runSourceLimit})
		if err != nil {
			return nil, fmt.Errorf("журнал запусков: %w", err)
		}
		add(same)
		// Окно не сдвинулось или дошло до начала периода — дальше читать
		// нечего (иначе цикл не кончился бы на странном хранилище).
		if !oldest.Before(until) || !oldest.After(from) {
			break
		}
		until = oldest
	}

	// Окна и дочитывание склеены не по порядку — сортируем заново: по
	// времени, при равенстве по ID.
	sort.Slice(all, func(i, j int) bool {
		if !all[i].Started.Equal(all[j].Started) {
			return all[i].Started.Before(all[j].Started)
		}
		return all[i].ID < all[j].ID
	})
	out := make([]trivia.RunInfo, 0, len(all))
	for _, run := range all {
		if run.Status == schedule.RunRunning {
			continue
		}
		out = append(out, trivia.RunInfo{
			Job:     run.Job,
			Status:  run.Status,
			Started: run.Started,
			CostUSD: run.CostUSD,
			Ref:     run.Ref,
			Detail:  run.Detail,
			Error:   run.Error,
		})
	}
	return out, nil
}
