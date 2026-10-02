package bench

import (
	"context"
	"fmt"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
)

// Cost — И-6, цена, выключатели, совместимость: длинный диалог для кэша,
// постоянной части и числа запросов; пары дорожек «механизм включён —
// выключен» для каждого механизма с блоком; памятники прошлых форматов;
// сверка оценщика токенов с usage.
type Cost struct {
	// Long — реплики длинного диалога.
	Long []string
	// Probe — реплики дорожек «включён — выключен» по умолчанию;
	// ProbeFor — свои реплики для механизмов, чей блок появляется только
	// в своей обстановке (состояние подборки, рабочая память).
	Probe    []string
	ProbeFor map[features.Name][]string
	// Mechanisms — какие механизмы проверять на ноль токенов; пусто — все
	// механизмы реестра с блоком.
	Mechanisms []features.Name
	// Preset — анкета собеседника дорожек: без неё блок профиля пуст на
	// обеих дорожках, и проверять было бы нечего.
	Preset string
}

// Пороги раздела 10 ТЗ.
const (
	CacheShareMin   = 0.60
	ConstantMax     = 6000
	RequestsPerTurn = 4.0
	CalibrationPct  = 5.0
)

// NewCost — сценарий ТЗ.
func NewCost() *Cost {
	collect := []string{"Собери подборку: две кошки нашей фауны — рысь и манул.", "Хорошо, план утверждаю."}
	return &Cost{
		Long: []string{
			"Привет! Меня зовут Алекс, я учитель биологии.",
			"рысь",
			"А чем она питается?",
			"Где она живёт?",
			"манул",
			"Чем манул отличается от рыси?",
			"Сколько живёт манул?",
			"Какие у рыси враги?",
			"А зимой рысь меняет шерсть?",
			"Что ещё интересного про манула?",
			"Напомни, как меня зовут?",
			"Какое из двух животных крупнее?",
		},
		Probe: []string{"Привет! Меня зовут Алекс, я люблю кошек.", "Расскажи, где живёт рысь?"},
		ProbeFor: map[features.Name][]string{
			features.CollectionState: collect,
			features.MemoryWork:      collect,
		},
		Preset: "amateur",
	}
}

func (*Cost) ID() string    { return "И-6" }
func (*Cost) Title() string { return "Цена, выключатели, совместимость" }

// pairFor — основная и контрольная дорожки для механизма: основная — с
// механизмом и без тех, кто от него зависит (страж без свода не бывает), —
// иначе контрольная отличалась бы двумя механизмами.
func pairFor(reg *features.Registry, base features.Set, n features.Name, setup func(*Stand, string) error) []Lane {
	on := reg.Complete(base).With(n, true)
	for _, m := range reg.All() {
		for _, dep := range m.Requires {
			if dep == n && on.On(m.Name) {
				on = on.With(m.Name, false)
			}
		}
	}
	if mm, ok := reg.Get(n); ok {
		for _, dep := range mm.Requires {
			on = on.With(dep, true)
		}
	}
	return []Lane{
		{Name: "с " + string(n), Features: on, Setup: setup},
		{Name: "без " + string(n), Features: on.With(n, false), Setup: setup},
	}
}

// blockMechanisms — механизмы реестра с блоком в запросе.
func blockMechanisms(reg *features.Registry) []features.Name {
	var out []features.Name
	for _, m := range reg.All() {
		if m.Kind.Has(features.KindBlock) {
			out = append(out, m.Name)
		}
	}
	return out
}

// leaked — сколько токенов блок выключенного механизма занял в ходах: по
// разбивке оценки и по самим запросам модели.
func leaked(t history.Turn, n features.Name) int {
	tokens := t.Context.Estimate.Blocks[n]
	if tokens == 0 {
		if _, ok := blockText(t, n); ok {
			tokens = 1
		}
	}
	return tokens
}

func (c *Cost) Run(ctx context.Context, s *Stand, r *Result) error {
	r.Goal = "постоянная часть и число запросов в бюджете, кэш работает, выключенный механизм не стоит ни токена, старые файлы читаются"
	reg := s.env.Registry
	main := "длинный диалог"

	// Длинный диалог — один, без сравнения: мерит цену продукта как есть.
	var longTurns []history.Turn
	if len(c.Long) > 0 {
		d, err := s.Solo("И-6: длинный диалог", Lane{Name: main, Note: "умолчания приложения", Features: s.env.Base})
		if err != nil {
			return err
		}
		for _, q := range c.Long {
			st, err := d.Ask(ctx, q)
			if err != nil {
				return err
			}
			longTurns = append(longTurns, st.Turn)
		}
		ls := statsOf(reg, main, longTurns)
		share := ls.CacheShare()
		r.check(Check{What: "доля кэша на длинном диалоге", Want: fmt.Sprintf("≥ %.0f %%", CacheShareMin*100), Lane: main,
			Got: fmt.Sprintf("%.0f %%", share*100), Status: statusIf(share >= CacheShareMin)})
		r.check(Check{What: "постоянная часть запроса", Want: fmt.Sprintf("≤ %d токенов", ConstantMax), Lane: main,
			Got: fmt.Sprintf("%d (в среднем %d)", ls.ConstantMax, ls.Constant), Status: statusIf(ls.ConstantMax > 0 && ls.ConstantMax <= ConstantMax)})
		r.check(Check{What: "запросов к модели на ход", Want: fmt.Sprintf("≤ %.0f в среднем", RequestsPerTurn), Lane: main,
			Got: fmt.Sprintf("%.1f", ls.PerTurn()), Status: statusIf(ls.PerTurn() <= RequestsPerTurn)})
		if ls.Calibration.Pairs == 0 {
			r.pending("ошибка оценки токенов после калибровки", fmt.Sprintf("≤ %.0f %%", CalibrationPct), main, "в журнале нет пар «оценка — факт»")
		} else {
			r.check(Check{What: "ошибка оценки токенов после калибровки", Want: fmt.Sprintf("≤ %.0f %%", CalibrationPct), Lane: main,
				Got:    fmt.Sprintf("%.1f %% (без поправки %+.1f %%, множитель %.3f, пар %d)", ls.Calibration.ResidualPct, ls.Calibration.RawPct, ls.Calibration.Factor, ls.Calibration.Pairs),
				Status: statusIf(ls.Calibration.ResidualPct <= CalibrationPct)})
		}
		r.metric("ходов длинного диалога", main, "%d", len(longTurns))
	}

	// Выключатели: пара дорожек на каждый механизм с блоком.
	mechs := c.Mechanisms
	if len(mechs) == 0 {
		mechs = blockMechanisms(reg)
	}
	var setup func(*Stand, string) error
	if c.Preset != "" {
		setup = withPreset(c.Preset)
	}
	total, leakedMechs := 0, []string{}
	for _, n := range mechs {
		lanes := pairFor(reg, s.env.Base, n, setup)
		if len(r.Lanes) == 0 {
			r.describeLanes(reg, lanes)
			r.Lanes[0].Name, r.Lanes[1].Name = "с механизмом", "без механизма"
			r.Lanes[0].Note, r.Lanes[1].Note = "пара на каждый механизм с блоком", "тот же набор без этого механизма"
			r.Lanes[1].Diff = "−механизм"
		}
		g, err := s.Group("И-6: "+string(n), lanes)
		if err != nil {
			return err
		}
		lines := c.Probe
		if own, ok := c.ProbeFor[n]; ok {
			lines = own
		}
		onTokens, offTokens, onTurns := 0, 0, 0
		for _, q := range lines {
			steps, err := g.Send(ctx, agents.Request{Text: q})
			if err != nil {
				return err
			}
			for i, st := range steps {
				if i == 0 {
					if v := st.Turn.Context.Estimate.Blocks[n]; v > 0 {
						onTokens += v
						onTurns++
					}
					continue
				}
				offTokens += leaked(st.Turn, n)
			}
		}
		total += offTokens
		if offTokens > 0 {
			leakedMechs = append(leakedMechs, fmt.Sprintf("%s: %d", n, offTokens))
		}
		avg := 0
		if onTurns > 0 {
			avg = onTokens / onTurns
		}
		m, _ := reg.Get(n)
		note := ""
		if onTurns == 0 {
			note = " (блок не появился и на включённой дорожке — сценарий его не вызвал)"
		}
		r.metric("блок "+string(n), "", "включён: ≈%d токенов (в реестре %d); выключен: %d%s", avg, m.Cost.Tokens, offTokens, note)
	}
	r.check(Check{What: "выключенный механизм добавляет токенов", Want: "0", Got: fmt.Sprint(total),
		Status: statusIf(total == 0), Note: strings.Join(leakedMechs, "; ")})

	// Совместимость: памятники прошлых форматов.
	if s.env.Legacy == "" {
		r.pending("миграции из testdata/legacy", "все без потерь", "", "каталог памятников не задан")
		return nil
	}
	ms, err := CheckLegacy(s.env.Legacy)
	if err != nil {
		r.pending("миграции из testdata/legacy", "все без потерь", "", err.Error())
		return nil
	}
	ok := 0
	var bad []string
	for _, m := range ms {
		if m.OK() {
			ok++
		} else {
			bad = append(bad, fmt.Sprintf("%s: %s%s", m.File, m.Err, strings.Join(firstN(m.Lost, 2), ", ")))
		}
		r.metric("памятник "+m.File, "", "формат v%d, текстов %d, потеряно %d", m.Schema, m.Texts, len(m.Lost))
	}
	c2 := Check{What: "миграции из testdata/legacy", Want: "все без потерь", Got: fmt.Sprintf("%d из %d", ok, len(ms)),
		Status: statusIf(ok == len(ms) && len(ms) > 0), Note: strings.Join(bad, "; ")}
	r.check(c2)
	return nil
}

func statusIf(ok bool) Status {
	if ok {
		return Pass
	}
	return Fail
}
