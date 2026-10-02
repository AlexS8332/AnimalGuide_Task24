package profile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

// Итог проверок одной строкой (ФТ-23): «не определить» в знаменатель не
// идёт, нарушения перечислены с тем, что просили.
func TestNoteLine(t *testing.T) {
	checks := []Check{
		{Field: FieldLength, Title: "длина ответа", Want: "коротко", Got: "длинно", OK: false},
		{Field: FieldLatin, Title: "латинские названия", OK: true},
		{Field: FieldAddress, Title: "обращение", NA: true},
	}
	got := Note(checks)
	want := "профиль соблюдён в 1 проверках из 2; нарушено — длина ответа: длинно вместо «коротко»"
	if got != want {
		t.Fatalf("итог:\n%q\nждали\n%q", got, want)
	}
	if got := Note(checks[1:]); got != "профиль соблюдён в 1 проверках из 1" {
		t.Fatalf("всё соблюдено: %q", got)
	}
	if got := Note(checks[2:]); got != "проверить нечего" {
		t.Fatalf("только неопределимое: %q", got)
	}
	if ok, total := Rate(checks); ok != 1 || total != 2 {
		t.Fatalf("Rate: %d/%d", ok, total)
	}
}

// Незаполненная анкета ничего не требует — и нарушить нечего.
func TestChecksEmptyProfile(t *testing.T) {
	if got := Checks(New("me", ""), "Рысь — кошка. Вы её видели? Lynx lynx 🐾"); len(got) != 0 {
		t.Fatalf("проверки пустой анкеты: %+v", got)
	}
}

// Get и Set: одинаковое значение — не правка, версия не растёт.
func TestGetSetClear(t *testing.T) {
	var p Profile // нулевая анкета: Values ещё нет
	if _, ok := p.Get(FieldLength); ok {
		t.Fatal("значение в пустой анкете")
	}
	if !p.Set(FieldLength, Value{Value: "short"}) || p.Version != 1 || p.Created == nil {
		t.Fatalf("первая правка: %+v", p)
	}
	if p.Set(FieldLength, Value{Value: "short", Quote: "другая цитата"}) || p.Version != 1 {
		t.Fatal("то же значение засчитано правкой")
	}
	if v, ok := p.Get(FieldLength); !ok || v.Value != "short" || p.Val(FieldLength) != "short" {
		t.Fatalf("Get: %+v", v)
	}
	if !p.Clear(FieldLength) || p.Clear(FieldLength) || p.Version != 2 {
		t.Fatalf("Clear: версия %d", p.Version)
	}
}

// Снятие ограничения без регистра и пробелов; несуществующее — не правка.
func TestDropLimit(t *testing.T) {
	p := New("me", "")
	p.AddLimit(Limit{Text: "Без   охоты"})
	if p.DropLimit("нет такого") {
		t.Fatal("снято несуществующее")
	}
	if !p.DropLimit("  без охоты ") || len(p.Limits) != 0 {
		t.Fatalf("снятие: %+v", p.Limits)
	}
	// Снятие через правку без такого ограничения — без записей в журнал.
	if ch := Apply(&p, Patch{Limits: []LimitOp{{Text: "без охоты", Drop: true, Quote: "можно про охоту"}}}, "можно про охоту", 1); len(ch) != 0 {
		t.Fatalf("снятие несуществующего: %+v", ch)
	}
	if added, _ := p.AddLimit(Limit{Text: "   "}); added {
		t.Fatal("пустое ограничение")
	}
}

// Ограничение по потолку вытесняет самое старое, и это видно в журнале.
func TestApplyLimitOverflowReportsDrop(t *testing.T) {
	p := New("me", "")
	for i := 0; i < MaxLimits; i++ {
		p.AddLimit(Limit{Text: "правило " + string(rune('а'+i))})
	}
	user := "и ещё: без картинок"
	ch := Apply(&p, Patch{Limits: []LimitOp{{Text: "без картинок", Scope: ScopeAlways, Quote: "без картинок"}}}, user, 3)
	if len(ch) != 2 || ch[0].Op != OpLimit || ch[1].Op != OpDrop || ch[1].Value != "правило а" {
		t.Fatalf("вытеснение: %+v", ch)
	}
	// Повтор того же ограничения — не правка.
	if ch := Apply(&p, Patch{Limits: []LimitOp{{Text: "Без картинок", Quote: "без картинок"}}}, user, 4); len(ch) != 0 {
		t.Fatalf("повтор: %+v", ch)
	}
}

// Значение поля словами: вариант анкеты — его название, пусто — «не задано»,
// незнакомое — как есть.
func TestLabels(t *testing.T) {
	if got := Label(FieldLength, "short"); got == "short" || got == "" {
		t.Fatalf("вариант: %q", got)
	}
	if got := Label(FieldLength, ""); got != "не задано" {
		t.Fatalf("пусто: %q", got)
	}
	if got := Label(FieldLength, "как-нибудь"); got != "как-нибудь" {
		t.Fatalf("незнакомое значение: %q", got)
	}
	if got := Label("нет-такого-поля", "x"); got != "x" {
		t.Fatalf("незнакомое поле: %q", got)
	}
}

// Битая анкета — ошибка, а не пустая анкета поверх: иначе запись затёрла
// бы заполненное.
func TestStoreBrokenProfile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(store.NewDir(dir))
	if err := os.MkdirAll(filepath.Join(dir, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profiles", "me.json"), []byte("[1]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("me", ""); err == nil {
		t.Fatal("битая анкета прочитана")
	}
	if _, err := s.Update("me", "", func(*Profile) (bool, error) { return true, nil }); err == nil {
		t.Fatal("правка поверх битой анкеты")
	}
	list, problems := s.List()
	if len(list) != 0 || len(problems) != 1 {
		t.Fatalf("список: %d, проблем %d", len(list), len(problems))
	}
	if p := s.DisplayPath("me"); !strings.Contains(p, "profiles") || !strings.HasSuffix(p, "me.json") {
		t.Fatalf("путь: %q", p)
	}
}

// Ошибка функции правки доходит до вызывающего, файл не пишется.
func TestStoreUpdateError(t *testing.T) {
	s := NewStore(store.NewDir(t.TempDir()))
	boom := errors.New("нельзя")
	_, err := s.Update("me", "Я", func(p *Profile) (bool, error) {
		p.Set(FieldLength, Value{Value: "short"})
		return true, boom
	})
	if !errors.Is(err, boom) || s.Has("me") {
		t.Fatalf("ошибка правки: %v, файл: %v", err, s.Has("me"))
	}
	if _, err := s.Update("../x", "", func(*Profile) (bool, error) { return true, nil }); err == nil {
		t.Fatal("опасный идентификатор")
	}
}
