package mcp

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPPath — путь Streamable HTTP; HealthPath — проверка живости без
// протокола (её дёргают curl, мониторинг и планировщик Windows).
const (
	MCPPath    = "/mcp"
	HealthPath = "/healthz"
)

// HTTPOptions — настройки HTTP-транспорта.
type HTTPOptions struct {
	// Token — если задан, каждый запрос к /mcp обязан нести заголовок
	// «Authorization: Bearer <Token>», иначе 401. /healthz открыт всегда:
	// в нём нет ни данных, ни платных действий.
	Token string
}

// Health — ответ /healthz.
type Health struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	Tools         int    `json:"tools"`
	UptimeSeconds int    `json:"uptime_seconds"`
}

// HTTPHandler — сервер по Streamable HTTP на MCPPath и GET HealthPath.
//
// Сессии — stateless: у нас несколько независимых клиентов (приложение,
// mcp-list, стенд) и простые вызовы «запрос — ответ», серверу не нужно ни
// спрашивать клиента (sampling, elicitation), ни слать ему уведомления вне
// ответа. Stateless-сервер не хранит сессий, не держит открытых
// SSE-потоков, не боится клиентов, которые исчезли, не закрыв сессию, и
// переживает перезапуск процесса незаметно для клиента. Ответы — обычный
// JSON, а не SSE: ответ всегда один, поток не нужен.
//
// Счётчики и журнал вызовов общие со stdio: это тот же сервер SDK с тем же
// промежуточным слоем, HTTP — лишь ещё один транспорт.
func (s *Server) HTTPHandler(o HTTPOptions) http.Handler {
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s.sdk },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: s.log})
	// Защита от запросов со страниц в браузере (CSRF на localhost): без
	// токена сервер на loopback доступен любой открытой вкладке. Клиенты
	// SDK заголовков Origin/Sec-Fetch-Site не шлют и проходят свободно.
	// Защиту от DNS rebinding SDK включает сам.
	mcpH := http.NewCrossOriginProtection().Handler(requireToken(o.Token, h))

	mux := http.NewServeMux()
	mux.Handle(MCPPath, mcpH)
	mux.HandleFunc("GET "+HealthPath, s.health)
	return s.accessLog(mux)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	// Число инструментов — как в server_info: без самого server_info.
	writeJSON(w, http.StatusOK, Health{Status: "ok", Version: s.version, Tools: len(s.names),
		UptimeSeconds: int(time.Since(s.started).Seconds())})
}

// requireToken пропускает запрос только с верным Bearer-токеном. Пустой
// токен — проверки нет: решение, можно ли так слушать адрес, принимает
// тот, кто запускает сервер (cmd/animals-mcp не даёт слушать не-loopback
// без токена).
func requireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, cred, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		// Сравнение за постоянное время: по времени ответа нельзя
		// подбирать токен по символу.
		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(cred)), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="animals-mcp"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "нужен заголовок Authorization: Bearer <токен>"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// accessLog — строка журнала на HTTP-запрос. Заголовок Authorization в
// журнал не попадает никогда. Вызовы инструментов журналирует ещё и
// промежуточный слой count — с именем и аргументами; здесь видно
// транспортное: кто, куда, с каким кодом.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"remote", r.RemoteAddr, "ms", time.Since(start).Milliseconds())
	})
}

// statusWriter запоминает код ответа. Flush и Unwrap сохраняют потоковую
// отдачу SDK: без них обёртка молча ломала бы SSE.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
