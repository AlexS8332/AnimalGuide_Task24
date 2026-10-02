package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Ошибки подключения.
var (
	// ErrDrift — набор инструментов сервера расходится с локальным: бинарник
	// сервера собран из другой версии кода. Модель получила бы другие
	// описания, и дорожки стенда отличались бы не путём, а текстом запроса.
	ErrDrift = errors.New("инструменты MCP-сервера расходятся с локальными — бинарник сервера устарел, пересоберите его")
	// ErrUnavailable — сервер падал слишком часто и отложен.
	ErrUnavailable = errors.New("сервер падал слишком часто и отложен")
	// ErrClosed — клиент закрыт.
	ErrClosed = errors.New("клиент MCP закрыт")
)

// Состояния клиента для пульта и окна «MCP-сервер».
const (
	StatusOff         = "off"         // ещё не запускался
	StatusReady       = "ready"       // подключён, набор сверен
	StatusDead        = "dead"        // соединение потеряно; следующий вызов перезапустит
	StatusDrift       = "drift"       // набор инструментов расходится с локальным
	StatusUnavailable = "unavailable" // перезапусков слишком много, отложен
)

// Пределы перезапуска: больше трёх перезапусков за минуту — сервер
// откладывается на пять минут, чтобы не плодить процессы каждый вызов.
const (
	restartWindow = time.Minute
	maxRestarts   = 3
	coolDown      = 5 * time.Minute
	// defaultConnectTimeout — initialize и tools/list; запуск процесса (и
	// сборка, если бинарника нет) — забота Dialer со своим пределом.
	defaultConnectTimeout = 20 * time.Second
)

// Dialer открывает новое соединение с сервером: запускает процесс или
// соединяет в памяти (тесты). Cmd — процесс сервера, если он есть: по нему
// номер процесса в журнале и принудительная остановка.
type Dialer func(ctx context.Context) (sdk.Transport, *exec.Cmd, error)

// Options — настройки клиента.
type Options struct {
	Dial Dialer
	// Want — отпечаток локального набора (tools.Fingerprint). Пусто — не
	// сверять.
	Want   string
	Logger *slog.Logger
	// Now — часы; в тестах подставные.
	Now            func() time.Time
	ConnectTimeout time.Duration
}

// Conn — сведения о текущем подключении: что сервер сказал о себе.
type Conn struct {
	Server   string    `json:"server"`
	Version  string    `json:"version"`
	Protocol string    `json:"protocol"`
	PID      int       `json:"pid,omitempty"`
	Since    time.Time `json:"since"`
	// N — номер подключения за жизнь приложения: 1 — первый запуск, дальше
	// перезапуски.
	N int `json:"n"`
	// Addr — адрес сервера-демона (HTTPDialer); у процесса пусто. Без
	// токена: он ходит заголовком.
	Addr string `json:"addr,omitempty"`
}

// ToolInfo — инструмент, как его описал сервер, и счётчики вызовов с
// нашей стороны.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Calls       int             `json:"calls"`
	Errors      int             `json:"errors"`
	// Millis — суммарное время вызовов: накладные MCP видны средним.
	Millis int64 `json:"millis"`
}

// State — снимок клиента.
type State struct {
	Status      string     `json:"status"`
	Reason      string     `json:"reason,omitempty"`
	Conn        *Conn      `json:"conn,omitempty"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	Want        string     `json:"want,omitempty"`
	Tools       []ToolInfo `json:"tools"`
	// Skipped — инструменты сервера, которые модели не выдаются: незнакомые
	// имена не проходят (ФТ-1).
	Skipped []string `json:"skipped,omitempty"`
	// Restarts — перезапуски за последнюю минуту; Until — до какого времени
	// сервер отложен.
	Restarts int        `json:"restarts"`
	Until    *time.Time `json:"until,omitempty"`
}

type counter struct {
	calls, errors int
	millis        int64
}

// Client — один клиент MCP на приложение. Реализует tools.Source: шесть
// инструментов источников, которые вызывают сервер. Подключается лениво,
// упавший сервер перезапускает следующим вызовом.
type Client struct {
	o   Options
	log *slog.Logger
	// life отменяется в Close: вызовы, идущие в этот момент, прерываются, а
	// не ждут ответа сервера, которого уже не будет.
	life context.Context
	end  context.CancelFunc

	connMu   sync.Mutex // одно подключение за раз
	mu       sync.Mutex
	sess     *sdk.ClientSession
	cmd      *exec.Cmd
	conn     *Conn
	status   string
	reason   string
	attempts []time.Time
	until    time.Time
	closed   bool
	n        int

	specs   []tools.Spec // описания от сервера, в порядке SourceTools
	schemas map[string]json.RawMessage
	skipped []string
	list    []tools.Tool
	reg     *tools.Registry
	print   string
	counts  map[string]*counter
}

// NewClient — клиент; соединение не открывается до первого запроса.
func NewClient(o Options) *Client {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = defaultConnectTimeout
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	life, end := context.WithCancel(context.Background())
	return &Client{o: o, log: log, life: life, end: end, status: StatusOff, counts: map[string]*counter{}}
}

// ID — имя источника.
func (c *Client) ID() string { return "mcp:" + ServerName }

// Tools — инструменты последнего удачного подключения; до него пусто.
// Подключается Connect или Registry.
func (c *Client) Tools() []tools.Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]tools.Tool(nil), c.list...)
}

// Connect подключается, если соединения нет: initialize → tools/list →
// сверка отпечатка. Второе значение — было ли подключение новым.
func (c *Client) Connect(ctx context.Context) (bool, error) {
	_, fresh, err := c.session(ctx)
	return fresh, err
}

// Registry — реестр шести инструментов через сервер; подключается при
// необходимости. Второе значение — было ли подключение новым.
func (c *Client) Registry(ctx context.Context) (*tools.Registry, bool, error) {
	_, fresh, err := c.session(ctx)
	if err != nil {
		return nil, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reg, fresh, nil
}

// session — живое соединение или новое. Подключение идёт под отдельным
// замком: два хода, стартовавших одновременно, не должны поднять два
// процесса, а окно состояния не должно ждать, пока сервер собирается.
func (c *Client) session(ctx context.Context) (*sdk.ClientSession, bool, error) {
	if sess, err, ok := c.current(); ok {
		return sess, false, err
	}
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if sess, err, ok := c.current(); ok { // подключил соседний ход
		return sess, false, err
	}

	c.mu.Lock()
	now := c.o.Now()
	if now.Before(c.until) {
		err := c.deferred()
		c.mu.Unlock()
		return nil, false, err
	}
	c.attempts = recent(c.attempts, now)
	if len(c.attempts) > maxRestarts { // первый запуск и три перезапуска уже были
		c.until = now.Add(coolDown)
		c.status = StatusUnavailable
		c.log.Warn("mcp: сервер отложен", "restarts", len(c.attempts)-1, "until", c.until.Format(time.TimeOnly))
		err := c.deferred()
		c.mu.Unlock()
		return nil, false, err
	}
	c.attempts = append(c.attempts, now)
	c.mu.Unlock()

	sess, cmd, addr, specs, err := c.dial(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && c.closed {
		err = ErrClosed
		go func() { sess.Close(); stop(cmd) }()
	}
	if err != nil {
		if !c.closed {
			c.reason = err.Error()
			c.status = StatusDead
			if errors.Is(err, ErrDrift) {
				c.status = StatusDrift
				c.until = now.Add(coolDown)
			}
		}
		return nil, false, err
	}
	c.adopt(specs)
	c.n++
	res := sess.InitializeResult()
	c.conn = &Conn{Protocol: res.ProtocolVersion, Since: now, N: c.n, Addr: addr}
	if res.ServerInfo != nil {
		c.conn.Server, c.conn.Version = res.ServerInfo.Name, res.ServerInfo.Version
	}
	if cmd != nil && cmd.Process != nil {
		c.conn.PID = cmd.Process.Pid
	}
	c.sess, c.cmd, c.status, c.reason = sess, cmd, StatusReady, ""
	c.log.Info("mcp: подключён", "server", c.conn.Server, "version", c.conn.Version, "protocol", c.conn.Protocol, "pid", c.conn.PID, "addr", c.conn.Addr, "n", c.n)
	go c.watch(sess)
	return sess, true, nil
}

// current — живое соединение (ok и сессия), закрытый клиент (ok и
// ошибка) или «надо подключаться» (не ok).
func (c *Client) current() (*sdk.ClientSession, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed, true
	}
	if c.sess != nil {
		return c.sess, nil, true
	}
	return nil, nil, false
}

// deferred — ошибка отложенного сервера. Вызывается под замком.
func (c *Client) deferred() error {
	if c.status == StatusDrift {
		return fmt.Errorf("%w (следующая попытка после %s)", ErrDrift, c.until.Format("15:04:05"))
	}
	return fmt.Errorf("%w (до %s): %s", ErrUnavailable, c.until.Format("15:04:05"), c.reason)
}

// dial — новое соединение со сверкой набора.
// Третье значение — адрес сервера, если он не процесс.
func (c *Client) dial(ctx context.Context) (*sdk.ClientSession, *exec.Cmd, string, *remoteSet, error) {
	// Соединение живёт дольше хода, который его открыл: отмена хода не
	// должна рвать процесс сервера.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.o.ConnectTimeout)
	defer cancel()
	if c.o.Dial == nil {
		return nil, nil, "", nil, errors.New("не задан способ запуска сервера")
	}
	t, cmd, err := c.o.Dial(ctx)
	if err != nil {
		return nil, nil, "", nil, err
	}
	addr := remoteAddr(t)
	cl := sdk.NewClient(&sdk.Implementation{Name: "animal-guide", Version: Version}, nil)
	sess, err := cl.Connect(ctx, t, nil)
	if err != nil {
		stop(cmd)
		if errors.Is(err, ErrUnauthorized) { // без обёрток SDK: человеку важна причина
			return nil, nil, "", nil, ErrUnauthorized
		}
		return nil, nil, "", nil, fmt.Errorf("initialize: %w", err)
	}
	var remote []*sdk.Tool
	for tool, err := range sess.Tools(ctx, nil) {
		if err != nil {
			sess.Close()
			stop(cmd)
			return nil, nil, "", nil, fmt.Errorf("tools/list: %w", err)
		}
		remote = append(remote, tool)
	}
	set, err := c.check(remote)
	if err != nil {
		sess.Close()
		stop(cmd)
		return nil, nil, "", nil, err
	}
	return sess, cmd, addr, set, nil
}

// remoteSet — сверенный набор инструментов сервера.
type remoteSet struct {
	specs   []tools.Spec
	schemas map[string]json.RawMessage
	skipped []string
	list    []tools.Tool
	print   string
}

// check — список инструментов сервера: только шесть имён ФТ-1, в их
// порядке, со сверкой отпечатка. Незнакомые — в журнал, модели не
// выдаются: сервер мог бы подсунуть инструмент, которого агенты не ждут.
func (c *Client) check(remote []*sdk.Tool) (*remoteSet, error) {
	byName := map[string]*sdk.Tool{}
	set := &remoteSet{schemas: map[string]json.RawMessage{}}
	for _, t := range remote {
		switch {
		case tools.IsSourceTool(t.Name):
			byName[t.Name] = t
		case t.Name == InfoTool:
		default:
			set.skipped = append(set.skipped, t.Name)
		}
	}
	sort.Strings(set.skipped)
	if len(set.skipped) > 0 {
		c.log.Warn("mcp: незнакомые инструменты сервера пропущены", "tools", strings.Join(set.skipped, ", "))
	}
	var missing []string
	for _, name := range tools.SourceTools {
		t, ok := byName[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("схема %s не читается: %w", name, err)
		}
		set.schemas[name] = tools.Canon(schema)
		set.specs = append(set.specs, tools.Spec{Name: name, Description: t.Description, Parameters: set.schemas[name],
			Untrusted: true, Via: tools.ViaMCP})
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: на сервере нет %s", ErrDrift, strings.Join(missing, ", "))
	}
	for _, s := range set.specs {
		set.list = append(set.list, c.adapter(s))
	}
	set.print = tools.Fingerprint(set.list)
	if c.o.Want != "" && set.print != c.o.Want {
		return nil, fmt.Errorf("%w: отпечаток сервера %s, локальный %s", ErrDrift, set.print, c.o.Want)
	}
	return set, nil
}

// adopt — принять сверенный набор. Вызывается под замком. Перезапуск с
// тем же отпечатком оставляет реестр прежним: ходы, которые уже держат
// его, продолжают работать.
func (c *Client) adopt(set *remoteSet) {
	if c.reg == nil || c.print != set.print {
		c.reg = tools.MustRegistry(set.list...)
		c.list = set.list
	}
	c.specs, c.schemas, c.skipped, c.print = set.specs, set.schemas, set.skipped, set.print
}

// adapter — инструмент, который вызывает сервер. Untrusted всегда: что бы
// ни сказал сервер, ответ — содержимое внешнего источника (ФТ-41).
func (c *Client) adapter(s tools.Spec) tools.Tool {
	return tools.Func{S: s, Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
		return c.call(ctx, s.Name, args)
	}}
}

// call — tools/call. Ошибка инструмента (IsError) возвращается тем же
// текстом, что и в процессе; сбой транспорта — словами «MCP-сервер
// недоступен», модель получит его как {"error"} и ход продолжится (ФТ-2).
// На локальный путь внутри хода не переключаемся: дорожка стенда должна
// быть чистой.
func (c *Client) call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	start := time.Now()
	out, err := c.do(ctx, name, args)
	c.mu.Lock()
	n := c.counts[name]
	if n == nil {
		n = &counter{}
		c.counts[name] = n
	}
	n.calls++
	n.millis += time.Since(start).Milliseconds()
	if err != nil {
		n.errors++
	}
	c.mu.Unlock()
	return out, err
}

func (c *Client) do(ctx context.Context, name string, args json.RawMessage) (string, error) {
	var arguments any = map[string]any{}
	if len(strings.TrimSpace(string(args))) > 0 {
		// Битый JSON от модели по проводу не передать: запрос не
		// соберётся. Ошибка та же, что дал бы инструмент в процессе —
		// синтаксис JSON проверяется до разбора в структуру.
		if !json.Valid(args) {
			var v any
			return "", tools.ParseArgs(args, &v)
		}
		arguments = args
	}
	sess, _, err := c.session(ctx)
	if err != nil {
		return "", unavailable(err)
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(c.life, cancel)()
	res, err := sess.CallTool(callCtx, &sdk.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if c.life.Err() != nil {
			return "", unavailable(ErrClosed)
		}
		c.drop(sess, err)
		return "", unavailable(err)
	}
	text := textOf(res)
	if res.IsError {
		return "", errors.New(text)
	}
	return text, nil
}

func unavailable(err error) error {
	return fmt.Errorf("MCP-сервер недоступен: %w", err)
}

// textOf — текст результата. Сервер отдаёт один TextContent; если частей
// несколько, они склеиваются.
func textOf(res *sdk.CallToolResult) string {
	var parts []string
	for _, ct := range res.Content {
		if t, ok := ct.(*sdk.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// watch ждёт конца соединения: сервер упал или закрылся — следующий вызов
// поднимет его снова.
func (c *Client) watch(sess *sdk.ClientSession) {
	err := sess.Wait()
	reason := "соединение закрыто сервером"
	if err != nil {
		reason = err.Error()
	}
	c.drop(sess, errors.New(reason))
}

// drop — соединение считается мёртвым.
func (c *Client) drop(sess *sdk.ClientSession, why error) {
	c.mu.Lock()
	if c.sess != sess {
		c.mu.Unlock()
		return
	}
	cmd := c.cmd
	c.sess, c.cmd = nil, nil
	if !c.closed {
		c.status, c.reason = StatusDead, why.Error()
		c.log.Warn("mcp: соединение потеряно", "reason", why.Error())
	}
	c.mu.Unlock()
	go func() {
		sess.Close()
		stop(cmd)
	}()
}

// stop — остановить процесс, если он ещё жив.
func stop(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill() // уже завершённый процесс вернёт ошибку — она не важна
	}
}

// Kill убивает процесс сервера, как если бы он упал: для стенда (сервер
// убит посреди хода) и для проверки перезапуска.
func (c *Client) Kill() error {
	c.mu.Lock()
	cmd, sess := c.cmd, c.sess
	c.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		if sess != nil {
			return sess.Close()
		}
		return errors.New("процесса сервера нет")
	}
	return cmd.Process.Kill()
}

// Close закрывает соединение и не даёт открыть новое. Вызовы, идущие в
// этот момент, получают ошибку «MCP-сервер недоступен».
func (c *Client) Close() error {
	c.mu.Lock()
	sess, cmd := c.sess, c.cmd
	c.closed, c.sess, c.cmd = true, nil, nil
	c.status, c.reason = StatusOff, "клиент закрыт"
	c.mu.Unlock()
	// Сначала прервать идущие вызовы и процесс: закрытие сессии SDK ждёт
	// ответов на все запросы.
	c.end()
	stop(cmd)
	var err error
	if sess != nil {
		err = sess.Close()
	}
	return err
}

// Started — запускался ли сервер хоть раз (пытались ли подключиться).
func (c *Client) Started() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n > 0 || len(c.attempts) > 0
}

// State — снимок для окна и пульта.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.o.Now()
	st := State{Status: c.status, Reason: c.reason, Fingerprint: c.print, Want: c.o.Want,
		Skipped: append([]string(nil), c.skipped...), Tools: []ToolInfo{}}
	if c.status == StatusUnavailable && !now.Before(c.until) {
		st.Status = StatusDead // срок вышел: следующий ход попробует снова
	}
	if now.Before(c.until) {
		u := c.until
		st.Until = &u
	}
	if n := len(recent(c.attempts, now)); n > 1 {
		st.Restarts = n - 1
	}
	if c.conn != nil {
		cn := *c.conn
		st.Conn = &cn
	}
	for _, s := range c.specs {
		ti := ToolInfo{Name: s.Name, Description: s.Description, InputSchema: c.schemas[s.Name]}
		if n := c.counts[s.Name]; n != nil {
			ti.Calls, ti.Errors, ti.Millis = n.calls, n.errors, n.millis
		}
		st.Tools = append(st.Tools, ti)
	}
	return st
}

// ServerInfo спрашивает у живого сервера его счётчики. Сервер не
// запускается ради этого: окно не должно поднимать процесс.
func (c *Client) ServerInfo(ctx context.Context) (*Info, error) {
	c.mu.Lock()
	sess := c.sess
	c.mu.Unlock()
	if sess == nil {
		return nil, errors.New("сервер не запущен")
	}
	res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: InfoTool, Arguments: map[string]any{}})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, errors.New(textOf(res))
	}
	var info Info
	if err := json.Unmarshal([]byte(textOf(res)), &info); err != nil {
		return nil, fmt.Errorf("ответ server_info не разобрался: %w", err)
	}
	return &info, nil
}

// recent — попытки за последнюю минуту.
func recent(ts []time.Time, now time.Time) []time.Time {
	out := ts[:0:0]
	for _, t := range ts {
		if now.Sub(t) < restartWindow {
			out = append(out, t)
		}
	}
	return out
}
