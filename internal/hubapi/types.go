// Package hubapi — REST реестра MCP-серверов и длинного флоу для окна
// «MCP-серверы»:
//
//	GET  /api/hub/servers        серверы (hub.ServerView) и маршруты (hub.Route)
//	POST /api/hub/connect        подключиться ко всем → то же, что GET servers
//	GET  /api/hub/presets        заготовки флоу (id, title, species)
//	POST /api/hub/flows          {preset, species} → 202 {id}; 409 — флоу уже идёт
//	GET  /api/hub/flows/{id}     прогон: состояние, вызовы по мере хода, итог (flow.Trace)
//	GET  /api/hub/flows          последние прогоны, новые сверху
//
// Прогон идёт в фоне, интерфейс опрашивает GET /api/hub/flows/{id} — как у
// вкладки «Конвейер» (feed.Pipelines).
package hubapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/flow"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/hub"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/server"
)

// Prefix — префикс раздела API.
const Prefix = "/api/hub/"

// RunFunc — прогон флоу (в приложении — flow.Run с Runner и реестром).
type RunFunc func(ctx context.Context, p flow.Preset, species string, onCall func(flow.Call)) (flow.Trace, error)

// API — раздел REST. Router nil — реестр выключен: GET servers отвечает
// пустым списком с причиной, POST — 503. Run nil — флоу недоступен (нет
// ключа модели): POST flows — 503 с подсказкой.
type API struct {
	Router  hub.Router
	Run     RunFunc
	Presets []flow.Preset
	// Why — почему реестр или флоу выключены (для 503 и окна).
	Why string
	// Timeout — предел одного прогона; 0 — flowTimeout.
	Timeout time.Duration

	mu   sync.Mutex
	seq  int
	runs []*flowRun // старые первыми
}

// Extension — раздел для server.New.
func (a *API) Extension() []server.Extension {
	return []server.Extension{{Prefix: Prefix, Handler: http.HandlerFunc(a.handle)}}
}
