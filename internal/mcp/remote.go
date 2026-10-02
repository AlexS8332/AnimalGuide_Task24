package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrUnauthorized — сервер ответил 401: токена нет или он не тот. Отдельная
// ошибка, а не «Unauthorized» из глубины SDK: человеку надо сказать, что
// чинить, а переподключаться с тем же токеном бесполезно.
var ErrUnauthorized = errors.New("MCP-сервер отверг токен (401): задай MCP_TOKEN")

// remotePath — путь MCP на сервере-демоне, если в адресе его нет.
const remotePath = "/mcp"

// HTTPDialer — Dialer для сервера, который уже работает демоном: соединение
// по Streamable HTTP, процесса нет (Cmd — nil). Каждое подключение — новый
// транспорт и новая сессия: после перезапуска демона старая сессия ему
// незнакома, и клиент поднимает её заново так же, как новый процесс.
//
// url — адрес сервера: «http://127.0.0.1:8766», «127.0.0.1:8766» или с
// путём; без пути добавляется /mcp. token — если не пуст, уходит заголовком
// Authorization: Bearer в каждом запросе; в URL и журнал он не попадает.
// hc — HTTP-клиент (nil — стандартный); Timeout у него задавать нельзя:
// поток событий сервера (GET) живёт всё соединение, и общий предел оборвал
// бы его. Пределы на вызовы задаёт контекст.
func HTTPDialer(url, token string, hc *http.Client) Dialer {
	endpoint, err := remoteEndpoint(url)
	return func(ctx context.Context) (sdk.Transport, *exec.Cmd, error) {
		if err != nil {
			return nil, nil, err
		}
		client := &http.Client{}
		if hc != nil {
			c := *hc
			client = &c
		}
		client.Transport = &remoteAuth{base: client.Transport, token: token}
		return &sdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}, nil, nil
	}
}

// remoteEndpoint — полный адрес MCP: схема по умолчанию http, путь по
// умолчанию /mcp. Логин и пароль в адресе не принимаются: секрет в URL
// рано или поздно окажется в журнале, для него есть токен.
func remoteEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("не задан адрес MCP-сервера")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("адрес MCP-сервера не разобран: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("адрес MCP-сервера: схема %q, нужна http или https", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("адрес MCP-сервера %q без хоста", raw)
	}
	if u.User != nil {
		return "", errors.New("адрес MCP-сервера с логином или паролем: токен передаётся отдельно (MCP_TOKEN)")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = remotePath
	}
	return u.String(), nil
}

// remoteAddr — адрес сервера для состояния и журнала, если транспорт
// сетевой; у процесса пусто. Токена в адресе нет: он живёт в заголовке.
func remoteAddr(t sdk.Transport) string {
	if st, ok := t.(*sdk.StreamableClientTransport); ok {
		return st.Endpoint
	}
	return ""
}

// remoteAuth — обёртка транспорта HTTP: добавляет токен и превращает 401 в
// ErrUnauthorized. Ошибку, а не ответ: SDK сворачивает статус в голое
// «Unauthorized», а ошибка транспорта доходит до вызывающего по цепочке %w.
type remoteAuth struct {
	base  http.RoundTripper
	token string
}

func (a *remoteAuth) RoundTrip(req *http.Request) (*http.Response, error) {
	base := a.base
	if base == nil {
		base = http.DefaultTransport
	}
	if a.token != "" {
		// Запрос чужой — по контракту RoundTripper его не меняют.
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, ErrUnauthorized
	}
	return resp, nil
}
