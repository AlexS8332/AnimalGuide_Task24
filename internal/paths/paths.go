// Package paths — мелочи про файлы, общие для хранилищ. Диалоги и слои
// памяти лежат в разных каталогах и ничего друг о друге не знают, но
// именуют файлы одинаково и одинаково показывают пути человеку; чтобы это
// правило было одно, а не два, оно живёт здесь.
package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// Display — путь в том виде, в каком его показывают в журнале запуска и в
// интерфейсе: относительно рабочего каталога, если лежит внутри него, и
// как есть в остальных случаях. Внутри хранилища держат абсолютный путь —
// он не зависит от того, сменит ли процесс каталог, — а наружу уходит
// короткий: обычно это просто «history» или «memory/long/me.json», а
// заодно из интерфейса и скриншотов не торчит устройство чужой машины.
func Display(path string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}

// ValidID — идентификатор безопасен как имя файла. Из него складывается
// путь, поэтому проверка строгая: только буквы латиницы, цифры, дефис и
// подчёркивание, и никаких точек — иначе «..» уводит запись из каталога
// хранилища.
func ValidID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// ValidHex — идентификатор из шестнадцатеричных знаков: такими пакет
// history именует диалоги.
func ValidHex(id string) bool {
	if len(id) < 8 || len(id) > 32 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
