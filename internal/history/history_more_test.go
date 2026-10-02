package history

import (
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/paths"
)

// Идентификаторы — 16 шестнадцатеричных знаков, годятся как имя файла и
// не повторяются (ФТ-51).
func TestNewIDFormatAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewID()
		if len(id) != 16 || !paths.ValidHex(id) {
			t.Fatalf("идентификатор %q", id)
		}
		if seen[id] {
			t.Fatalf("повтор %q", id)
		}
		seen[id] = true
	}
}

// Текущая ветка: активная, а если активная пропала (битый файл) — первая;
// без веток — nil, и карточка фактов пустая, а не паника.
func TestCurrentFallbacks(t *testing.T) {
	c := New("m", nil, features.Catalog().Defaults())
	if c.Current() == nil || c.Current().ID != c.Active {
		t.Fatal("активная ветка")
	}
	first := c.Branches[0].ID
	c.Active = "пропавшая"
	if b := c.Current(); b == nil || b.ID != first {
		t.Fatalf("откат на первую ветку: %+v", b)
	}
	c.Branches = nil
	if c.Current() != nil || !c.Facts().Empty() || c.Path("x") != nil || c.PathTurns("x") != nil {
		t.Fatal("диалог без веток")
	}
}
