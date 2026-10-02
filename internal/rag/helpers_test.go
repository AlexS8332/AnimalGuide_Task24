package rag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
)

// Тесты пакета идут на настоящем корпусе и настоящих контрольных вопросах:
// база собирается один раз на прогон (structure, hash-эмбеддер — секунда),
// модель — подставная, по сценарию.

const questionsFile = "../../eval/questions.json"

// sharedKB — kb.db прогона тестов пакета.
var sharedKB string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rag-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sharedKB = filepath.Join(dir, "kb.db")
	if err := buildKB(sharedKB); err != nil {
		fmt.Fprintln(os.Stderr, "сборка базы:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func buildKB(path string) error {
	ctx := context.Background()
	docs, man, err := corpus.Load("../../corpus")
	if err != nil {
		return err
	}
	st, err := kb.Open(ctx, path)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.PutCorpus(ctx, docs, man); err != nil {
		return err
	}
	_, err = st.Build(ctx, kb.NewStructure(0, 0), embed.Hash{}, nil)
	return err
}

// searcher — поиск по общей базе со своим соединением.
func searcher(t *testing.T) *kb.Searcher {
	t.Helper()
	st, err := kb.Open(context.Background(), sharedKB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &kb.Searcher{Store: st, Embedder: embed.Hash{}}
}

// questions — контрольные вопросы репозитория.
func questions(t *testing.T) kb.QuestionSet {
	t.Helper()
	qs, err := kb.LoadQuestions(questionsFile)
	if err != nil {
		t.Fatal(err)
	}
	return qs
}

// question — вопрос по id.
func question(t *testing.T, qs kb.QuestionSet, id string) kb.Question {
	t.Helper()
	for _, q := range qs.Questions {
		if q.ID == id {
			return q
		}
	}
	t.Fatalf("вопроса %s нет", id)
	return kb.Question{}
}

// lastUser — последнее сообщение пользователя запроса.
func lastUser(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == llm.RoleUser {
			return req.Messages[i].Content
		}
	}
	return ""
}

var judgedAnswer = regexp.MustCompile(`(?s)Ответ для проверки:\n<<<\n(.*)\n>>>`)

// judgedText — ответ, который судья получил на проверку.
func judgedText(req llm.Request) string {
	m := judgedAnswer.FindStringSubmatch(lastUser(req))
	if m == nil {
		return ""
	}
	return m[1]
}

// isJudge — запрос судьи.
func isJudge(req llm.Request) bool {
	return len(req.Messages) > 0 && req.Messages[0].Content == JudgeSystem()
}

// verdictJSON — ответ судьи.
func verdictJSON(v Verdict, reason string) llm.Response {
	return llmtest.Text(fmt.Sprintf(`{"verdict":%q,"reason":%q}`, v, reason))
}

// askedText — текст вопроса в user-сообщении отвечающего агента: после
// «Вопрос: », если есть контекст, и до пустой строки (за ней фрагменты).
func askedText(user string) string {
	if _, after, ok := strings.Cut(user, "\n\nВопрос: "); ok && strings.HasPrefix(user, "Предыдущие реплики пользователя:") {
		user = after
	}
	text, _, _ := strings.Cut(user, "\n\n")
	return text
}

// questionOf — вопрос набора по тексту реплики отвечающего агента.
func questionOf(qs kb.QuestionSet, req llm.Request) (kb.Question, bool) {
	text := askedText(lastUser(req))
	for _, q := range qs.Questions {
		if q.Q == text {
			return q, true
		}
	}
	return kb.Question{}, false
}
