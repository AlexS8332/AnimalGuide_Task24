package kb

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
)

// TestQuestions — сколько вопросов в наборе test (контрольные вопросы заданий).
const TestQuestions = 10

// LoadQuestions читает файл вопросов.
func LoadQuestions(path string) (QuestionSet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return QuestionSet{}, err
	}
	var qs QuestionSet
	if err := json.Unmarshal(b, &qs); err != nil {
		return QuestionSet{}, fmt.Errorf("%s: %w", path, err)
	}
	if qs.Schema != QuestionsSchema {
		return QuestionSet{}, fmt.Errorf("%s: schema %d, ожидается %d", path, qs.Schema, QuestionsSchema)
	}
	return qs, nil
}

// Verify — проверка разметки: id уникальны, split известен, у отвечаемых
// есть evidence и sources, каждый Evidence.Quote находится (corpus.Find) в
// своём документе, doc_id существует, у out — answerable=false и нет
// evidence, у followup есть Context, в test ровно TestQuestions вопросов.
// Список всех нарушений, а не первое.
func (qs QuestionSet) Verify(docs []corpus.Doc) []error {
	var errs []error
	bad := func(id, format string, a ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", id, fmt.Sprintf(format, a...)))
	}
	texts := make(map[string]string, len(docs))
	for _, d := range docs {
		texts[d.ID] = d.Text()
	}
	seen := map[string]bool{}
	tests := 0
	for i, q := range qs.Questions {
		id := q.ID
		if id == "" {
			id = fmt.Sprintf("#%d", i)
			bad(id, "пустой id")
		} else if seen[id] {
			bad(id, "id повторяется")
		}
		seen[q.ID] = true
		if q.Q == "" {
			bad(id, "пустой вопрос")
		}
		switch q.Split {
		case SplitTest:
			tests++
		case SplitDev:
		case SplitOut:
			if q.Answerable {
				bad(id, "вопрос out помечен answerable")
			}
			if len(q.Evidence) > 0 {
				bad(id, "у вопроса out есть evidence")
			}
		default:
			bad(id, "неизвестный split %q", q.Split)
		}
		if q.Answerable {
			if len(q.Evidence) == 0 {
				bad(id, "у отвечаемого вопроса нет evidence")
			}
			if len(q.Sources) == 0 {
				bad(id, "у отвечаемого вопроса нет sources")
			}
		}
		if q.Type == "followup" && len(q.Context) == 0 {
			bad(id, "у followup нет context")
		}
		for _, s := range q.Sources {
			if _, ok := texts[s.DocID]; !ok {
				bad(id, "source: нет документа %q", s.DocID)
			}
		}
		for j, e := range q.Evidence {
			text, ok := texts[e.DocID]
			if !ok {
				bad(id, "evidence %d: нет документа %q", j, e.DocID)
				continue
			}
			if e.Quote == "" {
				bad(id, "evidence %d: пустой фрагмент", j)
				continue
			}
			if start, _ := corpus.Find(text, e.Quote); start < 0 {
				bad(id, "evidence %d: фрагмент не найден в %s: %q", j, e.DocID, e.Quote)
			}
		}
	}
	if tests != TestQuestions {
		errs = append(errs, fmt.Errorf("в test %d вопросов, ожидается %d", tests, TestQuestions))
	}
	return errs
}

// Split — вопросы набора в порядке файла.
func (qs QuestionSet) Split(name string) []Question {
	var out []Question
	for _, q := range qs.Questions {
		if q.Split == name {
			out = append(out, q)
		}
	}
	return out
}
