package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisplayShortensInsideWorkdir(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(cwd, "memory", "long", "me.json")
	if got := Display(inside); got != filepath.Join("memory", "long", "me.json") {
		t.Errorf("путь внутри рабочего каталога должен стать относительным: %q", got)
	}
	// Путь снаружи остаётся как есть: сокращать его нечем, а «..» в
	// интерфейсе читается хуже полного пути.
	outside := filepath.Join(filepath.VolumeName(cwd)+string(filepath.Separator), "чужое", "место.json")
	if got := Display(outside); !strings.Contains(got, "место.json") || strings.HasPrefix(got, "..") {
		t.Errorf("путь снаружи: %q", got)
	}
}

func TestValidID(t *testing.T) {
	good := []string{"me", "marina", "task-1", "report_all", "0123456789abcdef"}
	for _, id := range good {
		if !ValidID(id) {
			t.Errorf("идентификатор %q должен проходить", id)
		}
	}
	// Из идентификатора складывается путь, поэтому проверка строгая: точки
	// увели бы запись из каталога хранилища.
	bad := []string{"", "..", "../побег", "имя", "a/b", "a.b", strings.Repeat("x", 65)}
	for _, id := range bad {
		if ValidID(id) {
			t.Errorf("идентификатор %q проходить не должен", id)
		}
	}
}

func TestValidHex(t *testing.T) {
	if !ValidHex("0123456789abcdef") {
		t.Error("шестнадцатеричный идентификатор должен проходить")
	}
	for _, id := range []string{"", "короткий", "0123456789ABCDEF", "0123456789abcdefg", "../x"} {
		if ValidHex(id) {
			t.Errorf("идентификатор %q проходить не должен", id)
		}
	}
}
