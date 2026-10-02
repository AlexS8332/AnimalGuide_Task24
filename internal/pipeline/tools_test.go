package pipeline_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// ------------------------------------------------------------------ фейки

// fakeSources — Checker, Collector и Википедия без сети.
type fakeSources struct {
	mu       sync.Mutex
	reason   string // "" — вид пригоден; иначе причина отказа
	noWiki   bool   // при отказе нет и статьи
	minOcc   []int  // пороги, с которыми звали Checker
	collects int
	fetchers int
}

func (s *fakeSources) apply(d pipeline.Deps) pipeline.Deps {
	d.NewFetcher = func() *tools.Fetcher {
		s.mu.Lock()
		s.fetchers++
		s.mu.Unlock()
		return tools.NewFetcher()
	}
	d.Checker = func(*tools.Fetcher) trivia.Checker { return fakeChecker{s} }
	d.Collector = func(*tools.Fetcher) trivia.Collector { return fakeCollector{s} }
	d.Wiki = func(*tools.Fetcher) pipeline.Wiki { return fakeWiki{} }
	return d
}

type fakeChecker struct{ s *fakeSources }

func (c fakeChecker) Check(_ context.Context, sp mdd.Species, minOcc int) (trivia.Eligibility, error) {
	c.s.mu.Lock()
	c.s.minOcc = append(c.s.minOcc, minOcc)
	reason, noWiki := c.s.reason, c.s.noWiki
	c.s.mu.Unlock()
	e := trivia.Eligibility{SpeciesID: sp.ID, SciName: sp.SciName, CheckedAt: time.Now(),
		GBIFKey: 42, Occurrences: 1234}
	if !noWiki {
		e.WikiLang, e.WikiTitle, e.WikiURL = "ru", "Манул", "https://ru.wikipedia.org/wiki/Манул"
	}
	if reason != "" {
		e.Reason = reason
		return e, nil
	}
	e.OK = true
	return e, nil
}

type fakeCollector struct{ s *fakeSources }

func (c fakeCollector) Collect(_ context.Context, p trivia.Pick, sp mdd.Species) (trivia.Dossier, error) {
	c.s.mu.Lock()
	c.s.collects++
	c.s.mu.Unlock()
	if p.Eligibility.WikiTitle == "" {
		return trivia.Dossier{}, errors.New("сборщику нужна статья из проверки")
	}
	return trivia.Dossier{
		Pick: p, Species: sp, NameRu: "Манул",
		Materials: []trivia.Material{
			{ID: "S1", Kind: trivia.KindMDD, Title: "MDD: " + sp.SciName, URL: "https://www.mammaldiversity.org/taxon/1006010",
				Text: sp.SciName + ", Felidae."},
			{ID: "S2", Kind: trivia.KindWikipedia, Title: "Манул", URL: "https://ru.wikipedia.org/wiki/Манул_(кошка)",
				Text: "Манул живёт в степях и горах Центральной Азии."},
		},
		Observations: trivia.Observations{GBIFKey: 42, Total: 1234, WindowDays: 30, Recent: 7},
		CollectedAt:  time.Date(2026, 9, 25, 11, 59, 0, 0, time.UTC),
	}, nil
}

// fakeWiki — русская Википедия: «манул» находится, вступление с биномом.
type fakeWiki struct{}

func (fakeWiki) Search(_ context.Context, q string) ([]tools.SearchHit, error) {
	switch strings.ToLower(q) {
	case "манул", "палласов кот":
		return []tools.SearchHit{{Title: "Манулы (род)"}, {Title: "Манул"}}, nil
	case "единорог":
		return []tools.SearchHit{{Title: "Единорог"}}, nil
	}
	return nil, nil
}

func (fakeWiki) Article(_ context.Context, title string) (*tools.Article, error) {
	switch title {
	case "Манулы (род)":
		// Род, а не вид: латинского бинома из MDD тут нет — берётся вторая статья.
		return &tools.Article{Title: title, Intro: "Манулы (лат. Otocolobus) — род кошачьих."}, nil
	case "Манул":
		return &tools.Article{Title: title,
			Intro: "Ману́л, или палласов кот (лат. Otocolobus manul) — хищное млекопитающее семейства кошачьих."}, nil
	case "Единорог":
		return &tools.Article{Title: title, Intro: "Единорог — мифическое существо, Monoceros."}, nil
	}
	return nil, fmt.Errorf("статьи «%s» нет", title)
}

// fakeLLM — редактор пишет пять фактов со ссылкой на S2, проверяющий
// подтверждает всё (reject=false) или ничего.
func fakeLLM(reject bool) *llmtest.Fake {
	return &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		var resp llm.Response
		switch req.Messages[0].Content {
		case trivia.EditorSystem:
			var facts []string
			for i := 1; i <= 5; i++ {
				facts = append(facts, fmt.Sprintf(`{"text":"Факт номер %d о мануле.","sources":["S2"]}`, i))
			}
			resp = llmtest.Text(`{"title":"Манул — кот с круглыми зрачками","lead":"Манул — дикая кошка степей.","facts":[` +
				strings.Join(facts, ",") + `]}`)
		case trivia.VerifySystem:
			var v []string
			for i := 1; i <= 6; i++ {
				if reject {
					v = append(v, fmt.Sprintf(`{"n":%d,"ok":false,"reason":"в S2 этого нет"}`, i))
				} else {
					v = append(v, fmt.Sprintf(`{"n":%d,"ok":true}`, i))
				}
			}
			resp = llmtest.Text("[" + strings.Join(v, ",") + "]")
		default:
			return llm.Response{}, errors.New("неожиданный запрос")
		}
		resp.Usage = llm.Usage{Prompt: 2000, Completion: 400, Total: 2400}
		return resp, nil
	}}
}

// testEnv — инструменты на Memory-хранилищах и фейках.
type testEnv struct {
	deps    pipeline.Deps
	tools   map[string]tools.Tool
	src     *fakeSources
	llm     *llmtest.Fake
	picks   *trivia.Memory
	arts    *pipeline.Memory
	mu      sync.Mutex
	records []pipeline.RunRecord
	budget  error
}

var testNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	species := mdd.NewMemory()
	if err := species.Replace(context.Background(), mddtest.Sample()); err != nil {
		t.Fatal(err)
	}
	e := &testEnv{src: &fakeSources{}, llm: fakeLLM(false), picks: trivia.NewMemory(), arts: pipeline.NewMemory()}
	e.deps = e.src.apply(pipeline.Deps{
		Species:     species,
		Artifacts:   e.arts,
		Picks:       e.picks,
		PickOptions: trivia.PickOptions{NoRepeat: -1, CheckTTL: -1, MinOccurrences: 50},
		LLM:         e.llm,
		ExportDir:   filepath.Join(t.TempDir(), "exports"),
		Budget: func(context.Context) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.budget
		},
		Record: func(_ context.Context, r pipeline.RunRecord) {
			e.mu.Lock()
			e.records = append(e.records, r)
			e.mu.Unlock()
		},
		Now: func() time.Time { return testNow },
	})
	e.rebuild()
	return e
}

func (e *testEnv) rebuild() {
	e.tools = map[string]tools.Tool{}
	for _, tl := range pipeline.Tools(e.deps) {
		e.tools[tl.Spec().Name] = tl
	}
}

// call зовёт инструмент в процессе и разбирает конверт ответа.
func (e *testEnv) call(t *testing.T, name string, args any) (pipeline.Envelope, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.tools[name].Call(context.Background(), raw)
	if err != nil {
		return pipeline.Envelope{}, err
	}
	var env pipeline.Envelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%s: ответ не конверт: %v\n%s", name, err, out)
	}
	return env, nil
}

func (e *testEnv) must(t *testing.T, name string, args any) pipeline.Envelope {
	t.Helper()
	env, err := e.call(t, name, args)
	if err != nil {
		t.Fatalf("%s(%v): %v", name, args, err)
	}
	return env
}

// ------------------------------------------------------------------ цепочка

func TestToolsSpecs(t *testing.T) {
	ts := pipeline.Tools(pipeline.Deps{})
	if got := tools.Names(ts); strings.Join(got, ",") != strings.Join(pipeline.ToolNames, ",") {
		t.Fatalf("инструменты %v, ждали %v", got, pipeline.ToolNames)
	}
	write := map[string]bool{pipeline.ToolSearch: false, pipeline.ToolSummarize: true, pipeline.ToolSaveFile: true}
	next := map[string]string{pipeline.ToolSearch: pipeline.ToolSummarize, pipeline.ToolSummarize: pipeline.ToolSaveFile}
	for _, tl := range ts {
		s := tl.Spec()
		if s.Write != write[s.Name] || !s.Untrusted {
			t.Errorf("%s: Write=%v Untrusted=%v", s.Name, s.Write, s.Untrusted)
		}
		if n := next[s.Name]; n != "" && !strings.Contains(s.Description, "Следующий шаг — "+n) {
			t.Errorf("%s: в описании нет следующего шага %s", s.Name, n)
		}
		for _, w := range []string{"ref", "input", "digest"} {
			if !strings.Contains(s.Description, w) {
				t.Errorf("%s: в описании нет %q", s.Name, w)
			}
		}
		var schema map[string]any
		if err := json.Unmarshal(s.Parameters, &schema); err != nil {
			t.Errorf("%s: схема не JSON: %v", s.Name, err)
		}
	}
}

// Сквозная цепочка в процессе: search → summarize → save_to_file, данные
// передаются конвертом целиком (inline) или по ref.
func TestChain(t *testing.T) {
	for _, c := range []struct {
		pass, format string
	}{
		{pipeline.PassInline, pipeline.FormatMarkdown},
		{pipeline.PassRef, pipeline.FormatMarkdown},
		{pipeline.PassRef, pipeline.FormatJSON},
		{pipeline.PassInline, pipeline.FormatJSON},
	} {
		t.Run(c.pass+"-"+c.format, func(t *testing.T) {
			e := newTestEnv(t)
			dossier := e.must(t, pipeline.ToolSearch, map[string]any{"query": "манул"})
			if dossier.Kind != pipeline.KindDossier || dossier.Input != "" {
				t.Fatalf("search: %+v", dossier)
			}
			if err := dossier.Open(pipeline.KindDossier); err != nil {
				t.Fatalf("конверт search не открывается: %v", err)
			}
			var dd pipeline.Dossier
			if err := json.Unmarshal(dossier.Data, &dd); err != nil {
				t.Fatal(err)
			}
			if dd.Query != "манул" || dd.Resolved != "русская Википедия: Манул → Otocolobus manul" ||
				dd.Dossier.Species.ID != mddtest.Manul {
				t.Errorf("досье: query=%q resolved=%q species=%d", dd.Query, dd.Resolved, dd.Dossier.Species.ID)
			}
			if dossier.Summary != "Манул (Otocolobus manul): 2 материала, 1234 наблюдения GBIF" {
				t.Errorf("summary search: %q", dossier.Summary)
			}
			if e.llm.Calls() != 0 {
				t.Error("search звал модель")
			}

			step := func(env pipeline.Envelope) map[string]any {
				if c.pass == pipeline.PassRef {
					return map[string]any{"ref": env.Digest}
				}
				return map[string]any{"input": env}
			}
			facts := e.must(t, pipeline.ToolSummarize, step(dossier))
			if facts.Kind != pipeline.KindFacts || facts.Input != dossier.Digest {
				t.Fatalf("summarize: kind=%s input=%s, ждали вход %s", facts.Kind, facts.Input, dossier.Digest)
			}
			if err := facts.Open(pipeline.KindFacts); err != nil {
				t.Fatal(err)
			}
			if facts.CostUSD <= 0 || facts.Summary != "Манул (Otocolobus manul): 5 фактов подтверждено из 5" {
				t.Errorf("summarize: cost=%v summary=%q", facts.CostUSD, facts.Summary)
			}
			var ff pipeline.Facts
			if err := json.Unmarshal(facts.Data, &ff); err != nil {
				t.Fatal(err)
			}
			if len(ff.Issue.Facts) != 5 || ff.Issue.SpeciesID != mddtest.Manul || ff.Issue.ID != 0 {
				t.Errorf("выпуск: facts=%d species=%d id=%d", len(ff.Issue.Facts), ff.Issue.SpeciesID, ff.Issue.ID)
			}
			if len(e.records) != 1 || e.records[0].CostUSD != facts.CostUSD || e.records[0].Error != "" ||
				e.records[0].Detail != facts.Summary {
				t.Errorf("журнал расхода: %+v", e.records)
			}

			args := step(facts)
			args["format"] = c.format
			file := e.must(t, pipeline.ToolSaveFile, args)
			if file.Kind != pipeline.KindFile || file.Input != facts.Digest {
				t.Fatalf("save_to_file: kind=%s input=%s", file.Kind, file.Input)
			}
			var fd pipeline.File
			if err := json.Unmarshal(file.Data, &fd); err != nil {
				t.Fatal(err)
			}
			wantName := "otocolobus-manul-20260925-120000." + c.format
			if fd.Path != "exports/"+wantName || fd.Format != c.format {
				t.Errorf("файл: %+v", fd)
			}
			if strings.Join(fd.Chain, ",") != dossier.Digest+","+facts.Digest {
				t.Errorf("цепочка %v", fd.Chain)
			}
			body, err := os.ReadFile(filepath.Join(e.deps.ExportDir, wantName))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			if hex.EncodeToString(sum[:]) != fd.SHA256 || len(body) != fd.Bytes {
				t.Errorf("sha256/размер файла на диске не совпали: %s %d, в конверте %s %d",
					hex.EncodeToString(sum[:]), len(body), fd.SHA256, fd.Bytes)
			}
			for _, d := range []string{dossier.Digest, facts.Digest} {
				if !strings.Contains(string(body), d) {
					t.Errorf("в файле нет отпечатка %s", d)
				}
			}
			if !strings.HasPrefix(string(body), fd.Preview[:20]) {
				t.Errorf("preview не начало файла: %q", fd.Preview)
			}
			switch c.format {
			case pipeline.FormatMarkdown:
				for _, w := range []string{"# Манул — кот с круглыми зрачками", "*Otocolobus manul*", "Факт номер 3 о мануле.",
					"[S2](https://ru.wikipedia.org/wiki/Манул_%28кошка%29)", "## Источники", "Цепочка: досье sha256:"} {
					if !strings.Contains(string(body), w) {
						t.Errorf("в Markdown нет %q:\n%s", w, body)
					}
				}
			case pipeline.FormatJSON:
				var js struct {
					Issue trivia.Issue `json:"issue"`
					Chain []string     `json:"chain"`
				}
				if err := json.Unmarshal(body, &js); err != nil {
					t.Fatalf("JSON-файл не разобрался: %v", err)
				}
				if len(js.Issue.Facts) != 5 || strings.Join(js.Chain, ",") != strings.Join(fd.Chain, ",") {
					t.Errorf("JSON-файл: %d фактов, цепочка %v", len(js.Issue.Facts), js.Chain)
				}
			}
			if !strings.HasPrefix(file.Summary, "exports/"+wantName+", ") {
				t.Errorf("summary save_to_file: %q", file.Summary)
			}
		})
	}
}

// ------------------------------------------------------------------ search

func TestSearchResolve(t *testing.T) {
	e := newTestEnv(t)
	for _, c := range []struct {
		args     any
		resolved string
	}{
		{map[string]any{"query": mddtest.Manul}, "mdd-id"}, // число, а не строка
		{map[string]any{"query": "1006010"}, "mdd-id"},
		{map[string]any{"query": "otocolobus manul"}, "латинское название"},
		{map[string]any{"query": "Otocolobus_manul"}, "латинское название"},
		{map[string]any{"query": "Pallas's Cat"}, "английское название"},
		{map[string]any{"query": "Палласов кот"}, "русская Википедия: Манул → Otocolobus manul"},
	} {
		env, err := e.call(t, pipeline.ToolSearch, c.args)
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		var dd pipeline.Dossier
		json.Unmarshal(env.Data, &dd)
		if dd.Resolved != c.resolved || dd.Dossier.Species.SciName != "Otocolobus manul" {
			t.Errorf("%v: resolved=%q вид %q", c.args, dd.Resolved, dd.Dossier.Species.SciName)
		}
	}
	// Названный человеком вид проверяется с порогом 1, а не 50.
	for _, m := range e.src.minOcc {
		if m != 1 {
			t.Errorf("порог наблюдений для запроса по имени %d", m)
		}
	}

	for _, c := range []struct {
		args any
		want string
	}{
		{map[string]any{}, "нужен один аргумент"},
		{map[string]any{"query": "манул", "random": true}, "не оба"},
		{map[string]any{"query": "Felis nonexistens"}, "в справочнике MDD нет"},
		{map[string]any{"query": "единорог"}, "латинского названия млекопитающего"},
		{map[string]any{"query": "чупакабра"}, "ни в MDD, ни в русской Википедии"},
		{map[string]any{"query": 99999999}, "mdd-id 99999999"},
		{map[string]any{"query": "манул", "extra": 1}, "аргументы не разобрались"},
	} {
		_, err := e.call(t, pipeline.ToolSearch, c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: ошибка %v, ждали %q", c.args, err, c.want)
		}
	}
}

func TestSearchRandom(t *testing.T) {
	e := newTestEnv(t)
	env := e.must(t, pipeline.ToolSearch, map[string]any{"random": true})
	var dd pipeline.Dossier
	if err := json.Unmarshal(env.Data, &dd); err != nil {
		t.Fatal(err)
	}
	if dd.Query != "" || !strings.HasPrefix(dd.Resolved, "случайный вид") || dd.Dossier.Species.SciName == "" {
		t.Errorf("случайный вид: %+v", dd)
	}
	// Порог — Picker'а (50), а выбор в журнал выборов ленты не пишется.
	if len(e.src.minOcc) == 0 || e.src.minOcc[0] != 50 {
		t.Errorf("порог случайного выбора: %v", e.src.minOcc)
	}
	if picks, _ := e.picks.Picks(context.Background(), 0); len(picks) != 0 {
		t.Errorf("выбор конвейера попал в журнал выборов: %d", len(picks))
	}
	if _, err := e.arts.Get(context.Background(), env.Digest); err != nil {
		t.Errorf("досье не в хранилище: %v", err)
	}
}

func TestSearchNotEligible(t *testing.T) {
	e := newTestEnv(t)
	e.src.reason, e.src.noWiki = trivia.ReasonNoArticle, true
	_, err := e.call(t, pipeline.ToolSearch, map[string]any{"query": "Otocolobus manul"})
	if err == nil || !strings.Contains(err.Error(), "нет статьи") || !strings.Contains(err.Error(), "Otocolobus manul") {
		t.Errorf("непригодный вид: %v", err)
	}
	if e.src.collects != 0 {
		t.Error("досье собиралось у непригодного вида")
	}
	// Статья есть, наблюдений мало — названный вид всё равно собирается.
	e.src.reason, e.src.noWiki = trivia.ReasonFewRecords, false
	if _, err := e.call(t, pipeline.ToolSearch, map[string]any{"query": "Otocolobus manul"}); err != nil {
		t.Errorf("мало наблюдений при статье: %v", err)
	}
}

// ------------------------------------------------------------ передача данных

func TestTransferErrors(t *testing.T) {
	e := newTestEnv(t)
	dossier := e.must(t, pipeline.ToolSearch, map[string]any{"query": "Otocolobus manul"})
	facts := e.must(t, pipeline.ToolSummarize, map[string]any{"ref": dossier.Digest})

	corrupt := func(env pipeline.Envelope) pipeline.Envelope {
		s := string(env.Data)
		i := strings.Index(s, "Манул")
		if i < 0 {
			t.Fatal("в данных нет слова для порчи")
		}
		env.Data = json.RawMessage(s[:i] + "Лунам" + s[i+len("Манул"):])
		return env
	}
	unknown := "sha256:" + strings.Repeat("ab", 32)
	calls := e.llm.Calls()

	for _, c := range []struct {
		name string
		tool string
		args map[string]any
		want error
		text string
	}{
		{"досье испорчено", pipeline.ToolSummarize, map[string]any{"input": corrupt(dossier)}, pipeline.ErrDigest, ""},
		{"факты испорчены", pipeline.ToolSaveFile, map[string]any{"input": corrupt(facts)}, pipeline.ErrDigest, ""},
		{"факты в summarize", pipeline.ToolSummarize, map[string]any{"ref": facts.Digest}, pipeline.ErrKind, "search"},
		{"досье в save_to_file", pipeline.ToolSaveFile, map[string]any{"input": dossier}, pipeline.ErrKind, "сначала"},
		{"досье по ref в save_to_file", pipeline.ToolSaveFile, map[string]any{"ref": dossier.Digest}, pipeline.ErrKind, "summarize"},
		{"неизвестный ref", pipeline.ToolSummarize, map[string]any{"ref": unknown}, pipeline.ErrRef, ""},
		{"неизвестный ref файла", pipeline.ToolSaveFile, map[string]any{"ref": unknown}, pipeline.ErrRef, ""},
		{"оба", pipeline.ToolSummarize, map[string]any{"ref": dossier.Digest, "input": dossier}, pipeline.ErrInput, "оба"},
		{"ни одного", pipeline.ToolSummarize, map[string]any{}, pipeline.ErrInput, ""},
		{"ни одного у файла", pipeline.ToolSaveFile, map[string]any{"format": "md"}, pipeline.ErrInput, ""},
		{"input не конверт", pipeline.ToolSummarize, map[string]any{"input": map[string]any{"kind": "dossier"}}, pipeline.ErrInput, ""},
	} {
		_, err := e.call(t, c.tool, c.args)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, ждали %v", c.name, err, c.want)
			continue
		}
		// Текст начинается с сообщения ошибки передачи: так его узнают
		// исполнитель и модель, получив через MCP только строку.
		if !strings.HasPrefix(err.Error(), c.want.Error()) || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: текст %q", c.name, err)
		}
	}
	if e.llm.Calls() != calls {
		t.Error("модель звалась на испорченном входе")
	}

	// Конверт, пришедший строкой с JSON, и переупаковка JSON (другой
	// порядок ключей, пробелы) — те же данные, отпечаток сходится.
	var generic any
	json.Unmarshal(facts.Data, &generic)
	repacked, _ := json.MarshalIndent(generic, "", "   ")
	env := facts
	env.Data = repacked
	raw, _ := json.Marshal(env)
	if _, err := e.call(t, pipeline.ToolSaveFile, map[string]any{"input": string(raw)}); err != nil {
		t.Errorf("переупакованный конверт строкой: %v", err)
	}
}

// ------------------------------------------------------------------ файл

func TestSaveFileNames(t *testing.T) {
	e := newTestEnv(t)
	dossier := e.must(t, pipeline.ToolSearch, map[string]any{"query": "Otocolobus manul"})
	facts := e.must(t, pipeline.ToolSummarize, map[string]any{"ref": dossier.Digest})
	parent := filepath.Dir(e.deps.ExportDir)

	for _, name := range []string{"../evil", `..\evil`, "../../etc/passwd", "/etc/passwd", `C:\Windows\evil`,
		"a/b", "sub\\file", "..", "evil..md", "c:evil"} {
		_, err := e.call(t, pipeline.ToolSaveFile, map[string]any{"ref": facts.Digest, "name": name})
		if err == nil || !strings.Contains(err.Error(), "без каталогов") {
			t.Errorf("name %q: %v", name, err)
		}
	}
	entries, _ := os.ReadDir(parent)
	for _, en := range entries {
		if en.Name() != "exports" {
			t.Errorf("рядом с каталогом выгрузок появилось %s", en.Name())
		}
	}

	for _, c := range []struct{ name, format, want string }{
		{"Манул: факты!", "md", "manul-fakty.md"},
		{"manul.md", "md", "manul.md"},
		{"Manul_Facts 2026", "json", "manul_facts-2026.json"},
		{"manul.json", "", "manul-json.md"},
		{"ёжик в тумане", "markdown", "ezhik-v-tumane.md"},
	} {
		args := map[string]any{"ref": facts.Digest, "name": c.name}
		if c.format != "" {
			args["format"] = c.format
		}
		env, err := e.call(t, pipeline.ToolSaveFile, args)
		if err != nil {
			t.Errorf("name %q: %v", c.name, err)
			continue
		}
		var fd pipeline.File
		json.Unmarshal(env.Data, &fd)
		if fd.Path != "exports/"+c.want {
			t.Errorf("name %q → %s, ждали exports/%s", c.name, fd.Path, c.want)
		}
		if _, err := os.Stat(filepath.Join(e.deps.ExportDir, c.want)); err != nil {
			t.Errorf("файла %s нет: %v", c.want, err)
		}
	}
	for _, bad := range []map[string]any{
		{"ref": facts.Digest, "name": "!!!"},
		{"ref": facts.Digest, "format": "pdf"},
	} {
		if _, err := e.call(t, pipeline.ToolSaveFile, bad); err == nil {
			t.Errorf("%v принят", bad)
		}
	}
	// Временных файлов после записи не остаётся.
	entries, _ = os.ReadDir(e.deps.ExportDir)
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".tmp-") {
			t.Errorf("остался временный файл %s", en.Name())
		}
	}
}

// ------------------------------------------------------------ лимит и расход

func TestSummarizeBudget(t *testing.T) {
	e := newTestEnv(t)
	dossier := e.must(t, pipeline.ToolSearch, map[string]any{"query": "Otocolobus manul"})
	e.budget = errors.New("дневной лимит расходов на модель исчерпан: потрачено $0.5000 из $0.50")
	_, err := e.call(t, pipeline.ToolSummarize, map[string]any{"ref": dossier.Digest})
	if err == nil || !strings.Contains(err.Error(), "лимит") {
		t.Fatalf("лимит исчерпан, а summarize: %v", err)
	}
	if e.llm.Calls() != 0 || len(e.records) != 0 {
		t.Errorf("при исчерпанном лимите: модель %d раз, записей журнала %d", e.llm.Calls(), len(e.records))
	}
}

func TestSummarizeNoFacts(t *testing.T) {
	e := newTestEnv(t)
	e.llm = fakeLLM(true)
	e.deps.LLM = e.llm
	e.rebuild()
	dossier := e.must(t, pipeline.ToolSearch, map[string]any{"query": "Otocolobus manul"})
	_, err := e.call(t, pipeline.ToolSummarize, map[string]any{"input": dossier})
	if err == nil || !strings.Contains(err.Error(), "ни одного подтверждённого факта") {
		t.Fatalf("ноль фактов: %v", err)
	}
	// Деньги ушли — запись в журнал со сбоем и расходом.
	if len(e.records) != 1 || e.records[0].CostUSD <= 0 || e.records[0].Error == "" ||
		!strings.Contains(e.records[0].Detail, "0 фактов подтверждено из 5") {
		t.Errorf("журнал сбоя: %+v", e.records)
	}
}
