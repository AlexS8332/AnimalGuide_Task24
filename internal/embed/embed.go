// Package embed — эмбеддинги текстов: интерфейс, HTTP-клиент
// OpenAI-совместимого /v1/embeddings, детерминированный подставной эмбеддер
// и кэш векторов.
//
// У DeepSeek эмбеддингов нет. Основной путь — локальный сайдкар
// (embedder/server.py, uv + sentence-transformers) с моделью
// intfloat/multilingual-e5-base; тот же клиент без изменений ходит в Ollama
// и облачные API — меняются только EMBED_BASE_URL, EMBED_MODEL и
// EMBED_API_KEY. Hash — только для тестов: смысла в его векторах нет, зато
// они одинаковы на любой машине и без сети.
package embed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/words"
)

const (
	// DefaultBaseURL — адрес сайдкара по умолчанию (embedder/server.py).
	DefaultBaseURL = "http://127.0.0.1:8777"
	// DefaultModel — модель по умолчанию: многоязычная, 768 измерений,
	// вход до 512 токенов, нужны префиксы query:/passage:.
	DefaultModel = "intfloat/multilingual-e5-base"
)

// Переменные окружения клиента.
const (
	EnvBaseURL = "EMBED_BASE_URL"
	EnvModel   = "EMBED_MODEL"
	EnvAPIKey  = "EMBED_API_KEY"
)

// Kind — что кодируется: вопрос или фрагмент базы. У моделей e5 это разные
// префиксы, без них качество поиска заметно падает.
type Kind int

const (
	Query Kind = iota
	Passage
)

// ErrNotImplemented — заглушка контракта.
var ErrNotImplemented = errors.New("не реализовано")

// ErrUnavailable — эмбеддер не отвечает (сайдкар не запущен, сеть). Поиск
// по нему откатывается на BM25; ошибка оборачивается с причиной.
var ErrUnavailable = errors.New("эмбеддер недоступен")

// Embedder — источник векторов. Векторы L2-нормированы: скалярное
// произведение равно косинусу.
type Embedder interface {
	// Model — имя модели, как его пишут в индекс: «intfloat/multilingual-e5-base»,
	// «hash-256». Индекс, построенный одной моделью, другой не ищется.
	Model() string
	// Dims — размерность; 0 — ещё неизвестна (HTTP до первого ответа).
	Dims() int
	// Embed кодирует тексты одним или несколькими батчами; порядок
	// векторов — порядок текстов.
	Embed(ctx context.Context, kind Kind, texts []string) ([][]float32, error)
}

// prefixRule — строка таблицы префиксов: если имя модели (в нижнем
// регистре) содержит sub, к вопросу и фрагменту добавляются query и
// passage. Первое совпадение выигрывает, поэтому частные правила — выше.
type prefixRule struct {
	sub            string
	query, passage string
}

// prefixes — таблица префиксов. e5 обучали с «query: »/«passage: », и без
// них косинусы сползают в одну кучу; bge-m3 префиксов не требует (это
// записано явно, чтобы «m3» не попал под какое-нибудь будущее правило).
// Модель, которой нет в таблице, кодирует текст как есть.
var prefixes = []prefixRule{
	{sub: "bge-m3"},
	{sub: "e5", query: "query: ", passage: "passage: "},
}

// Prefix — префикс текста для модели и вида: у e5 «query: »/«passage: »,
// у остальных пусто. Таблица — данными, по подстроке имени модели.
func Prefix(model string, kind Kind) string {
	m := strings.ToLower(model)
	for _, r := range prefixes {
		if strings.Contains(m, r.sub) {
			if kind == Query {
				return r.query
			}
			return r.passage
		}
	}
	return ""
}

// Таймауты HTTP-клиента. Индексация корпуса на CPU длится минуты, поэтому
// общий таймаут на весь Embed был бы либо слишком мал, либо бесполезен;
// ограничиваем каждый батч: 32 фрагмента по ~512 токенов e5-base на CPU
// кодирует за десяток секунд, две минуты — с запасом на холодный старт.
// Health — короткий: его зовут при старте и из интерфейса, ждать там
// нельзя.
const (
	batchTimeout  = 2 * time.Minute
	healthTimeout = 3 * time.Second
	defaultBatch  = 32
)

// HTTP — клиент OpenAI-совместимого API: POST {BaseURL}/v1/embeddings
// {"model", "input": [...]}, ответ {"data": [{"index", "embedding"}]}.
// Health — GET {BaseURL}/health (сайдкар) с запасным GET /v1/models
// (Ollama, облако).
//
// Префикс модели (Prefix) добавляет клиент, а не сервер: сайдкар отдаёт
// модель как есть, и тот же клиент без изменений работает с Ollama и
// облаком, которые о префиксах e5 ничего не знают.
type HTTP struct {
	BaseURL string
	Name    string // модель
	APIKey  string
	// Batch — сколько текстов в одном запросе; 0 → 32.
	Batch int

	// dims — размерность из первого ответа (или из /health). Атомик, потому
	// что один клиент делят индексация и поиск из разных горутин.
	dims atomic.Int64
}

// FromEnv — клиент по EMBED_BASE_URL / EMBED_MODEL / EMBED_API_KEY с
// умолчаниями DefaultBaseURL / DefaultModel.
func FromEnv() *HTTP {
	h := &HTTP{BaseURL: DefaultBaseURL, Name: DefaultModel}
	if v := strings.TrimSpace(os.Getenv(EnvBaseURL)); v != "" {
		h.BaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvModel)); v != "" {
		h.Name = v
	}
	h.APIKey = strings.TrimSpace(os.Getenv(EnvAPIKey))
	return h
}

func (h *HTTP) Model() string { return h.Name }
func (h *HTTP) Dims() int     { return int(h.dims.Load()) }

// url — адрес эндпоинта. Хвостовой «/» и «/v1» в BaseURL терпим: облачные
// провайдеры обычно дают адрес вида https://api.example.com/v1, и
// заставлять пользователя его обрезать — лишний повод для ошибки.
func (h *HTTP) url(path string) string {
	base := strings.TrimRight(h.BaseURL, "/")
	if strings.HasPrefix(path, "/v1/") {
		base = strings.TrimSuffix(base, "/v1")
	}
	return base + path
}

// Embed кодирует тексты батчами по Batch, каждый со своим таймаутом.
// Ошибка любого батча прерывает всё: частичный индекс хуже отсутствующего.
func (h *HTTP) Embed(ctx context.Context, kind Kind, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	n := h.Batch
	if n <= 0 {
		n = defaultBatch
	}
	pre := Prefix(h.Name, kind)
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += n {
		end := min(start+n, len(texts))
		in := make([]string, 0, end-start)
		for _, t := range texts[start:end] {
			in = append(in, pre+t)
		}
		vecs, err := h.batch(ctx, in)
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	// Model — какая модель кодировала (OpenAI, сайдкар и Ollama его
	// пишут); пусто — сервер не сказал.
	Model string `json:"model"`
	Data  []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// batch — один запрос /v1/embeddings: проверка ответа, порядок по index,
// нормировка. Сервер вправе вернуть data в любом порядке (OpenAI это прямо
// допускает), поэтому раскладываем по index, а не по позиции.
func (h *HTTP) batch(ctx context.Context, in []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: h.Name, Input: in})
	if err != nil {
		return nil, err
	}
	bctx, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(bctx, http.MethodPost, h.url("/v1/embeddings"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("эмбеддинги: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Отмена вызывающим — не «недоступен»: пользователь сам прервал
		// индексацию, откатываться на BM25 незачем.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, h.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: чтение ответа: %w", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("эмбеддинги: HTTP %d: %s", resp.StatusCode, errorText(raw))
		// Шлюз без живого бэкенда — то же, что недоступный сайдкар.
		switch resp.StatusCode {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		return nil, err
	}
	var er embedResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("эмбеддинги: разбор ответа: %w", err)
	}
	if er.Model != "" && !sameModel(er.Model, h.Name) {
		// Сайдкар кодирует своей моделью, что бы ни просил клиент: чужие
		// векторы под именем нашей модели испортили бы индекс молча.
		return nil, fmt.Errorf("эмбеддинги: ответила модель %s, а клиент ждёт %s", er.Model, h.Name)
	}
	if len(er.Data) != len(in) {
		return nil, fmt.Errorf("эмбеддинги: на %d текстов пришло %d векторов", len(in), len(er.Data))
	}
	out := make([][]float32, len(in))
	sort.SliceStable(er.Data, func(i, j int) bool { return er.Data[i].Index < er.Data[j].Index })
	dims := h.Dims()
	for i, d := range er.Data {
		if d.Index != i {
			return nil, fmt.Errorf("эмбеддинги: индексы ответа не 0..%d (встретился %d)", len(in)-1, d.Index)
		}
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("эмбеддинги: пустой вектор для текста %d", i)
		}
		if dims == 0 {
			dims = len(d.Embedding)
		}
		if len(d.Embedding) != dims {
			return nil, fmt.Errorf("эмбеддинги: размерность %d, ожидалась %d (сменилась модель?)", len(d.Embedding), dims)
		}
		// Нормируем сами, даже если сервер обещает normalize: Ollama и
		// часть облаков отдают ненормированные векторы, а поиск считает
		// косинус скалярным произведением.
		Normalize(d.Embedding)
		out[i] = d.Embedding
	}
	h.dims.CompareAndSwap(0, int64(dims))
	return out, nil
}

// sameModel — имя модели из ответа то же, что у клиента. Тег Ollama
// «:latest» не различает модели: «bge-m3» и «bge-m3:latest» — одна.
func sameModel(got, want string) bool {
	trim := func(s string) string { return strings.TrimSuffix(strings.TrimSpace(s), ":latest") }
	return trim(got) == trim(want)
}

// errorText — короткое описание ошибки из тела ответа: OpenAI пишет
// {"error":{"message"}}, сайдкар и Ollama — {"error":"..."}; иначе — начало
// тела.
func errorText(raw []byte) string {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && len(e.Error) > 0 {
		var s string
		if json.Unmarshal(e.Error, &s) == nil {
			return s
		}
		var m struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e.Error, &m) == nil && m.Message != "" {
			return m.Message
		}
	}
	s := strings.TrimSpace(string(raw))
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

// Status — состояние эмбеддера для интерфейса и стартового вывода.
type Status struct {
	OK     bool   `json:"ok"`
	URL    string `json:"url"`
	Model  string `json:"model"`
	Dims   int    `json:"dims,omitempty"`
	Device string `json:"device,omitempty"`
	Why    string `json:"why,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

// Hint — как поднять эмбеддер; одна строка для журнала и интерфейса.
const Hint = "запусти сайдкар: uv run embedder/server.py (первый запуск скачает модель ~1 ГБ) — или задай EMBED_BASE_URL/EMBED_MODEL для Ollama или облачного API; без эмбеддера поиск лексический (BM25)"

// Health спрашивает сайдкар (коротким таймаутом) и не кидает ошибку: всё,
// что пошло не так, — в Status.Why с подсказкой, как запустить сайдкар.
//
// Сначала /health сайдкара (он знает модель, размерность и устройство),
// при 404 и прочих отказах — /v1/models, который есть у Ollama и облаков.
func (h *HTTP) Health(ctx context.Context) (st Status) {
	st = Status{URL: h.BaseURL, Model: h.Name, Dims: h.Dims()}
	defer func() {
		// Health зовут при старте приложения: паника здесь уронила бы всё
		// ради справочной строки.
		if r := recover(); r != nil {
			st.OK = false
			st.Why = fmt.Sprintf("внутренняя ошибка проверки: %v", r)
			st.Hint = Hint
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	var hr struct {
		OK     bool   `json:"ok"`
		Model  string `json:"model"`
		Dims   int    `json:"dims"`
		Device string `json:"device"`
	}
	code, err := h.getJSON(ctx, "/health", &hr)
	if err == nil && code == http.StatusOK && hr.Model != "" {
		st.Device = hr.Device
		if hr.Dims > 0 {
			st.Dims = hr.Dims
		}
		if hr.Model != h.Name {
			// Сайдкар отдаёт свою модель, что бы ни просил клиент; если
			// это не та модель, индекс получит чужие векторы под своим
			// именем. Лучше сказать сразу.
			st.Why = fmt.Sprintf("сайдкар отдаёт модель %s, а клиент ждёт %s", hr.Model, h.Name)
			st.Hint = "запусти сайдкар с --model " + h.Name + " или поправь " + EnvModel
			return st
		}
		if hr.Dims > 0 {
			h.dims.CompareAndSwap(0, int64(hr.Dims))
		}
		st.OK = hr.OK
		if !st.OK {
			st.Why = "сайдкар отвечает, но не готов"
			st.Hint = Hint
		}
		return st
	}
	if err != nil && ctx.Err() == nil && isConnErr(err) {
		// Соединения нет вовсе — /v1/models спрашивать бессмысленно.
		st.Why = "нет соединения: " + err.Error()
		st.Hint = Hint
		return st
	}

	var mr struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	code2, err2 := h.getJSON(ctx, "/v1/models", &mr)
	switch {
	case err2 != nil:
		st.Why = "нет ответа: " + err2.Error()
	case code2 == http.StatusUnauthorized || code2 == http.StatusForbidden:
		st.Why = fmt.Sprintf("доступ запрещён (HTTP %d): проверь %s", code2, EnvAPIKey)
	case code2 != http.StatusOK:
		st.Why = fmt.Sprintf("/health и /v1/models ответили HTTP %d и %d", code, code2)
	default:
		st.OK = true
		return st
	}
	st.Hint = Hint
	return st
}

// getJSON — GET с ключом; тело разбирается только при 200.
func (h *HTTP) getJSON(ctx context.Context, path string, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url(path), nil)
	if err != nil {
		return 0, err
	}
	if h.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v); err != nil {
		return resp.StatusCode, fmt.Errorf("%w: %s: %w", errBadJSON, path, err)
	}
	return resp.StatusCode, nil
}

// errBadJSON — сервер ответил, но не тем: это не обрыв соединения.
var errBadJSON = errors.New("ответ не JSON")

// isConnErr — ошибка транспорта (отказ в соединении, DNS, таймаут), а не
// разбора ответа.
func isConnErr(err error) bool { return !errors.Is(err, errBadJSON) }

// Hash — подставной детерминированный эмбеддер для тестов: основы слов
// (words.Stems) и символьные триграммы хэшируются в вектор размерности D.
// Общие слова дают близкие векторы, так что поиск в тестах осмыслен.
//
// D = 0 → 256. Kind не учитывается: префиксов у Hash нет.
type Hash struct{ D int }

func (h Hash) dims() int {
	if h.D > 0 {
		return h.D
	}
	return 256
}

// Model включает размерность: индексы hash-64 и hash-256 несовместимы так
// же, как индексы разных моделей.
func (h Hash) Model() string { return fmt.Sprintf("hash-%d", h.dims()) }
func (h Hash) Dims() int     { return h.dims() }

func (h Hash) Embed(ctx context.Context, kind Kind, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = h.vector(t)
	}
	return out, nil
}

// vector — «хэширующий трюк»: каждый признак (основа слова с весом 2 и
// триграммы слова с весом 1) кладётся в ячейку по FNV-хэшу, знак — по
// отдельному биту того же хэша, чтобы коллизии гасили друг друга, а не
// копились. Основы дают совпадение по словам, триграммы — по общим
// корням при разных окончаниях («манул»/«манулы»/«манулов»).
func (h Hash) vector(text string) []float32 {
	d := h.dims()
	v := make([]float32, d)
	add := func(feat string, w float32) {
		f := fnv.New64a()
		f.Write([]byte(feat))
		x := f.Sum64()
		idx := int(x % uint64(d))
		if (x>>63)&1 == 1 {
			w = -w
		}
		v[idx] += w
	}
	for _, s := range words.Stems(text) {
		add("w:"+s, 2)
		r := []rune(" " + s + " ")
		for i := 0; i+3 <= len(r); i++ {
			add("t:"+string(r[i:i+3]), 1)
		}
	}
	Normalize(v)
	return v
}

// Cache — хранилище векторов по (модель, sha256 текста с префиксом).
// Реализует kb.Store (таблица kb_embed_cache).
type Cache interface {
	GetVec(ctx context.Context, model, sha string) ([]float32, bool, error)
	PutVec(ctx context.Context, model, sha string, v []float32) error
}

// Layered — кэш в два слоя: Own читается первым и принимает запись, Shared
// только читается. Так прогон во временной базе берёт векторы из общего
// кэша (kb.db репозитория) и не портит его: всё новое пишется в Own.
// Shared == nil — один слой.
type Layered struct {
	Own, Shared Cache
}

func (l Layered) GetVec(ctx context.Context, model, sha string) ([]float32, bool, error) {
	if v, ok, err := l.Own.GetVec(ctx, model, sha); err != nil || ok {
		return v, ok, err
	}
	if l.Shared == nil {
		return nil, false, nil
	}
	return l.Shared.GetVec(ctx, model, sha)
}

func (l Layered) PutVec(ctx context.Context, model, sha string, v []float32) error {
	return l.Own.PutVec(ctx, model, sha, v)
}

// Cached — эмбеддер с кэшем: повторная индексация и повторные вопросы не
// ходят в модель, а повторная оценка поиска совпадает побитно.
//
// Ключ — sha256 текста вместе с префиксом модели: «query: манул» и
// «passage: манул» у e5 — разные векторы. Счётчики не защищены мьютексом:
// Cached рассчитан на один поток (индексация, прогон оценки); C == nil —
// кэша нет, всё идёт в E.
type Cached struct {
	E Embedder
	C Cache
	// Hits, Misses — счётчики для отчёта.
	Hits, Misses int
}

func (c *Cached) Model() string { return c.E.Model() }
func (c *Cached) Dims() int     { return c.E.Dims() }

// Embed отдаёт попадания из кэша, а промахи (без повторов) отправляет во
// внутренний эмбеддер одним вызовом — батчи режет уже он.
func (c *Cached) Embed(ctx context.Context, kind Kind, texts []string) ([][]float32, error) {
	model := c.E.Model()
	pre := Prefix(model, kind)
	out := make([][]float32, len(texts))
	var missTexts, missKeys []string
	missAt := map[string][]int{} // ключ → позиции в texts
	for i, t := range texts {
		sum := sha256.Sum256([]byte(pre + t))
		key := hex.EncodeToString(sum[:])
		if pos, ok := missAt[key]; ok {
			missAt[key] = append(pos, i)
			continue
		}
		if c.C != nil {
			v, ok, err := c.C.GetVec(ctx, model, key)
			if err != nil {
				return nil, fmt.Errorf("кэш эмбеддингов: %w", err)
			}
			if ok {
				c.Hits++
				out[i] = v
				continue
			}
		}
		missAt[key] = []int{i}
		missTexts = append(missTexts, t)
		missKeys = append(missKeys, key)
	}
	if len(missTexts) == 0 {
		return out, nil
	}
	vecs, err := c.E.Embed(ctx, kind, missTexts)
	if err != nil {
		return nil, err
	}
	if len(vecs) != len(missTexts) {
		return nil, fmt.Errorf("эмбеддинги: на %d текстов пришло %d векторов", len(missTexts), len(vecs))
	}
	for j, key := range missKeys {
		c.Misses++
		if c.C != nil {
			if err := c.C.PutVec(ctx, model, key, vecs[j]); err != nil {
				return nil, fmt.Errorf("кэш эмбеддингов: %w", err)
			}
		}
		for _, i := range missAt[key] {
			out[i] = vecs[j]
		}
	}
	return out, nil
}

// Dot — скалярное произведение (для нормированных векторов — косинус).
func Dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		if i < len(b) {
			s += a[i] * b[i]
		}
	}
	return s
}

// Normalize приводит вектор к единичной длине на месте. Нулевой вектор
// (пустой текст у Hash) остаётся нулевым: делить на ноль и плодить NaN,
// которые потом отравят любую сортировку, нельзя.
func Normalize(v []float32) {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 || math.IsNaN(s) || math.IsInf(s, 0) {
		return
	}
	n := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= n
	}
}
