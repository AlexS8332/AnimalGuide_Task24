package mdd_test

import (
	"context"
	"sync"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
)

func TestMemoryConformance(t *testing.T) {
	mddtest.Conformance(t, func(t *testing.T) mdd.Store { return mdd.NewMemory() })
}

// TestMemoryConcurrent — читатели видят либо старый релиз, либо новый,
// целиком (запускать с -race).
func TestMemoryConcurrent(t *testing.T) {
	ctx := context.Background()
	st := mdd.NewMemory()
	if err := st.Replace(ctx, mddtest.Sample()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = st.Replace(ctx, mddtest.Sample())
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, total, err := st.Search(ctx, mdd.Query{}); err != nil || total != len(mddtest.SampleOrder) {
					t.Errorf("поиск во время замены: total %d, %v", total, err)
					return
				}
				if _, err := st.Find(ctx, "Manul"); err != nil {
					t.Errorf("Find во время замены: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
