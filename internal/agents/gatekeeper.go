package agents

import (
	"context"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
)

const gatekeeperName = "gatekeeper"

// gatekeeperMaxTokens — хватает на латинское имя и вердикт.
const gatekeeperMaxTokens = 32

// gatekeeperSystem — привратник названия (из упражнения 02). Отдельный
// запрос, а не ещё одно правило в промпте идентификатора: там модель уже
// настроена найти животное и подгоняет под запрос ближайшее известное.
// Здесь от неё требуется только суждение, и отказ даётся легче.
//
// Латынь стоит до вердикта: модель сначала называет таксон, а потом
// решает — так редкие, но настоящие названия («неясыть», «поручейник») не
// отсеиваются заодно с выдуманными.
const gatekeeperSystem = `Ты — зоолог-систематик. Проверяешь, существует ли название животного.

Сначала вспомни, какому таксону соответствует названное русское имя, и запиши его латинское название. Если конкретного таксона не вспоминается — поставь прочерк. Только после этого выноси вердикт.

Отвечай строго одной строкой: <латинское название или -> | ДА или НЕТ

ДА — если название устоялось в зоологии для вида, рода или группы животных и ты назвал конкретный таксон. Латинское название тоже годится как запрос.

Название может стоять в любом падеже и числе («манула», «рысью», «ежей», «о лисице») — это то же самое название: суди по его начальной форме.

НЕТ — если название выдумано, искажено, относится к мифическому существу или не к животному. НЕТ и тогда, когда настоящее название рода дополнено несуществующим уточнением: «малая выхухоль», «полосатый манул» — род реален, а такого вида нет.

Проверь себя: ты вспоминаешь название как зоологический термин или достраиваешь его по смыслу частей? Если достраиваешь — НЕТ. Похожее название не означает то же животное.
Никаких пояснений сверх этой строки.`

// Verdict — решение привратника.
type Verdict struct {
	OK    bool   `json:"ok"`
	Latin string `json:"latin,omitempty"`
	Raw   string `json:"raw"`
	// Skipped — привратник выключен или его ответ не получен: решение
	// передано идентификатору, дорогими шагами.
	Skipped bool `json:"skipped,omitempty"`
}

// ParseVerdict разбирает ответ привратника. Всё, кроме явного ДА в
// вердикте, — отказ: невнятица здесь означает сомнение, а сомнение — повод
// не выдумывать карточку.
func ParseVerdict(text string) Verdict {
	v := Verdict{Raw: strings.TrimSpace(text)}
	verdict := strings.ToUpper(v.Raw)
	if before, after, found := strings.Cut(v.Raw, "|"); found {
		verdict = strings.ToUpper(strings.TrimSpace(after))
		latin := strings.TrimSpace(before)
		if latin != "-" && latin != "—" {
			v.Latin = latin
		}
	}
	v.OK = strings.HasPrefix(strings.TrimSpace(verdict), "ДА")
	return v
}

// Gate — привратник до дорогих шагов (ФТ-9). Выключенный механизм не делает
// ни одного запроса; неудачный запрос хода не роняет — решение уходит
// идентификатору, а в журнале видно почему.
func Gate(ctx context.Context, d Deps, name string, fs features.Set, em agent.Emitter) (Verdict, agent.Stats) {
	if !fs.On(features.Gatekeeper) {
		mechanism(em, features.Gatekeeper, "привратник выключен — название проверит идентификатор",
			"Выдумку отсеет только отсутствие статьи и подтверждения GBIF: это дороже на несколько запросов.")
		return Verdict{OK: true, Skipped: true}, agent.Stats{}
	}
	spec := agent.Spec{Name: gatekeeperName, System: gatekeeperSystem, MaxSteps: 1, MaxTokens: gatekeeperMaxTokens}
	reply, err := d.Runner.Run(ctx, spec, agent.Prepared{User: name, Features: fs}, em)
	if err != nil {
		mechanism(em, features.Gatekeeper, "привратник не ответил: "+err.Error(), "Решение передано идентификатору.")
		return Verdict{OK: true, Skipped: true}, reply.Stats
	}
	v := ParseVerdict(reply.Text)
	title := "привратник: «" + name + "» — "
	if v.OK {
		title += "название известно"
		if v.Latin != "" {
			title += " (" + v.Latin + ")"
		}
	} else {
		title += "такого животного не знаю"
	}
	em.Log(agent.Event{Agent: gatekeeperName, Kind: agent.EventMechanism, Mechanism: string(features.Gatekeeper),
		Title: title, Detail: "Ответ привратника: " + v.Raw, Data: v})
	return v, reply.Stats
}
