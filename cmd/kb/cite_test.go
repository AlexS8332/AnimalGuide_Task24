package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAskCite — kb ask -mode rag+cite: ответ, источники, цитаты и проверка
// kb_answer кодом.
func TestAskCite(t *testing.T) {
	f := &fakeDeepSeek{}
	f.serve(t)
	db := ragDB(t)
	code, out, errOut := runKB("ask", "-mode", "rag+cite", "-db", db, "-embedder", "hash", "Сколько часов в день кошачий медведь тратит на еду?")
	if code != exitOK {
		t.Fatalf("ask: %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"== rag+cite ==", "Запрос в поиск:", "рамка якоря — только статьи red-panda", "По базе: ответ.\n\n**Источники:**\n[1] ",
		"**Цитаты:**\n> «", "Проверка kb_answer: answered; источников 1, цитат 1, дословных 1 из 1; отказов проверки 0; принят"} {
		if !strings.Contains(out, want) {
			t.Errorf("ask rag+cite: нет %q в\n%s", want, out)
		}
	}
}

// TestQACite — kb qa -modes rag+both,rag+cite -splits test,out: сводка
// kb_answer в выводе и отчёте.
func TestQACite(t *testing.T) {
	f := &fakeDeepSeek{}
	f.serve(t)
	db := ragDB(t)
	md := filepath.Join(t.TempDir(), "cite.md")
	code, out, errOut := runKB("qa", "-db", db, "-embedder", "hash", "-questions", "../../eval/questions.json", "-modes", "rag+both,rag+cite",
		"-splits", "test,out", "-out", md)
	if code != exitOK {
		t.Fatalf("qa: %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "rag+cite: ответов по существу") || !strings.Contains(out, "дословных цитат 100 %") ||
		!strings.Contains(errOut, "kb qa: rag+cite — порог") {
		t.Fatalf("qa:\n%s\n%s", out, errOut)
	}
	raw, _ := os.ReadFile(md)
	if !strings.Contains(string(raw), "## Источники и цитаты") || !strings.Contains(string(raw), "`rag+cite`") {
		t.Fatal("отчёт без раздела источников и цитат")
	}
}
