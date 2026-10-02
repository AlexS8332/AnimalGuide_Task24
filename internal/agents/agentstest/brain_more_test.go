package agentstest

import (
	"errors"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

func req(system string, msgs ...llm.Message) llm.Request {
	return llm.Request{Messages: append([]llm.Message{{Role: llm.RoleSystem, Content: system}}, msgs...)}
}

func user(s string) llm.Message { return llm.Message{Role: llm.RoleUser, Content: s} }

// Привратник: НЕТ на названия из GateNo, ДА на остальные; счётчик по агенту.
func TestBrainGatekeeper(t *testing.T) {
	b := &Brain{GateNo: []string{"единорог"}}
	no, _ := b.Chat(req("Ты — зоолог-систематик", user("Единорог")))
	yes, _ := b.Chat(req("Ты — зоолог-систематик", user("рысь")))
	if !strings.Contains(no.Message.Content, "НЕТ") || !strings.Contains(yes.Message.Content, "ДА") {
		t.Fatalf("привратник: %q / %q", no.Message.Content, yes.Message.Content)
	}
	if b.Calls("gatekeeper") != 2 || b.Calls("lead") != 0 {
		t.Fatal("счётчик")
	}
}

// Незнакомый системный промпт — ошибка: сценарий не угадывает агента.
func TestBrainUnknownAgent(t *testing.T) {
	if _, err := (&Brain{}).Chat(req("Ты — кто-то новый", user("x"))); err == nil {
		t.Fatal("незнакомый агент")
	}
}

// Составитель, извлекатель и ведущий: умолчания и свои сценарии.
func TestBrainScripts(t *testing.T) {
	b := &Brain{}
	if r, _ := b.Chat(req("Ты ведёшь подборку справочника", user("x"))); r.Message.Content != "Подборка идёт." {
		t.Fatalf("составитель по умолчанию: %q", r.Message.Content)
	}
	if r, _ := b.Chat(req("Ты ведёшь память и профиль", user("x"))); !strings.Contains(r.Message.Content, `"profile"`) {
		t.Fatalf("извлекатель по умолчанию: %q", r.Message.Content)
	}
	if r, _ := b.Chat(req("Ты ведёшь разговор справочника", user("x"))); r.Message.Content != "Ответ ведущего." {
		t.Fatalf("ведущий по умолчанию: %q", r.Message.Content)
	}
	b.Extract = func(llm.Request) (string, error) { return "", errors.New("сломался") }
	if _, err := b.Chat(req("Ты ведёшь память и профиль", user("x"))); err == nil {
		t.Fatal("ошибка извлекателя потерялась")
	}
	b.Extract = func(llm.Request) (string, error) { return "{}", nil }
	if r, _ := b.Chat(req("Ты ведёшь память и профиль", user("x"))); r.Message.Content != "{}" {
		t.Fatal("свой извлекатель")
	}
	steps := -1
	b.Compiler = func(_ llm.Request, step int) llm.Response { steps = step; return llm.Response{} }
	b.Chat(req("Ты ведёшь подборку справочника", user("x"), llm.Message{Role: llm.RoleTool, Content: "a"}))
	if steps != 1 || b.Calls("compiler") != 2 {
		t.Fatalf("шаг составителя: %d", steps)
	}
}

// Идентификатор без инструментов и в режиме «текстом».
func TestBrainIdentifier(t *testing.T) {
	b := &Brain{}
	r, _ := b.Chat(req("агент-идентификатор", user("Запрос пользователя: «рысь».")))
	if len(r.Message.ToolCalls) != 1 || r.Message.ToolCalls[0].Function.Name != "search_wikipedia" {
		t.Fatalf("первый шаг: %+v", r.Message)
	}
	b.TextOnly = true
	r, _ = b.Chat(req("агент-идентификатор", user("Запрос пользователя: «рысь».")))
	if !strings.Contains(r.Message.Content, "Lynx rufus") {
		t.Fatalf("текстом: %q", r.Message.Content)
	}
}

// Помощники разбора запроса: реплика, шаг хода, ответы инструментов.
func TestRequestHelpers(t *testing.T) {
	r := llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "s"},
		user("первая"),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Function: llm.FunctionCall{Name: "read_wikipedia"}}}},
		{Role: llm.RoleTool, ToolCallID: "c1", Content: "статья"},
		user("вторая"),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c2", Function: llm.FunctionCall{Name: "match_taxon"}}}},
		{Role: llm.RoleTool, ToolCallID: "c2", Content: "таксон"},
	}}
	if LastUser(r) != "вторая" || TurnSteps(r) != 1 || len(ToolReplies(r)) != 2 {
		t.Fatalf("реплика %q, шаг %d", LastUser(r), TurnSteps(r))
	}
	if LastReply(r, "read_wikipedia") != "статья" || LastReply(r, "search_wikipedia") != "" {
		t.Fatal("LastReply")
	}
	if LastUser(llm.Request{}) != "" || Args(map[string]int{"n": 1}) != `{"n":1}` {
		t.Fatal("мелочи")
	}
	if lastWord("") != "" || lastWord("обыкновенная рысь") != "рысь" || lastWord("кошки") != "кошк" {
		t.Fatalf("lastWord: %q", lastWord("кошки"))
	}
	if firstSentence("Одно. Два.") != "Одно." || firstSentence("без точки") != "без точки" {
		t.Fatal("firstSentence")
	}
}
