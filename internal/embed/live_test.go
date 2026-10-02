//go:build embedlive

// Живая проверка настоящего эмбеддера (сайдкар, Ollama, облако). В обычный
// go test не входит — нужен запущенный сервер:
//
//	uv run embedder/server.py
//	go test -tags embedlive -run Live -v ./internal/embed/
//
// Адрес и модель — EMBED_BASE_URL / EMBED_MODEL (по умолчанию сайдкар с
// multilingual-e5-base).
package embed_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
)

func TestLiveEmbedder(t *testing.T) {
	h := embed.FromEnv()
	st := h.Health(context.Background())
	if !st.OK {
		t.Fatalf("эмбеддер недоступен: %s (%s)", st.Why, st.Hint)
	}
	t.Logf("health: модель %s, размерность %d, устройство %s", st.Model, st.Dims, st.Device)
	if strings.Contains(h.Name, "e5-base") && st.Dims != 768 {
		t.Fatalf("e5-base: размерность %d, ждали 768", st.Dims)
	}

	cases := []struct{ q, rel, irr string }{
		{"чем питается манул",
			"Манул питается в основном пищухами и мелкими грызунами, иногда ловит птиц.",
			"Малая панда обитает в бамбуковых лесах Гималаев и юго-западного Китая."},
		{"где живёт ирбис",
			"Снежный барс населяет высокогорья Центральной Азии на высоте до 6000 метров.",
			"Харза — крупная куница с яркой жёлто-коричневой окраской, охотится парами."},
		{"сколько щенков у красного волка",
			"В помёте красного волка обычно от четырёх до шести щенков, их выкармливает вся стая.",
			"Полосатая гиена питается падалью и ведёт ночной образ жизни."},
	}
	ctx := context.Background()
	for _, c := range cases {
		q, err := h.Embed(ctx, embed.Query, []string{c.q})
		if err != nil {
			t.Fatal(err)
		}
		p, err := h.Embed(ctx, embed.Passage, []string{c.rel, c.irr})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range append(q, p...) {
			if len(v) != h.Dims() {
				t.Fatalf("размерность %d ≠ %d", len(v), h.Dims())
			}
			var s float64
			for _, x := range v {
				s += float64(x) * float64(x)
			}
			if math.Abs(math.Sqrt(s)-1) > 1e-4 {
				t.Fatalf("не нормирован: |v| = %v", math.Sqrt(s))
			}
		}
		rel, irr := embed.Dot(q[0], p[0]), embed.Dot(q[0], p[1])
		t.Logf("«%s»: релевантный %.3f, нерелевантный %.3f", c.q, rel, irr)
		if rel <= irr {
			t.Errorf("«%s»: релевантный %.3f не ближе нерелевантного %.3f", c.q, rel, irr)
		}
	}

	// Скорость: 64 фрагмента длиной с типичный чанк (~600 символов).
	chunk := strings.Repeat("Манул ведёт одиночный образ жизни, активен в сумерках, охотится у нор пищух. ", 8)
	texts := make([]string, 64)
	for i := range texts {
		texts[i] = fmt.Sprintf("%d. %s", i, chunk)
	}
	t0 := time.Now()
	if _, err := h.Embed(ctx, embed.Passage, texts); err != nil {
		t.Fatal(err)
	}
	d := time.Since(t0)
	t.Logf("скорость: %d фрагментов по ~%d символов за %v — %.1f текстов/с",
		len(texts), len([]rune(texts[0])), d.Round(time.Millisecond), float64(len(texts))/d.Seconds())
}
