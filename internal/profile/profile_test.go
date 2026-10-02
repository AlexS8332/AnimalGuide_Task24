package profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

func TestFieldsAndNormalize(t *testing.T) {
	if len(Fields()) != 7 {
		t.Fatalf("полей %d", len(Fields()))
	}
	for _, f := range Fields() {
		for _, o := range f.Options {
			if o.Directive == "" || o.Title == "" {
				t.Errorf("%s=%s без строки промпта", f.Key, o.Value)
			}
		}
	}
	cases := map[[2]string]string{
		{"length", "коротко"}: "short", {"длина ответа", "SHORT"}: "short", {"address", "на «ты»"}: "ty",
		{"latin", "скрывать"}: "hide", {"level", "специалист"}: "expert",
	}
	for in, want := range cases {
		if got, ok := Normalize(in[0], in[1]); !ok || got != want {
			t.Errorf("Normalize(%v) = %q", in, got)
		}
	}
	if _, ok := Normalize("length", "средне"); ok {
		t.Error("значение не из перечня принято")
	}
	if _, ok := Normalize("цвет", "красный"); ok {
		t.Error("несуществующее поле принято")
	}
	for _, k := range []string{"длина", "обращение", "латынь в ответах", "эмодзи", "level", "ограничения"} {
		if !Reserved(k) {
			t.Errorf("ключ %q не зарезервирован анкетой", k)
		}
	}
	if Reserved("интерес") {
		t.Error("свободный ключ зарезервирован")
	}
}

func TestApplyAlwaysNeedsQuoteFromCurrentReply(t *testing.T) {
	p := New("me", "")
	user := "Пиши мне всегда коротко и на ты, пожалуйста"
	ch := Apply(&p, Patch{Set: []SetOp{
		{Field: "length", Value: "short", Scope: ScopeAlways, Quote: "пиши мне всегда коротко"},
		{Field: "address", Value: "ty", Scope: ScopeAlways, Quote: "обращайся на вы"}, // не из реплики
		{Field: "form", Value: "list", Scope: ScopeAlways},                            // без цитаты
		{Field: "length", Value: "средне", Scope: ScopeAlways, Quote: "коротко"},      // не из перечня
		{Field: "цвет", Value: "x", Quote: "коротко"},
	}}, user, 3)
	if p.Val(FieldLength) != "short" || p.Val(FieldAddress) != "" || p.Val(FieldForm) != "" {
		t.Fatalf("анкета: %+v", p.Values)
	}
	if len(ch) != 5 || len(ch.Applied()) != 1 {
		t.Fatalf("правки: %+v", ch)
	}
	for _, c := range ch[1:] {
		if c.Op != OpReject || c.Reason == "" {
			t.Errorf("отказ без причины: %+v", c)
		}
	}
	if !strings.Contains(ch[1].Reason, "текущей реплике") || !strings.Contains(ch[2].Reason, "нет цитаты") {
		t.Fatalf("причины: %q / %q", ch[1].Reason, ch[2].Reason)
	}
	// Повтор того же — не правка.
	if again := Apply(&p, Patch{Set: []SetOp{{Field: "length", Value: "short", Scope: ScopeAlways, Quote: "коротко"}}}, "коротко", 4); len(again) != 0 {
		t.Fatalf("повтор: %+v", again)
	}
}

// Разовая просьба профиль не меняет, повторённая — закрепляется (ФТ-22).
func TestOnceThenRepeatedIsPromoted(t *testing.T) {
	p := New("me", "")
	op := SetOp{Field: "length", Value: "tiny", Scope: ScopeOnce, Quote: "ответь в двух словах"}
	ch := Apply(&p, Patch{Set: []SetOp{op}}, "ответь в двух словах", 1)
	if p.Val(FieldLength) != "" || len(ch) != 1 || ch[0].Op != OpOnce || len(ch.Once()) != 1 {
		t.Fatalf("разовая: %+v, анкета %+v", ch, p.Values)
	}
	if with := p.With(ch.Once()); with.Val(FieldLength) != "tiny" || p.Val(FieldLength) != "" {
		t.Fatal("разовая правка должна действовать на ход, не меняя анкету")
	}
	ch = Apply(&p, Patch{Set: []SetOp{op}}, "опять ответь в двух словах", 2)
	if p.Val(FieldLength) != "tiny" || ch[0].Op != OpSet || !strings.Contains(ch[0].Reason, "закреплено") {
		t.Fatalf("повторённая: %+v", ch)
	}
}

func TestLimits(t *testing.T) {
	p := New("me", "")
	ch := Apply(&p, Patch{Limits: []LimitOp{
		{Text: "не рассказывай про охоту", Scope: ScopeAlways, Quote: "не рассказывай мне про охоту"},
		{Text: "без картинок", Scope: ScopeOnce, Quote: "без картинок"},
		{Text: "x", Scope: ScopeAlways},
		{Text: "  ", Quote: "x"},
	}}, "Не рассказывай мне про охоту, и без картинок сегодня", 1)
	if len(p.Limits) != 1 || len(ch) != 3 || ch[1].Op != OpReject || ch[2].Op != OpReject {
		t.Fatalf("ограничения: %+v", ch)
	}
	for i := range MaxLimits + 1 {
		p.AddLimit(Limit{Text: "правило " + string(rune('а'+i))})
	}
	if len(p.Limits) != MaxLimits {
		t.Fatalf("потолок: %d", len(p.Limits))
	}
	ch = Apply(&p, Patch{Limits: []LimitOp{{Text: "правило б", Drop: true, Quote: "правило б больше не нужно"}}}, "правило б больше не нужно", 2)
	if len(ch) != 1 || ch[0].Op != OpUnlimit {
		t.Fatalf("снятие: %+v", ch)
	}
	if added, _ := p.AddLimit(Limit{Text: "Правило в"}); added {
		t.Fatal("повтор ограничения")
	}
}

func TestPromptAndSummary(t *testing.T) {
	p := New("me", "Саша")
	if p.Prompt() != "" || p.Summary() != "профиль пуст" {
		t.Fatal("пустая анкета даёт блок")
	}
	pr, _ := PresetOf("child")
	p = pr.Build("kid", "Маша")
	prompt := p.Prompt()
	if !strings.HasPrefix(prompt, "Как разговаривать с этим человеком") || !strings.Contains(prompt, "как ребёнку") ||
		!strings.Contains(prompt, "Латинских названий в ответе не пиши") {
		t.Fatalf("блок профиля:\n%s", prompt)
	}
	p.AddLimit(Limit{Text: "не пугай"})
	if !strings.Contains(p.Prompt(), "не пугай") || !strings.Contains(p.Summary(), "ребёнок 7–10 лет") {
		t.Fatal("ограничения в блоке и сводке")
	}
	if !strings.Contains(p.Describe(), "уровень изложения (level): ребёнок 7–10 лет") {
		t.Fatalf("Describe:\n%s", p.Describe())
	}
	if _, ok := PresetOf("nope"); ok {
		t.Fatal("PresetOf")
	}
	if Label("length", "short") != "коротко" || Label("x", "y") != "y" || labelOf(Field{}, "") != "не задано" {
		t.Fatal("Label")
	}
	if !p.Clear(FieldEmoji) || p.Clear(FieldEmoji) || p.Filled() != 6 {
		t.Fatal("Clear")
	}
	c := p.Clone()
	c.Values[FieldLevel] = Value{Value: "expert"}
	if p.Val(FieldLevel) != "child" {
		t.Fatal("клон делит значения")
	}
}

func TestChecks(t *testing.T) {
	pr, _ := PresetOf("child")
	kid := pr.Build("kid", "")
	ok := "Рысь — большая лесная кошка. Ты бы узнал её по кисточкам на ушах 🐾."
	checks := Checks(kid, ok)
	good, total := Rate(checks)
	if total == 0 || good != total {
		t.Fatalf("ответ ребёнку по правилам: %s", Note(checks))
	}
	bad := "## Рысь\n- Lynx lynx — вид семейства Felidae.\n- Вы можете встретить её в тайге."
	checks = Checks(kid, bad)
	good, total = Rate(checks)
	if good == total {
		t.Fatalf("нарушения не найдены: %s", Note(checks))
	}
	byField := map[string]Check{}
	for _, c := range checks {
		byField[c.Field] = c
	}
	if byField[FieldLatin].OK || byField[FieldForm].OK || byField[FieldAddress].OK {
		t.Fatalf("латынь/форма/обращение: %+v", byField)
	}
	pr, _ = PresetOf("expert")
	expert := pr.Build("x", "")
	// Без местоимений обращение не определить — в знаменатель не идёт.
	for _, c := range Checks(expert, "Lynx lynx (Linnaeus, 1758) — вид рода Lynx.") {
		if c.Field == FieldAddress && !c.NA {
			t.Fatal("обращение без местоимений определено")
		}
	}
	if Checks(expert, "  ") != nil || Note(nil) != "проверить нечего" {
		t.Fatal("пустой ответ")
	}
}

func TestLengthAndFormDetectors(t *testing.T) {
	// Ссылка вырезается целиком вместе с точкой: латынь в адресе — не латынь
	// ответа, а предложений остаётся три.
	m := Measure("Первое. Второе! Третье? Ссылка https://example.com/Lynx.")
	if m.Sentences != 3 || m.Latin != 0 {
		t.Fatalf("Measure: %+v", m)
	}
	long := strings.Repeat("слово ", 120) + "."
	cases := []struct {
		field, value, text string
		ok, na             bool
	}{
		{FieldLength, "tiny", "Коротко.", true, false},
		{FieldLength, "short", long, false, false},
		{FieldLength, "long", long, true, false},
		{FieldLength, "normal", long, true, false},
		{FieldForm, "list", "- а\n- б", true, false},
		{FieldForm, "prose", "| а | б |\n|---|---|", false, false},
		{FieldForm, "table", "текст", false, true},
		{FieldForm, "table", "| а | б |", true, false},
		{FieldLatin, "caption", "Рысь (Lynx lynx).", true, false},
		{FieldLatin, "caption", "Рысь.", false, true},
		{FieldLatin, "full", "Рысь.", false, true},
		{FieldLatin, "full", "Lynx lynx.", true, false},
		{FieldEmoji, "no", "Рысь 🐾.", false, false},
		{FieldEmoji, "some", "Рысь.", false, true},
		{FieldEmoji, "some", "Рысь 🐾.", true, false},
		{FieldAddress, "vy", "Ты и вы.", false, false},
	}
	for _, c := range cases {
		p := New("x", "")
		p.Set(c.field, Value{Value: c.value})
		got := Checks(p, c.text)
		if len(got) != 1 || got[0].OK != c.ok || got[0].NA != c.na {
			t.Errorf("%s=%s на %.30q: %+v", c.field, c.value, c.text, got)
		}
	}
	if plural(21, "а", "б", "в") != "21 а" || plural(3, "а", "б", "в") != "3 б" || plural(11, "а", "б", "в") != "11 в" {
		t.Error("plural")
	}
}

func TestChangeStrings(t *testing.T) {
	cs := Changes{
		{Op: OpSet, Title: "длина ответа", Label: "коротко", From: "обычно"},
		{Op: OpSet, Title: "длина ответа", Label: "коротко"},
		{Op: OpOnce, Title: "форма", Label: "списком"},
		{Op: OpLimit, Value: "a"}, {Op: OpUnlimit, Value: "b"}, {Op: OpDrop, Value: "c"},
		{Op: OpReject, Title: "t", Value: "v", Reason: "r"}, {Op: OpReject, Value: "v", Reason: "r"},
	}
	s := cs.Summary()
	for _, want := range []string{"вместо «обычно»", "разово", "в ограничения", "снято", "вытеснено", "отклонено: «t = v»"} {
		if !strings.Contains(s, want) {
			t.Errorf("нет %q в %s", want, s)
		}
	}
	if (Changes{}).Summary() != "профиль не изменился" {
		t.Error("пустая сводка")
	}
}

func TestStore(t *testing.T) {
	s := NewStore(store.NewDir(t.TempDir()))
	p, err := s.Get("me", "Я")
	if err != nil || !p.Empty() || p.Title != "Я" || s.Has("me") {
		t.Fatal("пустая анкета")
	}
	p, err = s.Update("me", "Я", func(p *Profile) (bool, error) {
		return p.Set(FieldLength, Value{Value: "short"}), nil
	})
	if err != nil || !s.Has("me") || p.Version != 1 {
		t.Fatalf("Update: %v", err)
	}
	if _, err := s.Update("me", "", func(*Profile) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	pr, _ := PresetOf("expert")
	if err := s.Save(pr.Build("x", "")); err != nil {
		t.Fatal(err)
	}
	list, problems := s.List()
	if len(list) != 2 || len(problems) != 0 {
		t.Fatalf("List: %d %v", len(list), problems)
	}
	path, raw, err := s.Raw("me")
	if err != nil || !strings.Contains(path, "profiles") || !strings.Contains(string(raw), `"schema": 2`) {
		t.Fatalf("Raw: %v", err)
	}
	if err := s.Delete("me"); err != nil || s.Has("me") {
		t.Fatal("Delete")
	}
}

// Памятник анкеты упражнения 12 поднимается до анкеты справочника: смысл
// значений переведён, то, чему места нет, лежит в legacy (ФТ-52).
func TestLegacyProfile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "legacy", "profiles", "v1-report-manager.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Values map[string]struct{ Value string }
		Limits []struct{ Text string }
	}
	json.Unmarshal(data, &raw)
	var p Profile
	meta, err := store.Decode(Kind, data, &p)
	if err != nil || meta.Schema != 1 || !meta.Migrated {
		t.Fatalf("миграция: %+v %v", meta, err)
	}
	if p.Val(FieldAddress) != "ty" || p.Val(FieldLength) != "long" || p.Val(FieldEmoji) != "some" || p.Val(FieldLevel) != "amateur" {
		t.Fatalf("значения: %+v", p.Values)
	}
	if raw.Values["shape"].Value != "" && p.Val(FieldForm) != raw.Values["shape"].Value {
		t.Fatalf("форма: %q", p.Val(FieldForm))
	}
	if p.Values[FieldAddress].Quote == "" {
		t.Fatal("цитата правки потерялась")
	}
	for _, k := range []string{"name", "role", "code", "examples"} {
		if _, ok := raw.Values[k]; ok {
			if _, kept := p.Legacy[k]; !kept {
				t.Errorf("поле %s потерялось", k)
			}
		}
	}
	if len(p.Limits) != len(raw.Limits) {
		t.Fatalf("ограничения: %d из %d", len(p.Limits), len(raw.Limits))
	}
	// Прежние поля в запрос не уходят.
	if strings.Contains(p.Prompt(), "Марина") {
		t.Fatal("поле прежней анкеты попало в блок профиля")
	}
}

func TestMigrateAsks(t *testing.T) {
	var p Profile
	doc := `{"id":"x","values":{},"asks":{"length=medium":2,"code=no":1}}`
	if _, err := store.Decode(Kind, []byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	if p.Asks["length=normal"] != 2 || p.Legacy["asks:code=no"] == nil {
		t.Fatalf("счётчики: %+v %v", p.Asks, p.Legacy)
	}
}
