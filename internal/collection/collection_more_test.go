package collection

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

// Ожидаемое действие вычисляется из этапа, шага, паузы и вопроса (ИП-6):
// на каждую ветку — своё действующее лицо.
func TestExpectedByState(t *testing.T) {
	withItems := New("x", "совы")
	withItems.Items = []Item{{N: 1, Name: "сипуха"}, {N: 2, Name: "филин"}}
	cases := []struct {
		name  string
		mod   func(s *State)
		actor string
		text  string
	}{
		{"пустой план", func(s *State) { s.Items = nil }, ActorAgent, "составить план"},
		{"план готов", func(s *State) {}, ActorUser, "утвердить план"},
		{"вопрос", func(s *State) { s.Question = "сколько видов?" }, ActorUser, "«сколько видов?»"},
		{"пауза важнее вопроса", func(s *State) { s.Question = "?"; s.Paused = &Pause{} }, ActorUser, "вернуться к подборке"},
		{"сбор с текущим видом", func(s *State) { s.Stage, s.Current = Collecting, 2 }, ActorAgent, "вид 2 «филин»"},
		{"сбор без текущего вида", func(s *State) { s.Stage, s.Current = Collecting, 0 }, ActorAgent, "собрать текущий вид"},
		{"сверка без отчёта", func(s *State) { s.Stage = Validation }, ActorAgent, "свести сверку"},
		{"сверка с отчётом", func(s *State) { s.Stage, s.Report = Validation, &Report{} }, ActorUser, "принять подборку"},
		{"принята", func(s *State) { s.Stage = Done }, ActorNone, "подборка принята"},
		{"неизвестный этап", func(s *State) { s.Stage = "x" }, ActorNone, ""},
	}
	for _, c := range cases {
		s := withItems.Clone()
		c.mod(&s)
		e := s.Expected()
		if e.Actor != c.actor || !strings.Contains(e.Text, c.text) {
			t.Errorf("%s: %+v", c.name, e)
		}
	}
}

// Сводка одной строкой называет этап, счёт видов, паузу и кто ждёт.
func TestSummaryLine(t *testing.T) {
	s := New("x", "совы")
	if got := s.Summary(); got != "этап «план», ждёт: справочник" {
		t.Fatalf("пустая подборка: %q", got)
	}
	s.Stage, s.Current = Collecting, 1
	s.Items = []Item{{N: 1, Name: "сипуха", Status: ItemDone}, {N: 2, Name: "филин"}}
	s.Paused = &Pause{}
	if got := s.Summary(); got != "этап «сбор», собрано 1 из 2, на паузе, ждёт: человек" {
		t.Fatalf("сбор на паузе: %q", got)
	}
	s.Paused, s.Stage = nil, Done
	if got := s.Summary(); strings.Contains(got, "ждёт") {
		t.Fatalf("принятая подборка никого не ждёт: %q", got)
	}
}

// Номера видов — с единицы; выход за границы — nil, а не паника.
func TestItemBounds(t *testing.T) {
	s := New("x", "")
	s.Items = []Item{{N: 1, Name: "a"}, {N: 2, Name: "b"}}
	for _, n := range []int{0, -1, 3} {
		if s.Item(n) != nil {
			t.Errorf("вид %d", n)
		}
	}
	if it := s.Item(2); it == nil || it.Name != "b" {
		t.Fatal("вид 2")
	}
	// Item отдаёт указатель в состояние: правка видна.
	s.Item(1).Result = "готово"
	if s.Items[0].Result != "готово" {
		t.Fatal("Item отдал копию")
	}
	for _, cur := range []int{0, 3} {
		s.Current = cur
		if s.CurrentItem() != nil {
			t.Errorf("текущий вид %d", cur)
		}
	}
	s.Current = 1
	if s.CurrentItem().Name != "a" {
		t.Fatal("текущий вид")
	}
}

// Отчёт сверки: nil-отчёт ничего не покрывает и не содержит провалов.
func TestReportCoversAndFailed(t *testing.T) {
	var none *Report
	if none.Covers(1) || none.Failed() != nil {
		t.Fatal("пустой отчёт")
	}
	r := &Report{Checks: []Check{{N: 1, OK: true}, {N: 2, OK: false, Issues: []string{"нет ареала"}}}}
	if !r.Covers(2) || r.Covers(3) {
		t.Fatal("Covers")
	}
	if f := r.Failed(); len(f) != 1 || f[0].N != 2 {
		t.Fatalf("Failed: %+v", f)
	}
}

// Сквозной номер хода подборки растёт через все её диалоги.
func TestBeginCountsTurns(t *testing.T) {
	s := New("x", "")
	if Begin(&s) != 1 || Begin(&s) != 2 || s.Turn != 2 {
		t.Fatalf("номер хода: %d", s.Turn)
	}
}

// Каждое событие таблицы называется словами, отклонённый переход — с
// причиной, переход между этапами — со стрелкой.
func TestEventAndChangeTitles(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Rules {
		title := EventTitle(r.Event)
		if title == r.Event || seen[title] {
			t.Errorf("событие %s: %q", r.Event, title)
		}
		seen[title] = true
	}
	moved := Change{Event: EvApprove, From: Point{Stage: Planning}, To: Point{Stage: Collecting, Item: 1}}
	if got := moved.Title(); got != "план утверждён: план → сбор, вид 1" {
		t.Fatalf("переход: %q", got)
	}
	rejected := Change{Event: EvAccept, Rejected: true, Reason: "нет отчёта"}
	if got := rejected.Title(); got != "подборка принята — отклонено: нет отчёта" {
		t.Fatalf("отказ: %q", got)
	}
	if ActorTitle(ActorUser) != "человек" || ActorTitle(ActorAgent) != "справочник" || ActorTitle(ActorNone) != "никто" {
		t.Fatal("ActorTitle")
	}
}

// Отказов хранится не больше MaxDenials, старые уходят первыми.
func TestDenyKeepsLast(t *testing.T) {
	s := New("x", "")
	for i := 0; i < MaxDenials+5; i++ {
		s.Deny(Denial{What: string(rune('a' + i%26)), Stage: Collecting})
	}
	if len(s.Denials) != MaxDenials || s.Denials[0].Stage != Collecting || s.Denials[0].Time.IsZero() {
		t.Fatalf("отказы: %d", len(s.Denials))
	}
}

// Битый файл подборки — ошибка, а не молча новая пустая подборка: иначе
// следующая запись затёрла бы собранное.
func TestStoreBrokenFile(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(store.NewDir(dir))
	id := "abcdefabcdefabcd"
	if err := os.MkdirAll(filepath.Join(dir, "collections"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "collections", id+".json"), []byte("{не json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(id, "совы"); err == nil {
		t.Fatal("битый файл прочитан как новая подборка")
	}
	wrote := false
	if _, err := st.Update(id, "совы", func(*State) bool { wrote = true; return true }); err == nil || wrote {
		t.Fatalf("правка поверх битого файла: %v, fn вызвана: %v", err, wrote)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "collections", id+".json")); string(raw) != "{не json" {
		t.Fatal("битый файл затёрт")
	}
	list, problems := st.List()
	if len(list) != 0 || len(problems) != 1 {
		t.Fatalf("список с битым файлом: %d, проблем %d", len(list), len(problems))
	}
}

// Нет файла — новая пустая подборка с названием; fn без записи файл не
// создаёт.
func TestStoreMissingAndNoWrite(t *testing.T) {
	st := NewStore(store.NewDir(t.TempDir()))
	id := "0123456789abcdef"
	got, err := st.Get(id, "совы")
	if err != nil || got.ID != id || got.Title != "совы" || got.Stage != Planning || got.Items == nil || got.Log == nil {
		t.Fatalf("новая подборка: %+v %v", got, err)
	}
	if _, err := st.Update(id, "совы", func(s *State) bool { s.Goal = "x"; return false }); err != nil || st.Has(id) {
		t.Fatalf("правка без записи создала файл: %v", err)
	}
	if _, _, err := st.Raw(id); err == nil {
		t.Fatal("файл несуществующей подборки")
	}
}

// Выгрузка подборки (ФТ-39): несданный вид и отчёт сверки видны.
func TestMarkdownPendingAndReport(t *testing.T) {
	s := New("x", "Совы")
	s.Goal = "для доклада"
	s.Items = []Item{{N: 1, Name: "сипуха"}}
	s.Report = &Report{Checks: []Check{{N: 1, OK: false, Issues: []string{"нет карточки", "нет ареала"}}}}
	md := Markdown(s)
	for _, want := range []string{"# Подборка: Совы", "для доклада", "Видов собрано: 0 из 1", "## 1. сипуха", "Карточка не сдана", "## Сверка", "не сошлось: нет карточки; нет ареала"} {
		if !strings.Contains(md, want) {
			t.Errorf("в выгрузке нет %q", want)
		}
	}
}
