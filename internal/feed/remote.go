package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/notes"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Ошибки клиента. Их три, а не «что-то сломалось»: человеку в каждом случае
// нужно сделать разное — ничего (раздел выключен), запустить демон или
// поправить токен.
var (
	// ErrOff — адрес демона не задан (-facts-server "").
	ErrOff = errors.New("демон «Интересных фактов» не настроен")
	// ErrDown — демон не отвечает: не запущен, упал или не успел ответить.
	ErrDown = errors.New("демон «Интересных фактов» не отвечает")
	// ErrDenied — демон отверг токен (401). Переподключаться с тем же
	// токеном бесполезно.
	ErrDenied = errors.New("демон «Интересных фактов» отверг токен (401)")
)

// ToolError — инструмент демона ответил ошибкой (IsError): текст для
// человека вроде «выпусков о виде … ещё не было». Демон при этом жив.
type ToolError struct{ Text string }

func (e *ToolError) Error() string { return e.Text }

// Инструменты демона, которые приложение зовёт само.
const (
	toolScheduleStatus = "schedule_status"
	toolRunNow         = "run_now"
	toolSummaryBuild   = "summary_build"
)

// writeTools — платные инструменты: их вызов после сбоя транспорта не
// повторяется. Сбой мог случиться уже после того, как демон принял запрос,
// и повтор стал бы вторым платным запуском. summarize конвейера платный так
// же, а save_to_file при повторе записал бы второй файл.
var writeTools = map[string]bool{toolRunNow: true, toolSummaryBuild: true,
	pipeline.ToolSummarize: true, pipeline.ToolSaveFile: true,
	// Блокнот: повтор nb_add добавил бы раздел дважды, nb_open открыл бы
	// второй блокнот, nb_close после записи отказал бы «уже закрыт».
	notes.ToolOpen: true, notes.ToolAdd: true, notes.ToolClose: true}

const (
	// connectTimeout — предел на initialize и tools/list: демон локальный,
	// дольше — значит, он завис.
	connectTimeout = 10 * time.Second
	// statusTTL — сколько живёт ответ Status: окно опрашивает состояние
	// раз в несколько секунд, и каждый опрос не должен доходить до демона.
	statusTTL = 5 * time.Second
	// statusTimeout — предел одной проверки состояния.
	statusTimeout = 5 * time.Second
)

// Remote — клиент к демону по MCP (Streamable HTTP). Подключается лениво,
// при первом вызове; упавшее соединение поднимает заново при следующем.
// Безопасен для одновременных вызовов.
type Remote struct {
	server string // как задан, без токена
	token  string
	log    *slog.Logger
	dial   mcp.Dialer
	// now — часы кэша состояния; подменяются в тестах.
	now func() time.Time

	connMu sync.Mutex // одно подключение за раз

	mu     sync.Mutex
	sess   *sdk.ClientSession
	ver    string
	list   []*sdk.Tool
	closed bool

	stMu     sync.Mutex // одна проверка состояния за раз, остальные ждут её
	st       Status
	stCached bool
}

// NewRemote — клиент к демону по адресу server («http://127.0.0.1:8766»,
// «127.0.0.1:8766» или с путём). Пустой адрес — клиент выключен: Status
// отвечает ConnOff, вызовы — ErrOff. token уходит заголовком Authorization
// и нигде не показывается.
func NewRemote(server, token string, log *slog.Logger) *Remote {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Remote{server: strings.TrimSpace(server), token: token, log: log, now: time.Now}
	if r.server != "" {
		r.dial = mcp.HTTPDialer(r.server, token, nil)
	}
	return r
}

// Configured — задан ли адрес демона.
func (r *Remote) Configured() bool { return r != nil && r.server != "" }

// Server — адрес демона для показа (токена в нём нет).
func (r *Remote) Server() string {
	if r == nil {
		return ""
	}
	return r.server
}

// Close закрывает соединение; после него вызовы отвечают ErrDown.
func (r *Remote) Close() {
	r.mu.Lock()
	sess := r.sess
	r.sess, r.closed = nil, true
	r.mu.Unlock()
	if sess != nil {
		sess.Close()
	}
}

// session — живое соединение или новое.
func (r *Remote) session(ctx context.Context) (*sdk.ClientSession, error) {
	if !r.Configured() {
		return nil, ErrOff
	}
	if sess, err := r.current(); sess != nil || err != nil {
		return sess, err
	}
	r.connMu.Lock()
	defer r.connMu.Unlock()
	if sess, err := r.current(); sess != nil || err != nil { // подключил соседний вызов
		return sess, err
	}
	sess, list, err := r.connect(ctx)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		sess.Close()
		return nil, fmt.Errorf("%w: клиент закрыт", ErrDown)
	}
	r.sess, r.list = sess, list
	r.ver = ""
	if res := sess.InitializeResult(); res != nil && res.ServerInfo != nil {
		r.ver = res.ServerInfo.Version
	}
	version := r.ver
	r.mu.Unlock()
	r.log.Info("факты: подключён к демону", "server", r.server, "version", version, "tools", len(list))
	go r.watch(sess)
	return sess, nil
}

func (r *Remote) current() (*sdk.ClientSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, fmt.Errorf("%w: клиент закрыт", ErrDown)
	}
	return r.sess, nil
}

// connect — initialize и tools/list. Соединение живёт дольше вызова,
// который его открыл, поэтому контекст подключения свой; но отмена
// вызывающего его прерывает — стартовая проверка не должна ждать дольше
// своих двух секунд.
func (r *Remote) connect(ctx context.Context) (*sdk.ClientSession, []*sdk.Tool, error) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), connectTimeout)
	defer cancel()
	defer context.AfterFunc(ctx, cancel)()
	t, _, err := r.dial(cctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrDown, err)
	}
	cl := sdk.NewClient(&sdk.Implementation{Name: "animal-guide", Version: mcp.Version}, nil)
	sess, err := cl.Connect(cctx, t, nil)
	if err != nil {
		return nil, nil, classify(err)
	}
	var list []*sdk.Tool
	for tool, err := range sess.Tools(cctx, nil) {
		if err != nil {
			sess.Close()
			return nil, nil, classify(fmt.Errorf("tools/list: %w", err))
		}
		list = append(list, tool)
	}
	return sess, list, nil
}

// classify — сбой транспорта словами одной из двух ошибок пакета.
func classify(err error) error {
	if errors.Is(err, mcp.ErrUnauthorized) {
		return fmt.Errorf("%w: задай MCP_TOKEN — тот же, что у демона", ErrDenied)
	}
	return fmt.Errorf("%w: %v", ErrDown, err)
}

// watch ждёт конца соединения: следующий вызов поднимет новое.
func (r *Remote) watch(sess *sdk.ClientSession) {
	sess.Wait()
	r.drop(sess)
}

func (r *Remote) drop(sess *sdk.ClientSession) {
	r.mu.Lock()
	if r.sess != sess {
		r.mu.Unlock()
		return
	}
	r.sess, r.list = nil, nil
	r.mu.Unlock()
	go sess.Close()
}

// Call — вызов инструмента демона; результат — текст ответа (JSON). args —
// аргументы: структура, map или json.RawMessage; nil — без аргументов.
//
// Ошибка инструмента — *ToolError. Сбой транспорта — одна попытка
// переподключиться и повторить (демон мог перезапуститься между
// вызовами), затем ErrDown; у платных инструментов повтора нет. 401 —
// ErrDenied.
func (r *Remote) Call(ctx context.Context, tool string, args any) (json.RawMessage, error) {
	if !r.Configured() {
		return nil, ErrOff
	}
	params := &sdk.CallToolParams{Name: tool, Arguments: arguments(args)}
	for attempt := 0; ; attempt++ {
		sess, err := r.session(ctx)
		if err != nil {
			return nil, err
		}
		res, err := sess.CallTool(ctx, params)
		if err == nil {
			text := textOf(res)
			if res.IsError {
				return nil, &ToolError{Text: text}
			}
			if !json.Valid([]byte(text)) {
				// Все инструменты демона отвечают JSON; если нет — отдаём
				// строкой, чтобы ответ REST остался JSON.
				raw, _ := json.Marshal(text)
				return raw, nil
			}
			return json.RawMessage(text), nil
		}
		if wire, ok := refused(err); ok {
			// Демон жив и ответил отказом протокола (например, нет такого
			// инструмента): соединение исправно.
			return nil, &ToolError{Text: fmt.Sprintf("демон отклонил %s: %s", tool, wire.Message)}
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %s не ответил вовремя: %v", ErrDown, tool, ctx.Err())
		}
		r.drop(sess)
		if errors.Is(err, mcp.ErrUnauthorized) {
			return nil, classify(err)
		}
		if attempt > 0 || writeTools[tool] {
			return nil, fmt.Errorf("%w: %v", ErrDown, err)
		}
		r.log.Warn("факты: соединение с демоном потеряно, переподключаюсь", "tool", tool, "err", err)
	}
}

// refused — отказ самого демона по протоколу. Только коды «нет метода»,
// «неверный запрос» и «неверные параметры»: остальные ошибки JSON-RPC SDK
// порождает и сам на сбое транспорта (например, «rejected by transport»),
// и их надо считать обрывом связи.
func refused(err error) (*jsonrpc.Error, bool) {
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		return nil, false
	}
	switch wire.Code {
	case jsonrpc.CodeMethodNotFound, jsonrpc.CodeInvalidParams, jsonrpc.CodeInvalidRequest:
		return wire, true
	}
	return nil, false
}

func arguments(args any) any {
	switch a := args.(type) {
	case nil:
		return map[string]any{}
	case json.RawMessage:
		if len(strings.TrimSpace(string(a))) == 0 {
			return map[string]any{}
		}
	}
	return args
}

// textOf — текст результата: демон отдаёт один TextContent.
func textOf(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Tools — инструменты демона для модели: описание и схема такие, какими
// их отдал демон в tools/list, вызов — Call. Ответ — пересказ внешних
// источников, поэтому Untrusted. Имени нет у демона — ошибка: выдать
// модели половину набора хуже, чем не выдать ничего и сказать почему.
func (r *Remote) Tools(ctx context.Context, names ...string) ([]tools.Tool, error) {
	if _, err := r.session(ctx); err != nil {
		return nil, err
	}
	r.mu.Lock()
	byName := make(map[string]*sdk.Tool, len(r.list))
	for _, t := range r.list {
		byName[t.Name] = t
	}
	r.mu.Unlock()
	out := make([]tools.Tool, 0, len(names))
	for _, name := range names {
		t, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("у демона нет инструмента %s — демон другой версии?", name)
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("схема %s не читается: %w", name, err)
		}
		out = append(out, tools.Func{
			S: tools.Spec{Name: name, Description: t.Description, Parameters: tools.Canon(schema),
				Untrusted: true, Via: tools.ViaMCP},
			Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
				raw, err := r.Call(ctx, name, args)
				if err != nil {
					// Модель читает ошибку словами и продолжает ход.
					return "", err
				}
				return string(raw), nil
			},
		})
	}
	return out, nil
}

// Status — состояние подключения и расписание демона. Ответ держится
// statusTTL: окно опрашивает его часто, а демону хватает проверки раз в
// несколько секунд.
func (r *Remote) Status(ctx context.Context) Status {
	if !r.Configured() {
		return Status{Conn: ConnOff, Reason: "адрес демона не задан (-facts-server \"\")",
			Hint: "запусти приложение с -facts-server " + DefaultServer + " или задай TRIVIA_SERVER", Checked: r.now()}
	}
	r.stMu.Lock()
	defer r.stMu.Unlock()
	if r.stCached && r.now().Sub(r.st.Checked) < statusTTL {
		return r.st
	}
	st := r.check(ctx)
	// Проверку, прерванную вызывающим (стартовая проверка на две
	// секунды), не кэшируем: следующий опрос спросит демон честно.
	if ctx.Err() == nil {
		r.st, r.stCached = st, true
	}
	return st
}

// Forget сбрасывает кэш состояния: следующий Status спросит демон.
func (r *Remote) Forget() {
	r.stMu.Lock()
	r.stCached = false
	r.stMu.Unlock()
}

func (r *Remote) check(ctx context.Context) Status {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	st := Status{Server: r.server}
	raw, err := r.Call(ctx, toolScheduleStatus, nil)
	st.Checked = r.now()
	var te *ToolError
	switch {
	case err == nil:
		st.Conn, st.Schedule = ConnOK, raw
	case errors.As(err, &te):
		// Демон жив, но расписание не отдал: подключение есть.
		st.Conn, st.Reason = ConnOK, "расписание не получено: "+te.Text
	case errors.Is(err, ErrDenied):
		st.Conn, st.Reason, st.Hint = ConnDenied, err.Error(), r.hintToken()
	default:
		st.Conn, st.Reason, st.Hint = ConnDown, err.Error(), r.hintStart()
	}
	if st.Conn == ConnOK {
		r.mu.Lock()
		st.Version = r.ver
		r.mu.Unlock()
	}
	return st
}

// hintStart — как поднять демон на том адресе, куда смотрит приложение.
func (r *Remote) hintStart() string {
	return "запусти animals-mcp -http " + r.listen() + "; если задан MCP_TOKEN, у демона он должен быть тот же"
}

func (r *Remote) hintToken() string {
	if r.token == "" {
		return "задай MCP_TOKEN — тот же токен, с которым запущен демон"
	}
	return "проверь MCP_TOKEN: у приложения и у демона он должен совпадать"
}

// listen — хост:порт из адреса демона для подсказки.
func (r *Remote) listen() string {
	raw := r.server
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return r.server
}

// Hint — подсказка к ошибке вызова: что сделать человеку.
func (r *Remote) Hint(err error) string {
	switch {
	case errors.Is(err, ErrOff):
		return "запусти приложение с -facts-server " + DefaultServer + " или задай TRIVIA_SERVER"
	case errors.Is(err, ErrDenied):
		return r.hintToken()
	case errors.Is(err, ErrDown):
		return r.hintStart()
	}
	return ""
}

// version — « (версия X)» для журнала; пусто, если демон её не назвал.
func (r *Remote) version() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ver == "" {
		return ""
	}
	return " (версия " + r.ver + ")"
}

// NewRemoteDial — клиент к любому MCP-серверу с готовым способом
// подключения: stdio-процесс (mcp.Launcher.Dial) или HTTP (mcp.HTTPDialer).
// label — как показывать сервер (имя из реестра серверов); вызовы, ошибки и
// неповторяемые инструменты — как у клиента демона. Status для такого
// клиента не годится: он спрашивает расписание демона.
func NewRemoteDial(label string, d mcp.Dialer, log *slog.Logger) *Remote {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Remote{server: label, log: log, dial: d, now: time.Now}
}

// List — инструменты сервера, как он их отдал в tools/list (подключается,
// если соединения ещё нет).
func (r *Remote) List(ctx context.Context) ([]*sdk.Tool, string, error) {
	if _, err := r.session(ctx); err != nil {
		return nil, "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*sdk.Tool(nil), r.list...), r.ver, nil
}

// Init — ответ initialize текущего соединения: имя, название и версия
// сервера и его инструкция. nil — соединения нет.
func (r *Remote) Init() *sdk.InitializeResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sess == nil {
		return nil
	}
	return r.sess.InitializeResult()
}
