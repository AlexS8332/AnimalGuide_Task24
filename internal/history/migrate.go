package history

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

// migrate1to2 поднимает диалог прежних упражнений до версии 2. Поля
// schema у предков нет, поэтому оба — «версия 1», но форма разная:
//
//   - есть branches — дерево веток (упражнение 10): добавить собеседника,
//     перевести стратегию в набор механизмов;
//   - есть messages на верхнем уровне — плоский диалог (упражнения 11–15):
//     обернуть в одну корневую ветку, флаги дорожек (preset, lane, noRules,
//     noGates, noState) перевести в набор механизмов, задачу — в подборку.
//
// Ничего не выбрасывается молча: то, чему в новом формате нет места
// (итоги механизмов прежнего формата, разбивка контекста), уходит в
// Extras хода под ключами legacy.* — данные не теряются (ФТ-52).
func migrate1to2(doc map[string]json.RawMessage) error {
	if _, ok := doc["branches"]; ok {
		return migrateTree(doc)
	}
	if _, ok := doc["messages"]; ok {
		return migrateFlat(doc)
	}
	return fmt.Errorf("ни веток, ни сообщений: неизвестная форма диалога")
}

// set — полная карта механизмов: всё, чего в прежнем упражнении не было,
// выключено. Диалог продолжается по тем правилам, с которыми его завели.
func legacySet(on map[features.Name]bool) features.Set {
	return features.Catalog().Complete(features.NewSet(on))
}

func migrateTree(doc map[string]json.RawMessage) error {
	var strategy string
	store.Get(doc, "strategy", &strategy)
	delete(doc, "strategy")
	on := map[features.Name]bool{features.Compact: true}
	switch strategy {
	case "window":
		on[features.Window] = true
	case "facts":
		on[features.Window], on[features.Facts] = true, true
	}
	if err := store.Set(doc, "features", legacySet(on)); err != nil {
		return err
	}
	if _, ok := doc["owners"]; !ok {
		if err := store.Set(doc, "owners", []string{DefaultOwner}); err != nil {
			return err
		}
	}

	var branches []map[string]json.RawMessage
	if _, err := store.Get(doc, "branches", &branches); err != nil {
		return fmt.Errorf("ветки: %w", err)
	}
	// Расход карточки фактов жил в каждой ветке нарастающим итогом; в новом
	// формате он — расход обвязки диалога. Берётся наибольший по ветке:
	// ветки наследуют расход родителя вместе с карточкой.
	var meter Meter
	for _, b := range branches {
		var f struct {
			Usage   json.RawMessage `json:"usage"`
			Cost    json.RawMessage `json:"cost"`
			Seconds float64         `json:"seconds"`
			Calls   int             `json:"calls"`
		}
		if raw, ok := b["facts"]; ok && json.Unmarshal(raw, &f) == nil && f.Calls > meter.Calls {
			meter.Calls, meter.Seconds = f.Calls, f.Seconds
			json.Unmarshal(f.Usage, &meter.Usage)
			json.Unmarshal(f.Cost, &meter.Cost)
		}
		if err := legacyTurns(b); err != nil {
			return err
		}
	}
	if err := store.Set(doc, "branches", branches); err != nil {
		return err
	}
	return store.Set(doc, "meter", meter)
}

func migrateFlat(doc map[string]json.RawMessage) error {
	var created time.Time
	store.Get(doc, "created", &created)
	var agentKey, preset, lane string
	var noRules, noGates, noState bool
	store.Get(doc, "agent", &agentKey)
	store.Get(doc, "preset", &preset)
	store.Get(doc, "lane", &lane)
	store.Get(doc, "noRules", &noRules)
	store.Get(doc, "noGates", &noGates)
	store.Get(doc, "noState", &noState)
	for _, k := range []string{"preset", "lane", "noRules", "noGates", "noState"} {
		delete(doc, k)
	}
	executor := agentKey == "executor"
	on := map[features.Name]bool{
		features.Compact: true, features.Extract: true,
		features.Window:     preset != "full",
		features.MemoryLong: preset == "" || preset == "all",
		features.MemoryWork: preset == "" || preset == "all" || preset == "work",
		features.Profile:    lane != "none",
		features.Charter:    executor && !noRules, features.Guard: executor && !noRules,
		features.CollectionState: executor && !noState, features.Gates: executor && !noState && !noGates,
	}
	if err := store.Set(doc, "features", legacySet(on)); err != nil {
		return err
	}
	if lane != "" && lane != "none" {
		store.Set(doc, "lane", lane)
	}

	// Собеседник прежнего-прежнего формата: одно поле owner.
	if _, ok := doc["owners"]; !ok {
		var owner string
		store.Get(doc, "owner", &owner)
		if owner == "" {
			owner = DefaultOwner
		}
		store.Set(doc, "owners", []string{owner})
	}
	delete(doc, "owner")
	delete(doc, "ownerTitle")

	// Задача — это подборка прежнего формата: тот же адрес рабочей памяти.
	var task struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if ok, _ := store.Get(doc, "task", &task); ok && task.ID != "" {
		store.Set(doc, "collection", task.ID)
		store.Set(doc, "collectionTitle", task.Title)
	}
	var tasks []struct {
		ID string `json:"id"`
	}
	store.Get(doc, "tasks", &tasks)
	var past []string
	for _, t := range tasks {
		past = append(past, t.ID)
	}
	if len(past) > 0 {
		store.Set(doc, "pastCollections", past)
	}
	delete(doc, "task")
	delete(doc, "tasks")

	var meter Meter
	store.Get(doc, "memoryMeter", &meter)
	delete(doc, "memoryMeter")
	store.Set(doc, "meter", meter)

	branch := map[string]json.RawMessage{}
	id := NewID()
	store.Set(branch, "id", id)
	store.Set(branch, "name", RootBranchName)
	store.Set(branch, "forkAt", 0)
	store.Set(branch, "forkTurn", 0)
	store.Set(branch, "created", created)
	branch["messages"] = doc["messages"]
	branch["turns"] = doc["turns"]
	store.Set(branch, "facts", map[string]any{"entries": []any{}, "version": 0})
	delete(doc, "messages")
	delete(doc, "turns")
	if err := legacyTurns(branch); err != nil {
		return err
	}
	if err := store.Set(doc, "branches", []map[string]json.RawMessage{branch}); err != nil {
		return err
	}
	return store.Set(doc, "active", id)
}

// legacyTurns переносит поля хода прежних форматов, которым нет места в
// новом, в Extras под legacy.*: итоги механизмов (память, профиль, автомат
// задачи, свод) и разбивку контекста в прежней форме.
func legacyTurns(branch map[string]json.RawMessage) error {
	var turns []map[string]json.RawMessage
	if _, err := store.Get(branch, "turns", &turns); err != nil {
		return fmt.Errorf("ходы: %w", err)
	}
	for _, t := range turns {
		extras := map[string]json.RawMessage{}
		for _, k := range []string{"memory", "profile", "checks", "state", "rules"} {
			if raw, ok := t[k]; ok {
				extras["legacy."+k] = raw
				delete(t, k)
			}
		}
		if raw, ok := t["context"]; ok {
			extras["legacy.context"] = raw
		}
		if raw, ok := t["task"]; ok {
			t["collection"] = raw
			delete(t, "task")
		}
		if err := legacyEvents(t); err != nil {
			return err
		}
		if len(extras) > 0 {
			if err := store.Set(t, "extras", extras); err != nil {
				return err
			}
		}
		if _, ok := t["status"]; !ok {
			store.Set(t, "status", TurnDone)
		}
	}
	if turns == nil {
		turns = []map[string]json.RawMessage{}
	}
	return store.Set(branch, "turns", turns)
}

// legacyEvents — поля событий журнала прежних форматов (правки памяти и
// профиля, переходы автомата, события свода) переезжают в Data события:
// журнал показывает их как есть, а не теряет.
func legacyEvents(t map[string]json.RawMessage) error {
	var events []map[string]json.RawMessage
	if ok, err := store.Get(t, "events", &events); !ok || err != nil {
		return err
	}
	for _, ev := range events {
		data := map[string]json.RawMessage{}
		for _, k := range []string{"memoryChanges", "profileChanges", "stateChange", "stateNow", "ruleEvent"} {
			if raw, ok := ev[k]; ok {
				data[k] = raw
				delete(ev, k)
			}
		}
		if len(data) > 0 {
			if err := store.Set(ev, "data", data); err != nil {
				return err
			}
		}
	}
	return store.Set(t, "events", events)
}
