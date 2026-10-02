// Package notes — «блокнот натуралиста»: третий MCP-сервер справочника
// (animals-mcp -role notes, stdio). Агент открывает блокнот о виде,
// добавляет разделы со ссылками на инструменты-источники и закрывает его —
// блокнот становится файлом Markdown или JSON в <data>/notes/.
//
// Сети и модели у сервера нет. Идентификатор блокнота случайный: модель не
// может его угадать, поэтому nb_add и nb_close с верным notebook_id
// доказывают, что они шли после nb_open, — это зависимость данных, по
// которой проверяется порядок вызовов в длинном флоу.
package notes

// Имя сервера в initialize и server_info и имена инструментов.
const (
	ServerName = "animals-notes"
	ToolOpen   = "nb_open"
	ToolAdd    = "nb_add"
	ToolClose  = "nb_close"
)

// ToolNames — инструменты блокнота по порядку работы с ним.
var ToolNames = []string{ToolOpen, ToolAdd, ToolClose}

// OpenArgs — аргументы nb_open.
type OpenArgs struct {
	Title   string `json:"title" jsonschema:"заголовок блокнота, например «Манул — паспорт вида»"`
	Species string `json:"species" jsonschema:"латинское название вида"`
}

// OpenResult — ответ nb_open. NotebookID — «nb-» и 12 случайных hex-знаков.
type OpenResult struct {
	NotebookID string `json:"notebook_id"`
	Path       string `json:"path"` // куда ляжет файл после nb_close (Markdown)
}

// AddArgs — аргументы nb_add. Cites — имена инструментов, из ответов
// которых взят текст раздела (read_wikipedia, mdd_get, taxon_tree…).
type AddArgs struct {
	NotebookID string   `json:"notebook_id" jsonschema:"notebook_id из ответа nb_open"`
	Heading    string   `json:"heading" jsonschema:"заголовок раздела"`
	Text       string   `json:"text" jsonschema:"текст раздела, Markdown"`
	Cites      []string `json:"cites" jsonschema:"имена инструментов, из ответов которых взяты сведения раздела"`
}

// AddResult — ответ nb_add: номер раздела (с 1) и сколько их всего.
type AddResult struct {
	NotebookID string `json:"notebook_id"`
	Section    int    `json:"section"`
	Sections   int    `json:"sections"`
}

// CloseArgs — аргументы nb_close.
type CloseArgs struct {
	NotebookID string `json:"notebook_id" jsonschema:"notebook_id из ответа nb_open"`
	Format     string `json:"format,omitempty" jsonschema:"md (по умолчанию) или json"`
}

// CloseResult — ответ nb_close: файл записан, перечитан и сверен по
// sha256 (pipeline.WriteVerified). Cites — все инструменты, на которые
// сослались разделы, без повторов, по алфавиту.
type CloseResult struct {
	NotebookID string   `json:"notebook_id"`
	Path       string   `json:"path"`
	SHA256     string   `json:"sha256"`
	Bytes      int      `json:"bytes"`
	Sections   int      `json:"sections"`
	Cites      []string `json:"cites"`
	Preview    string   `json:"preview"` // начало текста файла
}
