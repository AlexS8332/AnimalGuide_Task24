package trivia

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
)

// builderMaxFacts — больше фактов в выпуске не бывает: редактор просит 3–5,
// но модель иногда пишет больше, а лента из восьми фактов — уже не выпуск,
// а пересказ статьи.
const builderMaxFacts = 5

// Причины, с которыми факты отбрасывает сам Builder (Fact.Verdict).
const (
	builderVerdictEmpty     = "пустой факт"
	builderVerdictNoSource  = "нет источника"
	builderVerdictUnchecked = "не подтверждён проверяющим"
	builderVerdictOverLimit = "сверх лимита"
)

// Builder — IssueBuilder: досье → черновик → отбраковка кодом → проверка →
// сохранение. Выпуск сохраняется всегда, даже неудачный: сводка «сбои»
// строится по сохранённым выпускам с IssueFailed, а незаписанный сбой из
// неё бы просто пропал.
type Builder struct {
	Collector Collector
	Editor    Editor
	Verifier  Verifier
	Store     IssueStore
	Now       func() time.Time // nil — time.Now
}

var _ IssueBuilder = (*Builder)(nil)

// Build собирает и сохраняет выпуск.
//
// Ошибка шага (досье, редактор, проверка) — выпуск с IssueFailed и Error,
// в нём всё, что успели (источники, наблюдения, расход); он сохраняется и
// возвращается с ID вместе с ошибкой. Ноль подтверждённых фактов — тоже
// IssueFailed, но не ошибка: конвейер отработал, просто выпуска не вышло.
//
// Отменённый ctx — ctx.Err() без сохранения: демон гасится, это не сбой
// выпуска, и в сводке «сбои» ему не место.
//
// Не задан Collector, Editor, Verifier или Store — ошибка сразу, без
// выпуска: это ошибка сборки программы, а не сбой вида.
func (b *Builder) Build(ctx context.Context, p Pick, sp mdd.Species) (Issue, error) {
	if b.Collector == nil || b.Editor == nil || b.Verifier == nil || b.Store == nil {
		return Issue{}, errors.New("trivia: сборка выпуска: не заданы сборщик досье, редактор, проверяющий или хранилище")
	}
	now := b.Now
	if now == nil {
		now = time.Now
	}
	start := now()

	is := Issue{
		PickID:    p.ID,
		SpeciesID: sp.ID,
		SciName:   sp.SciName,
		IUCN:      sp.IUCN,
		Order:     sp.Order,
		Family:    sp.Family,
		Realms:    issueCloneSlice(sp.Realms),
		CreatedAt: start.Round(0), // без монотонных часов — как вернёт хранилище
	}
	// Вид из справочника и выбор должны совпадать; если вызывающий передал
	// пустой вид, выпуск всё равно должен сохраниться с понятным видом.
	if is.SpeciesID == 0 {
		is.SpeciesID = p.SpeciesID
	}
	if is.SciName == "" {
		is.SciName = p.SciName
	}
	if is.IUCN == "" {
		is.IUCN = p.IUCN
	}

	stepErr := b.run(ctx, p, sp, &is)
	if stepErr != nil {
		is.Status = IssueFailed
		is.Error = stepErr.Error()
		stepErr = fmt.Errorf("trivia: выпуск о %s: %w", is.SciName, stepErr)
	}
	is.Cost = llm.Cost{}
	for _, s := range is.Spend {
		is.Cost = is.Cost.Add(s.Cost)
	}
	is.Took = now().Sub(start)

	// Отмену проверяем перед самой записью: шаг мог упасть из-за неё (тогда
	// это не сбой выпуска), а мог и пройти, но демон уже уходит.
	if err := ctx.Err(); err != nil {
		return Issue{}, err
	}
	id, err := b.Store.SaveIssue(ctx, is)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Issue{}, ctxErr
		}
		return is, errors.Join(stepErr, fmt.Errorf("trivia: выпуск о %s не сохранён: %w", is.SciName, err))
	}
	is.ID = id
	return is, stepErr
}

// run — шаги сборки; заполняет is по мере продвижения, чтобы при ошибке в
// выпуске осталось всё, что успели. Ошибка — с именем упавшего шага: она
// же уходит в Issue.Error.
func (b *Builder) run(ctx context.Context, p Pick, sp mdd.Species, is *Issue) error {
	d, err := b.Collector.Collect(ctx, p, sp)
	if err != nil {
		return fmt.Errorf("досье: %w", err)
	}
	return b.compose(ctx, d, sp, is)
}

// Compose — вторая половина сборки над готовым досье: черновик редактора →
// отбраковка кодом → проверка. Ни Collector, ни Store не нужны: так шаг
// «обработать» можно вызвать отдельно (инструмент summarize конвейера), а
// досье получить другим шагом. Возвращает выпуск без ID и PickID — он не
// сохранён; вид берётся из d.Species (PickID — из d.Pick, если он есть).
// Ошибка шага — выпуск с IssueFailed и Error вместе с ошибкой, как у Build;
// расход и время заполнены всегда.
func (b *Builder) Compose(ctx context.Context, d Dossier) (Issue, error) {
	if b.Editor == nil || b.Verifier == nil {
		return Issue{}, errors.New("trivia: обработка досье: не заданы редактор или проверяющий")
	}
	now := b.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	sp := d.Species
	is := Issue{
		PickID:    d.Pick.ID,
		SpeciesID: sp.ID,
		SciName:   sp.SciName,
		IUCN:      sp.IUCN,
		Order:     sp.Order,
		Family:    sp.Family,
		Realms:    issueCloneSlice(sp.Realms),
		CreatedAt: start.Round(0),
	}
	err := b.compose(ctx, d, sp, &is)
	if err != nil {
		is.Status = IssueFailed
		is.Error = err.Error()
		err = fmt.Errorf("trivia: факты о %s: %w", is.SciName, err)
	}
	for _, s := range is.Spend {
		is.Cost = is.Cost.Add(s.Cost)
	}
	is.Took = now().Sub(start)
	return is, err
}

// compose — шаги после досье; заполняет is по мере продвижения.
func (b *Builder) compose(ctx context.Context, d Dossier, sp mdd.Species, is *Issue) error {
	is.NameRu = d.NameRu
	is.Observations = d.Observations
	for _, m := range d.Materials {
		m.Text = ""
		is.Sources = append(is.Sources, m)
	}

	draft, spend, err := b.Editor.Write(ctx, d)
	builderAddSpend(is, spend)
	if err != nil {
		return fmt.Errorf("редактор: %w", err)
	}
	is.Title = strings.TrimSpace(draft.Title)
	is.Lead = strings.TrimSpace(draft.Lead)

	kept, dropped := builderScreen(draft.Facts, d.Materials)
	is.Dropped = append(is.Dropped, dropped...)

	// Заголовок и вступление модель пишет так же свободно, как факты, и так
	// же может приукрасить («молчаливая мышь из перуанских туманов»).
	// Проверяются тем же запросом, последним пунктом со ссылкой на всё
	// досье; не подтвердились — заголовок становится названием вида, а
	// вступление убирается. Это дешевле, чем отдельный запрос.
	head := builderHead(is, d.Materials)
	var confirmed []Fact
	if len(kept) > 0 {
		check := kept
		if head != nil {
			check = append(append([]Fact(nil), kept...), *head)
		}
		verdicts, spend, err := b.Verifier.Verify(ctx, d, check)
		builderAddSpend(is, spend)
		if err != nil {
			return fmt.Errorf("проверка: %w", err)
		}
		if len(verdicts) != len(check) {
			return fmt.Errorf("проверка: %d вердиктов на %d фактов", len(verdicts), len(check))
		}
		if head != nil {
			if v := verdicts[len(kept)]; !v.OK {
				head.Verdict = builderHeadPrefix + strings.TrimSpace(v.Reason)
				is.Dropped = append(is.Dropped, *head)
				is.Title, is.Lead = builderFallbackTitle(is, sp), ""
			}
			verdicts = verdicts[:len(kept)]
		}
		for i, f := range kept {
			if verdicts[i].OK {
				f.Verdict = ""
				confirmed = append(confirmed, f)
				continue
			}
			f.Verdict = strings.TrimSpace(verdicts[i].Reason)
			if f.Verdict == "" {
				f.Verdict = builderVerdictUnchecked
			}
			is.Dropped = append(is.Dropped, f)
		}
	}
	if len(confirmed) > builderMaxFacts {
		for _, f := range confirmed[builderMaxFacts:] {
			f.Verdict = builderVerdictOverLimit
			is.Dropped = append(is.Dropped, f)
		}
		confirmed = confirmed[:builderMaxFacts]
	}

	is.Facts = confirmed
	switch n := len(confirmed); {
	case n >= MinFacts:
		is.Status = IssueOK
	case n > 0:
		is.Status = IssueThin
	default:
		is.Status = IssueFailed
		is.Error = fmt.Sprintf("ни одного подтверждённого факта: черновик %d, отброшено %d",
			len(draft.Facts), len(is.Dropped))
	}
	return nil
}

// builderHeadPrefix — метка отброшенных заголовка и вступления в Dropped.
const builderHeadPrefix = "заголовок и вступление: "

// builderHead — заголовок и вступление как пункт проверки со ссылкой на все
// материалы; nil, если проверять нечего.
func builderHead(is *Issue, materials []Material) *Fact {
	text := strings.TrimSpace(strings.TrimSpace(is.Title) + ". " + is.Lead)
	if is.Title == "" && is.Lead == "" || len(materials) == 0 {
		return nil
	}
	f := Fact{Text: text}
	for _, m := range materials {
		f.Sources = append(f.Sources, m.ID)
	}
	return &f
}

// builderFallbackTitle — заголовок вместо отвергнутого: название вида.
func builderFallbackTitle(is *Issue, sp mdd.Species) string {
	if is.NameRu != "" {
		return is.NameRu + " (" + is.SciName + ")"
	}
	if is.SciName != "" {
		return is.SciName
	}
	return sp.SciName
}

// builderAddSpend дописывает расход шага; пустой (шаг упал до модели) не
// пишется — строка «0 запросов» в расходе только путает.
func builderAddSpend(is *Issue, s Spend) {
	if s == (Spend{}) {
		return
	}
	is.Spend = append(is.Spend, s)
}

// builderScreen — отбраковка фактов кодом до проверяющего: пустые, без
// ссылок, со ссылкой на материал, которого нет в досье, и повторы. Модель
// проверяющего такие факты оценила бы непредсказуемо, а платить за это
// незачем. Ссылки чистятся от пробелов и пустых значений; повтор ссылки в
// одном факте схлопывается.
func builderScreen(facts []Fact, materials []Material) (kept, dropped []Fact) {
	known := make(map[string]bool, len(materials))
	for _, m := range materials {
		known[m.ID] = true
	}
	seen := map[string]int{} // нормализованный текст → номер факта в черновике (с 1)
	for i, f := range facts {
		f.Text = strings.TrimSpace(f.Text)
		f.Sources = builderCleanSources(f.Sources)
		f.Verdict = ""
		key := strings.Join(strings.Fields(strings.ToLower(f.Text)), " ")
		switch {
		case f.Text == "":
			f.Verdict = builderVerdictEmpty
		case len(f.Sources) == 0:
			f.Verdict = builderVerdictNoSource
		case seen[key] > 0:
			f.Verdict = fmt.Sprintf("повтор факта %d", seen[key])
		default:
			for _, s := range f.Sources {
				if !known[s] {
					f.Verdict = "ссылка на несуществующий материал " + s
					break
				}
			}
		}
		if f.Verdict != "" {
			dropped = append(dropped, f)
			continue
		}
		seen[key] = i + 1
		kept = append(kept, f)
	}
	return kept, dropped
}

// builderCleanSources — ссылки без пробелов по краям, пустых и повторов, в
// исходном порядке. Всегда новый срез: черновик редактора не меняется.
func builderCleanSources(src []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range src {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
