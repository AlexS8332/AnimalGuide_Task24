package main

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Команды ручного режима разбираются без соединения: всё, что не
// доходит до сервера, проверяется здесь.
func replFixture() *replSession {
	return &replSession{tools: []*mcp.Tool{
		{Name: "get_animal", Title: "Карточка животного", InputSchema: map[string]any{"type": "object"}},
		{Name: "list_animals", Title: "Список животных", InputSchema: map[string]any{"type": "object"}},
		{Name: "server_info", Title: "Сведения о сервере", InputSchema: map[string]any{"type": "object"}},
	}}
}

func TestReplCommands(t *testing.T) {
	r := replFixture()
	ctx := t.Context()

	for _, command := range []string{"quit", "exit", "q"} {
		done, err := r.exec(ctx, command)
		if err != nil || !done {
			t.Errorf("%s: done=%v, err=%v", command, done, err)
		}
	}

	for _, command := range []string{"help", "?", "tools", "tools animal", "tools ничего", "schema get_animal", "schema get_animal json"} {
		done, err := r.exec(ctx, command)
		if err != nil {
			t.Errorf("%s: неожиданная ошибка %v", command, err)
		}
		if done {
			t.Errorf("%s: команда завершила сессию", command)
		}
	}

	// «full on» включает полный вывод, «full off» выключает, «full» без
	// значения только показывает текущее состояние.
	if _, err := r.exec(ctx, "full on"); err != nil || !r.full {
		t.Errorf("full on: full=%v, err=%v", r.full, err)
	}
	if _, err := r.exec(ctx, "full"); err != nil || !r.full {
		t.Errorf("full без значения поменял режим: full=%v, err=%v", r.full, err)
	}
	if _, err := r.exec(ctx, "full off"); err != nil || r.full {
		t.Errorf("full off: full=%v, err=%v", r.full, err)
	}
}

func TestReplUnknownSuggests(t *testing.T) {
	r := replFixture()

	_, err := r.exec(t.Context(), "get_anim")
	if err == nil {
		t.Fatal("опечатка принята за команду")
	}
	if !strings.Contains(err.Error(), "get_animal") {
		t.Errorf("подсказка не предложила похожее: %v", err)
	}

	_, err = r.exec(t.Context(), "варкалось")
	if err == nil || !strings.Contains(err.Error(), "tools") {
		t.Errorf("непохожее имя: %v", err)
	}
}

func TestReplSchemaNeedsTool(t *testing.T) {
	r := replFixture()

	if _, err := r.exec(t.Context(), "schema"); err == nil {
		t.Error("schema без инструмента прошла")
	}
	if _, err := r.exec(t.Context(), "call"); err == nil {
		t.Error("call без инструмента прошёл")
	}
	if _, err := r.exec(t.Context(), "full наполовину"); err == nil {
		t.Error("full с чужим значением прошёл")
	}
}
