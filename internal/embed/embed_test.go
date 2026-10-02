package embed_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed/embedtest"
)

var ctx = context.Background()

func norm(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

func TestPrefix(t *testing.T) {
	cases := []struct {
		model string
		kind  embed.Kind
		want  string
	}{
		{"intfloat/multilingual-e5-base", embed.Query, "query: "},
		{"intfloat/multilingual-e5-base", embed.Passage, "passage: "},
		{"intfloat/Multilingual-E5-Large", embed.Query, "query: "},
		{"BAAI/bge-m3", embed.Query, ""},
		{"bge-m3", embed.Passage, ""},
		{"text-embedding-3-small", embed.Query, ""},
		{"hash-256", embed.Passage, ""},
	}
	for _, c := range cases {
		if got := embed.Prefix(c.model, c.kind); got != c.want {
			t.Errorf("Prefix(%q, %d) = %q, want %q", c.model, c.kind, got, c.want)
		}
	}
}

func TestNormalizeDot(t *testing.T) {
	v := []float32{3, 4}
	embed.Normalize(v)
	if math.Abs(norm(v)-1) > 1e-6 || math.Abs(float64(v[0])-0.6) > 1e-6 {
		t.Fatalf("Normalize = %v", v)
	}
	z := []float32{0, 0, 0}
	embed.Normalize(z)
	for _, x := range z {
		if x != 0 || math.IsNaN(float64(x)) {
			t.Fatalf("нулевой вектор испорчен: %v", z)
		}
	}
	if d := embed.Dot([]float32{1, 2, 3}, []float32{4, 5}); d != 14 {
		t.Fatalf("Dot по короткому = %v", d)
	}
}

func TestHash(t *testing.T) {
	h := embed.Hash{}
	if h.Model() != "hash-256" || h.Dims() != 256 {
		t.Fatalf("Hash{}: %s/%d", h.Model(), h.Dims())
	}
	if h64 := (embed.Hash{D: 64}); h64.Model() != "hash-64" || h64.Dims() != 64 {
		t.Fatalf("Hash{64}: %s/%d", h64.Model(), h64.Dims())
	}
	texts := []string{
		"Манул питается грызунами и пищухами",
		"Чем питается манул? Пищухи и мелкие грызуны",
		"Ирбис живёт высоко в горах Центральной Азии",
		"",
	}
	a, err := h.Embed(ctx, embed.Passage, texts)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := h.Embed(ctx, embed.Query, texts)
	for i := range a {
		if len(a[i]) != 256 {
			t.Fatalf("размерность %d", len(a[i]))
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				t.Fatalf("недетерминирован: текст %d", i)
			}
			if math.IsNaN(float64(a[i][j])) {
				t.Fatalf("NaN в тексте %d", i)
			}
		}
	}
	for i := 0; i < 3; i++ {
		if math.Abs(norm(a[i])-1) > 1e-5 {
			t.Errorf("текст %d не нормирован: %v", i, norm(a[i]))
		}
	}
	if norm(a[3]) != 0 {
		t.Errorf("пустой текст — не нулевой вектор")
	}
	near, far := embed.Dot(a[0], a[1]), embed.Dot(a[0], a[2])
	if near <= far {
		t.Errorf("похожие %.3f не ближе непохожих %.3f", near, far)
	}
}

func TestHTTPBatchesOrderNormalize(t *testing.T) {
	f := embedtest.Server(t)
	h := &embed.HTTP{BaseURL: f.URL, Name: embed.DefaultModel, APIKey: "k", Batch: 3}
	if h.Dims() != 0 {
		t.Fatal("Dims до первого ответа должна быть 0")
	}
	texts := make([]string, 7)
	for i := range texts {
		texts[i] = fmt.Sprintf("текст номер %d про манула и %d", i, i*i)
	}
	got, err := h.Embed(ctx, embed.Passage, texts)
	if err != nil {
		t.Fatal(err)
	}
	if f.Requests() != 3 {
		t.Fatalf("запросов %d, ждали 3 (7 по 3)", f.Requests())
	}
	in := f.Inputs()
	if len(in[0]) != 3 || len(in[2]) != 1 || in[0][0] != "passage: "+texts[0] {
		t.Fatalf("батчи/префикс: %q", in)
	}
	if f.Auth() != "Bearer k" {
		t.Fatalf("Authorization = %q", f.Auth())
	}
	// Сервер отдаёт data задом наперёд и ×3: проверяем порядок и нормировку.
	want, _ := f.Hash.Embed(ctx, embed.Passage, in[0])
	want2, _ := f.Hash.Embed(ctx, embed.Passage, in[2])
	if embed.Dot(got[0], want[0]) < 0.9999 || embed.Dot(got[6], want2[0]) < 0.9999 {
		t.Fatal("векторы не по index")
	}
	for i, v := range got {
		if math.Abs(norm(v)-1) > 1e-5 {
			t.Fatalf("вектор %d не нормирован: %v", i, norm(v))
		}
	}
	if h.Dims() != 32 {
		t.Fatalf("Dims = %d", h.Dims())
	}
	// Без ключа заголовка нет; по умолчанию батч 32.
	f.SetModel("bge-m3")
	h2 := &embed.HTTP{BaseURL: f.URL + "/v1/", Name: "bge-m3"}
	if _, err := h2.Embed(ctx, embed.Query, make([]string, 40)); err != nil {
		t.Fatal(err)
	}
	in = f.Inputs()
	if len(in) != 5 || len(in[3]) != 32 || len(in[4]) != 8 || f.Auth() != "" || in[3][0] != "" {
		t.Fatalf("батч по умолчанию/без префикса/без ключа: %d %q", len(in), f.Auth())
	}
	if v, err := h.Embed(ctx, embed.Query, nil); err != nil || v != nil {
		t.Fatalf("пустой вход: %v %v", v, err)
	}
}

func TestHTTPErrors(t *testing.T) {
	f := embedtest.Server(t)
	h := &embed.HTTP{BaseURL: f.URL, Name: embed.DefaultModel}

	f.FailWith(500)
	_, err := h.Embed(ctx, embed.Query, []string{"а"})
	if err == nil || errors.Is(err, embed.ErrUnavailable) || !strings.Contains(err.Error(), "подставной сбой") {
		t.Fatalf("500: %v", err)
	}
	f.FailWith(503)
	if _, err := h.Embed(ctx, embed.Query, []string{"а"}); !errors.Is(err, embed.ErrUnavailable) {
		t.Fatalf("503: %v", err)
	}
	f.FailWith(0)

	f.BadDims(true)
	if _, err := h.Embed(ctx, embed.Query, []string{"а", "б"}); err == nil || !strings.Contains(err.Error(), "размерность") {
		t.Fatalf("неверная размерность: %v", err)
	}
	f.BadDims(false)

	f.Short(true)
	if _, err := h.Embed(ctx, embed.Query, []string{"а", "б"}); err == nil || !strings.Contains(err.Error(), "векторов") {
		t.Fatalf("мало векторов: %v", err)
	}
	f.Short(false)

	// Размерность запомнена: сервер «сменил модель» — ошибка.
	if _, err := h.Embed(ctx, embed.Query, []string{"а"}); err != nil {
		t.Fatal(err)
	}
	f.SetHash(embed.Hash{D: 16})
	if _, err := h.Embed(ctx, embed.Query, []string{"а"}); err == nil {
		t.Fatal("смена размерности не замечена")
	}

	// Ответила не та модель — ошибка, а не чужие векторы под нашим именем;
	// «:latest» у Ollama — та же модель; без поля model — верим.
	f.SetHash(embed.Hash{D: 32})
	h3 := &embed.HTTP{BaseURL: f.URL, Name: embed.DefaultModel}
	f.SetModel("BAAI/bge-m3")
	if _, err := h3.Embed(ctx, embed.Query, []string{"а"}); err == nil || !strings.Contains(err.Error(), "BAAI/bge-m3") {
		t.Fatalf("чужая модель: %v", err)
	}
	f.SetModel(embed.DefaultModel + ":latest")
	if _, err := h3.Embed(ctx, embed.Query, []string{"а"}); err != nil {
		t.Fatalf(":latest: %v", err)
	}
	f.SetModel("")
	if _, err := h3.Embed(ctx, embed.Query, []string{"а"}); err != nil {
		t.Fatalf("без model: %v", err)
	}
	f.SetModel(embed.DefaultModel)

	// Никто не слушает — ErrUnavailable.
	dead := &embed.HTTP{BaseURL: deadURL(t), Name: embed.DefaultModel}
	if _, err := dead.Embed(ctx, embed.Query, []string{"а"}); !errors.Is(err, embed.ErrUnavailable) {
		t.Fatalf("нет сервера: %v", err)
	}
	// Отмена вызывающим — не «недоступен».
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := dead.Embed(cctx, embed.Query, []string{"а"}); !errors.Is(err, context.Canceled) || errors.Is(err, embed.ErrUnavailable) {
		t.Fatalf("отмена: %v", err)
	}
}

// deadURL — адрес порта, который только что освободили: соединение
// гарантированно отвергнут.
func deadURL(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}

func TestHealth(t *testing.T) {
	f := embedtest.Server(t)
	h := &embed.HTTP{BaseURL: f.URL, Name: embed.DefaultModel}
	st := h.Health(ctx)
	if !st.OK || st.Dims != 32 || st.Device != "test" || st.Model != embed.DefaultModel || st.Why != "" {
		t.Fatalf("сайдкар: %+v", st)
	}
	if h.Dims() != 32 {
		t.Fatalf("Health не запомнил размерность: %d", h.Dims())
	}

	other := &embed.HTTP{BaseURL: f.URL, Name: "BAAI/bge-m3"}
	if st := other.Health(ctx); st.OK || !strings.Contains(st.Why, "bge-m3") {
		t.Fatalf("чужая модель: %+v", st)
	}

	f.NoHealth(true)
	h2 := &embed.HTTP{BaseURL: f.URL, Name: "bge-m3"}
	if st := h2.Health(ctx); !st.OK || st.Device != "" {
		t.Fatalf("/v1/models: %+v", st)
	}

	dead := &embed.HTTP{BaseURL: deadURL(t), Name: embed.DefaultModel}
	st = dead.Health(ctx)
	if st.OK || st.Why == "" || !strings.Contains(st.Hint, "uv run embedder/server.py") {
		t.Fatalf("недоступен: %+v", st)
	}
	bad := &embed.HTTP{BaseURL: "::не url::", Name: embed.DefaultModel}
	if st := bad.Health(ctx); st.OK || st.Why == "" {
		t.Fatalf("кривой адрес: %+v", st)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(embed.EnvBaseURL, "")
	t.Setenv(embed.EnvModel, "")
	t.Setenv(embed.EnvAPIKey, "")
	h := embed.FromEnv()
	if h.BaseURL != embed.DefaultBaseURL || h.Name != embed.DefaultModel || h.APIKey != "" {
		t.Fatalf("умолчания: %+v", h)
	}
	t.Setenv(embed.EnvBaseURL, "http://127.0.0.1:11434")
	t.Setenv(embed.EnvModel, "bge-m3")
	t.Setenv(embed.EnvAPIKey, "секрет")
	h = embed.FromEnv()
	if h.BaseURL != "http://127.0.0.1:11434" || h.Name != "bge-m3" || h.APIKey != "секрет" {
		t.Fatalf("из окружения: %+v", h.BaseURL)
	}
}

// memCache — embed.Cache в памяти.
type memCache map[string][]float32

func (m memCache) GetVec(_ context.Context, model, sha string) ([]float32, bool, error) {
	v, ok := m[model+"|"+sha]
	return v, ok, nil
}

func (m memCache) PutVec(_ context.Context, model, sha string, v []float32) error {
	m[model+"|"+sha] = v
	return nil
}

// TestLayered — общий слой только читается: попадание из него, запись — в
// свой слой.
func TestLayered(t *testing.T) {
	own, shared := memCache{}, memCache{}
	shared["m|a"] = []float32{1}
	l := embed.Layered{Own: own, Shared: shared}
	if v, ok, err := l.GetVec(ctx, "m", "a"); err != nil || !ok || v[0] != 1 {
		t.Fatalf("из общего: %v %v %v", v, ok, err)
	}
	if err := l.PutVec(ctx, "m", "b", []float32{2}); err != nil {
		t.Fatal(err)
	}
	if _, ok := shared["m|b"]; ok || own["m|b"] == nil {
		t.Fatalf("запись ушла не туда: own %v, shared %v", own, shared)
	}
	own["m|a"] = []float32{3}
	if v, _, _ := l.GetVec(ctx, "m", "a"); v[0] != 3 {
		t.Fatal("свой слой не первый")
	}
	if _, ok, _ := (embed.Layered{Own: own}).GetVec(ctx, "m", "zz"); ok {
		t.Fatal("промах без общего слоя")
	}
}

func TestCached(t *testing.T) {
	f := embedtest.Server(t)
	inner := &embed.HTTP{BaseURL: f.URL, Name: embed.DefaultModel, Batch: 2}
	mc := memCache{}
	c := &embed.Cached{E: inner, C: mc}
	if c.Model() != embed.DefaultModel {
		t.Fatal(c.Model())
	}
	texts := []string{"манул", "ирбис", "манул", "харза", "солонгой"}
	a, err := c.Embed(ctx, embed.Passage, texts)
	if err != nil {
		t.Fatal(err)
	}
	// 4 уникальных промаха одним вызовом → 2 батча по 2.
	if c.Misses != 4 || c.Hits != 0 || f.Requests() != 2 || len(mc) != 4 {
		t.Fatalf("первый вызов: misses=%d hits=%d req=%d cache=%d", c.Misses, c.Hits, f.Requests(), len(mc))
	}
	if embed.Dot(a[0], a[2]) < 0.9999 {
		t.Fatal("повтор в одном вызове получил другой вектор")
	}
	if c.Dims() != 32 {
		t.Fatalf("Dims = %d", c.Dims())
	}
	b, err := c.Embed(ctx, embed.Passage, texts)
	if err != nil {
		t.Fatal(err)
	}
	if f.Requests() != 2 || c.Hits != 5 {
		t.Fatalf("второй вызов ходил в модель: req=%d hits=%d", f.Requests(), c.Hits)
	}
	for i := range a {
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				t.Fatal("из кэша пришёл другой вектор")
			}
		}
	}
	// Тот же текст как вопрос — другой префикс, другой ключ.
	if _, err := c.Embed(ctx, embed.Query, []string{"манул"}); err != nil {
		t.Fatal(err)
	}
	if f.Requests() != 3 || c.Misses != 5 {
		t.Fatalf("query и passage делят ключ: req=%d", f.Requests())
	}
	// Ошибка внутреннего эмбеддера пробрасывается.
	f.FailWith(503)
	if _, err := c.Embed(ctx, embed.Query, []string{"новое"}); !errors.Is(err, embed.ErrUnavailable) {
		t.Fatalf("ошибка внутреннего: %v", err)
	}
	// Без хранилища — просто прокси.
	nc := &embed.Cached{E: embed.Hash{}}
	if v, err := nc.Embed(ctx, embed.Query, []string{"x", "x"}); err != nil || len(v) != 2 || nc.Misses != 1 {
		t.Fatalf("без кэша: %v %v %d", len(v), err, nc.Misses)
	}
}
