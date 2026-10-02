package agent

import (
	"sync"
	"testing"
)

// Nop — приёмник, который ничего не хранит: агенты работают и без журнала.
func TestNopEmitter(t *testing.T) {
	var em Emitter = Nop{}
	em.Log(Event{Kind: EventNote, Title: "x"})
	em.Publish(Update{Kind: "card"})
}

// Safe даёт параллельным специалистам писать в один Recorder без гонки.
func TestSafeConcurrent(t *testing.T) {
	rec := &Recorder{}
	s := &Safe{E: rec}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Log(Event{Kind: EventNote})
			s.Publish(Update{Kind: "section"})
		}()
	}
	wg.Wait()
	if len(rec.Events) != 50 || len(rec.Updates) != 50 || len(rec.Kinds()) != 50 {
		t.Fatalf("событий %d, обновлений %d", len(rec.Events), len(rec.Updates))
	}
}
