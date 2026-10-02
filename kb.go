package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kbapi"
)

// kbFile — имя базы знаний в каталоге данных: там же её по умолчанию ищет
// и собирает `go run ./cmd/kb index`.
const kbFile = "kb.db"

// kbPath — путь к базе знаний: флаг -kb, иначе KB_DB, иначе <data>/kb.db.
func (o options) kbPath(dataDir string) string {
	if o.kb != "" {
		return o.kb
	}
	if v := strings.TrimSpace(os.Getenv("KB_DB")); v != "" {
		return v
	}
	return filepath.Join(dataDir, kbFile)
}

// embedder — клиент эмбеддера: флаг -embedder (адрес), иначе окружение
// EMBED_BASE_URL / EMBED_MODEL / EMBED_API_KEY, иначе сайдкар по умолчанию.
// Окружение читается здесь, а не в умолчании флага: .env-файлы
// подхватываются уже после разбора флагов.
func (o options) embedder() *embed.HTTP {
	e := embed.FromEnv()
	if o.embedURL != "" {
		e.BaseURL = o.embedURL
	}
	return e
}

// kbParts — база знаний приложения: REST окна «База знаний» и закрытие.
type kbParts struct {
	api   *kbapi.API
	close func()
}

// openKB открывает базу знаний, если она собрана. Базы нет — не ошибка:
// окно «База знаний» скажет, как её собрать, а остальное приложение от неё
// не зависит. Эмбеддер не опрашивается при старте: поиск сам откатывается на
// BM25, если сайдкар не отвечает.
func openKB(o options, dataDir string) kbParts {
	path := o.kbPath(dataDir)
	emb := o.embedder()
	api := &kbapi.API{Embedder: emb, Path: path}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			api.Why = "базы знаний нет: " + path
		} else {
			api.Why = "база знаний недоступна: " + err.Error()
		}
		return kbParts{api: api, close: func() {}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := kb.Open(ctx, path)
	if err != nil {
		api.Why = "база знаний не открылась: " + err.Error()
		return kbParts{api: api, close: func() {}}
	}
	api.Searcher = &kb.Searcher{Store: st, Embedder: emb}
	return kbParts{api: api, close: func() { st.Close() }}
}

// kbLine — строка о базе знаний для стартового вывода.
func kbLine(a *kbapi.API) string {
	if a == nil || a.Searcher == nil {
		why := "нет"
		if a != nil && a.Why != "" {
			why = a.Why
		}
		return why + " (собрать: go run ./cmd/kb index -strategy all)"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	docs, _ := a.Searcher.Store.Docs(ctx)
	idx, _ := a.Searcher.Store.Indexes(ctx)
	var names []string
	for _, i := range idx {
		names = append(names, fmt.Sprintf("%s (%d)", i.ID, i.Chunks))
	}
	line := fmt.Sprintf("%s — документов %d, индексы: %s", a.Path, len(docs), strings.Join(names, ", "))
	st := a.Embedder.Health(ctx)
	if st.OK {
		return line + "; эмбеддер " + st.Model + " (" + st.URL + ")"
	}
	return line + "; эмбеддер не отвечает — поиск по BM25"
}

// kbQuestionsFile — контрольные вопросы базы знаний по умолчанию.
const kbQuestionsFile = "eval/questions.json"

// kbQuestions — путь к контрольным вопросам: KB_QUESTIONS, иначе
// eval/questions.json от рабочего каталога (вопросы лежат в репозитории).
func (o options) kbQuestions() string {
	if v := strings.TrimSpace(os.Getenv("KB_QUESTIONS")); v != "" {
		return v
	}
	return kbQuestionsFile
}
