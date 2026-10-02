package pipeline

// WriteVerified пишет файл атомарно, перечитывает и сверяет sha256 —
// тем же путём, что save_to_file. Нужен другим серверам, которые
// сохраняют файлы (блокнот натуралиста). Возвращает sha256 файла.
func WriteVerified(full string, body []byte) (string, error) { return writeVerified(full, body) }
