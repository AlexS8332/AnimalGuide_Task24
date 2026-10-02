package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Подключение к серверу, который уже работает демоном по HTTP. Клиент
// остаётся независимым от приложения: транспорт и токен — здесь, своими
// двадцатью строками, а не через внутренние пакеты.

// remotePath — путь MCP, если в адресе его нет.
const remotePath = "/mcp"

// errUnauthorized — сервер ответил 401. Из SDK приходит голое
// «Unauthorized» в глубине цепочки, а человеку надо сказать, что чинить.
var errUnauthorized = errors.New("MCP-сервер отверг токен (401): задай MCP_TOKEN")

// httpTransport — Streamable HTTP на адрес; токен, если есть, уходит
// заголовком Authorization и больше никуда.
func httpTransport(raw, token string) (*mcp.StreamableClientTransport, error) {
	endpoint, err := remoteEndpoint(raw)
	if err != nil {
		return nil, err
	}
	return &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: &bearer{token: token}},
	}, nil
}

// remoteEndpoint дополняет адрес: схема по умолчанию http, путь — /mcp.
// Логин и пароль в адресе не принимаются: адрес печатается на экран.
func remoteEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("-url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("-url: схема %q, нужна http или https", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("-url: в %q нет хоста", raw)
	}
	if u.User != nil {
		return "", errors.New("-url: логин и пароль в адресе не нужны, токен задаётся -token или MCP_TOKEN")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = remotePath
	}
	return u.String(), nil
}

// bearer добавляет токен к каждому запросу и превращает 401 в
// errUnauthorized: ошибка транспорта доходит до Connect по цепочке %w.
type bearer struct {
	token string
}

func (b *bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	if b.token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, errUnauthorized
	}
	return resp, nil
}

// readOnly — инструмент объявил, что только читает. Без аннотации — не
// объявил: по спецификации MCP такой инструмент может менять состояние, и
// проверочные вызовы его не трогают.
func readOnly(t *mcp.Tool) bool {
	return t.Annotations != nil && t.Annotations.ReadOnlyHint
}

// writeMark — пометка в списке у инструментов, которые не только читают.
const writeMark = "[меняет состояние / платный]"
