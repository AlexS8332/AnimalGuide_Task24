package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/pipeline"
)

const chainToken = "chain-token"

// chainDaemon — подставной демон по Streamable HTTP с токеном: тот же
// MCP-сервер проекта, что у animals-mcp -http, но с инструментами chainServer.
func chainDaemon(t *testing.T) (*chainServer, *mcp.Server, string) {
	t.Helper()
	srv := newChainServer()
	ms := mcp.NewServer(srv.Tools(), mcp.ServerOptions{})
	hs := httptest.NewServer(ms.HTTPHandler(mcp.HTTPOptions{Token: chainToken}))
	t.Cleanup(hs.Close)
	return srv, ms, hs.URL
}

// Цепочка идёт по MCP: клиент приложения (feed.Remote) — Caller, счётчики
// MCP-сервера видят три вызова.
func TestRunOverMCP(t *testing.T) {
	for _, pass := range []string{pipeline.PassInline, pipeline.PassRef} {
		t.Run(pass, func(t *testing.T) {
			srv, ms, url := chainDaemon(t)
			remote := feed.NewRemote(url, chainToken, nil)
			defer remote.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			tr, err := pipeline.Run(ctx, remote, pipeline.Request{Query: "манул", Pass: pass}, nil)
			if err != nil || !tr.OK {
				t.Fatalf("Run: %v", err)
			}
			st := ms.Stats()
			for _, name := range pipeline.ToolNames {
				if st.Calls[name] != 1 {
					t.Errorf("MCP-сервер: %s вызван %d раз", name, st.Calls[name])
				}
			}
			if strings.Join(srv.Calls(), ",") != "search,summarize,save_to_file" {
				t.Errorf("вызовы: %v", srv.Calls())
			}
			if tr.File == nil || len(tr.File.Chain) != 2 || tr.File.Chain[1] != tr.Steps[1].Digest {
				t.Errorf("файл: %+v", tr.File)
			}
		})
	}
}

// Без токена демон не пускает — ошибка на первом шаге, остальные pending.
func TestRunOverMCPDenied(t *testing.T) {
	_, _, url := chainDaemon(t)
	remote := feed.NewRemote(url, "не тот", nil)
	defer remote.Close()
	tr, err := pipeline.Run(context.Background(), remote, pipeline.Request{Query: "манул"}, nil)
	if !errors.Is(err, feed.ErrDenied) {
		t.Fatalf("ждали ErrDenied, получили %v", err)
	}
	failedAt(t, tr, 1)
}

// sessionCaller — Caller прямо над сессией SDK: нужен, чтобы подставить
// свой HTTP-транспорт (feed.Remote берёт стандартный).
type sessionCaller struct{ s *sdk.ClientSession }

func (c sessionCaller) Call(ctx context.Context, tool string, args any) (json.RawMessage, error) {
	res, err := c.s.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, err
	}
	var parts []string
	for _, ct := range res.Content {
		if tc, ok := ct.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if res.IsError {
		return nil, errors.New(text)
	}
	return json.RawMessage(text), nil
}

// corruptSummarize — «сеть», которая портит один байт данных в запросе к
// summarize: латинское название в досье внутри input.data.
type corruptSummarize struct {
	base    http.RoundTripper
	touched atomic.Int32
}

func (c *corruptSummarize) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil && req.Method == http.MethodPost {
		body, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		if bytes.Contains(body, []byte(`"name":"summarize"`)) && bytes.Contains(body, []byte("Otocolobus")) {
			body = bytes.Replace(body, []byte("Otocolobus"), []byte("Otocolobuz"), 1)
			c.touched.Add(1)
		}
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	return c.base.RoundTrip(req)
}

// Порча данных по дороге к серверу: сервер пересчитывает отпечаток входа,
// отвечает ErrDigest, Run падает на шаге 2 и не зовёт save_to_file.
func TestRunOverMCPCorruptedInTransit(t *testing.T) {
	srv, ms, url := chainDaemon(t)
	rt := &corruptSummarize{base: http.DefaultTransport}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tp, _, err := mcp.HTTPDialer(url, chainToken, &http.Client{Transport: rt})(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sdk.NewClient(&sdk.Implementation{Name: "pipeline-test", Version: "1"}, nil).Connect(ctx, tp, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	tr, err := pipeline.Run(ctx, sessionCaller{sess}, pipeline.Request{Query: "манул"}, nil)
	if rt.touched.Load() != 1 {
		t.Fatalf("транспорт испортил %d запросов", rt.touched.Load())
	}
	if !errors.Is(err, pipeline.ErrDigest) {
		t.Fatalf("ждали ErrDigest, получили %v", err)
	}
	failedAt(t, tr, 2)
	if !strings.Contains(tr.Steps[1].Error, "шаг 2 (summarize)") || !strings.Contains(tr.Steps[1].Error, "отпечаток") {
		t.Errorf("ошибка шага 2: %s", tr.Steps[1].Error)
	}
	if ms.Stats().Errors[pipeline.ToolSummarize] != 1 || ms.Stats().Calls[pipeline.ToolSaveFile] != 0 {
		t.Errorf("счётчики сервера: %+v", ms.Stats())
	}
	if len(srv.Calls()) != 2 {
		t.Errorf("вызовы: %v", srv.Calls())
	}
}
