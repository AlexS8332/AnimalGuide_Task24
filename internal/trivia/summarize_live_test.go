package trivia_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia/triviatest"
)

// TestLiveSummary — сводка за сутки на настоящей модели: агрегат из шести
// выпусков разных видов (пять собраны, один упал на досье), выборов к ним
// и журнала с успехами, сбоем, пропуском по лимиту и новым релизом MDD.
// Идёт только с TRIVIA_LIVE=1 и DEEPSEEK_API_KEY; модель — DEEPSEEK_MODEL
// или умолчание. Один запрос, повтор при лишних числах — ещё один.
//
// Код проверяет, что числа текста есть в агрегате (это делает сам
// Summarize), а тест — что в тексте нет сведений о животных, которых нет в
// агрегате, и стран «вне ареала» без оговорки.
func TestLiveSummary(t *testing.T) {
	if os.Getenv("TRIVIA_LIVE") != "1" {
		t.Skip("живая проверка: задай TRIVIA_LIVE=1 и DEEPSEEK_API_KEY")
	}
	key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if key == "" {
		t.Skip("DEEPSEEK_API_KEY не задан")
	}
	ctx := context.Background()
	msk := time.FixedZone("MSK", 3*60*60)
	from := time.Date(2026, 9, 24, 0, 0, 0, 0, msk)
	to := from.Add(24 * time.Hour)
	st := trivia.NewMemory()
	issues := summaryLiveIssues(from)
	for _, is := range issues {
		p := triviatest.SamplePick(is.SpeciesID, is.CreatedAt.Add(-time.Minute))
		p.SciName, p.IUCN = is.SciName, is.IUCN
		id, err := st.SavePick(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		is.PickID = id
		if _, err := st.SaveIssue(ctx, is); err != nil {
			t.Fatal(err)
		}
	}
	var runs []trivia.RunInfo
	for _, is := range issues {
		r := trivia.RunInfo{Job: "issue", Status: "ok", Started: is.CreatedAt.Add(-time.Minute), CostUSD: is.Cost.USD}
		if is.Status == trivia.IssueFailed {
			r.Status, r.Error = "failed", is.Error
		}
		runs = append(runs, r)
	}
	runs = append(runs,
		trivia.RunInfo{Job: "issue", Status: "budget", Started: from.Add(20 * time.Hour), Detail: "дневной лимит 0.05 $ исчерпан"},
		trivia.RunInfo{Job: "mdd", Status: "ok", Started: from.Add(3*time.Hour + 30*time.Minute), Ref: "mdd:v2.5"},
		trivia.RunInfo{Job: "mdd", Status: "ok", Started: from.Add(15*time.Hour + 30*time.Minute), Ref: "mdd:v2.6",
			Detail: "новый релиз v2.6 (было v2.5)"},
	)
	agg, err := (&trivia.Aggregator{Issues: st, Picks: st, Runs: &aggFakeRuns{runs: runs}, Location: msk}).
		Aggregate(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("агрегат: выпусков %d, фактов %d, отброшено %d (%.3f), расход %.4f $, сбоев %v, релиз %v",
		agg.Issues, agg.Facts, agg.Dropped, agg.DroppedShare, agg.CostUSD, agg.Failures, agg.MDDRelease)

	// Каждый ответ модели — в журнал теста: при повторе видно, какие числа
	// сверка сочла лишними.
	client := summaryLiveLog{t: t, next: llm.NewClient(key, os.Getenv("DEEPSEEK_BASE_URL"))}
	sum := trivia.LLMSummarizer{LLM: client,
		Model: strings.TrimSpace(os.Getenv("DEEPSEEK_MODEL")), Temperature: 0.3}
	text, spend, err := sum.Summarize(ctx, agg)
	t.Logf("расход: модель %s, запросов %d, токенов %d (кэш %d / мимо %d, вывод %d), %.5f $ (%s), %s",
		spend.Model, spend.Requests, spend.Usage.Total, spend.Usage.CacheHit, spend.Usage.CacheMiss,
		spend.Usage.Completion, spend.Cost.USD, spend.Cost.Tariff, spend.Took.Round(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("сводка:\n%s", text)

	sentences := strings.Count(text, ". ") + strings.Count(text, "! ") + 1
	if sentences < 4 || sentences > 12 {
		t.Errorf("предложений около %d, ждали 5–10", sentences)
	}
	low := strings.ToLower(text)
	// Сведения, которых в агрегате нет: частые «знания» о этих видах.
	for _, w := range []string{"монгол", "гимала", "тибет", "килограм", " кг", "кени", "маврики", "перу",
		"бобр строит", "плотин"} {
		if strings.Contains(low, w) {
			t.Errorf("в сводке %q — этого нет в агрегате", w)
		}
	}
	// Страны вне ареала — только с оговоркой.
	for _, c := range []string{"герман", "великобритан", "британ", "боливи"} {
		if strings.Contains(low, c) && !strings.Contains(low, "зоопарк") && !strings.Contains(low, "завоз") &&
			!strings.Contains(low, "ошибк") {
			t.Errorf("страна вне ареала %q без оговорки", c)
		}
	}
}

// summaryLiveLog — модель, пишущая каждый ответ в журнал теста.
type summaryLiveLog struct {
	t    *testing.T
	next llm.Chatter
}

func (l summaryLiveLog) Chat(ctx context.Context, req llm.Request) (llm.Response, error) {
	resp, err := l.next.Chat(ctx, req)
	if n := len(req.Messages); n > 2 {
		l.t.Logf("повтор, напоминание: %s", req.Messages[n-1].Content)
	}
	l.t.Logf("ответ модели (%d сообщений в запросе): %s", len(req.Messages), resp.Message.Content)
	return resp, err
}

// summaryLiveIssues — шесть выпусков за сутки. Факты — из досье testdata
// (манул, акодон) и короткие пересказы статей о других видах; для сводки
// они — данные, модель должна только выбрать из них.
func summaryLiveIssues(from time.Time) []trivia.Issue {
	cost := func(usd float64) llm.Cost { return llm.Cost{USD: usd, Tariff: llm.TariffOffPeak, Known: true} }
	fact := func(texts ...string) []trivia.Fact {
		var out []trivia.Fact
		for _, s := range texts {
			out = append(out, trivia.Fact{Text: s, Sources: []string{"S2"}})
		}
		return out
	}
	dropped := func(n int) []trivia.Fact {
		var out []trivia.Fact
		for i := 0; i < n; i++ {
			out = append(out, trivia.Fact{Text: "…", Sources: []string{"S2"}, Verdict: "нет в материале"})
		}
		return out
	}

	manul := triviatest.SampleIssue(0, 1006010, from.Add(time.Hour))
	manul.Cost = cost(0.0042)

	akodon := trivia.Issue{SpeciesID: 1001234, SciName: "Akodon surdus", IUCN: "VU", Order: "Rodentia",
		Family: "Cricetidae", Realms: []string{"Neotropic"}, CreatedAt: from.Add(3 * time.Hour), Status: trivia.IssueOK,
		Title: "Akodon surdus — мышь из горных лесов Куско",
		Facts: fact("Akodon surdus живёт в основном в регионе Куско на высоте от 1500 до 3000 м.",
			"Этот грызун может жить и в выборочно вырубленных лесах.",
			"Вид внесён в Красный список МСОП как уязвимый, его численность, как считается, сокращается."),
		Dropped: dropped(2),
		Observations: trivia.Observations{Total: 143, WindowDays: 365, Recent: 4,
			OutOfRange: []trivia.CountryCount{{Code: "BO", Name: "Bolivia", Count: 4, Range: trivia.RangeOut}}},
		Cost: cost(0.0038)}

	panda := trivia.Issue{SpeciesID: 1006001, SciName: "Ailurus fulgens", NameRu: "Малая панда", IUCN: "EN",
		Order: "Carnivora", Family: "Ailuridae", Realms: []string{"Indomalayan", "Palearctic"},
		CreatedAt: from.Add(5 * time.Hour), Status: trivia.IssueOK, Title: "Малая панда: ложный большой палец",
		Facts: fact("У малой панды на запястье есть вырост — «ложный большой палец», которым она удерживает стебли бамбука.",
			"Малая панда проводит большую часть дня в поисках еды и за едой.",
			"Малая панда — единственный современный вид семейства Ailuridae."),
		Observations: trivia.Observations{Total: 2210, WindowDays: 365, Recent: 186},
		Cost:         cost(0.0045)}

	rhino := trivia.Issue{SpeciesID: 1007002, SciName: "Diceros bicornis", NameRu: "Чёрный носорог", IUCN: "CR",
		Order: "Perissodactyla", Family: "Rhinocerotidae", Realms: []string{"Afrotropic"},
		CreatedAt: from.Add(9 * time.Hour), Status: trivia.IssueThin, Title: "Чёрный носорог и его цепкая губа",
		Facts: fact("Верхняя губа чёрного носорога заострена и подвижна: ею он захватывает листья и ветки.",
			"Несмотря на название, чёрный носорог окрашен в серый цвет."),
		Dropped:      dropped(3),
		Observations: trivia.Observations{Total: 1320, WindowDays: 365, Recent: 58},
		Cost:         cost(0.0051)}

	bat := trivia.Issue{SpeciesID: 1008003, SciName: "Pteropus rodricensis", NameRu: "Родригесская летучая лисица",
		IUCN: "EN", Order: "Chiroptera", Family: "Pteropodidae", Realms: []string{"Afrotropic"},
		CreatedAt: from.Add(12 * time.Hour), Status: trivia.IssueOK, Title: "Летучая лисица с одного острова",
		Facts: fact("Родригесская летучая лисица в дикой природе живёт только на острове Родригес в Индийском океане.",
			"В 1970-х годах её численность в природе падала до нескольких десятков особей.",
			"Днём эти летучие лисицы отдыхают на деревьях большими группами."),
		Dropped: dropped(1),
		Observations: trivia.Observations{Total: 310, WindowDays: 365, Recent: 12,
			OutOfRange: []trivia.CountryCount{{Code: "GB", Name: "United Kingdom", Count: 41, Range: trivia.RangeOut}}},
		Cost: cost(0.0040)}

	beaver := trivia.Issue{SpeciesID: 1001001, SciName: "Castor fiber", NameRu: "Обыкновенный бобр", IUCN: "LC",
		Order: "Rodentia", Family: "Castoridae", Realms: []string{"Palearctic"}, CreatedAt: from.Add(18 * time.Hour),
		Status: trivia.IssueFailed, Error: "досье: Википедия: 503 Service Unavailable"}

	return []trivia.Issue{manul, akodon, panda, rhino, bat, beaver}
}
