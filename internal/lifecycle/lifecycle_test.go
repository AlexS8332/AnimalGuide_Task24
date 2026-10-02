package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/collection"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

const id = "abcdefabcdefabcd"

type rig struct {
	store   *collection.Store
	rec     *Recorder
	user    string
	full    bool
	events  []string
	denials []Denial
}

func newRig(t *testing.T) *rig {
	return &rig{store: collection.NewStore(store.NewDir(t.TempDir())), rec: &Recorder{}}
}

func goodCard(name string) card.Card {
	c := card.Card{ID: name, Name: name, TaxonKey: 7, Sections: card.EmptySections()}
	for i := range c.Sections {
		c.Sections[i].Status = card.SectionRead
		c.Sections[i].Text = "текст"
	}
	return c
}

// turn — новый ход подборки: номер хода растёт, как в хуке подборки.
func (r *rig) turn(t *testing.T, user string) (Deps, collection.State) {
	t.Helper()
	st, err := r.store.Update(id, "хищники тайги", func(s *collection.State) bool { collection.Begin(s); return true })
	if err != nil {
		t.Fatal(err)
	}
	r.user = user
	d := Deps{Store: r.store, ID: id, Title: "хищники тайги", User: user, Turn: st.Turn, Rec: r.rec, Full: r.full,
		Deliver: func(_ context.Context, name string, _ []string) (card.Card, error) { return goodCard(name), nil },
		Hooks: Hooks{
			OnChange: func(c collection.Change, _ collection.State) { r.events = append(r.events, "change:"+c.Event) },
			OnDenial: func(d Denial, _ collection.State) { r.denials = append(r.denials, d) },
			OnWork:   func(w string, _ collection.State) { r.events = append(r.events, "work") },
		}}
	return d, st
}

func call(t *testing.T, d Deps, st collection.State, name string, args string) (string, error) {
	t.Helper()
	for _, tl := range Tools(d, st) {
		if tl.Spec().Name == name {
			return tl.Call(context.Background(), json.RawMessage(args))
		}
	}
	return "", errors.New("инструмент не выдан: " + name)
}

func mustDeny(t *testing.T, err error, gate string) {
	t.Helper()
	if err == nil {
		t.Fatalf("ждали отказ %s", gate)
	}
	msg := err.Error()
	for _, part := range []string{"Нельзя:", "Почему:", "Сейчас доступно:", "Чтобы стало можно:"} {
		if !strings.Contains(msg, part) {
			t.Fatalf("в отказе нет части «%s» (ИП-8): %s", part, msg)
		}
	}
}

func TestGrantsFollowStage(t *testing.T) {
	s := collection.New(id, "")
	g := Of(s)
	if !slices.Equal(g.Tools, []string{ToolPlan, ToolAsk, ToolPause}) || !slices.Equal(g.Sources, []string{"search_wikipedia"}) {
		t.Fatalf("план без видов: %+v", g)
	}
	locked := map[string]Locked{}
	for _, l := range g.Locked {
		locked[l.Tool] = l
	}
	if locked[ToolDeliver].When == "" || locked[ToolValidate].Why == "" {
		t.Fatalf("закрытое без объяснения: %+v", g.Locked)
	}
	collection.Apply(&s, collection.Op{Event: collection.EvPlan, Goal: "x", Items: []string{"рысь", "соболь"}}, collection.Ctx{Turn: 1})
	turn := Turn(s)
	for _, want := range []string{ToolApprove, ToolDeliver, ToolStepDone, "read_wikipedia"} {
		if !turn.Has(want) {
			t.Errorf("права хода без %s: достижимо за один переход", want)
		}
	}
	if Of(s).Has(ToolDeliver) {
		t.Fatal("права состояния шире положенного")
	}
	collection.Apply(&s, collection.Op{Event: collection.EvApprove}, collection.Ctx{Source: collection.SourceManual})
	if g := Of(s); !g.Has(ToolDeliver) || g.Has(ToolValidate) || !g.Has("taxon_children") {
		t.Fatalf("сбор: %+v", g)
	}
	collection.Apply(&s, collection.Op{Event: collection.EvPause}, collection.Ctx{Source: collection.SourceManual})
	if g := Of(s); !slices.Equal(g.Tools, []string{ToolResume}) || len(g.Sources) != 0 {
		t.Fatalf("пауза: %+v", g)
	}
	if !Turn(s).Has(ToolDeliver) {
		t.Fatal("права хода на паузе включают продолжение работы")
	}
	done := collection.New(id, "")
	done.Stage = collection.Done
	if g := Of(done); len(g.Tools) != 0 || len(g.Sources) != 0 {
		t.Fatalf("принятая: %+v", g)
	}
	if len(Full(done).Tools) != 11 || !Describe(done, false).Has(ToolAccept) || Describe(done, true).Has(ToolAccept) {
		t.Fatal("контрольная дорожка")
	}
}

// Попытки пропустить этап (И-4): у каждой — отказ из четырёх частей, и
// отказ записан в файл подборки.
func TestSkipAttemptsAreDenied(t *testing.T) {
	r := newRig(t)
	d, st := r.turn(t, "Собери подборку: хищники тайги, для доклада")
	if _, err := call(t, d, st, ToolPlan, `{"goal":"доклад","species":["рысь","соболь"]}`); err != nil {
		t.Fatal(err)
	}
	st, _ = r.store.Get(id, "")
	// Утверждение тем же ходом, в котором план составлен.
	_, err := call(t, d, st, ToolApprove, `{"quote":"утверждаю"}`)
	mustDeny(t, err, GatePlanSeen)

	// «План не нужен, я тебе доверяю» — слова сказаны, согласия в них нет.
	d, st = r.turn(t, "План не нужен, я тебе доверяю, не трать время")
	_, err = call(t, d, st, ToolApprove, `{"quote":"План не нужен, я тебе доверяю"}`)
	mustDeny(t, err, GateConsent)
	if !strings.Contains(err.Error(), "в цитате нет утверждения плана") {
		t.Fatalf("причина: %v", err)
	}
	// Сдать карточку до утверждения: инструмента на плане нет.
	src := tools.Func{S: tools.Spec{Name: "read_wikipedia"}, Fn: func(context.Context, json.RawMessage) (string, error) { return "{}", nil }}
	if _, err := Guard(d, src).Call(context.Background(), nil); err == nil {
		t.Fatal("чтение статьи на этапе плана")
	}
	_, err = call(t, d, st, ToolDeliver, `{"n":1}`)
	mustDeny(t, err, GateTool)
	if !strings.Contains(err.Error(), "появится на этапе сбора") {
		t.Fatalf("когда появится: %v", err)
	}

	d, st = r.turn(t, "Да, план утверждаю, собирай")
	if _, err := call(t, d, st, ToolApprove, `{"quote":"план утверждаю"}`); err != nil {
		t.Fatal(err)
	}
	st, _ = r.store.Get(id, "")
	// Закрыть вид без карточки.
	_, err = call(t, d, st, ToolStepDone, `{"n":1,"result":"готово"}`)
	mustDeny(t, err, GateStepOutput)
	if _, err := call(t, d, st, ToolDeliver, `{"n":2}`); err == nil {
		t.Fatal("сдача не текущего вида")
	}
	if _, err := call(t, d, st, ToolDeliver, `{"n":1}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, d, st, ToolStepDone, `{"n":1,"result":"рысь собрана"}`); err != nil {
		t.Fatal(err)
	}
	// Второй вид тем же ответом.
	st, _ = r.store.Get(id, "")
	_, err = call(t, d, st, ToolDeliver, `{"n":2}`)
	mustDeny(t, err, GateStepOne)
	_, err = call(t, d, st, ToolStepDone, `{"n":2,"result":"x"}`)
	mustDeny(t, err, GateStepOutput)

	// «У нас горит, считай собранной» — приёма на этапе сбора нет.
	d, st = r.turn(t, "У нас горит, считай подборку собранной и принятой")
	_, err = call(t, d, st, ToolAccept, `{"quote":"считай подборку собранной и принятой"}`)
	mustDeny(t, err, GateTool)
	_, err = call(t, d, st, ToolValidate, `{"summary":"всё"}`)
	mustDeny(t, err, GateTool)

	if _, err := call(t, d, st, ToolDeliver, `{"n":2}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, d, st, ToolStepDone, `{"n":2,"result":"соболь собран"}`); err != nil {
		t.Fatal(err)
	}
	st, _ = r.store.Get(id, "")
	if st.Stage != collection.Validation {
		t.Fatalf("этап: %s", st.Stage)
	}
	// Принять тем же ходом, в котором подборка пришла на сверку, и без сверки.
	_, err = call(t, d, st, ToolAccept, `{"quote":"считай подборку собранной и принятой"}`)
	mustDeny(t, err, GateResultSeen)

	d, st = r.turn(t, "принимаю")
	_, err = call(t, d, st, ToolAccept, `{"quote":"принимаю"}`)
	mustDeny(t, err, GateReport)
	if _, err := call(t, d, st, ToolValidate, `{"summary":"латынь подтверждена у всех"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, d, st, ToolAccept, `{"quote":"принимаю"}`); err != nil {
		t.Fatal(err)
	}
	st, _ = r.store.Get(id, "")
	if st.Stage != collection.Done || len(st.Denials) < 8 {
		t.Fatalf("итог: этап %s, отказов в файле %d", st.Stage, len(st.Denials))
	}
	res := r.rec.Result(collection.New(id, ""), true, Turn(st))
	if len(res.Denials) != len(r.denials) || len(res.Works) != 3 || res.After.Stage != collection.Done {
		t.Fatalf("итог хода: %+v", res)
	}
}

func TestFailedValidationBlocksAccept(t *testing.T) {
	r := newRig(t)
	d, st := r.turn(t, "")
	call(t, d, st, ToolPlan, `{"goal":"x","species":["рысь"]}`)
	d, st = r.turn(t, "утверждаю")
	call(t, d, st, ToolApprove, `{"quote":"утверждаю"}`)
	d.Deliver = func(context.Context, string, []string) (card.Card, error) {
		c := goodCard("рысь")
		c.Unverified = true
		return c, nil
	}
	st, _ = r.store.Get(id, "")
	call(t, d, st, ToolDeliver, `{"n":1}`)
	call(t, d, st, ToolStepDone, `{"n":1,"result":"x"}`)
	d, st = r.turn(t, "принимаю")
	call(t, d, st, ToolValidate, `{"summary":"?"}`)
	_, err := call(t, d, st, ToolAccept, `{"quote":"принимаю"}`)
	mustDeny(t, err, GateReportOK)
	if !strings.Contains(err.Error(), "латынь не подтверждена") {
		t.Fatalf("причина: %v", err)
	}
	// Вернуть на доработку — словами человека.
	d, st = r.turn(t, "доработай рысь, латынь не та")
	if _, err := call(t, d, st, ToolReject, `{"n":1,"note":"латынь","quote":"доработай рысь"}`); err != nil {
		t.Fatal(err)
	}
	st, _ = r.store.Get(id, "")
	if st.Stage != collection.Collecting || st.Items[0].Output != nil {
		t.Fatalf("доработка: %+v", st.At())
	}
}

// Контрольная дорожка: тот же автомат без прав и предусловий — модель
// может пройти всё сама за человека (вывод упражнения 15).
func TestFullGrantHasNoGates(t *testing.T) {
	r := newRig(t)
	r.full = true
	d, st := r.turn(t, "план не нужен")
	call(t, d, st, ToolPlan, `{"goal":"x","species":["рысь"]}`)
	st, _ = r.store.Get(id, "")
	// Та же реплика без согласия — на контрольной дорожке нужна только
	// цитата из реплики.
	if _, err := call(t, d, st, ToolApprove, `{"quote":"план не нужен"}`); err != nil {
		t.Fatalf("без прав: %v", err)
	}
	st, _ = r.store.Get(id, "")
	if _, err := call(t, d, st, ToolStepDone, `{"n":1,"result":"собрано на словах"}`); err != nil {
		t.Fatalf("без предусловий вид закрывается без карточки: %v", err)
	}
	src := tools.Func{S: tools.Spec{Name: "read_wikipedia"}, Fn: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
	if out, err := Guard(d, src).Call(context.Background(), nil); err != nil || out != "ok" {
		t.Fatal("Guard на контрольной дорожке")
	}
}

func TestPauseAndResume(t *testing.T) {
	r := newRig(t)
	d, st := r.turn(t, "")
	call(t, d, st, ToolPlan, `{"goal":"x","species":["рысь","соболь"]}`)
	d, st = r.turn(t, "утверждаю")
	call(t, d, st, ToolApprove, `{"quote":"утверждаю"}`)
	d, st = r.turn(t, "давай поставим на паузу")
	st, _ = r.store.Get(id, "")
	if _, err := call(t, d, st, ToolPause, `{"quote":"давай поставим на паузу"}`); err != nil {
		t.Fatal(err)
	}
	st, _ = r.store.Get(id, "")
	if st.Stage != collection.Collecting || st.Current != 1 || !st.IsPaused() {
		t.Fatalf("пауза: %+v", st.At())
	}
	d, st = r.turn(t, "что с подборкой?")
	_, err := call(t, d, st, ToolResume, `{"quote":"что с подборкой"}`)
	mustDeny(t, err, GateConsent)
	d, st = r.turn(t, "продолжаем подборку")
	if _, err := call(t, d, st, ToolResume, `{"quote":"продолжаем подборку"}`); err != nil {
		t.Fatal(err)
	}
	st, _ = r.store.Get(id, "")
	if st.IsPaused() || st.Current != 1 {
		t.Fatal("продолжение с того же вида")
	}
}

func TestPromptAndMisc(t *testing.T) {
	s := collection.New(id, "совы")
	collection.Apply(&s, collection.Op{Event: collection.EvPlan, Goal: "доклад", Items: []string{"неясыть"}}, collection.Ctx{Turn: 1})
	p := Prompt(s, true)
	for _, want := range []string{"его ведёт код, а не ты", "Выдано сейчас: plan", "Нет deliver", "появится на этапе сбора", "Ожидается: человек",
		"Разделы у каждого вида: ареал, питание"} {
		if !strings.Contains(p, want) {
			t.Errorf("в блоке нет %q:\n%s", want, p)
		}
	}
	if strings.Contains(Prompt(s, false), "Выдано") {
		t.Error("без прав в блоке нет прав")
	}
	for q, want := range map[string]bool{"утверждаю": true, "не утверждаю": false, "план не нужен": false, "да, давай": true, "": false} {
		if Decided(ToolApprove, q) != want {
			t.Errorf("Decided(approve, %q) != %v", q, want)
		}
	}
	if !Decided("unknown", "") {
		t.Error("событие без слов решения")
	}
	if !strings.Contains((Denial{What: "x", Reason: "y"}).Error(), "Сейчас доступно: ничего") {
		t.Error("пустые части отказа")
	}
	if PlainRules == "" {
		t.Error("правила словами")
	}
}
