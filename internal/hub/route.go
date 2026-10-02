package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/feed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// Причины скрытых маршрутов.
const (
	reasonService    = "служебный"
	reasonNotAllowed = "не разрешён"
	reasonDupPrefix  = "дубль → "
)

// route — маршрут инструмента name сервера s: выдан ли он модели и если
// нет, то почему. Зависит только от конфигурации, поэтому чистая функция:
// один и тот же ответ в Routes, Tools и счётчиках Servers.
func (h *Hub) route(s *server, name string) Route {
	r := Route{Tool: name, Server: s.cfg.Name}
	switch {
	case name == mcp.InfoTool:
		r.Hidden, r.Reason = true, reasonService
	case allowed(s.cfg.Tools, name):
	default:
		r.Hidden, r.Reason = true, reasonNotAllowed
		// Имя выдано другому серверу — значит, это дубль: у демона те же
		// источники, что у stdio-сервера. Владелец один (Validate), поэтому
		// первый найденный и есть он.
		for _, o := range h.servers {
			if o != s && allowed(o.cfg.Tools, name) {
				r.Reason = reasonDupPrefix + o.cfg.Name
				break
			}
		}
	}
	return r
}

// Routes — маршруты всех инструментов всех подключённых серверов, включая
// скрытые, в порядке конфигурации и tools/list.
func (h *Hub) Routes(ctx context.Context) ([]Route, error) {
	if err := h.ensure(ctx); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Route
	for _, s := range h.servers {
		if s.status != StatusOK {
			continue
		}
		for _, t := range s.list {
			out = append(out, h.route(s, t.Name))
		}
	}
	return out, nil
}

// Tools — инструменты для модели (без скрытых), в порядке конфигурации и
// tools/list: порядок стабилен, а значит стабилен и префикс запроса к
// модели.
//
// Описание и схема — как их отдал сервер (схема в каноническом виде);
// ответ — пересказ внешних источников или текст модели, поэтому
// Untrusted. Вызов уходит клиенту своего сервера; сбой связи переписан
// словами этого сервера (unavailable), ошибка инструмента — как есть:
// модель читает её и продолжает ход.
func (h *Hub) Tools(ctx context.Context) ([]Bound, error) {
	if err := h.ensure(ctx); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Bound
	for _, s := range h.servers {
		if s.status != StatusOK {
			continue
		}
		for _, t := range s.list {
			r := h.route(s, t.Name)
			if r.Hidden {
				continue
			}
			tool, err := h.bind(s, t)
			if err != nil {
				return nil, err
			}
			out = append(out, Bound{Route: r, Tool: tool})
		}
	}
	return out, nil
}

// bind — инструмент сервера как tools.Tool. Write — если сервер не пометил
// его «только чтение» (mcp.Server так помечает всё, что не Write).
func (h *Hub) bind(s *server, t *sdk.Tool) (tools.Tool, error) {
	schema, err := json.Marshal(t.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("схема %s сервера %s не читается: %w", t.Name, s.cfg.Name, err)
	}
	name := t.Name
	write := t.Annotations == nil || !t.Annotations.ReadOnlyHint
	return tools.Func{
		S: tools.Spec{Name: name, Description: t.Description, Parameters: tools.Canon(schema),
			Untrusted: true, Write: write, Via: tools.ViaMCP},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			h.mu.Lock()
			s.calls++
			h.mu.Unlock()
			raw, err := s.remote.Call(ctx, name, args)
			if err != nil {
				return "", h.callFailed(ctx, s, err)
			}
			h.revive(ctx, s)
			return string(raw), nil
		},
	}, nil
}

// callFailed — ошибка вызова. Ошибка инструмента — как есть: сервер жив.
// Сбой связи — текст словами сервера и статус down (или denied): окно
// «MCP-серверы» покажет это сразу, не дожидаясь нового Connect.
func (h *Hub) callFailed(ctx context.Context, s *server, err error) error {
	if isToolError(err) {
		return err
	}
	// Вызывающий сам оборвал вызов (предел хода, отмена флоу) — сервер
	// тут ни при чём, статус не трогаем.
	if ctx.Err() == nil {
		h.setDown(s, err)
	}
	return s.unavailable(err)
}

// Snapshot — server_info каждого подключённого сервера. Сервер, который не
// ответил, в снимке отсутствует и получает статус down; ошибка — только
// если не ответил ни один из подключённых.
func (h *Hub) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := h.ensure(ctx); err != nil {
		return nil, err
	}
	h.mu.Lock()
	var live []*server
	for _, s := range h.servers {
		if s.status == StatusOK {
			live = append(live, s)
		}
	}
	h.mu.Unlock()

	snap := Snapshot{}
	var errs []error
	for _, s := range live {
		raw, err := s.remote.Call(ctx, mcp.InfoTool, nil)
		if err != nil {
			if !isToolError(err) && ctx.Err() == nil {
				h.setDown(s, err)
			}
			errs = append(errs, s.unavailable(err))
			continue
		}
		var info mcp.Info
		if err := json.Unmarshal(raw, &info); err != nil {
			errs = append(errs, fmt.Errorf("%s: %s не разобран: %w", s.cfg.Name, mcp.InfoTool, err))
			continue
		}
		snap[s.cfg.Name] = info
	}
	if len(snap) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return snap, nil
}

func isToolError(err error) bool {
	var te *feed.ToolError
	return errors.As(err, &te)
}

// revive — вызов прошёл, а сервер числится лежащим (прошлый вызов упал,
// клиент с тех пор переподключился сам): подключение проверяется заново,
// чтобы статус и число инструментов в окне снова были правдой.
func (h *Hub) revive(ctx context.Context, s *server) {
	h.mu.Lock()
	down := s.status == StatusDown || s.status == StatusDenied
	h.mu.Unlock()
	if down {
		h.connectOne(ctx, s)
	}
}
