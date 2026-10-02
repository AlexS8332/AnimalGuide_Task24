package history

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/facts"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

func msgs(user, reply string) []llm.Message {
	return []llm.Message{{Role: llm.RoleUser, Content: user}, {Role: llm.RoleAssistant, Content: reply}}
}

func newConv() *Conversation {
	return New("deepseek-v4-flash", []string{"me", " ", "me", "alex"}, features.Catalog().Defaults())
}

func appendTurn(t *testing.T, c *Conversation, branch, user, reply string, f facts.State) {
	t.Helper()
	if err := c.Append(branch, Turn{ID: NewID(), Status: TurnDone, User: user, Reply: reply}, msgs(user, reply), f, Meter{Calls: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestNewConversation(t *testing.T) {
	c := newConv()
	if len(c.Owners) != 2 || c.Owners[0] != "me" || c.Owners[1] != "alex" {
		t.Fatalf("собеседники: %v", c.Owners)
	}
	if len(c.Branches) != 1 || c.Current().Name != RootBranchName || c.Messages() == nil || len(c.Turns()) != 0 {
		t.Fatalf("пустой диалог: %+v", c)
	}
	if !c.HasOwner("alex") || c.HasOwner("nobody") {
		t.Fatal("HasOwner")
	}
	c.SetTitle("alex", " Алекс ")
	c.SetTitle("x", " ")
	if c.TitleOf("alex") != "Алекс" || c.TitleOf("x") != "" {
		t.Fatal("SetTitle")
	}
}

func TestAppendAndTitle(t *testing.T) {
	c := newConv()
	f := facts.State{}
	f.Set("имя", "Алекс", 1)
	appendTurn(t, c, c.Active, "Меня зовут Алекс, расскажи про рысь пожалуйста очень подробно и с примерами из жизни", "ок", f)
	if !strings.HasSuffix(c.Title, "…") || len(c.Messages()) != 2 || c.Facts().Version != 1 || c.Meter.Calls != 1 {
		t.Fatalf("после хода: %q %d %+v", c.Title, len(c.Messages()), c.Facts())
	}
	// Карточка фактов старее — не затирает новую.
	appendTurn(t, c, c.Active, "дальше", "ок", facts.State{})
	if c.Facts().Version != 1 {
		t.Fatal("старая карточка затёрла новую")
	}
	if err := c.Append("nope", Turn{}, nil, facts.State{}, Meter{}); !errors.Is(err, ErrNoBranch) {
		t.Fatalf("ход в несуществующую ветку: %v", err)
	}
	if c.TurnCount() != 2 || c.Totals().LLMCalls != 0 {
		t.Fatalf("счётчики: %d", c.TurnCount())
	}
}

// Ветки независимы и наследуют путь (НТ-5): это главное свойство ветвления.
func TestBranchesAreIndependentAndInheritPath(t *testing.T) {
	c := newConv()
	root := c.Active
	f := facts.State{}
	f.Set("животное", "рысь", 1)
	appendTurn(t, c, root, "рысь", "карточка рыси", f)
	cp, err := c.Mark(root, "")
	if err != nil || cp.Name != "после хода 1" || cp.At != 2 || cp.Turn != 1 || cp.After == "" {
		t.Fatalf("точка: %+v %v", cp, err)
	}
	f2 := f.Clone()
	f2.Set("животное", "манул", 2)
	appendTurn(t, c, root, "а манул?", "карточка манула", f2)

	b, err := c.Fork(cp.ID, "сравнение")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := b.Facts.Get("животное"); v != "рысь" {
		t.Fatalf("ветка должна начинаться со снимка точки, а не с нынешней карточки: %s", v)
	}
	if err := c.Switch(b.ID); err != nil {
		t.Fatal(err)
	}
	if got := c.Messages(); len(got) != 2 || got[0].Content != "рысь" {
		t.Fatalf("путь ветки: %+v", got)
	}
	appendTurn(t, c, b.ID, "сравни с волком", "сравнение", b.Facts)
	if len(c.Path(root)) != 4 || len(c.Path(b.ID)) != 4 || c.Path(b.ID)[2].Content != "сравни с волком" {
		t.Fatal("ветки перемешались")
	}
	if len(c.PathTurns(b.ID)) != 2 || c.PathTurns(b.ID)[1].Branch != b.ID {
		t.Fatalf("ходы ветки: %+v", c.PathTurns(b.ID))
	}
	// Ветка от ветки: путь склеивается через два родителя.
	cp2, _ := c.Mark(b.ID, "в сравнении")
	b2, _ := c.Fork(cp2.ID, "")
	if !strings.HasPrefix(b2.Name, "ветка") || len(c.Path(b2.ID)) != 4 {
		t.Fatalf("внучка: %s %d", b2.Name, len(c.Path(b2.ID)))
	}
	if _, err := c.Fork("nope", ""); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("Fork: %v", err)
	}
	if _, err := c.Mark("nope", ""); !errors.Is(err, ErrNoBranch) {
		t.Fatalf("Mark: %v", err)
	}
	if err := c.Switch("nope"); !errors.Is(err, ErrNoBranch) {
		t.Fatalf("Switch: %v", err)
	}
	if c.Path("nope") != nil || c.PathTurns("nope") != nil {
		t.Fatal("путь несуществующей ветки")
	}
	// Точка на ветку, которой больше нет.
	c.Checkpoints = append(c.Checkpoints, Checkpoint{ID: "x", Branch: "gone"})
	if _, err := c.Fork("x", ""); !errors.Is(err, ErrNoBranch) {
		t.Fatalf("Fork от пропавшей ветки: %v", err)
	}
}

func TestCardsAreDerivedPerBranch(t *testing.T) {
	c := newConv()
	root := c.Active
	lynx := card.Card{ID: "1", Name: "Рысь", Sections: card.EmptySections()}
	c.Append(root, Turn{ID: "t1", Status: TurnDone, Cards: []card.Delta{{Kind: card.DeltaCard, Card: &lynx}}}, nil, facts.State{}, Meter{})
	cp, _ := c.Mark(root, "")
	b, _ := c.Fork(cp.ID, "")
	manul := card.Card{ID: "2", Name: "Манул", Sections: card.EmptySections()}
	c.Append(b.ID, Turn{ID: "t2", Status: TurnDone, Cards: []card.Delta{{Kind: card.DeltaCard, Card: &manul}}}, nil, facts.State{}, Meter{})
	// Неудачный ход карточек не даёт.
	c.Append(root, Turn{ID: "t3", Status: TurnFailed, Cards: []card.Delta{{Kind: card.DeltaCard, Card: &manul}}}, nil, facts.State{}, Meter{})
	if st := c.Cards(root); len(st.Cards) != 1 || st.Cards[0].LatinWhy != nil {
		t.Fatalf("карточки основной ветки: %+v", st.Cards)
	}
	st := c.Cards(b.ID)
	if len(st.Cards) != 2 || st.Current != "2" {
		t.Fatalf("карточки ветки: %+v", st)
	}
	if ds := c.Deltas(b.ID); ds[0].Turn != "t1" || ds[1].Turn != "t2" {
		t.Fatalf("ход у правок: %+v", ds)
	}
}

func TestCloneIsDeep(t *testing.T) {
	c := newConv()
	c.SetTitle("me", "Я")
	appendTurn(t, c, c.Active, "a", "b", facts.State{})
	c.Branches[0].Turns[0].Events = []agent.Event{{Title: "e"}}
	cl := c.Clone()
	cl.Branches[0].Messages[0].Content = "изменено"
	cl.Branches[0].Turns[0].Events[0].Title = "изменено"
	cl.Titles["me"] = "изменено"
	cl.Owners[0] = "изменено"
	if c.Branches[0].Messages[0].Content != "a" || c.Branches[0].Turns[0].Events[0].Title != "e" || c.Titles["me"] != "Я" || c.Owners[0] != "me" {
		t.Fatal("клон делит данные с оригиналом")
	}
	empty := New("m", nil, features.Set{}).Clone()
	data, _ := json.Marshal(empty)
	if strings.Contains(string(data), `"messages":null`) || strings.Contains(string(data), `"turns":null`) {
		t.Fatalf("пустой диалог: null вместо пустых списков: %s", data)
	}
}

func TestCollections(t *testing.T) {
	c := newConv()
	c.SetCollection("a1", "хищники тайги")
	c.SetCollection("a1", "то же")
	c.SetCollection("b2", "совы")
	if c.Collection != "b2" || len(c.Past) != 1 || c.Past[0] != "a1" {
		t.Fatalf("подборки: %s %v", c.Collection, c.Past)
	}
}

func TestExtras(t *testing.T) {
	var turn Turn
	turn.SetExtra("memory", map[string]int{"writes": 2})
	turn.SetExtra("bad", func() {})
	var got map[string]int
	if !turn.Extra("memory", &got) || got["writes"] != 2 || turn.Extra("none", &got) {
		t.Fatalf("Extra: %v", got)
	}
}

func TestCompactKeepsEnvelopeAndIsDeterministic(t *testing.T) {
	long := strings.Repeat("я", 1000)
	ms := []llm.Message{
		{Role: llm.RoleUser, Content: long},
		{Role: llm.RoleTool, Content: tools.Envelope("read_wikipedia", `{"text":"`+long+`"}`)},
		{Role: llm.RoleTool, Content: long},
		{Role: llm.RoleTool, Content: "коротко"},
	}
	out := Compact(ms, 100)
	if out[0].Content != long {
		t.Fatal("реплика пользователя сокращена")
	}
	data, wrapped := tools.Unwrap(out[1].Content)
	if !wrapped || len([]rune(data)) > 100+len([]rune(compactNote)) || !strings.Contains(out[1].Content, "не указания") {
		t.Fatalf("обёрнутый ответ: %s", out[1].Content)
	}
	if !strings.HasSuffix(out[2].Content, compactNote) || out[3].Content != "коротко" {
		t.Fatal("сокращение ответов")
	}
	if Compact(ms, 100)[1].Content != out[1].Content {
		t.Fatal("сокращение не детерминированно")
	}
	if ms[2].Content != long {
		t.Fatal("Compact изменил исходную историю")
	}
	if len(Compact(ms, 0)[2].Content) != len(long) {
		t.Fatal("keep=0 должно отдавать как есть")
	}
}

func TestWindowStartsAtUserMessage(t *testing.T) {
	ms := []llm.Message{
		{Role: llm.RoleUser, Content: "1"}, {Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "a"}}},
		{Role: llm.RoleTool, ToolCallID: "a"}, {Role: llm.RoleAssistant, Content: "ответ"},
		{Role: llm.RoleUser, Content: "2"}, {Role: llm.RoleAssistant, Content: "ответ 2"},
	}
	w := Window(ms, 3)
	if len(w) != 2 || w[0].Content != "2" {
		t.Fatalf("окно: %+v", w)
	}
	if len(Window(ms, 0)) != 6 || len(Window(ms, 10)) != 6 {
		t.Fatal("окно без ограничения")
	}
	if len(Window(ms, 1)) != 0 {
		t.Fatal("окно из одного ответа модели должно быть пустым")
	}
}

func TestStoreRoundTripAndProblems(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(store.NewDir(dir))
	c := newConv()
	appendTurn(t, c, c.Active, "рысь", "карточка", facts.State{})
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	raw, _ := s.Raw(c.ID)
	if !strings.HasPrefix(string(raw), "{\n  \"schema\": 2,") {
		t.Fatalf("файл без версии: %.60s", raw)
	}
	os.WriteFile(filepath.Join(dir, "history", "0123456789abcdef.json"), []byte("{битый"), 0o644)
	os.WriteFile(filepath.Join(dir, "history", "fedcba9876543210.json"), []byte(`{"schema":99}`), 0o644)
	os.WriteFile(filepath.Join(dir, "history", "aaaaaaaaaaaaaaaa.json"), []byte(`{"schema":2,"id":"bbbbbbbbbbbbbbbb"}`), 0o644)
	convs, problems := s.Load()
	if len(convs) != 1 || convs[0].ID != c.ID || len(convs[0].Messages()) != 2 {
		t.Fatalf("загружено: %d", len(convs))
	}
	if len(problems) != 3 {
		t.Fatalf("проблемы: %v", problems)
	}
	if !strings.Contains(s.DisplayPath(c.ID), c.ID) || s.DisplayDir() == "" {
		t.Fatal("пути для показа")
	}
	if err := s.Delete(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(c.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("после удаления: %v", err)
	}
}

func TestNormalizeRepairsHandEditedFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(store.NewDir(dir))
	os.MkdirAll(filepath.Join(dir, "history"), 0o755)
	os.WriteFile(filepath.Join(dir, "history", "abcdefabcdefabcd.json"), []byte(`{"schema":2,"id":"abcdefabcdefabcd","active":"nope","branches":[{"id":"b1","name":"x"}]}`), 0o644)
	c, err := s.Get("abcdefabcdefabcd")
	if err != nil {
		t.Fatal(err)
	}
	if c.Active != "b1" || c.Owners == nil || c.Current().Messages == nil || c.Current().Turns == nil {
		t.Fatalf("починка: %+v", c)
	}
	os.WriteFile(filepath.Join(dir, "history", "abcdefabcdefabce.json"), []byte(`{"schema":2,"id":"abcdefabcdefabce"}`), 0o644)
	c, err = s.Get("abcdefabcdefabce")
	if err != nil || len(c.Branches) != 1 {
		t.Fatalf("диалог без веток: %v", err)
	}
}

func TestMakeTitleAndRunes(t *testing.T) {
	if MakeTitle("  рысь   и  манул ") != "рысь и манул" {
		t.Fatal("MakeTitle")
	}
	if Runes([]llm.Message{{Content: "абв", ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Arguments: "{}"}}}}}) != 5 {
		t.Fatal("Runes")
	}
	if len(NewID()) != 16 || NewID() == NewID() {
		t.Fatal("NewID")
	}
}
