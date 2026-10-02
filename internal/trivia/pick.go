package trivia

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
)

// ErrCheckFailed — ни одного кандидата не удалось проверить: все проверки
// упали с ошибкой (обычно сеть). Отличается от ErrNoCandidate: виды,
// возможно, пригодны, просто сейчас это не узнать — имеет смысл повторить
// позже. Ошибка оборачивает и последнюю ошибку Checker.
var ErrCheckFailed = errors.New("trivia: проверить кандидатов не удалось")

// OtherStatus — ключ WeightsReport (и Weights) для видов без кода МСОП или
// с кодом вне известного списка. Такие виды не найти фильтром Query.IUCN,
// поэтому они собираются в одну «корзину»: её размер — «все недомашние
// минус сумма по известным кодам». Вес — Weights[OtherStatus], иначе 1.
const OtherStatus = "—"

// pickKnownCodes — коды МСОП из комментария к mdd.Species.IUCN, в порядке
// шкалы. Порядок важен: по нему идёт розыгрыш статуса, и при фиксированном
// seed выбор должен повторяться.
var pickKnownCodes = []string{"LC", "NT", "VU", "EN", "CR", "EW", "EX", "DD", "NE"}

const (
	// pickPageSize — страница при переборе корзины OtherStatus (предел Search).
	pickPageSize = 100
	// pickRestTries — сколько раз пробовать «случайный недомашний вид, пока
	// не попадётся без известного кода», прежде чем перебирать страницами.
	// Если корзина меньше 1/pickRestTries всех видов, розыгрыш сразу идёт
	// перебором: попадания пришлось бы ждать слишком долго.
	pickRestTries = 16
	// pickSkipFactor — сколько «пустых» розыгрышей (вид выбирался недавно
	// или уже выпадал в этом запуске) допускается на одну попытку
	// MaxAttempts. Розыгрыш — один запрос к локальной базе, сети он не
	// стоит, поэтому запас щедрый: когда свободным остался один вид с малой
	// вероятностью, до него всё равно надо дотянуться. Но не бесконечно.
	pickSkipFactor = 32
)

// Picker выбирает вид для выпуска по алгоритму из комментария пакета.
// Безопасен для одновременных вызовов Pick, если безопасны Species, Checker,
// Store и заданный Rand (свой *rand.Rand с PCG — нет: его состояние общее).
type Picker struct {
	Species mdd.Store
	Checker Checker
	Store   PickStore
	Options PickOptions      // нулевые поля → Defaults()
	Rand    *rand.Rand       // nil → общий источник math/rand/v2
	Now     func() time.Time // nil → time.Now
}

// Pick выбирает, проверяет и сохраняет вид.
//
// Решения, которых нет в контракте:
//   - MaxAttempts ограничивает кандидатов, дошедших до проверки (кэш или
//     Checker). Отказ ReasonRecent (выбирался за NoRepeat) сети не стоит и
//     в MaxAttempts не входит — иначе у редких статусов, где за месяц
//     выбрана заметная доля видов, повторы съедали бы попытки. Вид, уже
//     выпавший в этом запуске, тянется заново молча: он уже есть в Rejected
//     со своей причиной, и второй записью список только засорился бы.
//     Пустых розыгрышей не больше pickSkipFactor×MaxAttempts, так что Pick
//     всегда завершается. Pick.Attempts — все различные рассмотренные
//     кандидаты, то есть len(Rejected)+1, и может превышать MaxAttempts.
//   - Отрицательный NoRepeat отключает запрет повторов, отрицательный
//     CheckTTL — кэш проверок (нулевые значат «по умолчанию»).
//   - Кэш проверок хранит факты, а порог — политика: сохранённая проверка
//     пересчитывается под текущий MinOccurrences (см. pickApplyThreshold).
func (p *Picker) Pick(ctx context.Context) (Pick, error) {
	if p.Species == nil || p.Checker == nil || p.Store == nil {
		return Pick{}, errors.New("trivia: Picker без Species, Checker или Store")
	}
	if err := ctx.Err(); err != nil {
		return Pick{}, err
	}
	opt, err := pickOptions(p.Options)
	if err != nil {
		return Pick{}, err
	}
	rng := p.Rand
	if rng == nil {
		rng = rand.New(pickGlobalSource{})
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}

	if _, err := p.Species.Release(ctx); err != nil {
		if errors.Is(err, mdd.ErrNotFound) {
			return Pick{}, fmt.Errorf("trivia: справочник MDD ещё не загружен: %w", err)
		}
		return Pick{}, pickCtxOr(ctx, fmt.Errorf("trivia: справочник MDD: %w", err))
	}
	plan, err := pickMakePlan(ctx, p.Species, opt.Weights)
	if err != nil {
		return Pick{}, pickCtxOr(ctx, err)
	}

	// Недавние выборы читаются один раз: за время Pick новых не появится,
	// кроме нашего собственного.
	recent := map[int]bool{}
	if opt.NoRepeat > 0 {
		ids, err := p.Store.RecentSpecies(ctx, now.Add(-opt.NoRepeat))
		if err != nil {
			return Pick{}, pickCtxOr(ctx, fmt.Errorf("trivia: недавние выборы: %w", err))
		}
		for _, id := range ids {
			recent[id] = true
		}
	}

	var (
		rejected []Rejection
		seen     = map[int]bool{}
		checked  int // дошли до проверки — это и ограничивает MaxAttempts
		failed   int // из них упали с ошибкой Checker
		skipped  int // пустые розыгрыши: повтор, недавний, устаревший план
		lastErr  error
	)
	reject := func(sp mdd.Species, reason, detail string) {
		rejected = append(rejected, Rejection{SpeciesID: sp.ID, SciName: sp.SciName, Reason: reason, Detail: detail})
	}
	for checked < opt.MaxAttempts && skipped < pickSkipFactor*opt.MaxAttempts {
		if err := ctx.Err(); err != nil {
			return Pick{}, err
		}
		sp, ok, err := pickDraw(ctx, p.Species, rng, plan)
		if err != nil {
			return Pick{}, pickCtxOr(ctx, err)
		}
		if !ok {
			// Число видов разошлось с поиском — справочник заменили во
			// время выбора. Пересчитываем корзины и тянем заново.
			skipped++
			if plan, err = pickMakePlan(ctx, p.Species, opt.Weights); err != nil {
				return Pick{}, pickCtxOr(ctx, err)
			}
			continue
		}
		switch {
		case seen[sp.ID]:
			skipped++
			continue
		case recent[sp.ID]:
			skipped++
			seen[sp.ID] = true
			reject(sp, ReasonRecent, "выбирался за последние "+pickDays(opt.NoRepeat))
			continue
		case sp.Domestic:
			// Search с Domestic=false таких не отдаёт; на случай чужой
			// реализации Store — не выбирать всё равно.
			skipped++
			seen[sp.ID] = true
			reject(sp, ReasonDomestic, "")
			continue
		}
		seen[sp.ID] = true
		checked++

		e, cached, err := p.pickCheck(ctx, opt, now, sp)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Pick{}, ctxErr
			}
			var ce pickCheckErr
			if !errors.As(err, &ce) {
				return Pick{}, err // хранилище
			}
			failed++
			lastErr = ce.err
			reject(sp, ReasonCheckFailed, ce.err.Error())
			continue
		}
		if !e.OK {
			reject(sp, e.Reason, pickDetail(e, opt.MinOccurrences, cached))
			continue
		}

		out := Pick{
			SpeciesID:   sp.ID,
			SciName:     sp.SciName,
			IUCN:        sp.IUCN,
			PickedAt:    now,
			Attempts:    len(rejected) + 1,
			Rejected:    rejected,
			Eligibility: e,
			Weights:     plan.report(),
		}
		id, err := p.Store.SavePick(ctx, out)
		if err != nil {
			return Pick{}, pickCtxOr(ctx, fmt.Errorf("trivia: сохранить выбор: %w", err))
		}
		out.ID = id
		return out, nil
	}

	if checked > 0 && failed == checked {
		return Pick{}, fmt.Errorf("%w: все %d проверок упали, последняя: %w", ErrCheckFailed, failed, lastErr)
	}
	return Pick{}, fmt.Errorf("%w: рассмотрено %d, проверено %d, из них с сетевой ошибкой %d",
		ErrNoCandidate, len(rejected), checked, failed)
}

// pickCheckErr — ошибка Checker (в отличие от ошибки хранилища, которая
// прерывает Pick): кандидат пропускается, итог не кэшируется.
type pickCheckErr struct{ err error }

func (e pickCheckErr) Error() string { return e.err.Error() }
func (e pickCheckErr) Unwrap() error { return e.err }

// pickCheck — пригодность вида: сначала кэш, потом Checker. cached — итог
// взят из кэша.
func (p *Picker) pickCheck(ctx context.Context, opt PickOptions, now time.Time, sp mdd.Species) (Eligibility, bool, error) {
	if opt.CheckTTL > 0 {
		e, ok, err := p.Store.CachedCheck(ctx, sp.ID, now.Add(-opt.CheckTTL))
		if err != nil {
			return Eligibility{}, false, fmt.Errorf("trivia: кэш проверок: %w", err)
		}
		if ok {
			if e, usable := pickApplyThreshold(e, opt.MinOccurrences); usable {
				return e, true, nil
			}
		}
	}
	e, err := p.Checker.Check(ctx, sp, opt.MinOccurrences)
	if err != nil {
		return Eligibility{}, false, pickCheckErr{err}
	}
	if e.Reason == ReasonCheckFailed {
		// Checker обязан сообщать о сбое ошибкой; если вернул причину —
		// обращаемся так же, и такой итог в кэш не попадает.
		return Eligibility{}, false, pickCheckErr{errors.New("проверка не удалась (" + ReasonCheckFailed + ")")}
	}
	if e.SpeciesID == 0 {
		e.SpeciesID = sp.ID
	}
	if e.SciName == "" {
		e.SciName = sp.SciName
	}
	if e.CheckedAt.IsZero() {
		e.CheckedAt = now
	}
	if err := p.Store.SaveCheck(ctx, e); err != nil {
		return Eligibility{}, false, fmt.Errorf("trivia: сохранить проверку: %w", err)
	}
	return e, false, nil
}

// pickApplyThreshold пересчитывает сохранённую проверку под текущий порог
// наблюдений. usable=false — из кэша вывод не сделать, нужна новая проверка.
//
// Кэш не помнит, с каким порогом проверяли, но помнит число наблюдений, и
// этого хватает:
//   - был пригоден, наблюдений ≥ порога — пригоден; порог с тех пор подняли
//     выше числа наблюдений — отказ ReasonFewRecords без запроса в сеть;
//   - отказ «мало наблюдений», а порог опустили до их числа — пригоден, но
//     только если известно, что статья есть (WikiTitle): Checker мог
//     считать наблюдения раньше, чем искать статью, и тогда её наличие не
//     проверено — идём в Checker;
//   - отказы «нет статьи», «GBIF не знает» от порога не зависят.
func pickApplyThreshold(e Eligibility, minOcc int) (Eligibility, bool) {
	switch {
	case e.Reason == ReasonCheckFailed:
		return e, false
	case e.OK:
		if e.Occurrences < minOcc {
			e.OK, e.Reason = false, ReasonFewRecords
		}
		return e, true
	case e.Reason == ReasonFewRecords:
		if e.Occurrences < minOcc {
			return e, true
		}
		if e.WikiTitle == "" {
			return e, false
		}
		e.OK, e.Reason = true, ReasonOK
		return e, true
	default:
		return e, true
	}
}

// pickDetail — пояснение к отказу для людей.
func pickDetail(e Eligibility, minOcc int, cached bool) string {
	var d string
	switch e.Reason {
	case ReasonFewRecords:
		d = fmt.Sprintf("%d наблюдений < %d", e.Occurrences, minOcc)
	case ReasonNoArticle:
		d = "нет статьи ни в ru, ни в en Википедии"
	case ReasonNoGBIF:
		d = "GBIF не знает названия"
	case ReasonDomestic:
		d = "домашний вид"
	}
	if cached {
		if d != "" {
			d += " "
		}
		d += "(по кэшу)"
	}
	return d
}

// pickOptions — PickOptions с подставленными значениями по умолчанию и
// нормализованными весами (ключи — коды в верхнем регистре, "" → OtherStatus).
func pickOptions(o PickOptions) (PickOptions, error) {
	d := Defaults()
	if o.NoRepeat == 0 {
		o.NoRepeat = d.NoRepeat
	}
	if o.CheckTTL == 0 {
		o.CheckTTL = d.CheckTTL
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = d.MaxAttempts
	}
	if o.MinOccurrences <= 0 {
		o.MinOccurrences = d.MinOccurrences
	}
	src := o.Weights
	if src == nil {
		src = d.Weights
	}
	w := make(map[string]float64, len(src))
	for k, v := range src {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return PickOptions{}, fmt.Errorf("trivia: вес статуса %q должен быть конечным и ≥ 0, а не %v", k, v)
		}
		w[pickKey(k)] = v
	}
	o.Weights = w
	return o, nil
}

// pickKey — ключ статуса: код в верхнем регистре; пустой — OtherStatus.
func pickKey(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return OtherStatus
	}
	return code
}

// pickBucket — статус МСОП и сколько недомашних видов с ним.
type pickBucket struct {
	key    string // код или OtherStatus
	count  int
	weight float64
}

// pickPlan — корзины статусов одного запуска.
type pickPlan struct {
	buckets []pickBucket
	all     int             // все недомашние виды
	known   map[string]bool // коды, у которых своя корзина (остальные — в OtherStatus)
	mass    float64         // Σ вес × число
}

// pickMakePlan считает виды по статусам: по запросу Search с Limit 1 на
// код (total не зависит от Limit), без загрузки справочника в память.
// Коды — известный список плюс коды, для которых заданы веса.
func pickMakePlan(ctx context.Context, species mdd.Store, weights map[string]float64) (pickPlan, error) {
	notDomestic := false
	_, all, err := species.Search(ctx, mdd.Query{Domestic: &notDomestic, Limit: 1})
	if err != nil {
		return pickPlan{}, fmt.Errorf("trivia: поиск в справочнике: %w", err)
	}
	if all == 0 {
		return pickPlan{}, errors.New("trivia: в справочнике нет недомашних видов")
	}

	codes := append([]string(nil), pickKnownCodes...)
	var extra []string
	for k := range weights {
		if k != OtherStatus && !slices.Contains(pickKnownCodes, k) {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	codes = append(codes, extra...)

	plan := pickPlan{all: all, known: make(map[string]bool, len(codes))}
	weightOf := func(k string) float64 {
		if w, ok := weights[k]; ok {
			return w
		}
		return 1
	}
	sum := 0
	for _, code := range codes {
		plan.known[code] = true
		_, n, err := species.Search(ctx, mdd.Query{IUCN: []string{code}, Domestic: &notDomestic, Limit: 1})
		if err != nil {
			return pickPlan{}, fmt.Errorf("trivia: поиск в справочнике (%s): %w", code, err)
		}
		if n > 0 {
			plan.buckets = append(plan.buckets, pickBucket{key: code, count: n, weight: weightOf(code)})
			sum += n
		}
	}
	// Остаток может оказаться отрицательным, только если справочник
	// заменили между запросами; тогда корзины просто нет.
	if rest := all - sum; rest > 0 {
		plan.buckets = append(plan.buckets, pickBucket{key: OtherStatus, count: rest, weight: weightOf(OtherStatus)})
	}
	for _, b := range plan.buckets {
		plan.mass += b.weight * float64(b.count)
	}
	if plan.mass <= 0 {
		return pickPlan{}, errors.New("trivia: у всех статусов, что есть в справочнике, нулевой вес")
	}
	return plan, nil
}

// report — вероятности статусов (сумма 1).
func (pl pickPlan) report() WeightsReport {
	r := make(WeightsReport, len(pl.buckets))
	for _, b := range pl.buckets {
		r[b.key] = b.weight * float64(b.count) / pl.mass
	}
	return r
}

// choose разыгрывает статус с вероятностью ∝ вес × число видов.
func (pl pickPlan) choose(rng *rand.Rand) pickBucket {
	x := rng.Float64() * pl.mass
	last := -1
	for i, b := range pl.buckets {
		m := b.weight * float64(b.count)
		if m <= 0 {
			continue
		}
		last = i
		if x < m {
			return b
		}
		x -= m
	}
	// Сюда попадаем только из-за округления на последней корзине.
	return pl.buckets[last]
}

// pickDraw — статус, затем равновероятный недомашний вид с ним. ok=false —
// поиск не нашёл вида, который по подсчёту должен быть (справочник
// сменился): план надо пересчитать.
func pickDraw(ctx context.Context, species mdd.Store, rng *rand.Rand, plan pickPlan) (mdd.Species, bool, error) {
	notDomestic := false
	b := plan.choose(rng)
	if b.key != OtherStatus {
		// Порядок Search стабилен (phylosort, id), поэтому случайный Offset
		// даёт равновероятный вид.
		list, _, err := species.Search(ctx, mdd.Query{IUCN: []string{b.key}, Domestic: &notDomestic,
			Limit: 1, Offset: rng.IntN(b.count)})
		if err != nil {
			return mdd.Species{}, false, fmt.Errorf("trivia: поиск в справочнике: %w", err)
		}
		if len(list) == 0 {
			return mdd.Species{}, false, nil
		}
		return list[0], true, nil
	}

	// Корзину OtherStatus фильтром не выбрать. Случайный недомашний вид,
	// повторяемый, пока не выпадет вид без известного кода, — равновероятен
	// внутри корзины; когда корзина мала, это долго, и вид ищется перебором
	// страниц до случайного номера внутри корзины (тоже равновероятно, а
	// смесь двух равновероятных способов остаётся равновероятной).
	if b.count*pickRestTries >= plan.all {
		for range pickRestTries {
			list, _, err := species.Search(ctx, mdd.Query{Domestic: &notDomestic, Limit: 1, Offset: rng.IntN(plan.all)})
			if err != nil {
				return mdd.Species{}, false, fmt.Errorf("trivia: поиск в справочнике: %w", err)
			}
			if len(list) == 0 {
				return mdd.Species{}, false, nil
			}
			if !plan.known[pickKey(list[0].IUCN)] {
				return list[0], true, nil
			}
		}
	}
	target := rng.IntN(b.count)
	for off := 0; off < plan.all; off += pickPageSize {
		if err := ctx.Err(); err != nil {
			return mdd.Species{}, false, err
		}
		list, _, err := species.Search(ctx, mdd.Query{Domestic: &notDomestic, Limit: pickPageSize, Offset: off})
		if err != nil {
			return mdd.Species{}, false, fmt.Errorf("trivia: поиск в справочнике: %w", err)
		}
		for _, sp := range list {
			if plan.known[pickKey(sp.IUCN)] {
				continue
			}
			if target == 0 {
				return sp, true, nil
			}
			target--
		}
		if len(list) < pickPageSize {
			break
		}
	}
	return mdd.Species{}, false, nil
}

// pickGlobalSource — общий источник math/rand/v2: безопасен для
// одновременного использования и засеян случайно при старте процесса.
type pickGlobalSource struct{}

func (pickGlobalSource) Uint64() uint64 { return rand.Uint64() }

// pickCtxOr — ошибка контекста, если он отменён (она важнее того, во что
// отмена превратилась по пути), иначе err.
func pickCtxOr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func pickDays(d time.Duration) string {
	days := int(d / (24 * time.Hour))
	if days*24*int(time.Hour) == int(d) && days > 0 {
		return fmt.Sprintf("%d дн.", days)
	}
	return d.String()
}
