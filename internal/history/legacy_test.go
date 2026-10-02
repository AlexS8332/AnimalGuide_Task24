package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

// Памятники прошлых форматов (ФТ-52): настоящие файлы упражнений 10 и 15,
// которые не правятся никогда. Тест поднимает их и проверяет, что данные не
// потерялись.

func legacy(t *testing.T, name string) (map[string]any, *Conversation) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "legacy", "history", name))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	json.Unmarshal(data, &raw)
	var c Conversation
	meta, err := store.Decode(Kind, data, &c)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if meta.Schema != 1 || !meta.Migrated {
		t.Fatalf("%s: meta %+v", name, meta)
	}
	normalize(&c)
	return raw, &c
}

func TestLegacyTreeFromExercise10(t *testing.T) {
	raw, c := legacy(t, "v1-tree-e82e0ce3f4b8cd12.json")
	rawBranches := raw["branches"].([]any)
	if len(c.Branches) != len(rawBranches) || len(c.Checkpoints) != len(raw["checkpoints"].([]any)) {
		t.Fatalf("ветки %d/%d", len(c.Branches), len(rawBranches))
	}
	for i, rb := range rawBranches {
		b := rb.(map[string]any)
		got := c.Branches[i]
		if len(got.Messages) != len(b["messages"].([]any)) || len(got.Turns) != len(b["turns"].([]any)) {
			t.Fatalf("ветка %d: сообщений %d, ходов %d", i, len(got.Messages), len(got.Turns))
		}
		if len(got.Facts.Entries) != len(b["facts"].(map[string]any)["entries"].([]any)) {
			t.Fatalf("ветка %d: факты потерялись", i)
		}
		for j, rt := range b["turns"].([]any) {
			if got.Turns[j].User != rt.(map[string]any)["user"] || got.Turns[j].Reply != rt.(map[string]any)["reply"] {
				t.Fatalf("ветка %d ход %d: текст изменился", i, j)
			}
			if _, ok := got.Turns[j].Extras["legacy.context"]; !ok {
				t.Fatalf("разбивка контекста прежнего формата потерялась")
			}
		}
	}
	// strategy=window → окно, без карточки фактов; того, чего не было, нет.
	fs := c.Features
	if !fs.On(features.Window) || fs.On(features.Facts) || fs.On(features.Tracker) || !fs.Known(features.Tracker) {
		t.Fatalf("механизмы: %v", fs.Map())
	}
	if len(c.Owners) != 1 || c.Owners[0] != DefaultOwner || c.Meter.Calls == 0 {
		t.Fatalf("собеседник и расход карточки: %v %+v", c.Owners, c.Meter)
	}
	// Путь ветки собирается через родителя, как в старом коде.
	for _, b := range c.Branches {
		if b.Parent != "" && len(c.Path(b.ID)) <= len(b.Messages) {
			t.Fatalf("ветка %s не наследует путь", b.Name)
		}
	}
	roundTrip(t, c)
}

func TestLegacyFlatFromExercise15(t *testing.T) {
	raw, c := legacy(t, "v1-flat-8495068e20195ef2.json")
	if len(c.Branches) != 1 || c.Active != c.Branches[0].ID {
		t.Fatalf("плоский диалог должен стать одной веткой: %d", len(c.Branches))
	}
	b := c.Branches[0]
	if len(b.Messages) != len(raw["messages"].([]any)) || len(b.Turns) != len(raw["turns"].([]any)) {
		t.Fatalf("сообщений %d, ходов %d", len(b.Messages), len(b.Turns))
	}
	task := raw["task"].(map[string]any)
	if c.Collection != task["id"] || c.CollectionTitle != task["title"] {
		t.Fatalf("задача → подборка: %q %q", c.Collection, c.CollectionTitle)
	}
	if len(c.Owners) != len(raw["owners"].([]any)) || c.Owners[0] != raw["owners"].([]any)[0] {
		t.Fatalf("собеседники: %v", c.Owners)
	}
	// Контрольная дорожка опыта 15: без профиля (lane=none), без свода
	// (noRules), автомат задачи есть, а прав этапа нет (noGates).
	fs := c.Features
	if fs.On(features.Profile) || fs.On(features.Charter) || fs.On(features.Guard) ||
		!fs.On(features.CollectionState) || fs.On(features.Gates) || !fs.On(features.MemoryLong) || fs.On(features.Facts) {
		t.Fatalf("механизмы дорожки: %v", fs.Map())
	}
	turn := b.Turns[0]
	rawTurn := raw["turns"].([]any)[0].(map[string]any)
	if turn.User != rawTurn["user"] || turn.Collection != task["id"] {
		t.Fatalf("ход: %q %q", turn.User, turn.Collection)
	}
	if _, ok := rawTurn["state"]; ok {
		if _, kept := turn.Extras["legacy.state"]; !kept {
			t.Fatal("итог автомата задачи прежнего формата потерялся")
		}
	}
	// События с полями прежнего формата: поля переехали в data, а не пропали.
	moved := 0
	for _, e := range turn.Events {
		if m, ok := e.Data.(map[string]any); ok && len(m) > 0 {
			moved++
		}
	}
	rawMoved := 0
	for _, re := range rawTurn["events"].([]any) {
		ev := re.(map[string]any)
		for _, k := range []string{"memoryChanges", "profileChanges", "stateChange", "stateNow", "ruleEvent"} {
			if _, ok := ev[k]; ok {
				rawMoved++
				break
			}
		}
	}
	if moved != rawMoved {
		t.Fatalf("событий с данными прежнего формата %d, перенесено %d", rawMoved, moved)
	}
	if c.Meter.Calls == 0 {
		t.Fatal("расход памяти потерялся")
	}
	roundTrip(t, c)
}

// roundTrip — поднятый диалог записывается в текущем формате и читается
// снова без изменений: миграция не зависит от того, сколько раз её
// прогнали.
func roundTrip(t *testing.T, c *Conversation) {
	t.Helper()
	dir := t.TempDir()
	s := NewStore(store.NewDir(dir))
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	again, err := s.Get(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(c)
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Fatalf("после записи и чтения диалог изменился:\n%.300s\n%.300s", a, b)
	}
}

func TestLegacyFirstWriteKeepsBackup(t *testing.T) {
	dir := t.TempDir()
	src, _ := os.ReadFile(filepath.Join("..", "..", "testdata", "legacy", "history", "v1-flat-8495068e20195ef2.json"))
	os.MkdirAll(filepath.Join(dir, "history"), 0o755)
	os.WriteFile(filepath.Join(dir, "history", "8495068e20195ef2.json"), src, 0o644)
	s := NewStore(store.NewDir(dir))
	convs, problems := s.Load()
	if len(convs) != 1 || len(problems) != 0 {
		t.Fatalf("загрузка памятника: %d %v", len(convs), problems)
	}
	if err := s.Save(convs[0]); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(filepath.Join(dir, "history", "8495068e20195ef2.v1.bak"))
	if err != nil || string(bak) != string(src) {
		t.Fatalf("копия старого формата: %v", err)
	}
}

func TestMigrateUnknownShape(t *testing.T) {
	var c Conversation
	if _, err := store.Decode(Kind, []byte(`{"id":"abcdefabcdefabcd"}`), &c); err == nil || !strings.Contains(err.Error(), "неизвестная форма") {
		t.Fatalf("неизвестная форма: %v", err)
	}
}

func TestMigrateFlatMinimal(t *testing.T) {
	var c Conversation
	doc := `{"id":"abcdefabcdefabcd","owner":"alex","preset":"full","agent":"tools","messages":[{"role":"user","content":"x"}],"turns":[{"id":"t","user":"x"}]}`
	if _, err := store.Decode(Kind, []byte(doc), &c); err != nil {
		t.Fatal(err)
	}
	normalize(&c)
	if c.Owners[0] != "alex" || c.Features.On(features.Window) || c.Features.On(features.Charter) || c.Branches[0].Turns[0].Status != TurnDone {
		t.Fatalf("минимальный плоский: %+v %v", c.Owners, c.Features.Map())
	}
	doc = `{"id":"abcdefabcdefabcd","messages":[],"turns":[],"lane":"none","preset":"work"}`
	c = Conversation{}
	store.Decode(Kind, []byte(doc), &c)
	if c.Owners[0] != DefaultOwner || c.Features.On(features.MemoryLong) || !c.Features.On(features.MemoryWork) || c.Features.On(features.Profile) {
		t.Fatalf("без собеседника: %+v %v", c.Owners, c.Features.Map())
	}
	doc = `{"id":"abcdefabcdefabcd","strategy":"facts","branches":[{"id":"b","turns":[{"id":"t","user":"x"}]}]}`
	c = Conversation{}
	store.Decode(Kind, []byte(doc), &c)
	if !c.Features.On(features.Facts) || !c.Features.On(features.Window) {
		t.Fatalf("стратегия facts: %v", c.Features.Map())
	}
}
