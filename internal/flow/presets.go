package flow

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/notes"
)

// Имена серверов реестра, как в встроенной конфигурации hub. Заготовка
// проверяет маршрут по ним: вызов mdd_get, ушедший не на daemon, — провал,
// даже если ответ пришёл.
const (
	ServerSources = "sources"
	ServerDaemon  = "daemon"
	ServerNotes   = "notes"
)

// SpeciesMark — место вида в условиях на аргументы заготовки. Spec
// заготовки параметрическая: Run подставляет вид прогона (SpecFor), в
// регулярное выражение — экранированным.
const SpeciesMark = "{species}"

// passportTask — задача модели словами: цель, что должно получиться и из
// чего это собирать. Жёсткого порядка вызовов нет: модель вправе
// переставлять независимые шаги, порядок проверяется зависимостями данных.
// Сказано только то, без чего проверка данных не сойдётся: id карточки MDD
// берётся из выдачи поиска, usage_key — из сверки, notebook_id — из nb_open.
const passportTask = `Составь в блокноте натуралиста паспорт вида «%s».

Что должно получиться: закрытый блокнот о виде с разделами «Таксономия», «Описание» и «Интересные факты».

Откуда брать сведения — только из ответов инструментов:
- статья русской Википедии: найди её поиском и читай нужные разделы (section), а не статью целиком;
- карточка вида в эталонном списке млекопитающих MDD: найди вид поиском по латинскому или английскому названию и открой карточку по id из выдачи поиска;
- сверка латинского названия из карточки MDD с GBIF, затем дерево классификации и русские народные названия по usage_key из ответа сверки;
- выпуск «Интересных фактов» демона о виде — по mdd-id или латинскому названию из карточки MDD. Если выпуска о виде нет, это нормальный ответ: возьми интересные факты из статьи.

Сначала собери сведения, затем открой блокнот, добавь в него разделы и закрой его. В каждом разделе в cites перечисли инструменты, из успешных ответов которых взят его текст; среди всех ссылок должны быть и Википедия или GBIF, и MDD.
Когда блокнот закрыт, вызови flow_done с коротким итогом: путь к файлу и что вошло в блокнот.`

// Presets — заготовки флоу; первая — по умолчанию («passport»).
func Presets() []Preset {
	return []Preset{passport()}
}

// Find — заготовка по ID; пустой ID — первая.
func Find(id string) (Preset, error) {
	ps := Presets()
	id = strings.TrimSpace(id)
	if id == "" {
		return ps[0], nil
	}
	for _, p := range ps {
		if p.ID == id {
			return p, nil
		}
	}
	ids := make([]string, len(ps))
	for i, p := range ps {
		ids[i] = p.ID
	}
	return Preset{}, fmt.Errorf("%w %q; есть: %s", ErrNoPreset, id, strings.Join(ids, ", "))
}

// passport — «Паспорт вида в блокнот»: одиннадцать шагов на трёх серверах.
//
// Цепочки данных: search_wikipedia → read_wikipedia (заголовок из выдачи);
// mdd_search → mdd_get (id из выдачи) → match_taxon (латынь из карточки) →
// taxon_tree и vernacular_names (usage_key из сверки); mdd_get → facts_get
// (mdd-id или название из карточки); nb_open → nb_add, nb_close
// (notebook_id). Где данных нет, порядок держит Before: блокнот пишется
// после того, как сведения собраны.
//
// Условие на аргументы одно — поиск в Википедии по самому виду: латинское
// название для mdd_search у произвольного вида заготовка знать не может, а
// угадывать его регулярным выражением — значит проверять не модель, а
// собственные догадки.
func passport() Preset {
	step := func(id, server, tool string, min, max int) StepSpec {
		return StepSpec{ID: id, Server: server, Tool: tool, Min: min, Max: max}
	}
	search := step("wiki_search", ServerSources, "search_wikipedia", 1, 3)
	search.Args = map[string]Match{"query": {Regexp: "(?i)" + SpeciesMark}}
	facts := step("facts", ServerDaemon, "facts_get", 1, 2)
	facts.AllowError = true // выпуска о виде может и не быть — это законный ответ

	return Preset{
		ID:      "passport",
		Title:   "Паспорт вида в блокнот",
		Task:    passportTask,
		Species: "манул",
		Spec: Spec{
			Steps: []StepSpec{
				search,
				step("wiki_read", ServerSources, "read_wikipedia", 1, 4),
				step("mdd_search", ServerDaemon, "mdd_search", 1, 3),
				step("mdd_get", ServerDaemon, "mdd_get", 1, 2),
				step("match", ServerSources, "match_taxon", 1, 2),
				step("tree", ServerSources, "taxon_tree", 1, 2),
				step("vernacular", ServerSources, "vernacular_names", 1, 2),
				facts,
				step("nb_open", ServerNotes, notes.ToolOpen, 1, 1),
				step("nb_add", ServerNotes, notes.ToolAdd, 2, 5),
				step("nb_close", ServerNotes, notes.ToolClose, 1, 1),
			},
			Flows: []Flow{
				{From: "wiki_search", Path: "results.*.title", To: "wiki_read", Arg: "title", Mode: FlowEqFold},
				{From: "mdd_search", Path: "species.*.id", To: "mdd_get", Arg: "id", Mode: FlowEq},
				{From: "mdd_get", Path: "sci_name", To: "match", Arg: "scientific_name", Mode: FlowEqFold},
				{From: "match", Path: "usage_key", To: "tree", Arg: "usage_key", Mode: FlowEq},
				{From: "match", Path: "usage_key", To: "vernacular", Arg: "usage_key", Mode: FlowEq},
				// facts_get принимает вид в любом из видов карточки: mdd-id,
				// латинское или английское название. Альтернативы пути — через «|».
				{From: "mdd_get", Path: "id|sci_name|common_name", To: "facts", Arg: "species", Mode: FlowEqFold},
				{From: "nb_open", Path: "notebook_id", To: "nb_add", Arg: "notebook_id", Mode: FlowEq},
				{From: "nb_open", Path: "notebook_id", To: "nb_close", Arg: "notebook_id", Mode: FlowEq},
			},
			Before: []Order{
				{A: "nb_open", B: "nb_add"},
				{A: "nb_add", B: "nb_close"},
				{A: "wiki_read", B: "nb_add"},
				{A: "mdd_get", B: "nb_open"},
			},
			// Платные и чужие этому флоу инструменты: запуск выпуска, сводка,
			// конвейер демона. Реестр их модели не выдаёт (ИП-7), так что вызов
			// — это выдуманное имя, и проверка ловит его даже без маршрута.
			Forbidden:   []string{"run_now", "summary_build", "summarize", "save_to_file", "search"},
			MaxCalls:    22,
			MinServers:  3,
			CiteTool:    notes.ToolAdd,
			CiteArg:     "cites",
			CiteServers: []string{ServerSources, ServerDaemon},
		},
	}
}

// SpecFor — проверки заготовки для вида species (пусто — вид заготовки):
// SpeciesMark в условиях на аргументы заменяется видом, в регулярных
// выражениях — экранированным. Исходная Spec не меняется.
func (p Preset) SpecFor(species string) Spec {
	species = strings.TrimSpace(species)
	if species == "" {
		species = p.Species
	}
	s := p.Spec
	s.Steps = make([]StepSpec, len(p.Spec.Steps))
	for i, st := range p.Spec.Steps {
		if len(st.Args) > 0 {
			args := make(map[string]Match, len(st.Args))
			for k, m := range st.Args {
				m.Eq = strings.ReplaceAll(m.Eq, SpeciesMark, species)
				m.Regexp = strings.ReplaceAll(m.Regexp, SpeciesMark, regexp.QuoteMeta(species))
				args[k] = m
			}
			st.Args = args
		}
		s.Steps[i] = st
	}
	return s
}

// TaskFor — задача модели о виде species (пусто — вид заготовки).
func (p Preset) TaskFor(species string) string {
	species = strings.TrimSpace(species)
	if species == "" {
		species = p.Species
	}
	if !strings.Contains(p.Task, "%s") {
		return p.Task
	}
	return fmt.Sprintf(p.Task, species)
}
