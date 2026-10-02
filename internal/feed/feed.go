// Package feed — «Интересные факты» в приложении: клиент к демону
// (animals-mcp -http) по MCP, механизм trivia для ведущего и REST для
// раздела интерфейса.
//
// Приложение ничего не собирает само: выпуски, сводки и расписание живут в
// демоне. Отсюда правило пакета — демона может не быть (не запущен, упал,
// другой токен), и приложение от этого не ломается: раздел показывает
// «демон не подключён» с подсказкой, ход идёт без инструментов механизма.
//
// # REST (префикс /api/facts)
//
// Все ответы — JSON. Тела ответов инструментов демона отдаются как есть
// (форматы — internal/daemon/tools.go), поле ошибки — {"error": "текст"}.
//
//	GET  /api/facts/status               → Status (ниже): подключение + schedule_status
//	GET  /api/facts/latest?limit=N        → facts_latest {"limit":N}
//	GET  /api/facts/issue?id=N            → facts_get {"id":N}
//	GET  /api/facts/issue?species=S       → facts_get {"species":S}
//	GET  /api/facts/search?text=&since=&until=&offset= → facts_search
//	GET  /api/facts/summary               → summary_get {}
//	GET  /api/facts/summary?id=N          → summary_get {"id":N}
//	GET  /api/facts/summaries             → summary_get {"list":true}
//	POST /api/facts/run      {"job":"issue"|"summary"|"mdd"} → run_now
//	POST /api/facts/summary/build {"hours":N}             → summary_build
//
// Коды: 200 — ответ инструмента; 422 — инструмент вернул ошибку (IsError:
// текст ошибки для человека, например «выпусков о виде … ещё не было»);
// 503 — демон недоступен или не настроен (текст с подсказкой, как
// запустить); 400 — неверный запрос; 405 — не тот метод.
//
// POST-запросы идут долго (выпуск — секунды, до пары минут): клиент ждёт
// ответа, сервер не держит свою очередь — «задание уже идёт» приходит 422
// от демона.
//
// REST конвейера search → summarize → save_to_file (/api/pipeline) — в
// pipes.go: прогон идёт в фоне, окно опрашивает его след.
package feed

import (
	"encoding/json"
	"time"
)

// DefaultServer — адрес демона по умолчанию (animals-mcp -http 127.0.0.1:8766).
const DefaultServer = "http://127.0.0.1:8766"

// Состояния подключения к демону.
const (
	ConnOff     = "off"     // адрес не задан (механизм и раздел выключены)
	ConnOK      = "ok"      // подключён
	ConnDown    = "down"    // не отвечает
	ConnDenied  = "denied"  // отверг токен (401)
	ConnUnknown = "unknown" // ещё не пробовали
)

// Status — ответ GET /api/facts/status.
type Status struct {
	Conn    string `json:"conn"`             // Conn*
	Server  string `json:"server,omitempty"` // адрес без токена
	Version string `json:"version,omitempty"`
	Reason  string `json:"reason,omitempty"` // почему не ok, словами
	// Hint — что сделать человеку, если не ok: «запусти animals-mcp -http
	// 127.0.0.1:8766» / «задай MCP_TOKEN».
	Hint string `json:"hint,omitempty"`
	// Schedule — ответ schedule_status как есть; nil, если не ok.
	Schedule json.RawMessage `json:"schedule,omitempty"`
	Checked  time.Time       `json:"checked"`
}
