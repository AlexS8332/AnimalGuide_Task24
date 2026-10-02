package mdd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Имена инструментов MDD.
const (
	ToolGet     = "mdd_get"
	ToolSearch  = "mdd_search"
	ToolChanges = "mdd_changes"
)

// ToolNames — инструменты MDD в порядке показа.
var ToolNames = []string{ToolGet, ToolSearch, ToolChanges}

// Пределы ответов инструментов.
const (
	toolTextMax        = 600 // типовое местонахождение, заметки
	toolReferenceMax   = 300 // ссылка на публикацию в изменении
	toolSearchDefault  = 20
	toolSearchMax      = 100
	toolChangesDefault = 20
	toolChangesMax     = 100
)

// toolAbout — общее начало описаний: что это за источник.
const toolAbout = "Mammal Diversity Database (MDD) — эталонный список видов млекопитающих " +
	"Американского общества маммалогов (локальная копия последнего релиза). Только млекопитающие. " +
	"Названия в MDD латинские и английские, русских нет: для русского названия сначала найди " +
	"латинское другими инструментами (Википедия, GBIF). "

// Tools — три инструмента справочного слоя MDD над хранилищем: вид, поиск,
// изменения систематики. Работают и в процессе, и через MCP-сервер.
func Tools(st Store) []tools.Tool {
	return []tools.Tool{toolGet(st), toolSearch(st), toolChanges(st)}
}

func toolGet(st Store) tools.Tool {
	return tools.Func{
		S: tools.Spec{
			Name: ToolGet,
			Description: toolAbout +
				"Карточка одного вида: проверенная систематика (отряд, семейство, подсемейство, род, автор и год описания), " +
				"статус МСОП, признаки вымершего и домашнего вида, страны распространения (countries — точно, " +
				"countries_uncertain — под вопросом), континенты, биогеографические области, типовое местонахождение, " +
				"заметки о распространении и систематике. Звать, когда латинское или английское название вида уже известно " +
				"и нужны сведения из эталонного списка. Ровно один аргумент: id (mdd-id) или name — точное латинское " +
				"или английское название без учёта регистра. Возвращает поля вида, ссылку url на страницу MDD и source с версией релиза.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "integer", "minimum": 1, "description": "mdd-id вида, например 1006010"},
    "name": {"type": "string", "description": "Точное латинское (Otocolobus manul) или английское (Pallas's Cat) название"}
  },
  "additionalProperties": false
}`),
			Untrusted: true,
			Via:       tools.ViaLocal,
		},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				ID   *int   `json:"id"`
				Name string `json:"name"`
			}
			if err := toolParse(args, &in); err != nil {
				return "", err
			}
			in.Name = strings.TrimSpace(in.Name)
			switch {
			case in.ID == nil && in.Name == "":
				return "", errors.New("нужен один аргумент: id (mdd-id) или name (латинское или английское название)")
			case in.ID != nil && in.Name != "":
				return "", errors.New("нужен ровно один аргумент: id или name, не оба")
			case in.ID != nil && *in.ID <= 0:
				return "", errors.New("id должен быть положительным числом (mdd-id)")
			}
			rel, err := toolRelease(ctx, st)
			if err != nil {
				return "", err
			}
			var s Species
			if in.ID != nil {
				s, err = st.Get(ctx, *in.ID)
			} else {
				s, err = st.Find(ctx, in.Name)
			}
			if errors.Is(err, ErrNotFound) {
				if in.ID != nil {
					return "", fmt.Errorf("вида с mdd-id %d в MDD %s нет. Найди вид через %s по названию", *in.ID, rel.Version, ToolSearch)
				}
				return "", fmt.Errorf("вид «%s» в MDD %s не найден: %s ищет только точное латинское или английское название. "+
					"Попробуй %s по части названия или сначала узнай латинское название — русских названий в MDD нет",
					in.Name, rel.Version, ToolGet, ToolSearch)
			}
			if err != nil {
				return "", fmt.Errorf("MDD: %w", err)
			}
			s.TypeLocality = tools.Truncate(s.TypeLocality, toolTextMax)
			s.DistributionNotes = tools.Truncate(s.DistributionNotes, toolTextMax)
			s.TaxonomyNotes = tools.Truncate(s.TaxonomyNotes, toolTextMax)
			return tools.Result(struct {
				Species
				URL    string `json:"url"`
				Source string `json:"source"`
			}{s, s.URL(), toolSource(rel)})
		},
	}
}

// toolSearchRow — строка выдачи mdd_search.
type toolSearchRow struct {
	ID         int    `json:"id"`
	SciName    string `json:"sci_name"`
	CommonName string `json:"common_name,omitempty"`
	Family     string `json:"family"`
	IUCN       string `json:"iucn,omitempty"`
	URL        string `json:"url"`
}

// toolIUCN — допустимые статусы МСОП в MDD.
var toolIUCN = []string{"LC", "NT", "VU", "EN", "CR", "EW", "EX", "DD", "NE"}

func toolSearch(st Store) tools.Tool {
	return tools.Func{
		S: tools.Spec{
			Name: ToolSearch,
			Description: toolAbout +
				"Поиск видов по части названия и фильтрам: отряд, семейство, род (латинские, точное совпадение), " +
				"страна (английское название, как в MDD: Kazakhstan, Russia, United States; учитываются и страны под вопросом), " +
				"биогеографическая область (Palearctic, Nearctic, Afrotropic, Indomalaya, Australasia, Neotropic, Oceania), " +
				"статусы МСОП, вымершие, домашние. Регистр не важен. Звать, чтобы найти вид, когда точное название неизвестно, " +
				"или чтобы перечислить виды группы, страны или статуса. Без аргументов — первые виды в систематическом порядке. " +
				"Возвращает total (сколько всего подходит), returned, краткие строки (id, sci_name, common_name, family, iucn, url) " +
				"в систематическом порядке MDD и подсказку о следующей странице. Полная карточка вида — " + ToolGet + " по id.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "Часть латинского или английского названия: manul, Panthera, snow leopard"},
    "order": {"type": "string", "description": "Отряд, латинское название: Carnivora"},
    "family": {"type": "string", "description": "Семейство, латинское название: Felidae"},
    "genus": {"type": "string", "description": "Род, латинское название: Panthera"},
    "country": {"type": "string", "description": "Страна по-английски, как в MDD: Kazakhstan"},
    "realm": {"type": "string", "description": "Биогеографическая область: Palearctic"},
    "iucn": {"type": "array", "items": {"type": "string", "enum": ["LC", "NT", "VU", "EN", "CR", "EW", "EX", "DD", "NE"]}, "description": "Статусы МСОП, подходит любой из них"},
    "extinct": {"type": "boolean", "description": "true — только вымершие, false — только ныне живущие; не задано — любые"},
    "domestic": {"type": "boolean", "description": "true — только домашние, false — только дикие; не задано — любые"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 100, "description": "Сколько видов вернуть, по умолчанию 20, не больше 100"},
    "offset": {"type": "integer", "minimum": 0, "description": "Сколько видов пропустить: для следующей страницы"}
  },
  "additionalProperties": false
}`),
			Untrusted: true,
			Via:       tools.ViaLocal,
		},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Text     string   `json:"text"`
				Order    string   `json:"order"`
				Family   string   `json:"family"`
				Genus    string   `json:"genus"`
				Country  string   `json:"country"`
				Realm    string   `json:"realm"`
				IUCN     []string `json:"iucn"`
				Extinct  *bool    `json:"extinct"`
				Domestic *bool    `json:"domestic"`
				Limit    int      `json:"limit"`
				Offset   int      `json:"offset"`
			}
			if err := toolParse(args, &in); err != nil {
				return "", err
			}
			if in.Limit < 0 || in.Offset < 0 {
				return "", errors.New("limit и offset не могут быть отрицательными")
			}
			limit := in.Limit
			if limit == 0 {
				limit = toolSearchDefault
			}
			limit = min(limit, toolSearchMax)
			var iucn []string
			for _, s := range in.IUCN {
				s = strings.ToUpper(strings.TrimSpace(s))
				if s == "" {
					continue
				}
				if !toolContains(toolIUCN, s) {
					return "", fmt.Errorf("неизвестный статус МСОП %q; допустимы: %s", s, strings.Join(toolIUCN, ", "))
				}
				iucn = append(iucn, s)
			}
			rel, err := toolRelease(ctx, st)
			if err != nil {
				return "", err
			}
			list, total, err := st.Search(ctx, Query{
				Text: strings.TrimSpace(in.Text), Order: strings.TrimSpace(in.Order),
				Family: strings.TrimSpace(in.Family), Genus: strings.TrimSpace(in.Genus),
				Country: strings.TrimSpace(in.Country), Realm: strings.TrimSpace(in.Realm),
				IUCN: iucn, Extinct: in.Extinct, Domestic: in.Domestic,
				Limit: limit, Offset: in.Offset,
			})
			if err != nil {
				return "", fmt.Errorf("MDD: %w", err)
			}
			rows := make([]toolSearchRow, 0, len(list))
			for _, s := range list {
				rows = append(rows, toolSearchRow{ID: s.ID, SciName: s.SciName, CommonName: s.CommonName,
					Family: s.Family, IUCN: s.IUCN, URL: s.URL()})
			}
			out := struct {
				Source     string          `json:"source"`
				Total      int             `json:"total"`
				Returned   int             `json:"returned"`
				Offset     int             `json:"offset"`
				Species    []toolSearchRow `json:"species"`
				NextOffset int             `json:"next_offset,omitempty"`
				Hint       string          `json:"hint,omitempty"`
			}{Source: toolSource(rel), Total: total, Returned: len(rows), Offset: in.Offset, Species: rows}
			switch next := in.Offset + len(rows); {
			case total == 0:
				out.Hint = "Ничего не найдено. В MDD только латинские и английские названия, русских нет; " +
					"страна — по-английски, как в MDD (Kazakhstan); отряд, семейство и род — латинские, точным совпадением."
			case len(rows) == 0:
				out.Hint = fmt.Sprintf("offset за пределами выдачи: всего подходит %d.", total)
			case next < total:
				out.NextOffset = next
				out.Hint = fmt.Sprintf("Показаны %d–%d из %d. Следующая страница — тот же запрос с offset=%d.",
					in.Offset+1, next, total, next)
			}
			return tools.Result(out)
		},
	}
}

func toolChanges(st Store) tools.Tool {
	return tools.Func{
		S: tools.Spec{
			Name: ToolChanges,
			Description: toolAbout +
				"Изменения систематики в текущем релизе MDD по сравнению с предыдущим: новые виды (de novo), " +
				"разделения (split), объединения (lump), переименования (name change), переносы в другой род " +
				"(genus change) и другие. Звать, когда спрашивают, что нового в систематике млекопитающих, " +
				"или почему название вида изменилось. Возвращает версию релиза, предыдущую версию и список изменений: " +
				"old_name (пусто — вид новый), new_name, comment, category, reference.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "category": {"type": "string", "description": "Категория изменения: de novo, split, lump, name change, genus change, subgenus change, subfamily change, family split, genus split, genus lump, de novo genus, deextinction. Не задано — все"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 100, "description": "Сколько изменений вернуть, по умолчанию 20, не больше 100"}
  },
  "additionalProperties": false
}`),
			Untrusted: true,
			Via:       tools.ViaLocal,
		},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Category string `json:"category"`
				Limit    int    `json:"limit"`
			}
			if err := toolParse(args, &in); err != nil {
				return "", err
			}
			if in.Limit < 0 {
				return "", errors.New("limit не может быть отрицательным")
			}
			limit := in.Limit
			if limit == 0 {
				limit = toolChangesDefault
			}
			limit = min(limit, toolChangesMax)
			category := strings.ToLower(strings.TrimSpace(in.Category))
			rel, err := toolRelease(ctx, st)
			if err != nil {
				return "", err
			}
			list, err := st.Changes(ctx, category, limit)
			if err != nil {
				return "", fmt.Errorf("MDD: %w", err)
			}
			changes := make([]Change, 0, len(list))
			for _, c := range list {
				c.Comment = tools.Truncate(c.Comment, toolTextMax)
				c.Reference = tools.Truncate(c.Reference, toolReferenceMax)
				changes = append(changes, c)
			}
			out := struct {
				Source      string   `json:"source"`
				Version     string   `json:"version"`
				PrevVersion string   `json:"prev_version,omitempty"`
				Category    string   `json:"category,omitempty"`
				Returned    int      `json:"returned"`
				Changes     []Change `json:"changes"`
				Hint        string   `json:"hint,omitempty"`
			}{Source: toolSource(rel), Version: rel.Version, PrevVersion: rel.PrevVersion,
				Category: category, Returned: len(changes), Changes: changes}
			switch {
			case len(changes) == 0 && category != "":
				out.Hint = "Изменений этой категории нет. Категории MDD: de novo, split, lump, name change, genus change, " +
					"subgenus change, subfamily change, family split, genus split, genus lump, de novo genus, deextinction."
			case len(changes) == limit:
				out.Hint = fmt.Sprintf("Показаны первые %d; изменений может быть больше — увеличь limit (до %d) или укажи category.",
					limit, toolChangesMax)
			}
			return tools.Result(out)
		},
	}
}

// toolRelease — текущий релиз; пустая база — понятная модели ошибка.
func toolRelease(ctx context.Context, st Store) (Release, error) {
	rel, err := st.Release(ctx)
	if errors.Is(err, ErrNotFound) {
		return Release{}, errors.New("справочник MDD ещё не загружен: база видов млекопитающих пуста. " +
			"Воспользуйся другими источниками")
	}
	if err != nil {
		return Release{}, fmt.Errorf("MDD: %w", err)
	}
	return rel, nil
}

// toolSource — «MDD v2.5 (2026-07-28)».
func toolSource(rel Release) string {
	if rel.Date == "" {
		return "MDD " + rel.Version
	}
	return fmt.Sprintf("MDD %s (%s)", rel.Version, rel.Date)
}

// toolParse — tools.ParseArgs со строгостью схемы (additionalProperties:
// false): лишний аргумент — ошибка, а не молча выброшенное условие поиска.
func toolParse(args json.RawMessage, target any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		return nil
	}
	if err := tools.ParseArgs(args, target); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("аргументы не разобрались: %w", err)
	}
	return nil
}

func toolContains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
