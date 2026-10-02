package runs

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm/llmtest"
)

// Список диалогов — свежие первыми: правка поднимает диалог наверх.
func TestListFreshFirst(t *testing.T) {
	r := newRig(t)
	if got := r.m.List(); len(got) != 0 {
		t.Fatalf("пустой менеджер: %d диалогов", len(got))
	}
	a, err := r.m.Create(StartOptions{Title: "первый"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	b, err := r.m.Create(StartOptions{Title: "второй"})
	if err != nil {
		t.Fatal(err)
	}
	list := r.m.List()
	if len(list) != 2 || list[0].ID != b.ID {
		t.Fatalf("порядок: %+v", list)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := r.m.Mark(a.ID, "точка"); err != nil {
		t.Fatal(err)
	}
	list = r.m.List()
	if list[0].ID != a.ID || list[0].Path == "" || list[0].Running {
		t.Fatalf("после правки первым должен стать %s: %+v", a.ID, list[0])
	}
}

// Conversation отдаёт копию: стенд и хуки не должны менять диалог в обход
// менеджера.
func TestConversationIsCopy(t *testing.T) {
	r := newRig(t)
	d, _ := r.m.Create(StartOptions{Title: "оригинал"})
	c, ok := r.m.Conversation(d.ID)
	if !ok || c.Title != "оригинал" {
		t.Fatalf("копия: %v %+v", ok, c)
	}
	c.Title = "испорчено"
	c.Owners[0] = "чужой"
	again, _ := r.m.Get(d.ID)
	if again.Title != "оригинал" || again.Owners[0] == "чужой" {
		t.Fatalf("правка копии просочилась в диалог: %+v", again.Summary)
	}
	if _, ok := r.m.Conversation("nope"); ok {
		t.Fatal("копия несуществующего диалога")
	}
	if _, ok := r.m.Get("nope"); ok {
		t.Fatal("несуществующий диалог")
	}
	if _, ok := r.m.Turn("nope"); ok {
		t.Fatal("несуществующий ход")
	}
}

// Аксессоры менеджера отдают то, с чем его собрали.
func TestManagerAccessors(t *testing.T) {
	r := newRig(t)
	if r.m.Registry() == nil || r.m.Registry().Diff(r.m.Defaults(), features.Catalog().Defaults()) != nil {
		t.Fatal("реестр и умолчания")
	}
	if dir := r.m.DisplayDir(); dir == "" || !strings.Contains(dir, "history") {
		t.Fatalf("каталог диалогов: %q", dir)
	}
	// Умолчания конструктора: нулевые срок, окно и сокращение заменяются.
	m := NewManager(Config{})
	if m.cfg.Timeout <= 0 || m.cfg.Window <= 0 || m.cfg.KeepToolRunes <= 0 {
		t.Fatalf("умолчания: %+v", m.cfg)
	}
}

// AddHook подключает хук к следующим ходам.
func TestAddHookRunsOnNextTurn(t *testing.T) {
	r := newRig(t)
	var before, after atomic.Int32
	r.m.AddHook(testHook{name: "late",
		before: func(*Turn) error { before.Add(1); return nil },
		after:  func(*Turn) error { after.Add(1); return nil }})
	s, err := r.m.Start(StartOptions{Request: agents.Request{Text: "рысь"}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s)
	if before.Load() != 1 || after.Load() != 1 {
		t.Fatalf("хук до/после: %d/%d", before.Load(), after.Load())
	}
}

// Неудачный ход не зовёт хуки «после»: проверять нечего (ФТ-15).
func TestHandlerErrorSkipsAfterHooks(t *testing.T) {
	r := newRig(t)
	var after atomic.Int32
	r.m = r.manager(testHook{name: "collection",
		before: func(tr *Turn) error {
			tr.Handler = func(context.Context) (agents.Result, error) {
				return agents.Result{}, errors.New("подборка сломалась")
			}
			return nil
		},
		after: func(*Turn) error { after.Add(1); return nil }})
	s, _ := r.m.Start(StartOptions{Request: agents.Request{Text: "собери подборку"}})
	v := wait(t, s)
	if v.Status != StatusFailed || !strings.Contains(v.Error, "подборка сломалась") {
		t.Fatalf("ход: %+v", v)
	}
	if after.Load() != 0 {
		t.Fatal("хук «после» вызван для неудачного хода")
	}
	d, _ := r.m.Get(v.ConversationID)
	if d.Messages != 0 || d.TurnList[0].User != "собери подборку" {
		t.Fatalf("неудачный ход в истории: %d сообщений, реплика %q", d.Messages, d.TurnList[0].User)
	}
}

// Текущая подборка диалога: ссылка на файл, без копирования (ФТ-28).
func TestSetCollection(t *testing.T) {
	r := newRig(t)
	d, _ := r.m.Create(StartOptions{})
	for _, bad := range []string{"../x", "ABCDEF12", "abc", "совы"} {
		if _, err := r.m.SetCollection(d.ID, bad, "x"); err == nil {
			t.Errorf("идентификатор подборки %q принят", bad)
		}
	}
	got, err := r.m.SetCollection(d.ID, "abcdef12", "совы")
	if err != nil || got.Collection != "abcdef12" {
		t.Fatalf("подборка: %+v %v", got.Summary, err)
	}
	c, _ := r.m.Conversation(d.ID)
	if c.CollectionTitle != "совы" {
		t.Fatalf("название подборки: %q", c.CollectionTitle)
	}
	// Пустой идентификатор — отложить подборку.
	got, err = r.m.SetCollection(d.ID, "", "")
	if err != nil || got.Collection != "" {
		t.Fatalf("отложить подборку: %+v %v", got.Summary, err)
	}
	if _, err := r.m.SetCollection("nope", "abcdef12", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("несуществующий диалог: %v", err)
	}
}

// Собеседник — часть имени файла: чужие символы не пропускаются.
func TestCreateValidatesOwnersAndFeatures(t *testing.T) {
	r := newRig(t)
	for _, owner := range []string{"../etc", "a.b", "", "имя"} {
		if _, err := r.m.Create(StartOptions{Owners: []string{owner}}); err == nil {
			t.Errorf("собеседник %q принят", owner)
		}
	}
	// Страж без свода не имеет смысла: набор с противоречием не принимается.
	bad := features.Catalog().Defaults().With(features.Charter, false)
	if _, err := r.m.Create(StartOptions{Features: bad}); err == nil {
		t.Fatal("страж без свода принят")
	}
	if _, err := r.m.Start(StartOptions{Features: bad, Request: agents.Request{Text: "рысь"}}); err == nil {
		t.Fatal("ход с противоречивым набором механизмов")
	}
	d, err := r.m.Create(StartOptions{Owners: []string{"anna", "boris"}, Titles: map[string]string{"anna": "Анна"}, Title: "  семейный  "})
	if err != nil || d.Title != "семейный" || len(d.Owners) != 2 {
		t.Fatalf("диалог двоих: %+v %v", d.Summary, err)
	}
	c, _ := r.m.Conversation(d.ID)
	if c.TitleOf("anna") != "Анна" {
		t.Fatalf("имя собеседника: %q", c.TitleOf("anna"))
	}
}

// Удаление дорожки убирает её из стенда, остальные дорожки остаются.
func TestDeleteLaneLeavesGroup(t *testing.T) {
	r := newRig(t)
	base := features.Catalog().Defaults()
	group, lanes, err := r.m.StartGroup("стенд", []Lane{{Name: "a", Features: base}, {Name: "b", Features: base.With(features.Scan, false)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.m.Delete(lanes[0].ID); err != nil {
		t.Fatal(err)
	}
	left, ok := r.m.Group(group)
	if !ok || len(left) != 1 || left[0].ID != lanes[1].ID {
		t.Fatalf("стенд после удаления: %+v", left)
	}
	if ids := r.m.Groups()[group]; len(ids) != 1 {
		t.Fatalf("дорожки стенда: %v", ids)
	}
	if _, ok := r.m.Group("nope"); ok {
		t.Fatal("несуществующий стенд")
	}
}

// Свой собеседник дорожки сохраняется как есть.
func TestLaneOwnOwner(t *testing.T) {
	r := newRig(t)
	base := features.Catalog().Defaults()
	_, lanes, err := r.m.StartGroup("стенд", []Lane{{Name: "a", Features: base, Owner: " vera "}})
	if err != nil || lanes[0].Owners[0] != "vera" {
		t.Fatalf("собеседник дорожки: %+v %v", lanes, err)
	}
	if _, _, err := r.m.StartGroup("стенд", []Lane{{Name: "a", Features: base, Owner: "../x"}}); err == nil {
		t.Fatal("дорожка с опасным собеседником")
	}
	if got := laneSlug(strings.Repeat("abc", 20)); len(got) != 24 {
		t.Fatalf("длинное имя дорожки: %q", got)
	}
	if got := laneSlug("  -a_b- "); got != "a-b" {
		t.Fatalf("laneSlug: %q", got)
	}
}

// Ход без вида — обычное сообщение; реплика пользователя берётся из
// запроса, если агент её не вернул.
func TestOrUser(t *testing.T) {
	if orUser("", "текст") != "текст" || orUser("своё", "текст") != "своё" {
		t.Fatal("orUser")
	}
	if !contains([]string{"a", "b"}, "b") || contains(nil, "a") {
		t.Fatal("contains")
	}
}

// Сравнение в пустом диалоге не заводит ветку: ветвиться не от чего.
func TestCompareInEmptyDialogStaysInMain(t *testing.T) {
	r := newRig(t)
	r.fake.Fn = func(llm.Request) (llm.Response, error) { return llmtest.Text("ок"), nil }
	d, _ := r.m.Create(StartOptions{})
	s, err := r.m.Send(d.ID, agents.Request{Kind: agents.KindCompare, A: "рысь", B: "манул"})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s)
	got, _ := r.m.Get(d.ID)
	if len(got.BranchTree) != 1 || len(got.Checkpoints) != 0 {
		t.Fatalf("ветки: %d, точки: %d", len(got.BranchTree), len(got.Checkpoints))
	}
}

// Пока идёт ход, Detail показывает его, а Summary помечает диалог занятым.
func TestDetailShowsRunningTurn(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	r.fake.Fn = func(llm.Request) (llm.Response, error) {
		<-release
		return llmtest.Text("ок"), nil
	}
	s, _ := r.m.Start(StartOptions{Request: agents.Request{Text: "как дела?"}})
	id := s.View().ConversationID
	d, _ := r.m.Get(id)
	running := d.Active != nil && d.Running
	if _, err := r.m.SetCollection(id, "abcdef12", ""); !errors.Is(err, ErrBusy) {
		t.Errorf("подборка во время хода: %v", err)
	}
	if _, err := r.m.SetFeature(id, features.Scan, false); !errors.Is(err, ErrBusy) {
		t.Errorf("механизм во время хода: %v", err)
	}
	close(release)
	wait(t, s)
	if !running {
		t.Fatal("идущий ход не виден в диалоге")
	}
	if d, _ = r.m.Get(id); d.Active != nil || d.Running {
		t.Fatal("завершённый ход всё ещё виден как идущий")
	}
}
