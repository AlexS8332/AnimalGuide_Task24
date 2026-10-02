package collection

import (
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

func plan(t *testing.T, s *State, turn int) {
	t.Helper()
	ch := Apply(s, Op{Event: EvPlan, Goal: "доклад о хищниках тайги", Items: []string{"рысь", "соболь", " ", "Рысь", "росомаха"}}, Ctx{Turn: turn})
	if ch.Rejected {
		t.Fatalf("план: %s", ch.Reason)
	}
}

func lynx() card.Card {
	c := card.Card{ID: "1", Name: "Рысь", TaxonKey: 1, Sections: card.EmptySections()}
	for i := range c.Sections {
		if c.Sections[i].Key == "habitat" || c.Sections[i].Key == "diet" {
			c.Sections[i].Status = card.SectionRead
		}
	}
	return c
}

func TestHappyPathThroughStages(t *testing.T) {
	s := New("abcdefabcdefabcd", "хищники тайги")
	if e := s.Expected(); e.Actor != ActorAgent || !strings.Contains(e.Text, "составить план") {
		t.Fatalf("ожидание: %+v", e)
	}
	plan(t, &s, 1)
	if len(s.Items) != 3 || s.Sections[0] != "habitat" || s.Expected().Actor != ActorUser {
		t.Fatalf("план: %+v", s.Items)
	}
	if ch := Apply(&s, Op{Event: EvApprove, Quote: "план утверждаю"}, Ctx{User: "Да, план утверждаю", Turn: 2}); ch.Rejected {
		t.Fatalf("утверждение: %s", ch.Reason)
	}
	if s.Stage != Collecting || s.Current != 1 || s.Items[0].Status != ItemActive {
		t.Fatalf("сбор: %+v", s.At())
	}
	for i := 1; i <= 3; i++ {
		c := lynx()
		c.Name = s.CurrentItem().Name
		if err := Deliver(&s, i, c, 2+i); err != nil {
			t.Fatal(err)
		}
		if ch := Apply(&s, Op{Event: EvStepDone, Item: i, Result: "карточка собрана"}, Ctx{Turn: 2 + i}); ch.Rejected {
			t.Fatalf("вид %d: %s", i, ch.Reason)
		}
	}
	if s.Stage != Validation || s.CheckTurn != 5 {
		t.Fatalf("сверка: %+v", s.At())
	}
	r := Verify(s, "всё на месте", 6)
	if len(r.Failed()) != 0 || !r.Covers(3) {
		t.Fatalf("отчёт: %+v", r)
	}
	SetReport(&s, r)
	if ch := Apply(&s, Op{Event: EvAccept, Quote: "принимаю"}, Ctx{User: "принимаю подборку", Turn: 7}); ch.Rejected {
		t.Fatalf("приём: %s", ch.Reason)
	}
	if s.Stage != Done || s.Active() || s.Expected().Actor != ActorNone || len(s.Log) != 6 {
		t.Fatalf("итог: %+v, записей %d", s.At(), len(s.Log))
	}
	if ch := Apply(&s, Op{Event: EvReplan, Quote: "заново"}, Ctx{User: "заново"}); !ch.Rejected {
		t.Fatal("переход из «принята»")
	}
	md := Markdown(s)
	if !strings.Contains(md, "## 1. рысь") || !strings.Contains(md, "## Сверка") || !strings.Contains(md, "сошлось") {
		t.Fatalf("выгрузка:\n%s", md)
	}
}

func TestTableRejections(t *testing.T) {
	s := New("abcdefabcdefabcd", "")
	cases := []Op{
		{Event: "fly"},
		{Event: EvApprove, Quote: "да"},
		{Event: EvStepDone, Result: "x"},
		{Event: EvAccept, Quote: "да"},
		{Event: EvResume, Quote: "продолжим"},
		{Event: EvPlan}, // без цели и видов
		{Event: EvAsk},
	}
	for _, op := range cases {
		if ch := Apply(&s, op, Ctx{User: "да продолжим"}); !ch.Rejected || ch.Reason == "" {
			t.Errorf("%s принято с пустого листа: %+v", op.Event, ch)
		}
	}
	plan(t, &s, 1)
	if ch := Apply(&s, Op{Event: EvApprove}, Ctx{User: "да"}); !ch.Rejected || !strings.Contains(ch.Reason, "цитаты") {
		t.Fatalf("утверждение без цитаты: %+v", ch)
	}
	if ch := Apply(&s, Op{Event: EvApprove, Quote: "утверждаю"}, Ctx{User: "поехали дальше"}); !ch.Rejected {
		t.Fatal("цитата не из реплики")
	}
	if ch := Apply(&s, Op{Event: EvApprove}, Ctx{Source: SourceManual}); ch.Rejected {
		t.Fatalf("кнопка без цитаты: %s", ch.Reason)
	}
	if ch := Apply(&s, Op{Event: EvStepDone, Item: 2, Result: "x"}, Ctx{}); !ch.Rejected || !strings.Contains(ch.Reason, "не текущий") {
		t.Fatalf("вид не по порядку: %+v", ch)
	}
	if ch := Apply(&s, Op{Event: EvStepDone}, Ctx{}); !ch.Rejected {
		t.Fatal("вид без итога")
	}
	many := make([]string, MaxItems+1)
	for i := range many {
		many[i] = string(rune('а' + i))
	}
	if ch := Apply(&s, Op{Event: EvPlan, Items: many}, Ctx{}); ch.Rejected {
		// План правят только на этапе плана — здесь этап уже сбор.
	} else {
		t.Fatal("план на этапе сбора")
	}
	fresh := New("abcdefabcdefabce", "")
	if ch := Apply(&fresh, Op{Event: EvPlan, Goal: "x", Items: many}, Ctx{}); !ch.Rejected {
		t.Fatal("потолок видов")
	}
}

func TestPauseKeepsStageAndItem(t *testing.T) {
	s := New("abcdefabcdefabcd", "")
	plan(t, &s, 1)
	Apply(&s, Op{Event: EvApprove, Quote: "утверждаю"}, Ctx{User: "утверждаю", Turn: 2})
	if ch := Apply(&s, Op{Event: EvPause, Quote: "давай паузу"}, Ctx{User: "давай паузу", Turn: 3}); ch.Rejected {
		t.Fatal(ch.Reason)
	}
	if s.Stage != Collecting || s.Current != 1 || !s.IsPaused() || !strings.Contains(s.Summary(), "на паузе") {
		t.Fatalf("пауза: %+v", s.At())
	}
	if ch := Apply(&s, Op{Event: EvStepDone, Result: "x"}, Ctx{}); !ch.Rejected || !strings.Contains(ch.Reason, "паузе") {
		t.Fatal("работа на паузе")
	}
	if s.Expected().Actor != ActorUser {
		t.Fatal("на паузе ждём человека")
	}
	if ch := Apply(&s, Op{Event: EvResume, Quote: "продолжаем"}, Ctx{User: "продолжаем", Turn: 4}); ch.Rejected || s.IsPaused() {
		t.Fatal("продолжение")
	}
	if ch := Apply(&s, Op{Event: EvResume, Quote: "продолжаем"}, Ctx{User: "продолжаем"}); !ch.Rejected {
		t.Fatal("продолжение не с паузы")
	}
}

func TestVerifyAndRejectLoop(t *testing.T) {
	s := New("abcdefabcdefabcd", "")
	plan(t, &s, 1)
	Apply(&s, Op{Event: EvApprove}, Ctx{Source: SourceManual})
	bad := lynx()
	bad.Unverified = true
	bad.Sections = card.EmptySections()
	Deliver(&s, 1, bad, 3)
	Apply(&s, Op{Event: EvStepDone, Result: "x"}, Ctx{Turn: 3})
	Deliver(&s, 0, lynx(), 4)
	Apply(&s, Op{Event: EvStepDone, Result: "x"}, Ctx{Turn: 4})
	Apply(&s, Op{Event: EvStepDone, Result: "без карточки"}, Ctx{Turn: 5}) // автомат пускает; предусловие — в lifecycle
	r := Verify(s, "", 6)
	f := r.Failed()
	if len(f) != 2 || f[0].N != 1 || len(f[0].Issues) != 3 || f[1].N != 3 || f[1].Issues[0] != "карточка не сдана" {
		t.Fatalf("несошедшиеся: %+v", f)
	}
	SetReport(&s, r)
	if ch := Apply(&s, Op{Event: EvReject, Quote: "доработай рысь"}, Ctx{User: "доработай рысь"}); !ch.Rejected || !strings.Contains(ch.Reason, "замечание") {
		t.Fatalf("возврат без замечания: %+v", ch)
	}
	if ch := Apply(&s, Op{Event: EvReject, Note: "латынь", Quote: "доработай рысь"}, Ctx{User: "доработай рысь", Turn: 7}); ch.Rejected {
		t.Fatal(ch.Reason)
	}
	if s.Stage != Collecting || s.Current != 1 || s.Items[0].Output != nil || s.Report != nil || s.Feedback != "латынь" {
		t.Fatalf("возврат: %+v", s.At())
	}
	if err := Deliver(&s, 2, lynx(), 8); err == nil {
		t.Fatal("сдача не текущего вида")
	}
	// Пересмотр плана.
	if ch := Apply(&s, Op{Event: EvReplan, Quote: "поменяем план"}, Ctx{User: "поменяем план", Turn: 9}); ch.Rejected || s.Stage != Planning {
		t.Fatal("пересмотр плана")
	}
}

func TestAskAndEndTurn(t *testing.T) {
	s := New("abcdefabcdefabcd", "")
	if ch := Apply(&s, Op{Event: EvAsk, Question: "Сколько видов?"}, Ctx{Turn: 1}); ch.Rejected {
		t.Fatal(ch.Reason)
	}
	if !strings.Contains(s.Expected().Text, "Сколько видов?") {
		t.Fatal("вопрос в ожидании")
	}
	if EndTurn(&s, 1) {
		t.Fatal("вопрос закрыт тем же ходом")
	}
	if !EndTurn(&s, 2) || s.Question != "" {
		t.Fatal("вопрос не закрыт следующим ходом")
	}
}

func TestStoreAndHelpers(t *testing.T) {
	st := NewStore(store.NewDir(t.TempDir()))
	id := "abcdefabcdefabcd"
	if st.Has(id) {
		t.Fatal("пустое хранилище")
	}
	got, err := st.Update(id, "совы", func(s *State) bool {
		plan(t, s, 1)
		return true
	})
	if err != nil || len(got.Items) != 3 || !st.Has(id) {
		t.Fatal(err)
	}
	again, _ := st.Get(id, "")
	if again.Title != "совы" || len(again.Items) != 3 {
		t.Fatalf("прочитано: %+v", again)
	}
	st.Update("abcdefabcdefabce", "второе", func(s *State) bool { s.touch(time.Now()); return true })
	list, problems := st.List()
	if len(list) != 2 || len(problems) != 0 {
		t.Fatalf("List: %d %v", len(list), problems)
	}
	path, raw, err := st.Raw(id)
	if err != nil || !strings.Contains(path, "collections") || !strings.Contains(string(raw), `"schema": 1`) || st.DisplayPath(id) == "" {
		t.Fatal("Raw")
	}
	if _, err := st.Update("bad id", "", func(*State) bool { return true }); err == nil {
		t.Fatal("плохой идентификатор")
	}
	for _, s := range Stages {
		if !s.Valid() || s.Title() == string(s) {
			t.Errorf("этап %s", s)
		}
	}
	if Stage("x").Valid() || ActorTitle("x") != "никто" || EventTitle("x") != "x" {
		t.Fatal("мелочи")
	}
	c := Change{Event: EvPlan, From: Point{Stage: Planning}, To: Point{Stage: Planning}}
	if !strings.Contains(c.Title(), "план подборки") {
		t.Fatal(c.Title())
	}
	s := New("x", "")
	s.Deny(Denial{What: "x"})
	if s.Denials[0].Stage != Planning || s.Clone().Denials[0].What != "x" {
		t.Fatal("Deny")
	}
	if (Point{Stage: Collecting, Item: 2, Paused: true}).String() != "сбор, вид 2, пауза" {
		t.Fatal("Point")
	}
}
