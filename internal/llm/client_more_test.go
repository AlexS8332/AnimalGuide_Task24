package llm

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Адрес провайдера: пусто — умолчание, хвостовой слеш и пробелы срезаются,
// чтобы путь не превратился в «//chat/completions».
func TestNewClientBaseURL(t *testing.T) {
	cases := map[string]string{
		"":                          DefaultBaseURL,
		"   ":                       DefaultBaseURL,
		"http://127.0.0.1:1/":       "http://127.0.0.1:1",
		" http://proxy.local/v1// ": "http://proxy.local/v1",
	}
	for in, want := range cases {
		c := NewClient("k", in)
		if c.BaseURL != want || c.HTTP == nil || c.HTTP.Timeout != requestTimeout || c.APIKey != "k" {
			t.Errorf("NewClient(%q): %+v", in, c)
		}
	}
}

// Клиент без своего http.Client работает: берётся клиент с таймаутом.
func TestChatWithoutHTTPClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"  привет  "},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4,"prompt_cache_hit_tokens":2,"prompt_cache_miss_tokens":1,"completion_tokens_details":{"reasoning_tokens":0}}}`))
	}))
	defer srv.Close()
	c := &Client{APIKey: "k", BaseURL: srv.URL}
	resp, err := c.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}}})
	if err != nil || resp.Message.Content != "привет" || resp.Message.Role != RoleAssistant {
		t.Fatalf("ответ: %+v %v", resp, err)
	}
	if resp.Usage != (Usage{Prompt: 3, Completion: 1, Total: 4, CacheHit: 2, CacheMiss: 1}) || resp.FinishReason != "stop" || resp.Model != "m" {
		t.Fatalf("расход: %+v", resp.Usage)
	}
}

// Ошибка без JSON (прокси, балансировщик) доходит текстом тела.
func TestChatNonJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("  upstream timeout \n"))
	}))
	defer srv.Close()
	_, err := NewClient("k", srv.URL).Chat(context.Background(), Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "upstream timeout") {
		t.Fatalf("ошибка шлюза: %v", err)
	}
}

// Ответ 200 не в JSON — ошибка разбора, а не пустой ответ.
func TestChatBrokenJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[`))
	}))
	defer srv.Close()
	_, err := NewClient("k", srv.URL).Chat(context.Background(), Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "разбор ответа") {
		t.Fatalf("битый JSON: %v", err)
	}
}

// Сеть недоступна: ошибка называет адрес, но не ключ (ключ в вывод и логи
// не попадает).
func TestChatNetworkErrorHidesKey(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + ln.Addr().String()
	ln.Close()
	const key = "sk-секретный-ключ"
	_, err = NewClient(key, addr).Chat(context.Background(), Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), addr) || strings.Contains(err.Error(), key) {
		t.Fatalf("сетевая ошибка: %v", err)
	}
}

// Отмена контекста прерывает запрос.
func TestChatContextCanceled(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := NewClient("k", srv.URL).Chat(ctx, Request{Model: "m"}); err == nil {
		t.Fatal("отменённый запрос вернулся без ошибки")
	}
}

// Ноль с любой стороны суммы — нейтральный элемент; одинаковый тариф
// сохраняется.
func TestCostAddZeroAndSameTariff(t *testing.T) {
	a := Cost{USD: 0.1, Tariff: TariffPeak, Known: true}
	if sum := a.Add(Cost{}); sum != a {
		t.Errorf("a плюс ноль: %+v", sum)
	}
	sum := a.Add(Cost{USD: 0.2, Tariff: TariffPeak, Known: true})
	if sum.Tariff != TariffPeak || !sum.Known || sum.USD < 0.3-1e-9 || sum.USD > 0.3+1e-9 {
		t.Errorf("один тариф: %+v", sum)
	}
}

// Пиковые часы DeepSeek — будни, 01–04 и 06–10 UTC; правые границы не
// входят.
func TestIsPeakBoundaries(t *testing.T) {
	monday := func(h, m int) time.Time { return time.Date(2026, 9, 7, h, m, 0, 0, time.UTC) }
	cases := []struct {
		at   time.Time
		want bool
	}{
		{monday(0, 59), false}, {monday(1, 0), true}, {monday(3, 59), true}, {monday(4, 0), false},
		{monday(5, 30), false}, {monday(6, 0), true}, {monday(9, 59), true}, {monday(10, 0), false},
		{time.Date(2026, 9, 6, 2, 0, 0, 0, time.UTC), false}, // воскресенье
		// Время в другом поясе переводится в UTC: 05:00 по Москве — 02:00 UTC.
		{time.Date(2026, 9, 7, 5, 0, 0, 0, time.FixedZone("MSK", 3*3600)), true},
	}
	for _, c := range cases {
		if got := IsPeak(c.at); got != c.want {
			t.Errorf("IsPeak(%s) = %v", c.at, got)
		}
	}
}

// Незнакомая модель не пропадает из суммы: итог становится неизвестным.
func TestUnknownModelPoisonsSum(t *testing.T) {
	known := Cost{USD: 0.1, Tariff: TariffOffPeak, Known: true}
	unknown := PriceOf("no-such-model", Usage{}, time.Now())
	if unknown.Known || unknown.Tariff != TariffUnknown {
		t.Fatalf("цена незнакомой модели: %+v", unknown)
	}
	if sum := known.Add(unknown); sum.Known {
		t.Errorf("сумма с незнакомой моделью посчитана: %+v", sum)
	}
}
