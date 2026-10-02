// Package history — диалог как данные: дерево веток, ходы и точки
// сохранения (ФТ-15, ФТ-19).
//
// Здесь живёт краткосрочная память: сообщения в том виде, в каком их
// получает модель (роли user, assistant, tool, вызовы с их id), и ходы — кто
// что спросил, что ответил справочник, во что это обошлось и что ход сделал
// с карточками. Системного промпта в файле нет: правка промпта действует и
// на старые диалоги (ФТ-54).
//
// Двух других слоёв памяти здесь нет. Подборка и человек живут в своих
// файлах, диалог хранит только адреса (ФТ-17): кто собеседник и над какой
// подборкой идёт работа. Адреса и набор механизмов — свойство диалога, общее
// для всех веток: «разветвить» адрес значило бы соврать, файл-то один.
// Сообщения, ходы и карточка фактов — свои у каждой ветки.
package history

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/facts"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

// Статусы хода.
const (
	TurnDone   = "done"
	TurnFailed = "failed"
)

// RootBranchName — имя первой ветки: пока не ветвились, дерево от ленты
// ничем не отличается.
const RootBranchName = "основная"

// DefaultOwner — собеседник по умолчанию. Приложение локальное, но
// долговременная память — это память о ком-то конкретном, и файл должен
// называться человеком, а не «default».
const DefaultOwner = "me"

const titleRunes = 60

var (
	ErrNoBranch     = errors.New("ветки нет")
	ErrNoCheckpoint = errors.New("точки сохранения нет")
)

// Totals — счётчики хода или диалога.
type Totals struct {
	LLMCalls  int       `json:"llmCalls"`
	ToolCalls int       `json:"toolCalls"`
	Usage     llm.Usage `json:"usage"`
	Cost      llm.Cost  `json:"cost"`
	Seconds   float64   `json:"seconds"`
}

// Add складывает счётчики.
func (t Totals) Add(o Totals) Totals {
	return Totals{
		LLMCalls:  t.LLMCalls + o.LLMCalls,
		ToolCalls: t.ToolCalls + o.ToolCalls,
		Usage:     t.Usage.Add(o.Usage),
		Cost:      t.Cost.Add(o.Cost),
		Seconds:   t.Seconds + o.Seconds,
	}
}

// Meter — во что обошлась обвязка хода (извлекатель, страж): считается
// отдельно от ответа, иначе «механизм снял четверть контекста» читалось бы
// без его цены.
type Meter struct {
	Calls   int       `json:"calls"`
	Usage   llm.Usage `json:"usage"`
	Cost    llm.Cost  `json:"cost"`
	Seconds float64   `json:"seconds"`
}

// Add складывает расход.
func (m Meter) Add(o Meter) Meter {
	return Meter{Calls: m.Calls + o.Calls, Usage: m.Usage.Add(o.Usage), Cost: m.Cost.Add(o.Cost), Seconds: m.Seconds + o.Seconds}
}

// Turn — один ход: реплика (или клик) и ответ справочника. Messages — сколько
// сообщений ход добавил в ветку; у неудачного хода ноль.
type Turn struct {
	ID      string    `json:"id"`
	Started time.Time `json:"started"`
	Status  string    `json:"status"`
	Branch  string    `json:"branch,omitempty"`
	// Kind — что пришло: реплика, клик по разделу, узлу, сравнение; Route —
	// что с этим сделал координатор.
	Kind     string        `json:"kind,omitempty"`
	Route    string        `json:"route,omitempty"`
	User     string        `json:"user"`
	Reply    string        `json:"reply,omitempty"`
	Error    string        `json:"error,omitempty"`
	Messages int           `json:"messages"`
	Totals   Totals        `json:"totals"`
	Context  agent.Context `json:"context"`
	// Requested — механизмы диалога на старте хода; Effective — с какими ход
	// прошёл на самом деле (путь до источников мог откатиться). Стенд
	// бракует ходы, где они разошлись.
	Requested features.Set `json:"requested"`
	Effective features.Set `json:"effective"`
	// Cards — что ход сделал с карточками: карточка выводится из этих правок.
	Cards []card.Delta `json:"cards,omitempty"`
	// Collection — подборка, в которой сделан ход.
	Collection string `json:"collection,omitempty"`
	// Extras — итоги механизмов хода для интерфейса и отчёта: правки памяти
	// и профиля, переходы подборки, вердикт стража. Сырой JSON по имени
	// механизма: история хранит, но не толкует их.
	Extras map[string]json.RawMessage `json:"extras,omitempty"`
	Events []agent.Event              `json:"events,omitempty"`
}

// Extra — итог механизма хода.
func (t Turn) Extra(name string, v any) bool {
	raw, ok := t.Extras[name]
	if !ok {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}

// SetExtra записывает итог механизма.
func (t *Turn) SetExtra(name string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	if t.Extras == nil {
		t.Extras = map[string]json.RawMessage{}
	}
	t.Extras[name] = raw
}

// Branch — ветка. Messages и Turns — только собственные, после места
// ветвления; всё, что было до него, берётся у родителя и не дублируется.
type Branch struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Parent string `json:"parent,omitempty"`
	// ForkAt — сколько первых сообщений пути родителя унаследовано,
	// ForkTurn — сколько его ходов.
	ForkAt   int           `json:"forkAt"`
	ForkTurn int           `json:"forkTurn"`
	Created  time.Time     `json:"created"`
	Messages []llm.Message `json:"messages"`
	Turns    []Turn        `json:"turns"`
	// Facts — карточка фактов ветки: при ветвлении копируется снимком точки.
	Facts facts.State `json:"facts"`
}

// Checkpoint — точка сохранения: место в ветке, от которого можно
// отпочковаться. Снимок карточки фактов лежит здесь: ветка должна начинаться
// с той памятью, какая была в точке, а не с нынешней (ФТ-19).
type Checkpoint struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Branch  string      `json:"branch"`
	At      int         `json:"at"`
	Turn    int         `json:"turn"`
	After   string      `json:"after,omitempty"`
	Created time.Time   `json:"created"`
	Facts   facts.State `json:"facts"`
}

// Conversation — диалог целиком.
type Conversation struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Model string `json:"model"`
	// Agent — тип агента прежних форматов; у продукта агент один.
	Agent string `json:"agent,omitempty"`
	// Owners — собеседники: у каждого свой профиль и своя долговременная
	// память. Titles — как их называть, если нигде больше названия нет.
	Owners []string          `json:"owners"`
	Titles map[string]string `json:"titles,omitempty"`
	// Collection — подборка, над которой идёт работа (адрес рабочей памяти
	// и состояния); Past — подборки, которые в этом диалоге были раньше.
	Collection      string   `json:"collection,omitempty"`
	CollectionTitle string   `json:"collectionTitle,omitempty"`
	Past            []string `json:"pastCollections,omitempty"`
	// Features — набор механизмов диалога (ФТ-46): свойство разговора, а не
	// сервера, иначе после перезапуска дорожки стенда продолжились бы
	// одинаково.
	Features features.Set `json:"features"`
	// Group и Lane — стенд и дорожка, если диалог заведён стендом.
	Group string `json:"group,omitempty"`
	Lane  string `json:"lane,omitempty"`

	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`

	Branches    []*Branch    `json:"branches"`
	Active      string       `json:"active"`
	Checkpoints []Checkpoint `json:"checkpoints,omitempty"`
	Meter       Meter        `json:"meter"`
}

// New — диалог с одной веткой.
func New(model string, owners []string, fs features.Set) *Conversation {
	now := time.Now()
	root := &Branch{ID: NewID(), Name: RootBranchName, Created: now, Messages: []llm.Message{}, Turns: []Turn{}}
	return &Conversation{
		ID: NewID(), Model: model, Owners: CleanOwners(owners), Features: fs,
		Created: now, Updated: now, Branches: []*Branch{root}, Active: root.ID,
	}
}

// CleanOwners — список собеседников без пустых строк и повторов. Всегда не
// nil: пустой список означает «собеседников нет».
func CleanOwners(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// HasOwner — участвует ли собеседник.
func (c *Conversation) HasOwner(id string) bool {
	for _, o := range c.Owners {
		if o == id {
			return true
		}
	}
	return false
}

// TitleOf — запасное название собеседника.
func (c *Conversation) TitleOf(id string) string { return c.Titles[id] }

// SetTitle запоминает запасное название собеседника.
func (c *Conversation) SetTitle(id, title string) {
	if title = strings.TrimSpace(title); title == "" {
		return
	}
	if c.Titles == nil {
		c.Titles = map[string]string{}
	}
	c.Titles[id] = title
}

// SetCollection делает подборку текущей; прежняя уходит в Past. Рабочая
// память и состояние адресуются идентификатором подборки, поэтому
// продолжить подборку — значит сослаться на те же файлы, ничего не копируя.
func (c *Conversation) SetCollection(id, title string) {
	if c.Collection != "" && c.Collection != id {
		c.Past = append(c.Past, c.Collection)
	}
	c.Collection, c.CollectionTitle = id, strings.TrimSpace(title)
	c.Updated = time.Now()
}

// Find — ветка по идентификатору.
func (c *Conversation) Find(id string) *Branch {
	for _, b := range c.Branches {
		if b.ID == id {
			return b
		}
	}
	return nil
}

// Current — ветка, в которой идёт разговор.
func (c *Conversation) Current() *Branch {
	if b := c.Find(c.Active); b != nil {
		return b
	}
	if len(c.Branches) > 0 {
		return c.Branches[0]
	}
	return nil
}

// Path — сообщения ветки в том порядке, в каком их видит модель:
// унаследованная часть пути родителя и собственные сообщения.
func (c *Conversation) Path(id string) []llm.Message {
	b := c.Find(id)
	if b == nil {
		return nil
	}
	var head []llm.Message
	if b.Parent != "" {
		head = c.Path(b.Parent)
		if b.ForkAt < len(head) {
			head = head[:b.ForkAt]
		}
	}
	out := make([]llm.Message, 0, len(head)+len(b.Messages))
	out = append(out, head...)
	return append(out, b.Messages...)
}

// PathTurns — ходы ветки вместе с унаследованными.
func (c *Conversation) PathTurns(id string) []Turn {
	b := c.Find(id)
	if b == nil {
		return nil
	}
	var head []Turn
	if b.Parent != "" {
		head = c.PathTurns(b.Parent)
		if b.ForkTurn < len(head) {
			head = head[:b.ForkTurn]
		}
	}
	out := make([]Turn, 0, len(head)+len(b.Turns))
	out = append(out, head...)
	return append(out, b.Turns...)
}

// Messages — путь текущей ветки.
func (c *Conversation) Messages() []llm.Message { return c.Path(c.Active) }

// Turns — ходы текущей ветки вместе с унаследованными.
func (c *Conversation) Turns() []Turn { return c.PathTurns(c.Active) }

// Facts — карточка фактов текущей ветки.
func (c *Conversation) Facts() facts.State {
	if b := c.Current(); b != nil {
		return b.Facts
	}
	return facts.State{}
}

// Deltas — правки карточек пути ветки, с ходом у каждой.
func (c *Conversation) Deltas(branch string) []card.Delta {
	var out []card.Delta
	for _, t := range c.PathTurns(branch) {
		if t.Status != TurnDone {
			continue
		}
		for _, d := range t.Cards {
			if d.Turn == "" {
				d.Turn = t.ID
			}
			out = append(out, d)
		}
	}
	return out
}

// Cards — карточки пути ветки: выводятся, а не хранятся (ИП-6).
func (c *Conversation) Cards(branch string) card.State { return card.Fold(c.Deltas(branch)) }

// Append записывает завершённый ход в ветку: сообщения — в историю ветки,
// ход — в её список, карточка фактов ветки заменяется той, с которой ход
// закончился. Название диалога берётся из первого сообщения.
func (c *Conversation) Append(branchID string, t Turn, added []llm.Message, f facts.State, m Meter) error {
	b := c.Find(branchID)
	if b == nil {
		return fmt.Errorf("%w: %s", ErrNoBranch, branchID)
	}
	t.Messages = len(added)
	t.Branch = b.ID
	if f.Version >= b.Facts.Version {
		b.Facts = f
	}
	b.Messages = append(b.Messages, added...)
	b.Turns = append(b.Turns, t)
	c.Meter = c.Meter.Add(m)
	c.Updated = time.Now()
	if c.Title == "" && t.User != "" && t.Status == TurnDone {
		c.Title = MakeTitle(t.User)
	}
	return nil
}

// Mark ставит точку сохранения на текущем конце ветки.
func (c *Conversation) Mark(branchID, name string) (Checkpoint, error) {
	b := c.Find(branchID)
	if b == nil {
		return Checkpoint{}, fmt.Errorf("%w: %s", ErrNoBranch, branchID)
	}
	turns := c.PathTurns(branchID)
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("после хода %d", len(turns))
	}
	after := ""
	if len(turns) > 0 {
		after = turns[len(turns)-1].ID
	}
	cp := Checkpoint{ID: NewID(), Name: name, Branch: b.ID, At: len(c.Path(branchID)), Turn: len(turns),
		After: after, Created: time.Now(), Facts: b.Facts.Clone()}
	c.Checkpoints = append(c.Checkpoints, cp)
	c.Updated = time.Now()
	return cp, nil
}

// Checkpoint — точка по идентификатору.
func (c *Conversation) Checkpoint(id string) (Checkpoint, bool) {
	for _, cp := range c.Checkpoints {
		if cp.ID == id {
			return cp, true
		}
	}
	return Checkpoint{}, false
}

// Fork заводит ветку от точки сохранения. Новая ветка пуста: всё, что было
// до точки, она берёт у родителя, а всё, что после, не видит. Карточка
// фактов — из снимка точки.
func (c *Conversation) Fork(checkpointID, name string) (*Branch, error) {
	cp, ok := c.Checkpoint(checkpointID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoCheckpoint, checkpointID)
	}
	if c.Find(cp.Branch) == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoBranch, cp.Branch)
	}
	if name = strings.TrimSpace(name); name == "" {
		name = fmt.Sprintf("ветка %d", len(c.Branches)+1)
	}
	b := &Branch{ID: NewID(), Name: name, Parent: cp.Branch, ForkAt: cp.At, ForkTurn: cp.Turn, Created: time.Now(),
		Messages: []llm.Message{}, Turns: []Turn{}, Facts: cp.Facts.Clone()}
	c.Branches = append(c.Branches, b)
	c.Updated = time.Now()
	return b, nil
}

// Switch переводит разговор в другую ветку: ничего не копирует и не удаляет.
func (c *Conversation) Switch(branchID string) error {
	if c.Find(branchID) == nil {
		return fmt.Errorf("%w: %s", ErrNoBranch, branchID)
	}
	c.Active = branchID
	c.Updated = time.Now()
	return nil
}

// Totals — сумма по всем ходам всех веток: заплачено и за ту ветку, в
// которой сейчас не сидим.
func (c *Conversation) Totals() Totals {
	var t Totals
	for _, b := range c.Branches {
		for _, turn := range b.Turns {
			t = t.Add(turn.Totals)
		}
	}
	return t
}

// TurnCount — число ходов во всех ветках.
func (c *Conversation) TurnCount() int {
	n := 0
	for _, b := range c.Branches {
		n += len(b.Turns)
	}
	return n
}

// Clone — глубокая копия: диалог отдаётся наружу, пока его дописывает ход.
func (c *Conversation) Clone() *Conversation {
	out := *c
	out.Owners = append([]string{}, c.Owners...)
	out.Past = append([]string(nil), c.Past...)
	if c.Titles != nil {
		out.Titles = make(map[string]string, len(c.Titles))
		for k, v := range c.Titles {
			out.Titles[k] = v
		}
	}
	out.Features = features.NewSet(c.Features.Map())
	out.Branches = make([]*Branch, len(c.Branches))
	for i, b := range c.Branches {
		cb := *b
		// make + copy, а не append к nil: у пустой ветки append вернул бы
		// nil, и в JSON вместо пустого списка ушёл бы null — интерфейс
		// запрашивает ветку сразу после создания, пока первый ход идёт.
		cb.Messages = make([]llm.Message, len(b.Messages))
		copy(cb.Messages, b.Messages)
		cb.Turns = make([]Turn, len(b.Turns))
		for j, t := range b.Turns {
			t.Events = append([]agent.Event(nil), t.Events...)
			t.Cards = append([]card.Delta(nil), t.Cards...)
			cb.Turns[j] = t
		}
		cb.Facts = b.Facts.Clone()
		out.Branches[i] = &cb
	}
	out.Checkpoints = make([]Checkpoint, len(c.Checkpoints))
	copy(out.Checkpoints, c.Checkpoints)
	return &out
}

// MakeTitle — название диалога по первому сообщению.
func MakeTitle(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	r := []rune(text)
	if len(r) <= titleRunes {
		return text
	}
	cut := string(r[:titleRunes])
	if i := strings.LastIndex(cut, " "); i > titleRunes/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// Runes — размер сообщений в символах вместе с аргументами вызовов.
func Runes(ms []llm.Message) int {
	n := 0
	for _, m := range ms {
		n += len([]rune(m.Content))
		for _, c := range m.ToolCalls {
			n += len([]rune(c.Function.Arguments))
		}
	}
	return n
}

// NewID — идентификатор диалога, ветки, хода: 16 шестнадцатеричных знаков.
// Идентификаторы стабильны и не перевыпускаются (ФТ-51).
func NewID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
