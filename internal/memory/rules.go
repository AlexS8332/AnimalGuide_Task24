package memory

import (
	"fmt"
	"strings"
)

// Op — правка, предложенная извлекателем.
type Op struct {
	Layer string `json:"layer"`
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

// Patch — ответ извлекателя про память.
type Patch struct {
	Set    []Op `json:"set"`
	Delete []Op `json:"delete"`
}

// Rules — правила раскладки на этом ходе: какие слои включены и какие ключи
// принадлежат не памяти. Правила проверяют предложения извлекателя, а не
// доверяют им (ФТ-18); каждое вмешательство правила — правка с причиной.
type Rules struct {
	UseLong bool
	UseWork bool
	// Reserved — чей это ключ, если не памяти: «профиль», «карточка
	// животного», «код». Пусто — ключ свободен.
	Reserved func(key string) string
	MaxLong  int
	MaxWork  int
}

// Apply раскладывает правки по слоям. long и work — карточки на начало
// хода, правятся на месте; выключенный слой передаётся nil.
func Apply(long, work *Card, patch Patch, r Rules, turn int) Changes {
	var out Changes
	use := func(layer string) *Card {
		switch {
		case layer == LayerLong && r.UseLong && long != nil:
			return long
		case layer == LayerWork && r.UseWork && work != nil:
			return work
		}
		return nil
	}
	other := func(layer string) *Card {
		if layer == LayerLong {
			return use(LayerWork)
		}
		return use(LayerLong)
	}

	for _, op := range patch.Set {
		layer := strings.TrimSpace(op.Layer)
		key := cleanKey(op.Key)
		if key == "" || strings.TrimSpace(op.Value) == "" {
			continue
		}
		if r.Reserved != nil {
			if owner := r.Reserved(key); owner != "" {
				out = append(out, Change{Op: OpSkip, Layer: layer, Key: key, Value: op.Value,
					Reason: fmt.Sprintf("ключ «%s» ведёт %s, а не память: один ключ живёт в одном месте", key, owner)})
				continue
			}
		}
		target := use(layer)
		if target == nil {
			// Слой выключен или не существует: сведение оседает уровнем ниже
			// — в том слое, что включён (ФТ-48), а не пропадает молча.
			if alt := other(layer); alt != nil && (layer == LayerLong || layer == LayerWork) {
				target, layer = alt, alt.Layer
				out = append(out, Change{Op: OpSkip, Layer: op.Layer, Key: key, Value: op.Value,
					Reason: fmt.Sprintf("слой «%s» выключен — запись осела в слое «%s»", Title(op.Layer), Title(layer))})
			} else {
				out = append(out, Change{Op: OpSkip, Layer: op.Layer, Key: key, Value: op.Value,
					Reason: "слой выключен или неизвестен — запись некуда положить"})
				continue
			}
		}
		// Один ключ — в одном слое: если он уже есть в другом, это переезд,
		// а не второй экземпляр.
		if o := other(layer); o != nil {
			if old, had := o.Delete(key); had {
				target.Set(key, op.Value, turn, SourceExtract)
				out = append(out, Change{Op: OpMove, Layer: layer, From: o.Layer, Key: key, Value: op.Value, Old: old,
					Reason: "ключ уже был " + TitleIn(o.Layer) + " — перенесён, а не продублирован"})
				continue
			}
		}
		if old, changed := target.Set(key, op.Value, turn, SourceExtract); changed {
			out = append(out, Change{Op: OpSet, Layer: layer, Key: key, Value: op.Value, Old: old})
		}
	}
	for _, op := range patch.Delete {
		key := cleanKey(op.Key)
		if r.Reserved != nil && r.Reserved(key) != "" {
			continue
		}
		for _, c := range []*Card{use(LayerLong), use(LayerWork)} {
			if c == nil {
				continue
			}
			if op.Layer != "" && op.Layer != c.Layer {
				continue
			}
			if old, had := c.Delete(key); had {
				out = append(out, Change{Op: OpDelete, Layer: c.Layer, Key: key, Old: old})
			}
		}
	}
	for _, x := range []struct {
		c   *Card
		max int
	}{{use(LayerLong), r.MaxLong}, {use(LayerWork), r.MaxWork}} {
		if x.c == nil {
			continue
		}
		for _, k := range x.c.Trim(x.max) {
			out = append(out, Change{Op: OpDelete, Layer: x.c.Layer, Key: k, Reason: "вытеснено по потолку слоя"})
		}
	}
	return out
}
