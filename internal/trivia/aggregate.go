package trivia

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Агрегат сводки считает код: модель потом только пересказывает его, так
// что каждое число сводки воспроизводимо из хранилищ и проверяется тестом.

// AggNoKey — ключ разбивки для пустого значения (вид без кода МСОП, без
// отряда или без биогеографической области): пустая строка в сводке
// выглядела бы как потерянные данные.
const AggNoKey = "—"

// Формат релиза MDD в журнале. Задание демона "mdd" пишет в Ref текущий
// релиз с приставкой («mdd:v2.5»), а если само заметило смену релиза — ещё
// и Detail с этой подстрокой («новый релиз v2.6 (было v2.5)»). Сводка
// считает релиз новым, если Ref успешного запуска отличается от Ref
// предыдущего успешного запуска mdd в периоде, или если Detail содержит
// AggMDDNewRelease: так ловится и релиз, вышедший первым запуском периода,
// когда сравнивать не с чем.
const (
	AggMDDJob        = "mdd"
	AggMDDRefPrefix  = "mdd:"
	AggMDDNewRelease = "новый релиз"
)

// Статусы запусков, которые сводка различает (как в schedule.Run).
const (
	aggRunOK     = "ok"
	aggRunFailed = "failed"
	aggRunBudget = "budget"
)

// aggPage — страница выборки выпусков: предел IssueQuery.Limit.
const aggPage = issueMaxLimit

// aggPicksFirst — сколько выборов читать первым запросом. PickStore отдаёт
// только «последние N», без фильтра по времени, поэтому выборы читаются
// окном, которое удваивается, пока не дойдёт до выбора раньше from (или до
// конца журнала). Для сводки за вчера хватает одного запроса (24 выбора в
// сутки); сводка за давний период читает все выборы после него — это
// линейно по их числу, но таких сводок демон не строит.
const aggPicksFirst = 64

// Aggregator считает агрегат за период [from, to) по выпускам, выборам и
// журналу запусков. Runs может быть nil — тогда разделы журнала пустые.
type Aggregator struct {
	Issues IssueStore
	Picks  PickStore
	Runs   RunSource
	// Location — зона времени в строках Failures («15:04 issue: …»); nil —
	// time.Local. Демон и человек, читающий сводку, живут в одной зоне, а
	// UTC в тексте сбоя пришлось бы пересчитывать в уме.
	Location *time.Location
}

// Aggregate считает агрегат. Выпуски читаются постранично (по 100) с
// фильтром CreatedAt ∈ [from, to); выборы — по PickedAt ∈ [from, to);
// запуски — RunSource за тот же период. to раньше from или нулевые границы —
// ошибка: нулевой to в IssueQuery значил бы «без верхней границы».
func (a *Aggregator) Aggregate(ctx context.Context, from, to time.Time) (Aggregate, error) {
	if a.Issues == nil || a.Picks == nil {
		return Aggregate{}, errors.New("сводка: хранилища выпусков и выборов не подключены")
	}
	if from.IsZero() || to.IsZero() {
		return Aggregate{}, errors.New("сводка: границы периода не заданы")
	}
	if to.Before(from) {
		return Aggregate{}, fmt.Errorf("сводка: конец периода %s раньше начала %s",
			to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	out := Aggregate{From: from, To: to}

	issues, err := a.aggIssues(ctx, from, to)
	if err != nil {
		return Aggregate{}, err
	}
	aggFillIssues(&out, issues)

	picks, err := a.aggPicks(ctx, from, to)
	if err != nil {
		return Aggregate{}, err
	}
	aggFillPicks(&out, picks)

	if a.Runs != nil {
		runs, err := a.Runs.RunsBetween(ctx, from, to)
		if err != nil {
			return Aggregate{}, fmt.Errorf("сводка: журнал запусков: %w", err)
		}
		loc := a.Location
		if loc == nil {
			loc = time.Local
		}
		aggFillRuns(&out, runs, loc, to.Sub(from) > 24*time.Hour)
	}
	return out, nil
}

// aggIssues — все выпуски периода, по возрастанию CreatedAt (при равном —
// по ID). Страницы идут новыми первыми; выпуск, сохранённый между
// страницами, сдвинул бы смещение и повторил строку — повтор отсеивается по
// ID. Для периода в прошлом (обычный случай — «вчера») сдвига нет вовсе.
func (a *Aggregator) aggIssues(ctx context.Context, from, to time.Time) ([]Issue, error) {
	var (
		all  []Issue
		seen = map[int64]bool{}
	)
	for offset := 0; ; {
		list, total, err := a.Issues.Issues(ctx, IssueQuery{Since: from, Until: to, Limit: aggPage, Offset: offset})
		if err != nil {
			return nil, fmt.Errorf("сводка: выпуски: %w", err)
		}
		for _, is := range list {
			if !seen[is.ID] {
				seen[is.ID] = true
				all = append(all, is)
			}
		}
		offset += len(list)
		if len(list) == 0 || offset >= total {
			break
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.Before(all[j].CreatedAt)
		}
		return all[i].ID < all[j].ID
	})
	return all, nil
}

// aggPicks — выборы с PickedAt ∈ [from, to). Окно Picks удваивается, пока
// самый старый из прочитанных не окажется раньше from: список отсортирован
// по PickedAt убыванию, значит, все выборы периода уже в нём.
func (a *Aggregator) aggPicks(ctx context.Context, from, to time.Time) ([]Pick, error) {
	for limit := aggPicksFirst; ; limit *= 2 {
		list, err := a.Picks.Picks(ctx, limit)
		if err != nil {
			return nil, fmt.Errorf("сводка: выборы: %w", err)
		}
		complete := len(list) < limit
		if n := len(list); n > 0 && list[n-1].PickedAt.Before(from) {
			complete = true
		}
		if !complete {
			continue
		}
		var out []Pick
		for _, p := range list {
			if !p.PickedAt.Before(from) && p.PickedAt.Before(to) {
				out = append(out, p)
			}
		}
		return out, nil
	}
}

// aggFillIssues — поля агрегата, которые считаются по выпускам.
func aggFillIssues(out *Aggregate, issues []Issue) {
	var (
		status = aggCounter{}
		order  = aggCounter{}
		iucn   = aggCounter{}
		realm  = aggCounter{}
		oor    = map[string]bool{}
	)
	out.Issues = len(issues)
	for _, is := range issues {
		status.add(is.Status)
		order.add(is.Order)
		iucn.add(is.IUCN)
		// Вид с двумя областями считается в обеих; повтор области у одного
		// вида — один раз. Вид без областей — в «—», чтобы сумма по
		// разбивке не теряла виды молча.
		realms := map[string]bool{}
		for _, r := range is.Realms {
			if r = strings.TrimSpace(r); r != "" && !realms[r] {
				realms[r] = true
				realm.add(r)
			}
		}
		if len(realms) == 0 {
			realm.add("")
		}

		line := SpeciesLine{
			IssueID: is.ID, SpeciesID: is.SpeciesID, SciName: is.SciName, NameRu: is.NameRu,
			IUCN: is.IUCN, Order: is.Order, Status: is.Status, Title: is.Title,
			Facts: len(is.Facts), Recent: is.Observations.Recent,
		}
		if len(is.Facts) > 0 {
			line.Highlight = strings.TrimSpace(is.Facts[0].Text)
		}
		for _, c := range is.Observations.OutOfRange {
			name := strings.TrimSpace(c.Name)
			if name == "" {
				name = c.Code
			}
			line.OutOfRange = append(line.OutOfRange, name)
		}
		out.Species = append(out.Species, line)

		out.Facts += len(is.Facts)
		out.Dropped += len(is.Dropped)
		out.RecentObservations += is.Observations.Recent
		if len(line.OutOfRange) > 0 {
			// Один вид дважды за период (NoRepeat отключён) — одна строка.
			s := is.SciName + ": " + strings.Join(line.OutOfRange, ", ")
			if !oor[s] {
				oor[s] = true
				out.OutOfRangeSpecies = append(out.OutOfRangeSpecies, s)
			}
		}
	}
	if n := out.Facts + out.Dropped; n > 0 {
		out.DroppedShare = float64(out.Dropped) / float64(n)
	}
	out.ByStatus = status.list()
	out.ByOrder = order.list()
	out.ByIUCN = iucn.list()
	out.ByRealm = realm.list()
}

// aggFillPicks — выборы периода и отвергнутые кандидаты по причинам.
func aggFillPicks(out *Aggregate, picks []Pick) {
	reasons := aggCounter{}
	out.Picks = len(picks)
	for _, p := range picks {
		out.Rejected += len(p.Rejected)
		for _, r := range p.Rejected {
			reasons.add(r.Reason)
		}
	}
	out.RejectedByReason = reasons.list()
}

// aggFillRuns — раздел журнала. Запуски сортируются по Started: RunSource
// порядок не обещает, а релиз MDD определяется сравнением с предыдущим.
// long — период длиннее суток: тогда у времени сбоя нужна и дата.
func aggFillRuns(out *Aggregate, runs []RunInfo, loc *time.Location, long bool) {
	runs = append([]RunInfo(nil), runs...)
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Started.Before(runs[j].Started) })

	counts := aggCounter{}
	layout := "15:04"
	if long {
		layout = "02.01 15:04"
	}
	prevRef := ""
	for _, r := range runs {
		counts.add(r.Job + "/" + r.Status)
		out.CostUSD += r.CostUSD
		switch r.Status {
		case aggRunBudget:
			out.BudgetSkips++
		case aggRunFailed:
			msg := strings.TrimSpace(r.Error)
			if msg == "" {
				msg = strings.TrimSpace(r.Detail)
			}
			if msg == "" {
				msg = "без текста ошибки"
			}
			out.Failures = append(out.Failures, fmt.Sprintf("%s %s: %s", r.Started.In(loc).Format(layout), r.Job, msg))
		}
		if r.Job != AggMDDJob || r.Status != aggRunOK {
			continue
		}
		if line, ok := aggMDDRelease(r, prevRef); ok {
			out.MDDRelease = append(out.MDDRelease, line)
		}
		if ref := strings.TrimSpace(r.Ref); ref != "" {
			prevRef = ref
		}
	}
	out.Runs = counts.list()
}

// aggMDDRelease — строка о новом релизе для успешного запуска mdd, если он
// принёс релиз. prev — Ref предыдущего успешного запуска mdd в периоде
// (пусто — не было).
func aggMDDRelease(r RunInfo, prev string) (string, bool) {
	ref := strings.TrimSpace(r.Ref)
	detail := strings.TrimSpace(r.Detail)
	changed := prev != "" && ref != "" && ref != prev
	flagged := strings.Contains(strings.ToLower(detail), AggMDDNewRelease)
	switch {
	case changed:
		return fmt.Sprintf("вышел релиз %s (было %s)", aggRelease(ref), aggRelease(prev)), true
	case flagged:
		// Сравнить не с чем — берётся текст задания как есть: оно знает,
		// какой релиз был до него.
		return detail, true
	}
	return "", false
}

// aggRelease — релиз без приставки «mdd:».
func aggRelease(ref string) string { return strings.TrimPrefix(ref, AggMDDRefPrefix) }

// aggCounter — разбивка «ключ → число»; пустой ключ — AggNoKey.
type aggCounter map[string]int

func (c aggCounter) add(key string) {
	if key = strings.TrimSpace(key); key == "" {
		key = AggNoKey
	}
	c[key]++
}

// list — разбивка по убыванию Count, затем по Key; nil у пустой.
func (c aggCounter) list() []Count {
	if len(c) == 0 {
		return nil
	}
	out := make([]Count, 0, len(c))
	for k, n := range c {
		out = append(out, Count{Key: k, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}
