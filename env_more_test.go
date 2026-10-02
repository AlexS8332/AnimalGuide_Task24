package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unsetForTest убирает переменные на время теста и возвращает их после.
func unsetForTest(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		old, had := os.LookupEnv(key)
		os.Unsetenv(key)
		t.Cleanup(func() {
			if had {
				os.Setenv(key, old)
			} else {
				os.Unsetenv(key)
			}
		})
	}
}

// Значение режется по первому «=»: в URL и ключах знак равенства бывает;
// одинарные кавычки снимаются так же, как двойные.
func TestLoadEnvFileValueWithEquals(t *testing.T) {
	unsetForTest(t, "TEST_URL", "TEST_SINGLE")
	loadEnvFile(writeEnvFile(t, "TEST_URL=http://x/?a=1&b=2\nTEST_SINGLE='одинарные'\n"))
	if got := os.Getenv("TEST_URL"); got != "http://x/?a=1&b=2" {
		t.Errorf("TEST_URL = %q", got)
	}
	if got := os.Getenv("TEST_SINGLE"); got != "одинарные" {
		t.Errorf("TEST_SINGLE = %q", got)
	}
}

// Строка длиннее буфера сканера: ключи до неё прочитаны, а обрыв не
// проходит молча (предупреждение в stderr).
func TestLoadEnvFileTooLongLine(t *testing.T) {
	unsetForTest(t, "TEST_BEFORE_LONG", "TEST_AFTER_LONG")
	content := "TEST_BEFORE_LONG=до\nTEST_LONG=" + strings.Repeat("x", 70<<10) + "\nTEST_AFTER_LONG=после\n"

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	loadEnvFile(writeEnvFile(t, content))
	os.Stderr = oldStderr
	w.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	r.Close()

	if os.Getenv("TEST_BEFORE_LONG") != "до" {
		t.Error("ключ до длинной строки потерян")
	}
	if !strings.Contains(string(buf[:n]), "прочитан не полностью") {
		t.Errorf("нет предупреждения об обрыве: %q", buf[:n])
	}
}

// Ближний .env.local важнее дальнего .env: поиск идёт снизу вверх, а уже
// заданное не перетирается.
func TestLoadEnvFilesNearestWins(t *testing.T) {
	const key = "TEST_NEAREST_KEY"
	unsetForTest(t, key)
	root := t.TempDir()
	nested := filepath.Join(root, "проект")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, ".env"), []byte(key+"=дальний\n"), 0o600)
	os.WriteFile(filepath.Join(nested, ".env"), []byte(key+"=ближний .env\n"), 0o600)
	os.WriteFile(filepath.Join(nested, ".env.local"), []byte(key+"=ближний .env.local\n"), 0o600)
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(original) })

	loadEnvFiles()
	if got := os.Getenv(key); got != "ближний .env.local" {
		t.Fatalf("%s = %q", key, got)
	}
}
