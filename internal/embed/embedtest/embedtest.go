// Package embedtest — подставной сервер эмбеддингов для тестов: тот же API,
// что у сайдкара (GET /health, POST /v1/embeddings, GET /v1/models), а
// векторы — от embed.Hash, так что тесты идут без сети и без Python.
//
// Сервер намеренно отдаёт data в обратном порядке (с верными index):
// клиент обязан раскладывать векторы по index, и это проверяется в каждом
// тесте бесплатно. Сбои включаются полями под мьютексом через сеттеры.
package embedtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
)

// Fake — запущенный подставной сервер. URL — базовый адрес для embed.HTTP.
type Fake struct {
	URL string
	// Hash — чем кодируются тексты (входы — уже с префиксом клиента).
	Hash embed.Hash
	// Model — что сервер называет своей моделью в /health и ответах.
	Model string

	mu       sync.Mutex
	requests int        // POST /v1/embeddings
	inputs   [][]string // входы каждого POST, по порядку
	auth     string     // последний заголовок Authorization
	status   int        // ≠0 — отвечать этим кодом на POST
	badDims  bool       // последний вектор батча — на 1 короче
	short    bool       // вернуть на один вектор меньше
	noHealth bool       // /health → 404 (как у Ollama и облаков)
}

// Server поднимает подставной сервер и гасит его по окончании теста.
// Модель — embed.DefaultModel, векторы — embed.Hash{D: 32}.
func Server(t testing.TB) *Fake {
	t.Helper()
	f := &Fake{Hash: embed.Hash{D: 32}, Model: embed.DefaultModel}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", f.health)
	mux.HandleFunc("GET /v1/models", f.models)
	mux.HandleFunc("POST /v1/embeddings", f.embeddings)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// Requests — сколько было POST /v1/embeddings.
func (f *Fake) Requests() int { f.mu.Lock(); defer f.mu.Unlock(); return f.requests }

// Inputs — входы всех POST по порядку (копия).
func (f *Fake) Inputs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.inputs))
	for i, in := range f.inputs {
		out[i] = append([]string(nil), in...)
	}
	return out
}

// Auth — последний полученный заголовок Authorization.
func (f *Fake) Auth() string { f.mu.Lock(); defer f.mu.Unlock(); return f.auth }

// FailWith — отвечать на POST этим HTTP-кодом (0 — нормально).
func (f *Fake) FailWith(code int) { f.mu.Lock(); f.status = code; f.mu.Unlock() }

// BadDims — портить размерность последнего вектора батча.
func (f *Fake) BadDims(on bool) { f.mu.Lock(); f.badDims = on; f.mu.Unlock() }

// Short — возвращать на один вектор меньше, чем просили.
func (f *Fake) Short(on bool) { f.mu.Lock(); f.short = on; f.mu.Unlock() }

// SetHash — сменить кодировщик на ходу (под мьютексом: обработчики читают
// Hash из своих горутин).
func (f *Fake) SetHash(h embed.Hash) { f.mu.Lock(); f.Hash = h; f.mu.Unlock() }

// SetModel — сервер называет себя другой моделью (под мьютексом, как
// SetHash).
func (f *Fake) SetModel(m string) { f.mu.Lock(); f.Model = m; f.mu.Unlock() }

func (f *Fake) model() string { f.mu.Lock(); defer f.mu.Unlock(); return f.Model }

// NoHealth — /health отвечает 404; остаётся только /v1/models.
func (f *Fake) NoHealth(on bool) { f.mu.Lock(); f.noHealth = on; f.mu.Unlock() }

func (f *Fake) health(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	no, dims := f.noHealth, f.Hash.Dims()
	f.mu.Unlock()
	if no {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "model": f.model(), "dims": dims, "device": "test"})
}

func (f *Fake) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"object": "list", "data": []map[string]any{{"id": f.model(), "object": "model"}}})
}

func (f *Fake) embeddings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	f.mu.Lock()
	f.requests++
	f.inputs = append(f.inputs, req.Input)
	f.auth = r.Header.Get("Authorization")
	status, badDims, short, hash := f.status, f.badDims, f.short, f.Hash
	f.mu.Unlock()
	if status != 0 {
		writeJSON(w, status, map[string]any{"error": "подставной сбой"})
		return
	}
	vecs, _ := hash.Embed(r.Context(), embed.Passage, req.Input)
	if badDims && len(vecs) > 0 {
		vecs[len(vecs)-1] = vecs[len(vecs)-1][:len(vecs[len(vecs)-1])-1]
	}
	if short && len(vecs) > 0 {
		vecs = vecs[:len(vecs)-1]
	}
	data := make([]map[string]any, 0, len(vecs))
	for i := len(vecs) - 1; i >= 0; i-- {
		// Масштаб ×3 — клиент обязан нормировать сам.
		v := make([]float32, len(vecs[i]))
		for j, x := range vecs[i] {
			v[j] = 3 * x
		}
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": v})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data, "model": f.model(),
		"usage": map[string]int{"prompt_tokens": 0, "total_tokens": 0}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
