package runs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/facts"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/paths"
)

// maxTurnsInMemory — сколько завершённых ходов держим для потока событий:
// журнал хода после завершения лежит в файле диалога.
const maxTurnsInMemory = 64

var (
	ErrNotFound = errors.New("диалог не найден")
	// ErrBusy — в диалоге уже идёт ход (один ход за раз на диалог).
	ErrBusy  = errors.New("справочник ещё отвечает на предыдущее сообщение")
	ErrEmpty = errors.New("сообщение пустое")
)

// Hook — механизм вокруг хода: память, профиль, подборка, свод. До хода он
// готовит свой блок запроса и может взять ход на себя (подборка ведёт ход
// своим агентом); после — проверяет ответ. Ошибка хука не роняет ход: она
// уходит в журнал, и ход идёт с тем, что есть (живучесть хода при неудачном
// извлечении, НТ-5).
type Hook interface {
	Name() string
	Before(ctx context.Context, t *Turn) error
	After(ctx context.Context, t *Turn) error
}

// Turn — рабочее состояние хода, которое видят хуки.
type Turn struct {
	ID     string
	Number int
	Conv   *history.Conversation // копия на старте хода
	Branch string
	// History — путь ветки целиком, до сокращения.
	History  []llm.Message
	Request  agents.Request
	Features features.Set
	Facts    facts.State
	Owners   []string
	Em       agent.Emitter
	// Handler — если хук взял ход на себя (подборка), он и отвечает.
	Handler func(ctx context.Context) (agents.Result, error)
	Result  *agents.Result
	Err     error
	Meter   history.Meter
	Extras  map[string]any
	// Collection — подборка, которую ход сделал текущей.
	Collection      string
	CollectionTitle string
}

// AddBlock добавляет блок механизма в запрос хода.
func (t *Turn) AddBlock(b features.Block) {
	t.Request.Blocks = append(t.Request.Blocks, b)
}

// Extra записывает итог механизма в ход.
func (t *Turn) Extra(name string, v any) {
	if t.Extras == nil {
		t.Extras = map[string]any{}
	}
	t.Extras[name] = v
}

// Config — настройки менеджера.
type Config struct {
	Agents   agents.Deps
	Store    *history.Store
	Registry *features.Registry
	// Defaults — набор механизмов новых диалогов.
	Defaults features.Set
	Timeout  time.Duration
	// Window — сколько последних сообщений уходит модели; KeepToolRunes —
	// до скольких символов сокращать ответы инструментов прошлых ходов.
	Window        int
	KeepToolRunes int
	Hooks         []Hook
}

// Manager хранит диалоги, запускает ходы и записывает историю.
type Manager struct {
	cfg Config

	mu     sync.Mutex
	convs  map[string]*history.Conversation
	active map[string]*Session // диалог → ход в работе
	turns  map[string]*Session // ход → сессия
	order  []string
	groups map[string][]string // стенд → диалоги дорожек по порядку
}

// NewManager собирает менеджер.
func NewManager(cfg Config) *Manager {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.Window == 0 {
		cfg.Window = history.DefaultWindow
	}
	if cfg.KeepToolRunes == 0 {
		cfg.KeepToolRunes = history.DefaultKeepToolRunes
	}
	return &Manager{cfg: cfg, convs: map[string]*history.Conversation{}, active: map[string]*Session{},
		turns: map[string]*Session{}, groups: map[string][]string{}}
}

// Registry — реестр механизмов.
func (m *Manager) Registry() *features.Registry { return m.cfg.Registry }

// Defaults — механизмы новых диалогов.
func (m *Manager) Defaults() features.Set { return m.cfg.Defaults }

// AddHook подключает механизм вокруг хода.
func (m *Manager) AddHook(h Hook) { m.cfg.Hooks = append(m.cfg.Hooks, h) }

// Load поднимает диалоги из хранилища: продолжение после перезапуска по
// построению.
func (m *Manager) Load() (int, []error) {
	convs, problems := m.cfg.Store.Load()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range convs {
		m.convs[c.ID] = c
		if c.Group != "" && !contains(m.groups[c.Group], c.ID) {
			m.groups[c.Group] = append(m.groups[c.Group], c.ID)
		}
	}
	for g, ids := range m.groups {
		sort.SliceStable(ids, func(i, j int) bool { return m.convs[ids[i]].Created.Before(m.convs[ids[j]].Created) })
		m.groups[g] = ids
	}
	return len(convs), problems
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// DisplayDir — каталог диалогов для показа.
func (m *Manager) DisplayDir() string { return m.cfg.Store.DisplayDir() }

// StartOptions — с чем начинается новый диалог.
type StartOptions struct {
	Request agents.Request
	// Features — набор механизмов; пусто — умолчания менеджера.
	Features features.Set
	Owners   []string
	Titles   map[string]string
	Title    string
	Group    string
	Lane     string
}

// Start создаёт диалог и делает в нём первый ход.
func (m *Manager) Start(o StartOptions) (*Session, error) {
	c, err := m.create(o)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.convs[c.ID] = c
	return m.sendLocked(c, o.Request)
}

// Create заводит пустой диалог без хода: новый человек начинает с пустой
// ленты, а первый ход — отдельным запросом.
func (m *Manager) Create(o StartOptions) (Detail, error) {
	c, err := m.create(o)
	if err != nil {
		return Detail{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.convs[c.ID] = c
	if err := m.cfg.Store.Save(c); err != nil {
		return Detail{}, err
	}
	return m.detailLocked(c), nil
}

func (m *Manager) create(o StartOptions) (*history.Conversation, error) {
	fs := o.Features
	if fs.Empty() {
		fs = m.cfg.Defaults
	}
	fs = m.cfg.Registry.Complete(fs)
	if err := m.cfg.Registry.Validate(fs); err != nil {
		return nil, err
	}
	owners := o.Owners
	if owners == nil {
		owners = []string{history.DefaultOwner}
	}
	for _, id := range owners {
		if !paths.ValidID(strings.TrimSpace(id)) {
			return nil, fmt.Errorf("некорректный идентификатор собеседника %q", id)
		}
	}
	c := history.New(m.cfg.Agents.Runner.Model, owners, fs)
	for id, t := range o.Titles {
		c.SetTitle(id, t)
	}
	c.Title, c.Group, c.Lane = strings.TrimSpace(o.Title), o.Group, o.Lane
	return c, nil
}

// Send — следующий ход в текущей ветке диалога.
func (m *Manager) Send(id string, req agents.Request) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	if !ok {
		return nil, ErrNotFound
	}
	if _, running := m.active[id]; running {
		return nil, ErrBusy
	}
	return m.sendLocked(c, req)
}

// sendLocked запускает ход и уходит: HTTP-запрос не ждёт модель. Сравнение
// по умолчанию уходит в ветку (ФТ-19): точка ставится на конце текущей
// ветки, от неё заводится новая, и разговор переключается туда — вернуться
// к прежнему можно, ничего не потеряв.
func (m *Manager) sendLocked(c *history.Conversation, req agents.Request) (*Session, error) {
	if req.Kind == "" {
		req.Kind = agents.KindMessage
	}
	if req.Kind == agents.KindMessage && strings.TrimSpace(req.Text) == "" {
		return nil, ErrEmpty
	}
	if req.Kind == agents.KindMessage {
		if kind, a, b := agents.Classify(req.Text, c.Cards(c.Active)); kind == agents.KindCompare {
			req.Kind, req.A, req.B = kind, a, b
		}
	}
	if req.Kind == agents.KindCompare && len(c.PathTurns(c.Active)) > 0 {
		cp, err := c.Mark(c.Active, "до сравнения")
		if err != nil {
			return nil, err
		}
		b, err := c.Fork(cp.ID, fmt.Sprintf("сравнение: %s и %s", req.A, req.B))
		if err != nil {
			return nil, err
		}
		c.Switch(b.ID)
	}
	s := newSession(View{ID: history.NewID(), ConversationID: c.ID, Branch: c.Active, Kind: req.Kind,
		Started: time.Now(), User: req.Text})
	m.active[c.ID] = s
	m.turns[s.view.ID] = s
	m.order = append(m.order, s.view.ID)
	m.evictLocked()

	t := &Turn{ID: s.view.ID, Number: len(c.PathTurns(c.Active)) + 1, Conv: c.Clone(), Branch: c.Active,
		History: c.Path(c.Active), Request: req, Features: c.Features, Facts: c.Facts().Clone(),
		Owners: append([]string(nil), c.Owners...), Em: s}
	go m.run(s, t)
	return s, nil
}

// run ведёт ход: хуки до, ход, хуки после, запись.
func (m *Manager) run(s *Session, t *Turn) {
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.Timeout)
	defer cancel()

	m.prepare(t)
	for _, h := range m.cfg.Hooks {
		if err := h.Before(ctx, t); err != nil {
			t.Em.Log(agent.Event{Agent: h.Name(), Kind: agent.EventNote, Mechanism: h.Name(),
				Title: h.Name() + ": не сработал до хода — " + err.Error(), Detail: "Ход идёт с тем, что есть."})
		}
	}
	t.Request.Blocks = m.cfg.Registry.Order(t.Request.Blocks, t.Features)

	var res agents.Result
	var err error
	if t.Handler != nil {
		res, err = t.Handler(ctx)
	} else {
		res, err = agents.Run(ctx, m.cfg.Agents, t.Request, t.Em)
	}
	t.Result, t.Err = &res, err
	if err == nil {
		for _, h := range m.cfg.Hooks {
			if herr := h.After(ctx, t); herr != nil {
				t.Em.Log(agent.Event{Agent: h.Name(), Kind: agent.EventNote, Mechanism: h.Name(),
					Title: h.Name() + ": не сработал после хода — " + herr.Error()})
			}
		}
	}
	s.setRoute(res.Route)
	s.finish(res.Text, res.User, res.Stats.Context, err)
	m.record(s, t)
}

// prepare — то, что делает сам менеджер: окно, сокращение и карточка фактов
// по механизмам диалога. Механизмы, которые ведут хуки, готовят себя сами.
func (m *Manager) prepare(t *Turn) {
	fs := t.Features
	window := t.History
	if fs.On(features.Compact) {
		window = history.Compact(window, m.cfg.KeepToolRunes)
	}
	dropped := 0
	if fs.On(features.Window) {
		w := history.Window(window, m.cfg.Window)
		dropped = len(window) - len(w)
		window = w
	}
	t.Request.Window = window
	t.Request.Deltas = t.Conv.Deltas(t.Branch)
	t.Request.Features = fs
	if dropped > 0 && !fs.On(features.Facts) {
		t.Em.Log(agent.Event{Agent: "window", Kind: agent.EventMechanism, Mechanism: string(features.Facts),
			Title:  fmt.Sprintf("окно: модели ушли последние %d сообщений, %d — нет; карточка фактов выключена", len(window), dropped),
			Detail: "То, что ушло из окна, модели не видно — только в файле диалога (ФТ-48)."})
	}
	if fs.On(features.Facts) && !t.Facts.Empty() {
		t.AddBlock(features.Block{Feature: features.Facts, Title: "карточка фактов", Text: t.Facts.Prompt()})
	}
}

// record — ход в историю. Неудачный ход записывается без сообщений: история
// хранит только завершённые (ФТ-15).
func (m *Manager) record(s *Session, t *Turn) {
	view := s.View()
	res := t.Result
	turn := history.Turn{ID: view.ID, Started: view.Started, Status: view.Status, Kind: t.Request.Kind,
		Route: res.Route, User: orUser(res.User, t.Request.Text), Reply: view.Reply, Error: view.Error,
		Totals: view.Totals, Context: res.Stats.Context, Requested: t.Features, Effective: res.Effective,
		Cards: res.Deltas, Events: s.Events()}
	if turn.Effective.Empty() {
		turn.Effective = t.Features
	}
	for k, v := range t.Extras {
		turn.SetExtra(k, v)
	}
	added := res.Added
	if t.Err != nil {
		added = nil
	}

	m.mu.Lock()
	c := m.convs[view.ConversationID]
	var saveErr error
	if c != nil {
		turn.Collection = c.Collection
		if t.Collection != "" && t.Collection != c.Collection {
			c.SetCollection(t.Collection, t.CollectionTitle)
			turn.Collection = t.Collection
		}
		if err := c.Append(t.Branch, turn, added, t.Facts, t.Meter); err != nil {
			saveErr = err
		} else {
			saveErr = m.cfg.Store.Save(c)
		}
	}
	delete(m.active, view.ConversationID)
	m.mu.Unlock()

	s.setSaveError(saveErr)
	s.closeSubs()
}

func orUser(user, text string) string {
	if user != "" {
		return user
	}
	return text
}

// Turn — ход по идентификатору, для потока событий.
func (m *Manager) Turn(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.turns[id]
	return s, ok
}

func (m *Manager) evictLocked() {
	for len(m.order) > maxTurnsInMemory {
		id := m.order[0]
		if s := m.turns[id]; s != nil && s.View().Status == StatusRunning {
			return
		}
		delete(m.turns, id)
		m.order = m.order[1:]
	}
}

// edit — правка диалога вне хода: во время хода нельзя, ход её перезапишет.
func (m *Manager) edit(id string, fn func(c *history.Conversation) error) (Detail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	if !ok {
		return Detail{}, ErrNotFound
	}
	if _, running := m.active[id]; running {
		return Detail{}, ErrBusy
	}
	if err := fn(c); err != nil {
		return Detail{}, err
	}
	c.Updated = time.Now()
	if err := m.cfg.Store.Save(c); err != nil {
		return Detail{}, err
	}
	return m.detailLocked(c), nil
}

// Mark — точка сохранения на конце текущей ветки.
func (m *Manager) Mark(id, name string) (Detail, error) {
	return m.edit(id, func(c *history.Conversation) error {
		_, err := c.Mark(c.Active, name)
		return err
	})
}

// Fork — ветка от точки сохранения; разговор переходит в неё.
func (m *Manager) Fork(id, checkpoint, name string) (Detail, error) {
	return m.edit(id, func(c *history.Conversation) error {
		b, err := c.Fork(checkpoint, name)
		if err != nil {
			return err
		}
		return c.Switch(b.ID)
	})
}

// Switch — переключение ветки.
func (m *Manager) Switch(id, branch string) (Detail, error) {
	return m.edit(id, func(c *history.Conversation) error { return c.Switch(branch) })
}

// SetCollection делает подборку текущей в диалоге: продолжить подборку —
// значит сослаться на тот же файл, ничего не копируя (ФТ-28). Пустой
// идентификатор — отложить подборку: она остаётся в своём файле.
func (m *Manager) SetCollection(id, collectionID, title string) (Detail, error) {
	if collectionID != "" && !paths.ValidHex(collectionID) {
		return Detail{}, fmt.Errorf("некорректный идентификатор подборки %q", collectionID)
	}
	return m.edit(id, func(c *history.Conversation) error {
		c.SetCollection(collectionID, title)
		return nil
	})
}

// SetFeature включает или выключает механизм диалога. Зависимости
// проверяются: страж без свода не имеет смысла.
func (m *Manager) SetFeature(id string, name features.Name, on bool) (Detail, error) {
	if _, ok := m.cfg.Registry.Get(name); !ok {
		return Detail{}, fmt.Errorf("неизвестный механизм %q", name)
	}
	return m.edit(id, func(c *history.Conversation) error {
		next := c.Features.With(name, on)
		if err := m.cfg.Registry.Validate(next); err != nil {
			return err
		}
		c.Features = next
		return nil
	})
}

// Delete удаляет диалог; во время хода нельзя.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	if !ok {
		return ErrNotFound
	}
	if _, running := m.active[id]; running {
		return ErrBusy
	}
	if err := m.cfg.Store.Delete(id); err != nil {
		return err
	}
	delete(m.convs, id)
	if c.Group != "" {
		ids := m.groups[c.Group][:0]
		for _, x := range m.groups[c.Group] {
			if x != id {
				ids = append(ids, x)
			}
		}
		m.groups[c.Group] = ids
	}
	return nil
}

// Raw — файл диалога как есть.
func (m *Manager) Raw(id string) (string, []byte, error) {
	m.mu.Lock()
	_, ok := m.convs[id]
	m.mu.Unlock()
	if !ok {
		return "", nil, ErrNotFound
	}
	data, err := m.cfg.Store.Raw(id)
	return m.cfg.Store.DisplayPath(id), data, err
}

// Export — выгрузка в markdown (ФТ-39): карточка по ключу или сравнение по
// номеру из текущей ветки.
func (m *Manager) Export(id, kind, key string) (string, string, error) {
	m.mu.Lock()
	c, ok := m.convs[id]
	if !ok {
		m.mu.Unlock()
		return "", "", ErrNotFound
	}
	st := c.Cards(c.Active)
	m.mu.Unlock()
	switch kind {
	case "card":
		cd := st.Card(key)
		if cd == nil {
			return "", "", fmt.Errorf("карточки %q в этой ветке нет", key)
		}
		return fileName(cd.Name), card.Markdown(*cd), nil
	case "comparison":
		var i int
		if _, err := fmt.Sscan(key, &i); err != nil || i < 0 || i >= len(st.Comparisons) {
			return "", "", fmt.Errorf("сравнения %q в этой ветке нет", key)
		}
		cmp := st.Comparisons[i]
		return fileName(cmp.A.Name + " и " + cmp.B.Name), card.ComparisonMarkdown(cmp), nil
	}
	return "", "", fmt.Errorf("неизвестный вид выгрузки %q", kind)
}

func fileName(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, s)
	return strings.TrimSpace(s) + ".md"
}
