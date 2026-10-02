package kb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
)

// TestRealQuestions — «проверка проверки»: настоящая разметка сходится с
// настоящим корпусом. Корпус изменился — тест падает, а не молча портит
// recall.
func TestRealQuestions(t *testing.T) {
	docs, _, err := corpus.Load("../../corpus")
	if err != nil {
		t.Fatal(err)
	}
	qs, err := LoadQuestions("../../eval/questions.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range qs.Verify(docs) {
		t.Error(e)
	}
	test, dev, out := qs.Split(SplitTest), qs.Split(SplitDev), qs.Split(SplitOut)
	if len(test)+len(dev)+len(out) != len(qs.Questions) {
		t.Errorf("наборы не покрывают файл: %d+%d+%d != %d", len(test), len(dev), len(out), len(qs.Questions))
	}
	if len(dev) < 14 || len(dev) > 20 {
		t.Errorf("dev: %d вопросов, нужно 14–20", len(dev))
	}
	if len(out) < 6 || len(out) > 8 {
		t.Errorf("out: %d вопросов, нужно 6–8", len(out))
	}
	// Десятка test покрывает все десять типов раздела 3.
	types := map[string]bool{}
	for _, q := range test {
		types[q.Type] = true
	}
	for _, ty := range []string{"fact", "number", "conflict", "section", "compare",
		"multihop", "synonym", "followup", "aspect-missing", "out-of-base"} {
		if !types[ty] {
			t.Errorf("в test нет вопроса типа %s", ty)
		}
	}
	// Фрагменты-доказательства — одно-два предложения, а не раздел целиком.
	for _, q := range qs.Questions {
		for _, e := range q.Evidence {
			if n := len([]rune(e.Quote)); n < 40 || n > 250 {
				t.Errorf("%s: фрагмент %d символов, нужно 40–250", q.ID, n)
			}
		}
		if q.Type == "compare" && distinctDocs(q.Evidence) < 2 {
			t.Errorf("%s: сравнение без доказательств из двух документов", q.ID)
		}
	}
}

func distinctDocs(ev []Evidence) int {
	m := map[string]bool{}
	for _, e := range ev {
		m[e.DocID] = true
	}
	return len(m)
}

func testDocs() []corpus.Doc {
	return []corpus.Doc{
		{ID: "a", Intro: "Манул, или палласов кот — хищник.", Sections: []corpus.Section{
			{Path: []string{"Описание"}, Title: "Описание", Level: 2, Text: "Масса — 2—5 кг. Ёж «рядом»."},
		}},
		{ID: "b", Intro: "Корсак — степная лисица."},
	}
}

func tenTests() []Question {
	var qs []Question
	for i := 0; i < TestQuestions; i++ {
		qs = append(qs, Question{ID: "T" + string(rune('A'+i)), Split: SplitTest, Type: "fact", Q: "?",
			Answerable: true, Sources: []SourceRef{{DocID: "a"}},
			Evidence: []Evidence{{DocID: "a", Quote: "масса - 2-5 кг"}}})
	}
	return qs
}

func TestVerifyOK(t *testing.T) {
	qs := QuestionSet{Schema: QuestionsSchema, Questions: append(tenTests(),
		Question{ID: "D1", Split: SplitDev, Type: "followup", Q: "а он?", Context: []string{"Кто такой корсак?"},
			Answerable: true, Sources: []SourceRef{{DocID: "b"}},
			Evidence: []Evidence{{DocID: "a", Quote: "еж \"рядом\""}}},
		Question{ID: "O1", Split: SplitOut, Type: "out-of-base", Q: "жираф?"},
	)}
	if errs := qs.Verify(testDocs()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if n := len(qs.Split(SplitTest)); n != TestQuestions {
		t.Fatalf("Split(test) = %d", n)
	}
	if got := qs.Split(SplitOut); len(got) != 1 || got[0].ID != "O1" {
		t.Fatalf("Split(out) = %v", got)
	}
	if got := qs.Split("nope"); got != nil {
		t.Fatalf("Split(nope) = %v", got)
	}
}

func TestVerifyErrors(t *testing.T) {
	qs := QuestionSet{Schema: QuestionsSchema, Questions: []Question{
		{ID: "T1", Split: SplitTest, Type: "fact", Q: "?", Answerable: true}, // нет evidence и sources
		{ID: "T1", Split: SplitTest, Type: "fact", Q: "?"},                   // повтор id
		{ID: "X1", Split: "train", Q: "?"},                                   // неизвестный split
		{ID: "D1", Split: SplitDev, Q: "?", Answerable: true, Sources: []SourceRef{{DocID: "zzz"}},
			Evidence: []Evidence{{DocID: "a", Quote: "этого фрагмента нет в документе"}, {DocID: "zzz", Quote: "x"}}},
		{ID: "O1", Split: SplitOut, Q: "?", Answerable: true, Evidence: []Evidence{{DocID: "a", Quote: "Масса"}}},
		{ID: "D2", Split: SplitDev, Type: "followup", Q: "а он?"},
	}}
	errs := qs.Verify(testDocs())
	var all []string
	for _, e := range errs {
		all = append(all, e.Error())
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{
		"T1: у отвечаемого вопроса нет evidence",
		"T1: у отвечаемого вопроса нет sources",
		"T1: id повторяется",
		"X1: неизвестный split",
		"D1: source: нет документа \"zzz\"",
		"D1: evidence 0: фрагмент не найден",
		"D1: evidence 1: нет документа \"zzz\"",
		"O1: вопрос out помечен answerable",
		"O1: у вопроса out есть evidence",
		"D2: у followup нет context",
		"в test 2 вопросов, ожидается 10",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("нет ошибки %q в:\n%s", want, joined)
		}
	}
}

func TestLoadQuestions(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "q.json")
	if err := os.WriteFile(good, []byte(`{"schema":1,"version":3,"questions":[{"id":"D1","split":"dev","type":"followup","q":"а он?","context":["Кто такой корсак?"],"answerable":false}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	qs, err := LoadQuestions(good)
	if err != nil {
		t.Fatal(err)
	}
	if qs.Version != 3 || len(qs.Questions) != 1 || len(qs.Questions[0].Context) != 1 {
		t.Fatalf("LoadQuestions = %+v", qs)
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"schema":2,"questions":[]}`), 0o644)
	if _, err := LoadQuestions(bad); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("schema 2: err = %v", err)
	}
	broken := filepath.Join(dir, "broken.json")
	os.WriteFile(broken, []byte(`{`), 0o644)
	if _, err := LoadQuestions(broken); err == nil {
		t.Fatal("битый JSON без ошибки")
	}
	if _, err := LoadQuestions(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("нет файла — без ошибки")
	}
}
