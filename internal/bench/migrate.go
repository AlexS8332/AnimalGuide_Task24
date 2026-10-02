package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/memory"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/profile"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

// Migration — памятник прошлого формата и что с ним стало при чтении.
type Migration struct {
	File   string `json:"file"`
	Kind   string `json:"kind"`
	Schema int    `json:"schema"`
	// Lost — тексты памятника, которых нет в поднятом файле.
	Lost  []string `json:"lost,omitempty"`
	Texts int      `json:"texts"`
	Err   string   `json:"error,omitempty"`
}

// OK — поднялся без ошибок и без потерь.
func (m Migration) OK() bool { return m.Err == "" && len(m.Lost) == 0 }

// legacyKinds — какой каталог памятников каким видом файла читается.
func legacyKind(dir, name string) (*store.Kind, any, error) {
	switch dir {
	case "history":
		return history.Kind, &history.Conversation{}, nil
	case "profiles":
		return profile.Kind, &profile.Profile{}, nil
	case "memory":
		layer := memory.LayerLong
		if strings.Contains(name, "work") {
			layer = memory.LayerWork
		}
		k, err := memory.KindOf(layer)
		return k, &memory.Card{}, err
	}
	return nil, nil, fmt.Errorf("неизвестный вид памятника %q", dir)
}

// CheckLegacy поднимает все памятники каталога (ФТ-52) и сверяет, что
// ни один текст не потерялся: каждая содержательная строка старого файла
// (реплика, ответ, значение памяти, цитата) должна найтись в поднятом.
// Коды значений, которые миграция переводит («ты» → «ty»), в сверку не
// идут: это не текст, а смысл, и его проверяют тесты пакетов.
func CheckLegacy(root string) ([]Migration, error) {
	var out []Migration
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
				continue
			}
			out = append(out, checkOne(filepath.Join(root, d.Name(), f.Name()), d.Name(), f.Name()))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

func checkOne(path, dir, name string) Migration {
	m := Migration{File: dir + "/" + name, Kind: dir}
	data, err := os.ReadFile(path)
	if err != nil {
		m.Err = err.Error()
		return m
	}
	kind, v, err := legacyKind(dir, name)
	if err != nil {
		m.Err = err.Error()
		return m
	}
	meta, err := store.Decode(kind, data, v)
	if err != nil {
		m.Err = err.Error()
		return m
	}
	m.Schema = meta.Schema
	now, err := json.Marshal(v)
	if err != nil {
		m.Err = err.Error()
		return m
	}
	var before, after any
	if err := json.Unmarshal(data, &before); err != nil {
		m.Err = err.Error()
		return m
	}
	json.Unmarshal(now, &after)
	var have []string
	walkStrings(after, func(s string) { have = append(have, s) })
	joined := strings.Join(have, "\x00")
	walkStrings(before, func(s string) {
		if !content(s) {
			return
		}
		m.Texts++
		if !strings.Contains(joined, s) {
			m.Lost = append(m.Lost, clip(s, 60))
		}
	})
	return m
}

func walkStrings(v any, fn func(string)) {
	switch x := v.(type) {
	case string:
		fn(x)
	case []any:
		for _, e := range x {
			walkStrings(e, fn)
		}
	case map[string]any:
		for _, e := range x {
			walkStrings(e, fn)
		}
	}
}

// content — строка, которую человек или модель написали словами: не код
// значения, не время и не идентификатор.
func content(s string) bool {
	if len([]rune(s)) < 12 || !strings.ContainsRune(s, ' ') {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return false
	}
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}
