package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/card"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/collection"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// collecting — подборка на этапе сбора с двумя видами.
func (r *rig) collecting(t *testing.T) (Deps, collection.State) {
	t.Helper()
	d, st := r.turn(t, "")
	if _, err := call(t, d, st, ToolPlan, `{"goal":"доклад","species":["рысь","соболь"]}`); err != nil {
		t.Fatal(err)
	}
	d, st = r.turn(t, "утверждаю")
	st, _ = r.store.Get(id, "")
	if _, err := call(t, d, st, ToolApprove, `{"quote":"утверждаю"}`); err != nil {
		t.Fatal(err)
	}
	d, _ = r.turn(t, "собирай")
	st, _ = r.store.Get(id, "")
	return d, st
}

// Страж источника: инструмент, которого на этапе нет, не вызывается вовсе
// (права этапа на момент вызова, ФТ-27).
func TestGuardDeniesSourceOutOfStage(t *testing.T) {
	r := newRig(t)
	d, _ := r.turn(t, "собери подборку")
	calls := 0
	src := func(name string) tools.Tool {
		return tools.Func{S: tools.Spec{Name: name}, Fn: func(context.Context, json.RawMessage) (string, error) {
			calls++
			return "ok", nil
		}}
	}
	// На плане без видов разрешён только поиск.
	if out, err := Guard(d, src("search_wikipedia")).Call(context.Background(), nil); err != nil || out != "ok" {
		t.Fatalf("поиск на плане: %q %v", out, err)
	}
	_, err := Guard(d, src("taxon_children")).Call(context.Background(), nil)
	mustDeny(t, err, GateStage)
	if calls != 1 || len(r.denials) != 1 || r.denials[0].What != "taxon_children" {
		t.Fatalf("соседи таксона на плане: вызовов %d, отказы %+v", calls, r.denials)
	}
	st, _ := r.store.Get(id, "")
	if len(st.Denials) == 0 || st.Denials[len(st.Denials)-1].What != "taxon_children" {
		t.Fatalf("отказ не записан в подборку: %+v", st.Denials)
	}
}

// На сборе источники открыты: страж пропускает вызов.
func TestGuardAllowsSourceWhenStageGrants(t *testing.T) {
	r := newRig(t)
	d, _ := r.collecting(t)
	src := tools.Func{S: tools.Spec{Name: "taxon_children"}, Fn: func(context.Context, json.RawMessage) (string, error) { return "дети", nil }}
	if out, err := Guard(d, src).Call(context.Background(), nil); err != nil || out != "дети" {
		t.Fatalf("соседи на сборе: %q %v", out, err)
	}
}

// Битый файл подборки: страж не пускает к источнику и не затирает файл.
func TestGuardBrokenStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "collections", id+".json")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("{битый"), 0o600)
	d := Deps{Store: collection.NewStore(store.NewDir(dir)), ID: id}
	src := tools.Func{S: tools.Spec{Name: "search_wikipedia"}, Fn: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
	if _, err := Guard(d, src).Call(context.Background(), nil); err == nil {
		t.Fatal("вызов при битом файле подборки")
	}
}

// Сдача карточки: сборка не подключена, сломалась, не тот вид — ошибки, и
// подборка не меняется.
func TestDeliverErrors(t *testing.T) {
	r := newRig(t)
	d, st := r.collecting(t)

	noDeliver := d
	noDeliver.Deliver = nil
	if _, err := call(t, noDeliver, st, ToolDeliver, `{"n":1}`); err == nil || !strings.Contains(err.Error(), "не подключена") {
		t.Fatalf("без сборки: %v", err)
	}
	broken := d
	broken.Deliver = func(context.Context, string, []string) (card.Card, error) {
		return card.Card{}, errors.New("GBIF молчит")
	}
	if _, err := call(t, broken, st, ToolDeliver, `{"n":1}`); err == nil || !strings.Contains(err.Error(), "«рысь» не собрана") {
		t.Fatalf("сборка сломалась: %v", err)
	}
	_, err := call(t, d, st, ToolDeliver, `{"n":2}`)
	mustDeny(t, err, GateStage)
	if last := r.denials[len(r.denials)-1]; !strings.Contains(last.Reason, "не текущий") || last.Hint != "сдай вид 1" {
		t.Fatalf("не тот вид: %+v", last)
	}
	if _, err := call(t, d, st, ToolDeliver, `{"n":"один"}`); err == nil {
		t.Fatal("битые аргументы")
	}
	after, _ := r.store.Get(id, "")
	if after.Items[0].Output != nil {
		t.Fatal("карточка легла в подборку после ошибок")
	}
	// Номер можно не называть: сдаётся текущий вид.
	out, err := call(t, d, st, ToolDeliver, `{}`)
	if err != nil || !strings.Contains(out, `"item":1`) {
		t.Fatalf("сдача без номера: %q %v", out, err)
	}
}

// Битые аргументы переходов — ошибка разбора, а не переход с пустыми
// полями.
func TestTransitionBadArgs(t *testing.T) {
	r := newRig(t)
	d, st := r.turn(t, "утверждаю")
	for _, name := range []string{ToolPlan, ToolAsk, ToolApprove, ToolPause} {
		if _, err := call(t, d, st, name, `{"goal":`); err == nil {
			t.Errorf("%s: битые аргументы приняты", name)
		}
	}
	after, _ := r.store.Get(id, "")
	if after.Stage != collection.Planning || len(after.Items) != 0 || len(after.Log) != 0 {
		t.Fatalf("подборка изменилась: %+v", after.At())
	}
	d2, _ := r.collecting(t)
	for _, name := range []string{ToolStepDone, ToolValidate} {
		if _, err := direct(d2, name, `[`); err == nil {
			t.Errorf("%s: битые аргументы приняты", name)
		}
	}
}

// direct — вызов инструмента по имени в обход прав хода (для инструментов,
// которых на этом ходе нет).
func direct(d Deps, name, args string) (string, error) {
	t := d.toolByName(name)
	if t == nil {
		return "", errors.New("нет инструмента " + name)
	}
	return t.Call(context.Background(), json.RawMessage(args))
}

// Незнакомое имя инструмента — nil, а не пустой инструмент.
func TestToolByNameUnknown(t *testing.T) {
	if (Deps{}).toolByName("нет-такого") != nil {
		t.Fatal("инструмент по незнакомому имени")
	}
	for _, name := range Full(collection.New(id, "")).Tools {
		if (Deps{}).toolByName(name) == nil {
			t.Errorf("инструмент %s не собирается", name)
		}
	}
}

// Подсказка этапа: принятой подборке подсказывать нечего.
func TestStageHint(t *testing.T) {
	done := collection.New(id, "")
	done.Stage = collection.Done
	if stageHint(done) != "" {
		t.Fatalf("подсказка принятой подборке: %q", stageHint(done))
	}
	if got := stageHint(collection.New(id, "")); !strings.HasPrefix(got, "сейчас ожидается: справочник") {
		t.Fatalf("подсказка плана: %q", got)
	}
}

// Отрицание рядом со словом решения отменяет его, дальнее — нет; «ё» и
// знаки препинания не мешают.
func TestDecidedNegationWindow(t *testing.T) {
	cases := map[string]bool{
		"Не, не согласен":                       false,
		"нет, не годится":                       false,
		"не знаю... а впрочем, согласен":        true,
		"ПРИНИМАЮ!":                             true,
		"без согласия":                          false,
		"я не уверен, но в целом годится, да":   true,
		"не принимаю, не принимаю и не приму":   false,
		"не нравится вначале, потом — принимаю": true,
	}
	for q, want := range cases {
		if got := Decided(ToolAccept, q); got != want {
			t.Errorf("Decided(accept, %q) = %v", q, got)
		}
	}
	if !Decided(ToolResume, "ну что, продолжаем?") || Decided(ToolPause, "продолжаем") {
		t.Fatal("пауза и продолжение")
	}
}
