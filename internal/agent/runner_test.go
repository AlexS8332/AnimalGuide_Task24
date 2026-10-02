package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tokens"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

func echo(name string, untrusted bool, out string) tools.Tool {
	return tools.Func{
		S: tools.Spec{Name: name, Description: "инструмент " + name, Parameters: json.RawMessage(`{"type":"object"}`),
			Untrusted: untrusted, Via: tools.ViaLocal},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			if out == "" {
				return fmt.Sprintf(`{"args":%s,"call":%q}`, string(args), tools.CallID(ctx)), nil
			}
			return out, nil
		},
	}
}

func failing(name string) tools.Tool {
	return tools.Func{S: tools.Spec{Name: name, Description: "ломается"},
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return "", errors.New("источник недоступен")
		}}
}

func runner(fn func(llm.Request) (llm.Response, error)) (Runner, *llmtest.Fake) {
	f := &llmtest.Fake{Fn: fn}
	return Runner{LLM: f, Model: llm.DefaultModel}, f
}

func allOn() features.Set { return features.Catalog().AllOn() }

func TestRunnerToolLoopWithHistoryAndBlocks(t *testing.T) {
	r, fake := runner(func(req llm.Request) (llm.Response, error) {
		if llmtest.ToolReplies(req) == 0 {
			return llmtest.ToolCall("lookup", `{"q":"рысь"}`), nil
		}
		return llmtest.Text("Рысь живёт в лесу."), nil
	})
	history := []llm.Message{{Role: llm.RoleUser, Content: "привет"}, {Role: llm.RoleAssistant, Content: "здравствуй"}}
	blocks := []features.Block{{Feature: features.Charter, Text: "свод"}, {Feature: features.Profile, Text: "профиль"}}
	rec := &Recorder{}
	reply, err := r.Run(context.Background(), Spec{Name: "lead", System: "система", Tools: []tools.Tool{echo("lookup", false, "")}},
		Prepared{Blocks: blocks, History: history, User: "где живёт рысь?", Features: allOn()}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "Рысь живёт в лесу." {
		t.Fatalf("ответ: %q", reply.Text)
	}
	// user, assistant(call), tool, assistant(text)
	if len(reply.Added) != 4 || reply.Added[0].Role != llm.RoleUser || reply.Added[2].Role != llm.RoleTool {
		t.Fatalf("добавлено: %+v", reply.Added)
	}
	req := fake.Requests[0]
	roles := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		roles[i] = m.Role
	}
	if strings.Join(roles, ",") != "system,system,system,user,assistant,user" {
		t.Fatalf("порядок сообщений: %v", roles)
	}
	if req.Messages[1].Content != "свод" || req.Messages[2].Content != "профиль" {
		t.Fatalf("блоки не по порядку: %+v", req.Messages[1:3])
	}
	if reply.Stats.Steps != 2 || reply.Stats.ToolCalls != 1 || reply.Stats.Context.Estimate.Block(features.Charter) == 0 {
		t.Fatalf("счётчики: %+v", reply.Stats)
	}
	// CallID доходит до инструмента: трекер связывает по нему факт и журнал.
	if !strings.Contains(reply.Added[2].Content, `"call":"call_0_lookup"`) {
		t.Fatalf("идентификатор вызова не дошёл: %s", reply.Added[2].Content)
	}
	kinds := strings.Join(rec.Kinds(), ",")
	if !strings.HasPrefix(kinds, "prompt,llm.request,llm.response,tool.call,tool.result,llm.request,llm.response") {
		t.Fatalf("журнал: %s", kinds)
	}
	var p Prompt
	if err := json.Unmarshal([]byte(rec.Events[0].Detail), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Blocks) != 2 || p.Fingerprint == "" || p.History != 2 || len(p.Tools) != 1 {
		t.Fatalf("промпт: %+v", p)
	}
}

func TestRunnerEmptyHistoryFirstTurn(t *testing.T) {
	r, _ := runner(func(llm.Request) (llm.Response, error) { return llmtest.Text("ок"), nil })
	reply, err := r.Run(context.Background(), Spec{Name: "lead", System: "s"}, Prepared{User: "первый ход"}, nil)
	if err != nil || reply.Text != "ок" || len(reply.Added) != 2 {
		t.Fatalf("первый ход нового диалога: %+v, %v", reply, err)
	}
}

func TestRunnerEnvelopesUntrustedOnly(t *testing.T) {
	r, fake := runner(func(req llm.Request) (llm.Response, error) {
		if llmtest.ToolReplies(req) == 0 {
			return llmtest.ToolCalls(llmtest.Call{Name: "src", Args: `{}`}, llmtest.Call{Name: "own", Args: `{}`}), nil
		}
		return llmtest.Text("готово"), nil
	})
	spec := Spec{Name: "a", System: "s", Tools: []tools.Tool{echo("src", true, `{"text":"статья"}`), echo("own", false, `{"ok":true}`)}}
	reply, err := r.Run(context.Background(), spec, Prepared{User: "u", Features: allOn()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	src, own := reply.Added[2].Content, reply.Added[3].Content
	if !strings.Contains(src, tools.TrustNote) {
		t.Fatalf("ответ источника без пометки: %s", src)
	}
	if data, ok := tools.Unwrap(src); !ok || data != `{"text":"статья"}` {
		t.Fatalf("под пометкой не те данные: %s", data)
	}
	if own != `{"ok":true}` {
		t.Fatalf("свой инструмент обёрнут: %s", own)
	}
	_ = fake

	// Механизм выключен — ни токена пометки.
	r2, _ := runner(func(req llm.Request) (llm.Response, error) {
		if llmtest.ToolReplies(req) == 0 {
			return llmtest.ToolCall("src", `{}`), nil
		}
		return llmtest.Text("готово"), nil
	})
	off := allOn().With(features.Envelope, false)
	reply, _ = r2.Run(context.Background(), spec, Prepared{User: "u", Features: off}, nil)
	if reply.Added[2].Content != `{"text":"статья"}` {
		t.Fatalf("пометка при выключенном механизме: %s", reply.Added[2].Content)
	}
}

func TestRunnerScansForInjection(t *testing.T) {
	r, _ := runner(func(req llm.Request) (llm.Response, error) {
		if llmtest.ToolReplies(req) == 0 {
			return llmtest.ToolCall("src", `{}`), nil
		}
		return llmtest.Text("Лесной кот питается грызунами."), nil
	})
	payload, _ := json.Marshal(map[string]string{"text": toolstest.InjectedExtract})
	spec := Spec{Name: "a", System: "s", Tools: []tools.Tool{echo("src", true, string(payload))}}
	rec := &Recorder{}
	if _, err := r.Run(context.Background(), spec, Prepared{User: "u", Features: allOn()}, rec); err != nil {
		t.Fatal(err)
	}
	var found *Event
	for i := range rec.Events {
		if rec.Events[i].Kind == EventInjection {
			found = &rec.Events[i]
		}
	}
	if found == nil || len(found.Hits) == 0 || found.Mechanism != string(features.Scan) || found.CallID == "" {
		t.Fatalf("попытка управлять агентом не отмечена: %+v", found)
	}

	rec = &Recorder{}
	r.Run(context.Background(), spec, Prepared{User: "u", Features: allOn().With(features.Scan, false)}, rec)
	for _, e := range rec.Events {
		if e.Kind == EventInjection {
			t.Fatal("поиск при выключенном механизме")
		}
	}
}

func TestRunnerFinisherRejectsThenAccepts(t *testing.T) {
	calls := 0
	r, _ := runner(func(req llm.Request) (llm.Response, error) {
		calls++
		switch calls {
		case 1:
			return llmtest.ToolCall("submit", `{"latin":"Lynx striatus"}`), nil
		default:
			return llmtest.ToolCall("submit", `{"latin":"Lynx lynx"}`), nil
		}
	})
	var gotCall string
	fin := Finisher{Name: "submit", Description: "сдать", Parameters: json.RawMessage(`{"type":"object"}`),
		Handle: func(_ context.Context, callID string, args json.RawMessage) (any, error) {
			var in struct{ Latin string }
			json.Unmarshal(args, &in)
			if in.Latin != "Lynx lynx" {
				return nil, fmt.Errorf("латынь «%s» не подтверждена: вызови match_taxon", in.Latin)
			}
			gotCall = callID
			return in.Latin, nil
		}}
	rec := &Recorder{}
	reply, err := r.Run(context.Background(), Spec{Name: "id", System: "s", Finish: []Finisher{fin}}, Prepared{User: "рысь"}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Final != "Lynx lynx" || reply.FinalTool != "submit" || reply.FinalCallID != gotCall || reply.Stats.Rejected != 1 {
		t.Fatalf("итог: %+v", reply)
	}
	// Отказ ушёл модели словами.
	if !strings.Contains(reply.Added[2].Content, "match_taxon") {
		t.Fatalf("отказ не объяснён модели: %s", reply.Added[2].Content)
	}
	rejected := false
	for _, e := range rec.Events {
		if e.Rejected && e.Final {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("отказ не записан в журнал")
	}
}

func TestRunnerRemindsThenFailsProtocol(t *testing.T) {
	r, fake := runner(func(llm.Request) (llm.Response, error) {
		return llmtest.Text("вот карточка текстом"), nil
	})
	fin := Finisher{Name: "submit", Handle: func(context.Context, string, json.RawMessage) (any, error) { return nil, nil }}
	_, err := r.Run(context.Background(), Spec{Name: "id", System: "s", Finish: []Finisher{fin}}, Prepared{User: "u"}, nil)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("ждали ошибку протокола, получили %v", err)
	}
	if fake.Calls() != MaxReminders+1 {
		t.Fatalf("запросов %d, ждали %d", fake.Calls(), MaxReminders+1)
	}
	last := fake.Requests[len(fake.Requests)-1]
	if !strings.Contains(last.Messages[len(last.Messages)-1].Content, "submit") {
		t.Fatal("напоминание не называет завершающий инструмент")
	}
}

func TestRunnerAllowTextWithFinishers(t *testing.T) {
	r, _ := runner(func(llm.Request) (llm.Response, error) { return llmtest.Text("просто текст"), nil })
	fin := Finisher{Name: "submit", Handle: func(context.Context, string, json.RawMessage) (any, error) { return nil, nil }}
	reply, err := r.Run(context.Background(), Spec{Name: "a", System: "s", Finish: []Finisher{fin}, AllowText: true}, Prepared{User: "u"}, nil)
	if err != nil || reply.Text != "просто текст" || reply.Final != nil {
		t.Fatalf("%+v %v", reply, err)
	}
}

func TestRunnerSecondFinisherInSameReplyIsRefused(t *testing.T) {
	r, _ := runner(func(llm.Request) (llm.Response, error) {
		return llmtest.ToolCalls(llmtest.Call{Name: "submit", Args: `{"n":1}`}, llmtest.Call{Name: "submit", Args: `{"n":2}`}), nil
	})
	n := 0
	fin := Finisher{Name: "submit", Handle: func(_ context.Context, _ string, args json.RawMessage) (any, error) {
		n++
		return string(args), nil
	}}
	reply, err := r.Run(context.Background(), Spec{Name: "a", System: "s", Finish: []Finisher{fin}}, Prepared{User: "u"}, nil)
	if err != nil || reply.Final != `{"n":1}` || n != 1 {
		t.Fatalf("%+v %v (вызовов %d)", reply, err, n)
	}
	// На каждый вызов модели есть ответ инструмента — иначе API отвергнет
	// следующий запрос с этой историей.
	if len(reply.Added) != 4 || reply.Added[3].Role != llm.RoleTool {
		t.Fatalf("ответы на вызовы: %+v", reply.Added)
	}
}

func TestRunnerStepLimitAndToolErrors(t *testing.T) {
	r, _ := runner(func(llm.Request) (llm.Response, error) {
		return llmtest.ToolCalls(llmtest.Call{Name: "broken", Args: `{}`}, llmtest.Call{Name: "ghost", Args: `{}`}), nil
	})
	rec := &Recorder{}
	reply, err := r.Run(context.Background(), Spec{Name: "a", System: "s", Tools: []tools.Tool{failing("broken")}, MaxSteps: 2}, Prepared{User: "u"}, rec)
	if !errors.Is(err, ErrStepLimit) || reply.Stats.Steps != 2 {
		t.Fatalf("лимит шагов: %v, %+v", err, reply.Stats)
	}
	errs := 0
	for _, e := range rec.Events {
		if e.Kind == EventToolError {
			errs++
		}
	}
	if errs != 4 {
		t.Fatalf("ошибок инструментов в журнале %d, ждали 4", errs)
	}
}

func TestRunnerToolErrorGoesToModelAsJSON(t *testing.T) {
	r, fake := runner(func(req llm.Request) (llm.Response, error) {
		if llmtest.ToolReplies(req) == 0 {
			return llmtest.ToolCall("broken", `{}`), nil
		}
		return llmtest.Text("источник недоступен, сведений нет"), nil
	})
	reply, err := r.Run(context.Background(), Spec{Name: "a", System: "s", Tools: []tools.Tool{failing("broken")}}, Prepared{User: "u"}, nil)
	if err != nil || !strings.Contains(reply.Text, "сведений нет") {
		t.Fatalf("ошибка источника уронила ход: %v", err)
	}
	if got := llmtest.LastToolReply(fake.Requests[1]); got != `{"error":"источник недоступен"}` {
		t.Fatalf("модель получила: %s", got)
	}
}

func TestRunnerLLMError(t *testing.T) {
	r, _ := runner(func(llm.Request) (llm.Response, error) { return llm.Response{}, errors.New("сеть") })
	if _, err := r.Run(context.Background(), Spec{Name: "a", System: "s"}, Prepared{User: "u"}, nil); err == nil || !strings.Contains(err.Error(), "сеть") {
		t.Fatalf("ошибка модели потерялась: %v", err)
	}
}

func TestRunnerCountsTokensAndCalibrates(t *testing.T) {
	cal := &tokens.Calibration{}
	r, _ := runner(func(llm.Request) (llm.Response, error) {
		resp := llmtest.Text("ответ")
		resp.Usage = llm.Usage{Prompt: 40, Completion: 5, Total: 45, CacheHit: 30, CacheMiss: 10}
		return resp, nil
	})
	r.Calibration = cal
	rec := &Recorder{}
	reply, err := r.Run(context.Background(), Spec{Name: "a", System: "системный промпт"}, Prepared{User: "вопрос"}, rec)
	if err != nil {
		t.Fatal(err)
	}
	c := reply.Stats.Context
	if c.Estimate.Total == 0 || c.Peak == 0 || c.FirstPrompt != 40 || c.CacheHit != 30 {
		t.Fatalf("контекст: %+v", c)
	}
	if cal.Pairs() != 1 {
		t.Fatalf("калибровка не получила пару: %d", cal.Pairs())
	}
	if !reply.Stats.Cost.Known {
		t.Fatalf("цена не посчитана: %+v", reply.Stats.Cost)
	}
	for _, e := range rec.Events {
		if e.Kind == EventLLMReply && (e.Tokens == nil || e.Tokens.Actual != 40) {
			t.Fatalf("факт токенов не записан в журнал: %+v", e.Tokens)
		}
	}
}

func longHistory(n int) []llm.Message {
	var h []llm.Message
	for i := range n {
		h = append(h,
			llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("вопрос %d %s", i, strings.Repeat("слово ", 50))},
			llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("ответ ", 50)})
	}
	return h
}

func TestRunnerOverflowFails(t *testing.T) {
	r, fake := runner(func(llm.Request) (llm.Response, error) { return llmtest.Text("ok"), nil })
	r.ContextLimit, r.OnOverflow = 100, OverflowFail
	_, err := r.Run(context.Background(), Spec{Name: "a", System: "s"}, Prepared{History: longHistory(5), User: "u"}, nil)
	if !errors.Is(err, ErrContextOverflow) || fake.Calls() != 0 {
		t.Fatalf("переполнение не поймано до отправки: %v, запросов %d", err, fake.Calls())
	}
}

func TestRunnerOverflowTrims(t *testing.T) {
	r, fake := runner(func(llm.Request) (llm.Response, error) { return llmtest.Text("ok"), nil })
	full := tokens.Of(tokens.Parts{System: "s", History: longHistory(6), User: "u"}).Total
	r.ContextLimit, r.OnOverflow = full/2, OverflowTrim
	reply, err := r.Run(context.Background(), Spec{Name: "a", System: "s"}, Prepared{History: longHistory(6), User: "u"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Stats.Context.Trimmed == 0 || reply.Stats.Context.Trimmed%2 != 0 {
		t.Fatalf("выброшено %d сообщений — ходы должны уходить целиком", reply.Stats.Context.Trimmed)
	}
	if fake.Requests[0].Messages[1].Role != llm.RoleUser {
		t.Fatal("после обрезки история начинается не с реплики пользователя")
	}
	// Даже без истории не влезает — ошибка.
	r.ContextLimit = 3
	if _, err := r.Run(context.Background(), Spec{Name: "a", System: "s"}, Prepared{History: longHistory(2), User: "u"}, nil); !errors.Is(err, ErrContextOverflow) {
		t.Fatalf("ждали переполнение: %v", err)
	}
}

func TestRunnerOverflowOffLetsAPIAnswer(t *testing.T) {
	r, fake := runner(func(llm.Request) (llm.Response, error) { return llmtest.Text("ok"), nil })
	r.ContextLimit, r.OnOverflow = 10, OverflowOff
	if _, err := r.Run(context.Background(), Spec{Name: "a", System: "s"}, Prepared{History: longHistory(3), User: "u"}, nil); err != nil || fake.Calls() != 1 {
		t.Fatalf("off: %v, запросов %d", err, fake.Calls())
	}
}

func TestRunnerOverflowInsideTurn(t *testing.T) {
	r, _ := runner(func(req llm.Request) (llm.Response, error) {
		return llmtest.ToolCall("big", `{}`), nil
	})
	r.ContextLimit, r.OnOverflow = 400, OverflowFail
	spec := Spec{Name: "a", System: "s", Tools: []tools.Tool{echo("big", false, strings.Repeat("я", 2000))}}
	_, err := r.Run(context.Background(), spec, Prepared{User: "u"}, nil)
	if !errors.Is(err, ErrContextOverflow) || !strings.Contains(err.Error(), "шаг 2") {
		t.Fatalf("рост контекста внутри хода не пойман: %v", err)
	}
}

func TestHelpers(t *testing.T) {
	if prettyJSON(`{"a":1}`) != "{\n  \"a\": 1\n}" || prettyJSON("не json") != "не json" {
		t.Fatal("prettyJSON")
	}
	if sizeLabel("абв") != "3 символов" || sizeLabel(strings.Repeat("я", 1500)) != "1.5 тыс. символов" {
		t.Fatal("sizeLabel")
	}
	if lastMessageDetail(nil) != "" || lastMessageDetail([]llm.Message{{Role: llm.RoleTool, Content: "x"}}) != "" {
		t.Fatal("lastMessageDetail")
	}
	if replyDetail(llm.Response{}) != "" {
		t.Fatal("replyDetail")
	}
	if viaNote(tools.ViaMCP) == "" || viaNote(tools.ViaLocal) != "" {
		t.Fatal("viaNote")
	}
}

func TestStatsAdd(t *testing.T) {
	a := Stats{Steps: 1, ToolCalls: 2, Context: Context{Estimate: tokens.Estimate{Total: 100}, Peak: 120}}
	b := Stats{Steps: 3, ToolCalls: 1, Rejected: 1, Context: Context{Estimate: tokens.Estimate{Total: 50}, Peak: 500}}
	s := a.Add(b)
	if s.Steps != 4 || s.ToolCalls != 3 || s.Rejected != 1 || s.Context.Estimate.Total != 100 || s.Context.Peak != 500 {
		t.Fatalf("%+v", s)
	}
	// Тяжёлый прогон после лёгкого: контекст хода — его, пик — наибольший.
	heavy := Stats{Context: Context{Estimate: tokens.Estimate{Total: 900}, Peak: 950}}
	s = Stats{Context: Context{Estimate: tokens.Estimate{Total: 300}, Peak: 2000}}.Add(heavy)
	if s.Context.Estimate.Total != 900 || s.Context.Peak != 2000 {
		t.Fatalf("контекст хода от тяжёлого прогона: %+v", s.Context)
	}
}

func TestEmitters(t *testing.T) {
	a, b := &Recorder{}, &Recorder{}
	tee := Tee{a, &Safe{E: b}}
	tee.Log(Event{Kind: "x"})
	tee.Publish(Update{Kind: "card"})
	if len(a.Events) != 1 || len(b.Events) != 1 || len(a.Updates) != 1 || len(b.Updates) != 1 {
		t.Fatal("Tee/Safe")
	}
	Nop{}.Log(Event{})
	Nop{}.Publish(Update{})
}

// Вызов кодом до первого запроса: модель получает его как свой первый шаг
// (вызов и ответ с пометкой источника), запросом к модели он не считается,
// а инструмента, которого у агента нет, код не зовёт.
func TestRunnerPreloadSavesRequest(t *testing.T) {
	r, fake := runner(func(req llm.Request) (llm.Response, error) {
		return llmtest.Text("готово по " + llmtest.LastToolReply(req)), nil
	})
	rec := &Recorder{}
	reply, err := r.Run(context.Background(),
		Spec{Name: "section.diet", System: "система", Tools: []tools.Tool{echo("read_wikipedia", true, `{"text":"ест зайцев"}`)}},
		Prepared{User: "тема: питание", Features: allOn(), Preload: []Preload{
			{Tool: "read_wikipedia", Args: `{"title":"Рысь","section":"Питание"}`},
			{Tool: "match_taxon", Args: `{}`},
		}}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if fake.Calls() != 1 || reply.Stats.Steps != 1 || reply.Stats.ToolCalls != 1 {
		t.Fatalf("запросов %d, шагов %d, вызовов %d", fake.Calls(), reply.Stats.Steps, reply.Stats.ToolCalls)
	}
	req := fake.Requests[0]
	roles := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		roles[i] = m.Role
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool" {
		t.Fatalf("порядок сообщений: %v", roles)
	}
	call := req.Messages[2].ToolCalls
	if len(call) != 1 || call[0].Function.Name != "read_wikipedia" || !strings.HasPrefix(call[0].ID, "pre_read_wikipedia_") {
		t.Fatalf("вызов кодом: %+v", call)
	}
	if _, wrapped := tools.Unwrap(req.Messages[3].Content); !wrapped || req.Messages[3].ToolCallID != call[0].ID {
		t.Fatalf("ответ без пометки источника или не к тому вызову: %+v", req.Messages[3])
	}
	kinds := strings.Join(rec.Kinds(), ",")
	if !strings.HasPrefix(kinds, "prompt,tool.call,tool.result,note,llm.request,llm.response") {
		t.Fatalf("журнал: %s", kinds)
	}
}

func TestEmitterInContext(t *testing.T) {
	if _, ok := EmitterFrom(context.Background()).(Nop); !ok {
		t.Fatal("без журнала в контексте — не Nop")
	}
	rec := &Recorder{}
	EmitterFrom(WithEmitter(context.Background(), rec)).Log(Event{Kind: EventNote})
	if len(rec.Events) != 1 {
		t.Fatal("журнал из контекста не тот")
	}
	if _, ok := EmitterFrom(WithEmitter(context.Background(), nil)).(Nop); !ok {
		t.Fatal("nil в контексте — не Nop")
	}
}
