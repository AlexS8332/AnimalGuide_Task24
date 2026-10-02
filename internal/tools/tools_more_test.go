package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools/toolstest"
)

// Пустой адрес источника — боевой адрес по умолчанию, хвостовой слеш
// срезается; источники называют себя.
func TestSourceBases(t *testing.T) {
	f := NewFetcher()
	if w := NewWikipedia("", f); w.Base != DefaultWikipediaBase || w.ID() != "wikipedia" || len(w.Tools()) != 2 {
		t.Fatalf("Википедия: %+v", w)
	}
	if w := NewWikipedia(" http://wiki.local/ ", f); w.Base != "http://wiki.local" {
		t.Fatalf("адрес Википедии: %q", w.Base)
	}
	if g := NewGBIF("", f); g.Base != DefaultGBIFBase || g.ID() != "gbif" || len(g.Tools()) != 4 {
		t.Fatalf("GBIF: %+v", g)
	}
	if g := NewGBIF("http://gbif.local//", f); g.Base != "http://gbif.local" {
		t.Fatalf("адрес GBIF: %q", g.Base)
	}
}

// Шесть инструментов источников (ФТ-1) — в порядке показа, и все помечены
// как содержимое внешнего источника (ФТ-41).
func TestLocalToolsOrderAndUntrusted(t *testing.T) {
	list := LocalTools(NewFetcher(), "", "")
	if len(list) != len(SourceTools) {
		t.Fatalf("инструментов %d", len(list))
	}
	for i, tl := range list {
		s := tl.Spec()
		if s.Name != SourceTools[i] || !IsSourceTool(s.Name) {
			t.Errorf("инструмент %d: %s", i, s.Name)
		}
		if !s.Untrusted {
			t.Errorf("%s не помечен внешним источником", s.Name)
		}
	}
	if IsSourceTool("submit_card") {
		t.Fatal("завершающий инструмент среди источников")
	}
}

// Сквозной путь по подставным источникам: поиск → чтение → сверка латыни.
func TestLocalToolsAgainstFakes(t *testing.T) {
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	defer wiki.Close()
	defer gbif.Close()
	f := NewFetcher()
	reg := MustRegistry(LocalTools(f, wiki.URL, gbif.URL)...)
	call := func(name, args string) string {
		t.Helper()
		tl, ok := reg.Get(name)
		if !ok {
			t.Fatalf("нет %s", name)
		}
		out, err := tl.Call(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	if out := call("search_wikipedia", `{"query":"рысь"}`); !strings.Contains(out, "Обыкновенная рысь") {
		t.Fatalf("поиск: %s", out)
	}
	if out := call("read_wikipedia", `{"title":"Рысь"}`); !strings.Contains(out, "Lynx lynx") {
		t.Fatalf("чтение по перенаправлению: %s", out)
	}
	if out := call("match_taxon", `{"scientific_name":"Lynx lynx"}`); !strings.Contains(out, "2435240") {
		t.Fatalf("сверка: %s", out)
	}
	if wiki.Calls.Load() == 0 || gbif.Calls.Load() == 0 {
		t.Fatal("подставные источники не вызывались")
	}
	// Повтор берётся из кэша: в сеть запросов столько же, сколько дошло до
	// подставных источников.
	call("match_taxon", `{"scientific_name":"Lynx lynx"}`)
	if got := f.Requests(); got != wiki.Calls.Load()+gbif.Calls.Load() {
		t.Fatalf("запросов в сеть %d, до источников дошло %d", got, wiki.Calls.Load()+gbif.Calls.Load())
	}
}
