package rag

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
)

// Проба: модель без базы знает T01 всегда, T02 — в одном повторе из трёх;
// остальное не знает. Судья судит правилом.
func TestProbe(t *testing.T) {
	qs := questions(t)
	calls := map[string]int{}
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if isJudge(req) {
			m := judgeQ.FindStringSubmatch(lastUser(req))
			for _, q := range qs.Questions {
				if q.Q == m[1] {
					return verdictJSON(Rule(q, judgedText(req)).Verdict, "по эталону"), nil
				}
			}
			return llmtest.Text("?"), nil
		}
		if len(req.Tools) > 0 || req.Messages[0].Content != System(NoRAG) {
			t.Error("проба — только без базы")
		}
		q, _ := questionOf(qs, req)
		calls[q.ID]++
		switch {
		case q.ID == "T01", q.ID == "T02" && calls[q.ID] == 2:
			return llmtest.Text(q.Expect.Note), nil
		}
		return llmtest.Text("Не знаю."), nil
	}}
	rows, err := Probe(context.Background(), &Answerer{LLM: fake}, qs, &Judge{LLM: fake}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Отвечаемые test (8) и dev (19), по 3 повтора и по оценке судьи.
	if len(rows) != 27 || fake.Calls() != 27*3*2 || rows[0].ID != "T01" || rows[8].ID != "D01" {
		t.Fatalf("строк %d, запросов %d", len(rows), fake.Calls())
	}
	if rows[0].Discriminative || !rows[1].Discriminative || len(rows[0].Verdicts) != 3 || rows[0].Verdicts[0] != Correct ||
		rows[1].Verdicts[1] != Correct || rows[2].Verdicts[0] != Abstain || len(rows[0].Answers) != 3 || !rows[0].Cost.Known {
		t.Fatalf("строки: %+v %+v", rows[0], rows[1])
	}
	if rows[7].ID != "T08" || !strings.Contains(rows[7].Q, "харза") {
		t.Fatalf("вопрос-продолжение с контекстом: %+v", rows[7])
	}
	md := ProbeMarkdown(rows, "m", 3, true, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	for _, want := range []string{"# Проба дискриминативности", "недискриминативных 1 (T01)", "| T01 |", "correct, correct, correct | **нет** |",
		"| T02 |", "| да |", "<details><summary>T01", "судья-модель"} {
		if !strings.Contains(md, want) {
			t.Errorf("probe.md без %q", want)
		}
	}
	// Без судьи — правило.
	fake2 := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) { return llmtest.Text("Не знаю."), nil }}
	rows, err = Probe(context.Background(), &Answerer{LLM: fake2}, qs, nil, 1)
	if err != nil || len(rows) != 27 || fake2.Calls() != 27 || !rows[0].Discriminative {
		t.Fatalf("без судьи: %v %d", err, fake2.Calls())
	}
	// Ошибка модели обрывает пробу с id вопроса.
	fake2.Fn = func(llm.Request) (llm.Response, error) { return llm.Response{}, context.DeadlineExceeded }
	if _, err := Probe(context.Background(), &Answerer{LLM: fake2}, qs, nil, 1); err == nil || !strings.HasPrefix(err.Error(), "T01:") {
		t.Fatalf("ошибка: %v", err)
	}
}

// Правка eval/questions.json: только поля discriminative, формат файла
// (массивы в строку, порядок полей) сохраняется. Вход — снимок набора до первой
// пробы (testdata/questions-v1.json): в живом файле поля уже проставлены.
func TestSetDiscriminative(t *testing.T) {
	raw, err := os.ReadFile("testdata/questions-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]bool{"T01": false, "T02": true, "T09": true, "D18": true}
	out, err := SetDiscriminative(raw, vals)
	if err != nil {
		t.Fatal(err)
	}
	before, after := strings.Split(string(raw), "\n"), strings.Split(string(out), "\n")
	if len(after) != len(before)+4 {
		t.Fatalf("строк было %d, стало %d", len(before), len(after))
	}
	added := diffAdded(before, after)
	if len(added) != 4 {
		t.Fatalf("добавлено строк: %q", added)
	}
	for _, l := range added {
		if !strings.HasPrefix(l, `      "discriminative": `) {
			t.Fatalf("лишняя правка: %q", l)
		}
	}
	var qs kb.QuestionSet
	if err := json.Unmarshal(out, &qs); err != nil {
		t.Fatal(err)
	}
	for _, q := range qs.Questions {
		v, ok := vals[q.ID]
		if ok != (q.Discriminative != nil) || (ok && *q.Discriminative != v) {
			t.Errorf("%s: %v", q.ID, q.Discriminative)
		}
	}
	// Вопрос с note — поле перед note; без note — последним полем, с
	// запятой у бывшего последнего.
	s := string(out)
	if !strings.Contains(s, "      \"discriminative\": false,\n      \"note\": \"Свежий (2024)") {
		t.Fatal("T01: не перед note")
	}
	if !strings.Contains(s, "]\n      ],\n      \"discriminative\": true\n    }") && !strings.Contains(s, "],\n      \"discriminative\": true\n    }") {
		t.Fatalf("T02: не последним полем:\n%s", s[strings.Index(s, `"id": "T02"`):strings.Index(s, `"id": "T03"`)])
	}
	// Повторная правка меняет значение на месте, не добавляя строк.
	again, err := SetDiscriminative(out, map[string]bool{"T01": true, "T02": false})
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(string(again), "\n")) != len(after) || !strings.Contains(string(again), "\"discriminative\": true,\n      \"note\": \"Свежий") {
		t.Fatal("повторная правка")
	}
	// CRLF сохраняется.
	crlf := []byte(strings.ReplaceAll(string(raw), "\n", "\r\n"))
	got, err := SetDiscriminative(crlf, map[string]bool{"T03": true})
	if err != nil || strings.Count(string(got), "\r\n") != strings.Count(string(crlf), "\r\n")+1 {
		t.Fatalf("CRLF: %v", err)
	}
	if _, err := SetDiscriminative(raw, map[string]bool{"X99": true}); err == nil {
		t.Fatal("нет вопроса")
	}
}

// diffAdded — строки after, которых нет в before на тех же местах (before
// — подпоследовательность after с точностью до запятой в конце строки:
// бывшему последнему полю она дописывается).
func diffAdded(before, after []string) []string {
	var out []string
	i := 0
	for _, l := range after {
		if i < len(before) && strings.TrimSuffix(before[i], ",") == strings.TrimSuffix(l, ",") {
			i++
			continue
		}
		out = append(out, l)
	}
	return out
}
