// Package runs — диалоги в работе: менеджер держит загруженные диалоги,
// запускает ходы и записывает историю после каждого хода. Ход в процессе —
// Session: состояние для интерфейса, журнал событий, промежуточные
// результаты и подписчики SSE. Журнал и результат идут разными событиями
// потока (ФТ-40): журнал — «log», карточка по мере сборки — «update»,
// состояние хода — «state».
package runs

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
)

// Статусы хода в работе.
const (
	StatusRunning = "running"
	StatusDone    = history.TurnDone
	StatusFailed  = history.TurnFailed
)

// subscriberBuffer — сколько сообщений может копиться у медленного
// подписчика, прежде чем они начнут теряться.
const subscriberBuffer = 1024

// View — состояние хода для интерфейса.
type View struct {
	ID             string         `json:"id"`
	ConversationID string         `json:"conversationId"`
	Branch         string         `json:"branch"`
	Kind           string         `json:"kind"`
	Route          string         `json:"route,omitempty"`
	Started        time.Time      `json:"started"`
	Status         string         `json:"status"`
	User           string         `json:"user"`
	Reply          string         `json:"reply,omitempty"`
	Error          string         `json:"error,omitempty"`
	Totals         history.Totals `json:"totals"`
	Events         int            `json:"events"`
	SaveError      string         `json:"saveError,omitempty"`
	Context        agent.Context  `json:"context"`
}

// Message — одно сообщение подписчику: вид события SSE и данные.
type Message struct {
	Event string
	Data  string
}

// Snapshot — что получает новый подписчик: состояние, журнал и
// промежуточные результаты.
type Snapshot struct {
	View    View           `json:"view"`
	Events  []agent.Event  `json:"events"`
	Updates []agent.Update `json:"updates"`
}

// Session — ход в работе. Реализует agent.Emitter.
type Session struct {
	mu      sync.Mutex
	view    View
	events  []agent.Event
	updates []agent.Update
	subs    map[chan Message]struct{}
	closed  bool
	done    chan struct{}
}

func newSession(view View) *Session {
	view.Status = StatusRunning
	return &Session{view: view, subs: map[chan Message]struct{}{}, done: make(chan struct{})}
}

// View — копия состояния.
func (s *Session) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view
}

// Events — копия журнала.
func (s *Session) Events() []agent.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.Event(nil), s.events...)
}

// Log — событие от агента: номер, время, счётчики, рассылка.
func (s *Session) Log(ev agent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev.Seq = len(s.events) + 1
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	s.events = append(s.events, ev)
	s.view.Events = len(s.events)
	switch {
	case ev.Usage != nil:
		s.view.Totals.LLMCalls++
		s.view.Totals.Usage = s.view.Totals.Usage.Add(*ev.Usage)
		if ev.Cost != nil {
			s.view.Totals.Cost = s.view.Totals.Cost.Add(*ev.Cost)
		}
	case ev.Kind == agent.EventToolCall && !ev.Final:
		s.view.Totals.ToolCalls++
	}
	s.view.Totals.Seconds = time.Since(s.view.Started).Seconds()
	s.broadcastLocked(Message{Event: "log", Data: mustJSON(ev)})
	s.broadcastLocked(Message{Event: "state", Data: mustJSON(s.view)})
}

// Publish — промежуточный результат: карточка, раздел, соседи, сравнение.
func (s *Session) Publish(u agent.Update) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, u)
	s.broadcastLocked(Message{Event: "update", Data: mustJSON(u)})
}

func (s *Session) setRoute(route string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.view.Route = route
}

// finish закрывает ход ответом или ошибкой.
func (s *Session) finish(reply, user string, ctx agent.Context, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.view.Context = ctx
	s.view.Totals.Seconds = time.Since(s.view.Started).Seconds()
	if user != "" {
		s.view.User = user
	}
	if err != nil {
		s.view.Status, s.view.Error = StatusFailed, err.Error()
	} else {
		s.view.Status, s.view.Reply = StatusDone, reply
	}
	s.broadcastLocked(Message{Event: "state", Data: mustJSON(s.view)})
}

func (s *Session) setSaveError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.view.SaveError = ""
	if err != nil {
		s.view.SaveError = err.Error()
	}
	s.broadcastLocked(Message{Event: "state", Data: mustJSON(s.view)})
}

// Subscribe возвращает снимок на момент подписки, канал последующих
// сообщений и отписку. Канал закрывается, когда ход завершён и записан.
func (s *Session) Subscribe() (Snapshot, <-chan Message, func()) {
	ch := make(chan Message, subscriberBuffer)
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{View: s.view, Events: append([]agent.Event{}, s.events...), Updates: append([]agent.Update{}, s.updates...)}
	if s.closed {
		close(ch)
		return snap, ch, func() {}
	}
	s.subs[ch] = struct{}{}
	return snap, ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
	}
}

// broadcastLocked рассылает без ожидания: медленный подписчик теряет
// сообщения, но не задерживает агентов.
func (s *Session) broadcastLocked(msg Message) {
	for ch := range s.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (s *Session) closeSubs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		close(s.done)
	}
	s.closed = true
	for ch := range s.subs {
		delete(s.subs, ch)
		close(ch)
	}
}

// Wait — дождаться завершения хода вместе с записью в историю (для стенда
// и тестов).
func (s *Session) Wait(d time.Duration) View {
	select {
	case <-s.done:
	case <-time.After(d):
	}
	return s.View()
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}
