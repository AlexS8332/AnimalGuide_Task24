package tokens

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
)

func TestTextByCharClass(t *testing.T) {
	lat := Default.Text(strings.Repeat("a", 100))
	cyr := Default.Text(strings.Repeat("я", 100))
	if !(lat < cyr) {
		t.Errorf("латиница %.1f должна быть дешевле кириллицы %.1f", lat, cyr)
	}
	if got := Default.Text(""); got != 0 {
		t.Errorf("пустая строка: %.1f", got)
	}
	if Default.Text("中") <= Default.Text("a") {
		t.Errorf("иероглиф должен стоить дороже латинской буквы")
	}
	if Default.Text(" ") >= Default.Text("a") {
		t.Errorf("пробел должен стоить меньше буквы")
	}
	if Default.Text("7") != Default.Digit || Default.Text("—") != Default.Other {
		t.Errorf("цифры и знаки считаются не по своим весам")
	}
}

func TestMessageCountsToolCalls(t *testing.T) {
	plain := llm.Message{Role: llm.RoleAssistant, Content: "Рысь — хищник"}
	withCall := plain
	withCall.ToolCalls = []llm.ToolCall{{
		ID: "call_1", Type: "function",
		Function: llm.FunctionCall{Name: "read_wikipedia", Arguments: `{"title":"Рысь","section":"Питание"}`},
	}}
	if Default.Message(withCall) <= Default.Message(plain) {
		t.Errorf("аргументы вызова инструмента должны считаться")
	}
	if got := Default.Message(llm.Message{Role: llm.RoleUser}); got != Default.PerMessage {
		t.Errorf("обвязка пустого сообщения: %.1f, ждали %.1f", got, Default.PerMessage)
	}
}

func TestOfSplitsRequestByBlocks(t *testing.T) {
	defs := []llm.ToolDef{llm.NewToolDef("read_wikipedia", "читает раздел статьи", json.RawMessage(`{"type":"object"}`))}
	history := []llm.Message{
		{Role: llm.RoleUser, Content: "Расскажи про рысь"},
		{Role: llm.RoleAssistant, Content: strings.Repeat("Рысь — хищник семейства кошачьих. ", 20)},
	}
	blocks := []features.Block{
		{Feature: features.Charter, Text: "Свод: факты только из источников."},
		{Feature: features.Profile, Text: "Профиль: коротко, на «ты»."},
		{Feature: features.MemoryLong, Text: ""},
	}
	e := Of(Parts{System: "Ты справочник по животным.", Blocks: blocks, Tools: defs, History: history, User: "А чем она питается?"})
	if e.System == 0 || e.Tools == 0 || e.History == 0 || e.User == 0 {
		t.Fatalf("пустая доля в разбивке: %+v", e)
	}
	if e.Block(features.Charter) == 0 || e.Block(features.Profile) == 0 {
		t.Fatalf("блоки не посчитаны: %+v", e.Blocks)
	}
	if _, ok := e.Blocks[features.MemoryLong]; ok {
		t.Fatalf("пустой блок попал в разбивку: %+v", e.Blocks)
	}
	sum := e.System + e.Tools + e.History + e.User + e.Block(features.Charter) + e.Block(features.Profile)
	if e.Total != sum+round(Default.PerRequest) {
		t.Errorf("итог %d не равен сумме частей %d + обвязка", e.Total, sum)
	}
	if e.Constant != e.Total-e.History-e.User {
		t.Errorf("постоянная часть %d посчитана не так", e.Constant)
	}
}

// Выключенный механизм не добавляет ни токена: блока нет — нет и доли.
func TestDisabledBlockCostsNothing(t *testing.T) {
	base := Parts{System: "Системный промпт.", User: "Вопрос?"}
	bare := Of(base)
	base.Blocks = []features.Block{{Feature: features.Profile, Text: "профиль"}}
	with := Of(base)
	if with.Total-bare.Total != with.Block(features.Profile) {
		t.Errorf("разница %d не равна весу блока %d", with.Total-bare.Total, with.Block(features.Profile))
	}
	if len(bare.Blocks) != 0 {
		t.Errorf("блоки у запроса без блоков: %v", bare.Blocks)
	}
}

func TestOfMessagesGrowsWithHistory(t *testing.T) {
	defs := []llm.ToolDef{llm.NewToolDef("t", "описание", nil)}
	var ms []llm.Message
	prev := OfMessages(defs, ms)
	for i := range 5 {
		ms = append(ms, llm.Message{Role: llm.RoleUser, Content: "ещё один ход диалога"})
		got := OfMessages(defs, ms)
		if got <= prev {
			t.Fatalf("после %d-го сообщения оценка не выросла: %d → %d", i+1, prev, got)
		}
		prev = got
	}
}

func TestCompare(t *testing.T) {
	if got := Compare(110, 100); math.Abs(got.ErrorPct-10) > 0.001 {
		t.Errorf("ошибка оценки: %.3f, ждали 10", got.ErrorPct)
	}
	if got := Compare(90, 100); math.Abs(got.ErrorPct+10) > 0.001 {
		t.Errorf("ошибка оценки: %.3f, ждали -10", got.ErrorPct)
	}
	if got := Compare(50, 0); got.ErrorPct != 0 || got.Actual != 0 {
		t.Errorf("без факта ошибки быть не должно: %+v", got)
	}
}

func TestCalibration(t *testing.T) {
	var c Calibration
	if c.Factor() != 1 || c.Apply(100) != 100 || c.ErrorPct() != 0 {
		t.Fatal("пустая калибровка должна быть единичной")
	}
	c.Observe(110, 100)
	c.Observe(220, 200)
	c.Observe(50, 0) // без факта — не учитывается
	if c.Pairs() != 2 {
		t.Fatalf("пар %d", c.Pairs())
	}
	if math.Abs(c.ErrorPct()-10) > 0.001 {
		t.Fatalf("ошибка без поправки: %.3f", c.ErrorPct())
	}
	if got := c.Apply(330); got != 300 {
		t.Fatalf("с поправкой: %d, ждали 300", got)
	}
	var wild Calibration
	wild.Observe(10, 1000)
	if wild.Factor() != 1.3 {
		t.Fatalf("множитель не ограничен сверху: %.2f", wild.Factor())
	}
	wild = Calibration{}
	wild.Observe(1000, 10)
	if wild.Factor() != 0.7 {
		t.Fatalf("множитель не ограничен снизу: %.2f", wild.Factor())
	}
	var nilCal *Calibration
	if nilCal.Apply(42) != 42 {
		t.Fatal("nil-калибровка должна отдавать оценку как есть")
	}
}

func TestCalibrationIsConcurrent(t *testing.T) {
	var c Calibration
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Observe(100, 100)
			_ = c.Factor()
		}()
	}
	wg.Wait()
	if c.Pairs() != 50 {
		t.Fatalf("пар %d", c.Pairs())
	}
}
