package corpus

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
)

// MDDDocID — документ «Хищные по MDD» в корпусе.
const MDDDocID = "mdd-carnivora"

// MDDTitle — заголовок документа MDD.
const MDDTitle = "Хищные (Carnivora) в Mammal Diversity Database"

// mddOrder — отряд, которому посвящён документ.
const mddOrder = "Carnivora"

// mddEurasianFamilies — семейства хищных с видами в Евразии: по ним
// отдельные разделы со строкой на вид. Остальные семейства (ластоногие,
// енотовые, скунсовые, мадагаскарские виверры…) есть только в общей
// таблице: корпус — о хищных Евразии.
var mddEurasianFamilies = []string{
	"Felidae", "Canidae", "Mustelidae", "Ursidae",
	"Hyaenidae", "Ailuridae", "Viverridae", "Herpestidae",
}

// mddFamilyRu — русские названия семейств хищных. Неизвестное семейство
// (новое в будущем релизе) пишется только латынью.
var mddFamilyRu = map[string]string{
	"Felidae":        "Кошачьи",
	"Canidae":        "Псовые",
	"Mustelidae":     "Куньи",
	"Ursidae":        "Медвежьи",
	"Hyaenidae":      "Гиеновые",
	"Ailuridae":      "Пандовые",
	"Viverridae":     "Виверровые",
	"Herpestidae":    "Мангустовые",
	"Procyonidae":    "Енотовые",
	"Mephitidae":     "Скунсовые",
	"Otariidae":      "Ушастые тюлени",
	"Phocidae":       "Настоящие тюлени",
	"Odobenidae":     "Моржовые",
	"Eupleridae":     "Мадагаскарские виверры",
	"Nandiniidae":    "Нандиниевые",
	"Prionodontidae": "Линзанговые",
}

// mddIUCNRu — статусы МСОП по-русски.
var mddIUCNRu = map[string]string{
	"LC": "вызывающий наименьшие опасения",
	"NT": "близкий к уязвимому положению",
	"VU": "уязвимый",
	"EN": "вымирающий",
	"CR": "на грани исчезновения",
	"EW": "исчезнувший в дикой природе",
	"EX": "исчезнувший",
	"DD": "недостаточно данных",
	"NE": "не оценивался",
}

// mddPage — сколько видов просить у Search за раз: больше 100 хранилище
// не отдаёт.
const mddPage = 100

// MDDDoc собирает документ «Хищные (Carnivora) в Mammal Diversity Database»
// из справочника MDD — кодом, по шаблону, без модели (иначе база знаний
// пересказывала бы саму модель). Документ по-русски; латинские и английские
// названия — как в MDD.
//
// Состав:
//   - вступление: релиз (версия, дата, предыдущая версия), сколько видов
//     млекопитающих всего и сколько хищных, ссылка и лицензия CC BY 4.0;
//   - раздел «Семейства хищных»: таблица-абзацы «семейство — число видов,
//     число родов»;
//   - по разделу на каждое семейство, где есть виды Евразии (Felidae,
//     Canidae, Mustelidae, Ursidae, Hyaenidae, Ailuridae, Viverridae,
//     Herpestidae): роды и по строке на вид — латинское название, автор и
//     год, английское название, статус МСОП, вымерший ли;
//   - раздел «Изменения релиза в отряде»: строки Diff, относящиеся к хищным
//     (если их нет — так и сказано: «в релизе vX изменений в отряде нет»).
//
// Section.Path — ["Семейства хищных"], ["Кошачьи (Felidae)"], …
func MDDDoc(ctx context.Context, st mdd.Store) (Doc, error) {
	rel, err := st.Release(ctx)
	if err != nil {
		return Doc{}, fmt.Errorf("MDD: релиз: %w", err)
	}

	// Все хищные — постранично, в систематическом порядке MDD: в нём же
	// идут семейства и роды документа.
	var all []mdd.Species
	for {
		list, total, err := st.Search(ctx, mdd.Query{Order: mddOrder, Limit: mddPage, Offset: len(all)})
		if err != nil {
			return Doc{}, fmt.Errorf("MDD: хищные: %w", err)
		}
		all = append(all, list...)
		if len(list) == 0 || len(all) >= total {
			break
		}
	}
	if len(all) == 0 {
		return Doc{}, errors.New("MDD: в справочнике нет хищных")
	}

	type family struct {
		name    string
		species []mdd.Species
		genera  []string // в порядке появления
	}
	var families []*family
	byName := map[string]*family{}
	genera := map[string]bool{}
	for _, s := range all {
		f := byName[s.Family]
		if f == nil {
			f = &family{name: s.Family}
			byName[s.Family] = f
			families = append(families, f)
		}
		if n := len(f.species); n == 0 || f.species[n-1].Genus != s.Genus {
			if !containsString(f.genera, s.Genus) {
				f.genera = append(f.genera, s.Genus)
			}
		}
		f.species = append(f.species, s)
		genera[s.Genus] = true
	}

	version := strings.TrimSpace(rel.Version)
	if version == "" {
		version = "без номера"
	}

	// Вступление.
	var intro []string
	line := fmt.Sprintf("Mammal Diversity Database (MDD) — эталонный список видов млекопитающих мира, который ведёт Американское общество маммалогов (American Society of Mammalogists). Этот документ собран по релизу MDD %s", version)
	if rel.Date != "" {
		line += " от " + rel.Date
	}
	if rel.PrevVersion != "" {
		line += " (предыдущий релиз — " + rel.PrevVersion + ")"
	}
	intro = append(intro, line+".")
	intro = append(intro, fmt.Sprintf("В релизе MDD %s %s млекопитающих. Из них к отряду хищных (Carnivora) относятся %s из %s в %s.",
		version, plural(rel.Species, "вид", "вида", "видов"),
		plural(len(all), "вид", "вида", "видов"),
		plural(len(genera), "рода", "родов", "родов"),
		plural(len(families), "семействе", "семействах", "семействах")))
	intro = append(intro, "Латинские и английские названия, авторы описаний и статусы МСОП приведены так, как они записаны в MDD. Русские названия семейств добавлены для удобства, русских названий видов в MDD нет.")
	if rel.Citation != "" {
		intro = append(intro, "Ссылка на релиз: "+rel.Citation)
	}
	intro = append(intro, "Источник: "+mdd.SiteBase+", лицензия "+LicenseMDD+".")

	var sections []Section

	// Сводка по семействам.
	var sum []string
	for _, f := range families {
		sum = append(sum, fmt.Sprintf("%s — %s, %s.", familyTitle(f.name),
			plural(len(f.species), "вид", "вида", "видов"),
			plural(len(f.genera), "род", "рода", "родов")))
	}
	sections = append(sections, Section{
		Path: []string{"Семейства хищных"}, Title: "Семейства хищных", Level: 2,
		Text: fmt.Sprintf("Хищные по MDD %s — %s:\n", version, plural(len(families), "семейство", "семейства", "семейств")) + strings.Join(sum, "\n"),
	})

	// Семейства с видами Евразии.
	for _, name := range mddEurasianFamilies {
		f := byName[name]
		if f == nil {
			continue
		}
		title := familyTitle(name)
		var b []string
		b = append(b, fmt.Sprintf("Семейство %s по MDD %s: %s в %s.", title, version,
			plural(len(f.species), "вид", "вида", "видов"),
			plural(len(f.genera), "роде", "родах", "родах")))
		for _, g := range f.genera {
			var list []mdd.Species
			for _, s := range f.species {
				if s.Genus == g {
					list = append(list, s)
				}
			}
			b = append(b, fmt.Sprintf("Род %s — %s:", g, plural(len(list), "вид", "вида", "видов")))
			for _, s := range list {
				b = append(b, speciesLine(s))
			}
		}
		sections = append(sections, Section{Path: []string{title}, Title: title, Level: 2, Text: strings.Join(b, "\n")})
	}

	// Изменения релиза, касающиеся хищных: имя (старое или новое) из рода
	// хищных. Убранный вид исчезнувшего рода так не найдётся, но Diff
	// других признаков отряда не несёт.
	changes, err := st.Changes(ctx, "", 0)
	if err != nil {
		return Doc{}, fmt.Errorf("MDD: изменения: %w", err)
	}
	var ch []string
	for _, c := range changes {
		if !genera[firstWord(c.OldName)] && !genera[firstWord(c.NewName)] {
			continue
		}
		ch = append(ch, changeLine(c))
	}
	const changesTitle = "Изменения релиза в отряде"
	var changesText string
	if len(ch) == 0 {
		changesText = fmt.Sprintf("В релизе %s изменений в отряде нет: ни одна из %s списка изменений", version,
			plural(len(changes), "строки", "строк", "строк"))
		if rel.PrevVersion != "" {
			changesText += " относительно " + rel.PrevVersion
		}
		changesText += " не касается хищных (Carnivora). Состав и названия видов хищных такие же, как в предыдущем релизе."
	} else {
		head := fmt.Sprintf("В релизе %s в отряде хищных %s", version, plural(len(ch), "изменение", "изменения", "изменений"))
		if rel.PrevVersion != "" {
			head += " относительно " + rel.PrevVersion
		}
		changesText = head + ":\n" + strings.Join(ch, "\n")
	}
	sections = append(sections, Section{Path: []string{changesTitle}, Title: changesTitle, Level: 2, Text: changesText})

	fetched := ""
	if !rel.LoadedAt.IsZero() {
		fetched = rel.LoadedAt.UTC().Format(time.RFC3339)
	}
	return Doc{
		Schema:   Schema,
		ID:       MDDDocID,
		Source:   SourceMDD,
		Title:    MDDTitle,
		URL:      mdd.SiteBase,
		Fetched:  fetched,
		License:  LicenseMDD,
		Intro:    strings.Join(intro, "\n"),
		Sections: sections,
	}, nil
}

// familyTitle — «Кошачьи (Felidae)» или только латынь.
func familyTitle(name string) string {
	if ru, ok := mddFamilyRu[name]; ok {
		return ru + " (" + name + ")"
	}
	return name
}

// speciesLine — строка о виде: «Otocolobus manul (Pallas, 1776) — англ.
// Pallas's Cat; МСОП: LC (вызывающий наименьшие опасения).»
func speciesLine(s mdd.Species) string {
	line := s.SciName
	switch {
	case s.Authority != "":
		line += " " + s.Authority
	case s.Year > 0:
		line += ", " + strconv.Itoa(s.Year)
	}
	var parts []string
	if s.CommonName != "" {
		parts = append(parts, "англ. "+s.CommonName)
	}
	switch code := strings.ToUpper(s.IUCN); {
	case code == "":
		parts = append(parts, "статус МСОП не указан")
	case mddIUCNRu[code] != "":
		parts = append(parts, "МСОП: "+code+" ("+mddIUCNRu[code]+")")
	default:
		parts = append(parts, "МСОП: "+code)
	}
	if s.Extinct {
		parts = append(parts, "вымерший вид")
	}
	if s.Domestic {
		parts = append(parts, "домашний вид")
	}
	return line + " — " + strings.Join(parts, "; ") + "."
}

// changeLine — строка изменения релиза.
func changeLine(c mdd.Change) string {
	var s string
	switch {
	case c.OldName == "":
		s = "Новый вид " + c.NewName
	case c.NewName == "":
		s = "Вид " + c.OldName + " убран из списка"
	case c.OldName == c.NewName:
		s = c.NewName
	default:
		s = c.OldName + " → " + c.NewName
	}
	var extra []string
	if c.Category != "" {
		extra = append(extra, "категория: "+c.Category)
	}
	if c.Comment != "" {
		extra = append(extra, c.Comment)
	}
	if len(extra) > 0 {
		s += " (" + strings.Join(extra, "; ") + ")"
	}
	if c.Reference != "" {
		s += ". Источник: " + c.Reference
	}
	return strings.TrimSuffix(s, ".") + "."
}

func firstWord(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "_", " "))
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i]
	}
	return s
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// plural — число со словом в нужной форме: 1 вид, 2 вида, 5 видов, 21 вид,
// 11 видов.
func plural(n int, one, few, many string) string {
	w := many
	switch m10, m100 := n%10, n%100; {
	case m10 == 1 && m100 != 11:
		w = one
	case m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14):
		w = few
	}
	return strconv.Itoa(n) + " " + w
}
