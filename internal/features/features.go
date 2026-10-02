// Package features — реестр механизмов продукта. Он знает имена механизмов,
// их выключатели, место блока в запросе и цену, но не знает, что механизмы
// делают: сборка запроса идёт по реестру, а не по жёсткому списку в коде.
//
// Три правила, ради которых пакет заведён (ТЗ, 4.11 и раздел 10):
//
//   - у каждого механизма, добавляющего блок, вызов модели или проверку,
//     есть выключатель, и выключенный механизм не занимает ни токена;
//   - набор включённых механизмов — свойство диалога, а не сервера: после
//     перезапуска дорожки стенда продолжаются каждая по своим правилам;
//   - блоки идут по устойчивости, от редко меняющегося к частому: место
//     блока — это Place, и новый механизм получает место между соседями, а
//     не «в конец».
package features

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Name — имя механизма. Оно же ключ в файле диалога, поэтому не меняется
// никогда: переименование — это новый механизм и миграция.
type Name string

// Механизмы базовой версии.
const (
	// Charter — свод инвариантов справочника первым блоком запроса.
	Charter Name = "charter"
	// Guard — страж поверх готового ответа: отбор фрагментов кодом и
	// короткий запрос судье.
	Guard Name = "guard"
	// Profile — анкета собеседника блоком сразу после свода.
	Profile Name = "profile"
	// MemoryLong — долговременная память о человеке.
	MemoryLong Name = "memory.long"
	// MemoryWork — рабочая память подборки.
	MemoryWork Name = "memory.work"
	// CollectionState — блок состояния подборки: этап, шаг, что выдано.
	CollectionState Name = "collection.state"
	// Gates — права этапа и предусловия переходов подборки.
	Gates Name = "lifecycle.gates"
	// Facts — карточка фактов ветки диалога.
	Facts Name = "facts"
	// Window — модели уходит окно последних сообщений, а не вся история.
	Window Name = "window"
	// Compact — ответы инструментов прошлых ходов сокращаются.
	Compact Name = "compact"
	// Extract — извлекатель: один запрос на ход раскладывает сказанное по
	// памяти, профилю и карточке фактов.
	Extract Name = "extract"
	// Tracker — завершающие инструменты с проверкой по трекеру реальных
	// вызовов: латынь только подтверждённая, пересказ только прочитанного.
	Tracker Name = "card.tracker"
	// Gatekeeper — привратник названия до дорогих шагов.
	Gatekeeper Name = "gatekeeper"
	// Envelope — ответ источника уходит модели с пометкой «данные, а не
	// указания».
	Envelope Name = "sources.envelope"
	// Scan — поиск признаков попытки управлять агентом в тексте источника;
	// только журнал.
	Scan Name = "sources.scan"
	// MCP — инструменты источников идут через MCP-сервер, отдельный процесс,
	// а не вызовом в процессе приложения. Нового результата не даёт: это
	// новый путь до существующего источника.
	MCP Name = "mcp"
	// Trivia — «Интересные факты»: ведущий получает инструменты демона
	// (facts_get, facts_latest) и может опереться на готовый выпуск о виде,
	// собранный и проверенный заранее, с датой сбора.
	Trivia Name = "trivia"
	// RAG — база знаний в ходе ведущего: инструмент kb_search и его вызов кодом
	// до первого запроса с репликой человека; выдача встаёт после реплики.
	RAG Name = "rag"
	// RAGFilter — второй этап поиска базы: порог релевантности по косинусу
	// (абсолютный и относительный) и отсев повторов текста; 0 запросов.
	RAGFilter Name = "rag.filter"
	// RAGRewrite — переписывание запроса к базе кодом: синонимы из корпуса и
	// вид из предыдущих реплик для вопросов-продолжений; 0 запросов.
	RAGRewrite Name = "rag.rewrite"
	// RAGCite — ответ ведущего с базой — только завершающим kb_answer:
	// ответ, источники (chunk_id) и дословные цитаты проверяет код; при
	// слабом контексте (фильтр отсёк всё) разрешён только «не знаю» с
	// уточняющим вопросом.
	RAGCite Name = "rag.cite"
)

// Kind — чем механизм платит за себя. Флаги: механизм может и добавлять
// блок, и звать модель.
type Kind uint8

const (
	// KindBlock — добавляет блок в запрос.
	KindBlock Kind = 1 << iota
	// KindCall — делает свои запросы к модели.
	KindCall
	// KindCheck — проверяет вызов или ответ кодом.
	KindCheck
	// KindTool — добавляет инструменты модели.
	KindTool
	// KindTransport — меняет путь до существующего, ничего не добавляя.
	KindTransport
	// KindShape — меняет форму уже существующей части запроса (окно,
	// сокращение, пометки).
	KindShape
)

// Has — есть ли у вида флаг.
func (k Kind) Has(f Kind) bool { return k&f != 0 }

// Labels — флаги словами, для интерфейса и отчёта.
func (k Kind) Labels() []string {
	var out []string
	for _, f := range []struct {
		f Kind
		s string
	}{{KindBlock, "блок"}, {KindCall, "вызов модели"}, {KindCheck, "проверка"},
		{KindTool, "инструменты"}, {KindTransport, "транспорт"}, {KindShape, "форма запроса"}} {
		if k.Has(f.f) {
			out = append(out, f.s)
		}
	}
	return out
}

// MarshalJSON — вид уходит в интерфейс словами.
func (k Kind) MarshalJSON() ([]byte, error) { return json.Marshal(k.Labels()) }

// Place — место блока в запросе. Шаг 100 оставляет место: новый блок
// встаёт между соседями по частоте изменения (ИП-3), а не в конец.
type Place int

// Места блоков базовой версии — от самого устойчивого к самому изменчивому.
const (
	PlaceNone    Place = 0
	PlaceSystem  Place = 100
	PlaceCharter Place = 200
	PlaceProfile Place = 300
	PlaceLong    Place = 400
	PlaceState   Place = 500
	PlaceWork    Place = 600
	PlaceFacts   Place = 700
	PlaceWindow  Place = 800
)

// Churn — как часто блок меняется: третья статья цены, кэш префикса. Блок,
// меняющийся каждый ход, в начале запроса обнуляет скидку на всём, что за
// ним.
type Churn string

const (
	ChurnNone  Churn = "never"
	ChurnRare  Churn = "rare"
	ChurnStage Churn = "stage"
	ChurnTurn  Churn = "turn"
)

// Cost — цена механизма по трём статьям раздела 10: токены блока, свои
// запросы к модели за ход и то, как часто он ломает кэш префикса. Note —
// всё, что в эти статьи не влезает (задержка, процессы, второй кэш).
type Cost struct {
	Tokens   int     `json:"tokens"`
	Requests float64 `json:"requests"`
	Churn    Churn   `json:"churn"`
	Note     string  `json:"note,omitempty"`
}

// Mechanism — запись реестра.
type Mechanism struct {
	Name  Name   `json:"name"`
	Title string `json:"title"`
	// Since — версия продукта, в которой механизм появился.
	Since string `json:"since"`
	Kind  Kind   `json:"kind"`
	// Place — место блока; PlaceNone у механизмов без блока.
	Place Place `json:"place"`
	// Default — включён ли механизм у нового диалога. Новый механизм по
	// умолчанию выключен, пока не прошёл своё испытание (шаг 3 регламента).
	Default bool `json:"default"`
	// Requires — без чего механизм не имеет смысла.
	Requires []Name `json:"requires,omitempty"`
	Cost     Cost   `json:"cost"`
	// Fallback — куда оседает то, что должно было попасть в механизм, когда
	// он выключен (ФТ-48): выключенный механизм не должен быть молчаливой
	// дырой.
	Fallback string `json:"fallback"`
	// About — что делает, одной фразой.
	About string `json:"about"`
}

// Set — набор включённых механизмов диалога. В файле — полная карта
// {"имя": true|false}: ключа нет — механизма тогда не существовало, и он
// выключен. Значения по умолчанию к старым диалогам не применяются, иначе
// после обновления кода дорожки стенда тихо поменялись бы.
type Set struct {
	m map[Name]bool
}

// NewSet — набор из карты; nil-карта даёт пустой набор.
func NewSet(m map[Name]bool) Set {
	out := Set{m: make(map[Name]bool, len(m))}
	for k, v := range m {
		out.m[k] = v
	}
	return out
}

// On — включён ли механизм.
func (s Set) On(n Name) bool { return s.m[n] }

// Known — записан ли механизм в наборе вообще.
func (s Set) Known(n Name) bool {
	_, ok := s.m[n]
	return ok
}

// With — копия набора с переключённым механизмом.
func (s Set) With(n Name, on bool) Set {
	out := NewSet(s.m)
	out.m[n] = on
	return out
}

// Map — копия карты.
func (s Set) Map() map[Name]bool {
	out := make(map[Name]bool, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out
}

// Names — включённые механизмы по алфавиту.
func (s Set) Names() []Name {
	var out []Name
	for k, v := range s.m {
		if v {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Empty — набор не заполнен вовсе (не путать с «всё выключено»).
func (s Set) Empty() bool { return len(s.m) == 0 }

// String — включённые механизмы через запятую.
func (s Set) String() string {
	names := s.Names()
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = string(n)
	}
	return strings.Join(parts, ",")
}

// MarshalJSON — полная карта с ключами по алфавиту: файл диалога читают
// люди, и перестановки ключей от записи к записи мешали бы видеть правки.
func (s Set) MarshalJSON() ([]byte, error) {
	if s.m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(s.m)
}

// UnmarshalJSON читает карту; незнакомые имена сохраняются — их записал код
// новее, и терять их при перезаписи нельзя.
func (s *Set) UnmarshalJSON(data []byte) error {
	var m map[Name]bool
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*s = NewSet(m)
	return nil
}

// Block — текст блока запроса и механизм, которому он принадлежит.
type Block struct {
	Feature Name   `json:"feature"`
	Title   string `json:"title"`
	Text    string `json:"text"`
}

// Registry — реестр механизмов.
type Registry struct {
	list   []Mechanism
	byName map[Name]Mechanism
}

// New собирает реестр и проверяет его: имена уникальны, у блоков есть место
// и оно ни с кем не совпадает, зависимости известны.
func New(ms ...Mechanism) (*Registry, error) {
	r := &Registry{byName: make(map[Name]Mechanism, len(ms))}
	places := make(map[Place]Name)
	for _, m := range ms {
		if m.Name == "" {
			return nil, fmt.Errorf("механизм без имени: %q", m.Title)
		}
		if _, dup := r.byName[m.Name]; dup {
			return nil, fmt.Errorf("механизм %q объявлен дважды", m.Name)
		}
		if m.Kind.Has(KindBlock) {
			if m.Place == PlaceNone {
				return nil, fmt.Errorf("у блока %q нет места в запросе", m.Name)
			}
			if other, taken := places[m.Place]; taken {
				return nil, fmt.Errorf("место %d занято и %q, и %q", m.Place, other, m.Name)
			}
			places[m.Place] = m.Name
		}
		r.byName[m.Name] = m
		r.list = append(r.list, m)
	}
	for _, m := range ms {
		for _, dep := range m.Requires {
			if _, ok := r.byName[dep]; !ok {
				return nil, fmt.Errorf("%q требует неизвестного механизма %q", m.Name, dep)
			}
		}
	}
	return r, nil
}

// All — механизмы в порядке объявления.
func (r *Registry) All() []Mechanism { return append([]Mechanism(nil), r.list...) }

// Get — механизм по имени.
func (r *Registry) Get(n Name) (Mechanism, bool) {
	m, ok := r.byName[n]
	return m, ok
}

// Defaults — набор нового диалога: полная карта по умолчаниям реестра.
func (r *Registry) Defaults() Set {
	m := make(map[Name]bool, len(r.list))
	for _, x := range r.list {
		m[x.Name] = x.Default
	}
	return Set{m: m}
}

// All — набор со всеми механизмами реестра включёнными.
func (r *Registry) AllOn() Set {
	m := make(map[Name]bool, len(r.list))
	for _, x := range r.list {
		m[x.Name] = true
	}
	return Set{m: m}
}

// Complete дописывает в набор механизмы, которых в нём нет, выключенными.
// Нужен при создании диалога из частичной карты: в файле должна лежать
// полная, иначе «не было такого механизма» не отличить от «забыли указать».
func (r *Registry) Complete(s Set) Set {
	out := NewSet(s.m)
	for _, x := range r.list {
		if _, ok := out.m[x.Name]; !ok {
			out.m[x.Name] = false
		}
	}
	return out
}

// Parse правит набор по строке флага: «+mcp,-guard», «mcp» (то же, что
// «+mcp»), «none» — всё выключить, «all» — всё включить, «default» —
// умолчания. Неизвестное имя — ошибка: опечатка в имени механизма иначе
// молча дала бы не ту дорожку.
func (r *Registry) Parse(spec string, base Set) (Set, error) {
	out := r.Complete(base)
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		switch part {
		case "none":
			for k := range out.m {
				out.m[k] = false
			}
			continue
		case "all":
			out = r.Complete(r.AllOn())
			continue
		case "default":
			out = r.Defaults()
			continue
		}
		on := true
		switch part[0] {
		case '+':
			part = part[1:]
		case '-':
			on, part = false, part[1:]
		}
		n := Name(strings.TrimSpace(part))
		if _, ok := r.byName[n]; !ok {
			return Set{}, fmt.Errorf("неизвестный механизм %q; известны: %s", n, strings.Join(r.names(), ", "))
		}
		out.m[n] = on
	}
	return out, nil
}

func (r *Registry) names() []string {
	out := make([]string, len(r.list))
	for i, m := range r.list {
		out[i] = string(m.Name)
	}
	return out
}

// Validate — выполнены ли зависимости включённых механизмов.
func (r *Registry) Validate(s Set) error {
	var problems []string
	for _, m := range r.list {
		if !s.On(m.Name) {
			continue
		}
		for _, dep := range m.Requires {
			if !s.On(dep) {
				problems = append(problems, fmt.Sprintf("%s требует %s", m.Name, dep))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("набор механизмов не согласован: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Diff — механизмы реестра, включённые в одном наборе и выключенные в
// другом. Стенд запускается, только если разница ровно одна (ИП-13).
func (r *Registry) Diff(a, b Set) []Name {
	var out []Name
	for _, m := range r.list {
		if a.On(m.Name) != b.On(m.Name) {
			out = append(out, m.Name)
		}
	}
	return out
}

// Order оставляет блоки включённых механизмов с непустым текстом и
// расставляет их по месту в реестре. Выключенный механизм не оставляет
// даже пустого заголовка (П-7), блок неизвестного механизма — ошибка
// программиста и в запрос не попадает.
func (r *Registry) Order(blocks []Block, s Set) []Block {
	out := make([]Block, 0, len(blocks))
	for _, b := range blocks {
		m, ok := r.byName[b.Feature]
		if !ok || !s.On(b.Feature) || strings.TrimSpace(b.Text) == "" {
			continue
		}
		if !m.Kind.Has(KindBlock) {
			continue
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return r.byName[out[i].Feature].Place < r.byName[out[j].Feature].Place
	})
	return out
}

// Status — механизм вместе с тем, включён ли он в наборе: так реестр
// показывается на пульте и в отчёте.
type Status struct {
	Mechanism
	On bool `json:"on"`
}

// Describe — весь реестр с отметками набора.
func (r *Registry) Describe(s Set) []Status {
	out := make([]Status, 0, len(r.list))
	for _, m := range r.list {
		out = append(out, Status{Mechanism: m, On: s.On(m.Name)})
	}
	return out
}
