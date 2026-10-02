package facts

import (
	"strings"
	"testing"
)

func TestSetReplacesByKeyAndKeepsOrder(t *testing.T) {
	var s State
	if !s.Set("имя", "Алекс", 1) || !s.Set("животное", "рысь", 2) {
		t.Fatal("новые факты не записаны")
	}
	if s.Set("Имя", "Алекс", 3) {
		t.Fatal("то же значение считается правкой")
	}
	if !s.Set(" ИМЯ ", "Саша", 4) {
		t.Fatal("новое значение не записано")
	}
	if v, _ := s.Get("имя"); v != "Саша" || len(s.Entries) != 2 || s.Entries[0].Since != 1 || s.Entries[0].Turn != 4 {
		t.Fatalf("карточка: %+v", s)
	}
	if s.Version != 3 {
		t.Fatalf("версия %d", s.Version)
	}
	if s.Set("", "x", 1) || s.Set("k", " ", 1) {
		t.Fatal("пустое записано")
	}
}

func TestDeleteAndHas(t *testing.T) {
	var s State
	s.Set("a", "1", 1)
	if !s.Has("A") || !s.Delete(" a ") || s.Has("a") || s.Delete("a") {
		t.Fatal("Delete/Has")
	}
}

func TestTrimDropsOldest(t *testing.T) {
	var s State
	for i := range DefaultMaxFacts + 3 {
		s.Set(strings.Repeat("k", i+1), "v", i+1)
	}
	if len(s.Entries) != DefaultMaxFacts {
		t.Fatalf("записей %d", len(s.Entries))
	}
	if s.Has("k") {
		t.Fatal("самый старый факт остался")
	}
}

func TestCloneIsDeep(t *testing.T) {
	var s State
	s.Set("a", "1", 1)
	c := s.Clone()
	c.Entries[0].Value = "2"
	if v, _ := s.Get("a"); v != "1" {
		t.Fatal("клон делит записи с оригиналом")
	}
}

func TestPromptAndRender(t *testing.T) {
	var s State
	if s.Prompt() != "" || !s.Empty() {
		t.Fatal("пустая карточка даёт блок")
	}
	s.Set("имя", "Алекс", 1)
	s.Set("длинное", strings.Repeat("я", DefaultMaxValueRunes+10), 1)
	p := s.Prompt()
	if !strings.Contains(p, "Карточка фактов") || !strings.Contains(p, "- имя: Алекс") || !strings.Contains(p, "…") {
		t.Fatalf("блок: %s", p)
	}
	if s.Runes() == 0 {
		t.Fatal("Runes")
	}
}
