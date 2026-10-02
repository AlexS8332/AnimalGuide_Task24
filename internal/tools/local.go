package tools

// Local — источники, которые работают в процессе приложения: Википедия и
// GBIF поверх общего кэша процесса (ФТ-2). Тот же набор отдаёт наружу
// MCP-сервер, поэтому базы адресов параметрами: в тестах это подставные
// серверы.
func Local(f *Fetcher, wikiBase, gbifBase string) []Source {
	return []Source{NewWikipedia(wikiBase, f), NewGBIF(gbifBase, f)}
}

// LocalTools — шесть инструментов источников в порядке SourceTools.
func LocalTools(f *Fetcher, wikiBase, gbifBase string) []Tool {
	r, err := FromSources(Local(f, wikiBase, gbifBase)...)
	if err != nil {
		panic(err)
	}
	return r.Pick(SourceTools...)
}
