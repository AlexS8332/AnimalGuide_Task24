package bench

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
)

// TestRerankTrial — И-11 на настоящем корпусе, hash-эмбеддере и
// подставной модели: все проверки части A посчитаны (числа hash-поиска
// смысла не имеют — проверяется, что испытание их считает и называет), часть
// B даёт отчётные числа обоих режимов.
func TestRerankTrial(t *testing.T) {
	r, _ := ragRig(t, honest)
	tr := &Rerank{KBPath: ragKB(t), Questions: "../../eval/questions.json", Embedder: embed.Hash{}}
	res := r.run(t, tr)
	if res.Mechanism != features.RAGFilter || len(res.Lanes) != 3 || res.Lanes[2].Name != laneAnsBoth {
		t.Fatalf("дорожки: %s %+v", res.Mechanism, res.Lanes)
	}
	for _, what := range rerankChecks {
		c := find(t, res, what, laneRetrieve)
		if c.Status == Pending || c.Got == "" || c.Got == "—" {
			t.Errorf("%s: %+v", what, c)
		}
	}
	if c := find(t, res, rerankChecks[0], ""); !strings.Contains(c.Got, "из 7") {
		t.Errorf("вопросы вне базы — 6 out и T10: %+v", c)
	}
	if c := find(t, res, rerankChecks[1], ""); !strings.Contains(c.Want, "≤ 0 из 8") {
		t.Errorf("допуск падения: %+v", c)
	}
	if metric(res, "filter: пусто на вопросах «аспекта нет» (вид в базе есть; отчётно)", laneRetrieve) == "" {
		t.Error("нет отчётного числа aspect-missing")
	}
	for _, lane := range []string{laneAnsRAG, laneAnsBoth} {
		if metric(res, "верно / частично / неверно / «не знаю»", lane) == "" || metric(res, "токенов фрагментов на ответ", lane) == "" {
			t.Errorf("часть B, %s: нет чисел", lane)
		}
	}
	notes := strings.Join(res.Notes, "\n")
	if !strings.Contains(notes, "Калибровка на dev+out (в индекс не записана): порог") || !strings.Contains(notes, "откалиброван в прогоне") {
		t.Errorf("заметки: %s", notes)
	}
}

func TestRerankTrialPending(t *testing.T) {
	r := newRig(t)
	tr := &Rerank{KBPath: filepath.Join(t.TempDir(), "nope.db"), Questions: "../../eval/questions.json", Embedder: embed.Hash{}}
	res := r.run(t, tr)
	for _, what := range rerankChecks {
		if c := find(t, res, what, laneRetrieve); c.Status != Pending || !strings.Contains(c.Note, "базы знаний нет") {
			t.Errorf("%s: %+v", what, c)
		}
	}

	// Без модели — только часть A, с заметкой о пропуске части B; порог
	// записан в индекс — проверки идут с ним.
	path := ragKB(t)
	st, err := kb.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetMinScore(context.Background(), "structure", 0.5); err != nil {
		t.Fatal(err)
	}
	st.Close()
	tr = &Rerank{KBPath: path, Questions: "../../eval/questions.json", Embedder: embed.Hash{}}
	res = r.run(t, tr)
	notes := strings.Join(res.Notes, "\n")
	if !strings.Contains(notes, "Часть B (ответы rag и rag+both) пропущена") || !strings.Contains(notes, "Порог проверок 0.500 — из индекса") {
		t.Errorf("заметки без модели: %v", res.Notes)
	}
}
