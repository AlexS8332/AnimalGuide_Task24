package llmtest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

// Без сценария подставная модель отвечает ошибкой, но запрос запоминает.
func TestFakeWithoutScript(t *testing.T) {
	f := &Fake{}
	if _, err := f.Chat(context.Background(), llm.Request{Model: "m"}); err == nil {
		t.Fatal("ответ без сценария")
	}
	if f.Calls() != 1 || f.Requests[0].Model != "m" {
		t.Fatalf("запросы: %+v", f.Requests)
	}
}

// Параллельные агенты зовут модель одновременно: счётчик не теряет запросы.
func TestFakeConcurrentCalls(t *testing.T) {
	boom := errors.New("сеть")
	f := &Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if req.Model == "bad" {
			return llm.Response{}, boom
		}
		return Text("ок"), nil
	}}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Chat(context.Background(), llm.Request{Model: "m"})
		}()
	}
	wg.Wait()
	if f.Calls() != 20 {
		t.Fatalf("запросов %d", f.Calls())
	}
	if _, err := f.Chat(context.Background(), llm.Request{Model: "bad"}); !errors.Is(err, boom) {
		t.Fatalf("ошибка сценария: %v", err)
	}
}

// Готовые ответы: текст и вызовы инструментов с разными идентификаторами.
func TestResponses(t *testing.T) {
	txt := Text("привет")
	if txt.Message.Content != "привет" || txt.Message.Role != llm.RoleAssistant || txt.FinishReason != "stop" || txt.Usage.Total != 15 {
		t.Fatalf("текст: %+v", txt)
	}
	one := ToolCall("search_wikipedia", `{"query":"рысь"}`)
	if len(one.Message.ToolCalls) != 1 || one.FinishReason != llm.FinishToolCalls || one.Message.ToolCalls[0].Function.Arguments != `{"query":"рысь"}` {
		t.Fatalf("вызов: %+v", one)
	}
	two := ToolCalls(Call{Name: "a"}, Call{Name: "a"})
	if two.Message.ToolCalls[0].ID == two.Message.ToolCalls[1].ID || two.Message.ToolCalls[1].Type != "function" {
		t.Fatalf("идентификаторы вызовов совпали: %+v", two.Message.ToolCalls)
	}
}

// Помощники сценариев читают запрос: инструменты, шаг, последний ответ.
func TestRequestHelpers(t *testing.T) {
	req := llm.Request{
		Tools: []llm.ToolDef{llm.NewToolDef("read_wikipedia", "чтение", nil)},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "рысь"},
			{Role: llm.RoleTool, Content: "первый"},
			{Role: llm.RoleAssistant, Content: "думаю"},
			{Role: llm.RoleTool, Content: "второй"},
		},
	}
	if !HasTool(req, "read_wikipedia") || HasTool(req, "match_taxon") {
		t.Fatal("HasTool")
	}
	if ToolReplies(req) != 2 || LastToolReply(req) != "второй" {
		t.Fatalf("шаг %d, последний ответ %q", ToolReplies(req), LastToolReply(req))
	}
	empty := llm.Request{}
	if ToolReplies(empty) != 0 || LastToolReply(empty) != "" || HasTool(empty, "x") {
		t.Fatal("пустой запрос")
	}
}
