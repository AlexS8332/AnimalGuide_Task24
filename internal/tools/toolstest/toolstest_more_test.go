package toolstest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func get(t *testing.T, raw string, ua bool) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, raw, nil)
	if ua {
		req.Header.Set("User-Agent", "AnimalGuide-test")
	} else {
		// Пустое значение — клиент Go не подставляет своё по умолчанию.
		req.Header["User-Agent"] = []string{""}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// Подставная Википедия требует User-Agent, как настоящая, и считает запросы.
func TestWikiNeedsUserAgent(t *testing.T) {
	w := NewWiki()
	defer w.Close()
	if code, _ := get(t, w.URL+"/w/api.php?list=search&srsearch=рысь", false); code != http.StatusForbidden {
		t.Fatalf("без User-Agent: %d", code)
	}
	if code, _ := get(t, w.URL+"/w/api.php?action=unknown", true); code != http.StatusNotFound {
		t.Fatalf("незнакомый запрос: %d", code)
	}
	if w.Calls.Load() != 2 {
		t.Fatalf("запросов %d", w.Calls.Load())
	}
}

// Поиск: похожее — не то же самое; незнакомое — пустой список, а не ошибка.
func TestWikiSearch(t *testing.T) {
	w := NewWiki()
	defer w.Close()
	titles := func(q string) []string {
		_, body := get(t, w.URL+"/w/api.php?list=search&srsearch="+url.QueryEscape(q), true)
		var out struct {
			Query struct {
				Search []struct{ Title string } `json:"search"`
			} `json:"query"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		var ts []string
		for _, s := range out.Query.Search {
			ts = append(ts, s.Title)
		}
		return ts
	}
	if got := titles("рысь"); len(got) != 2 || got[1] != "Обыкновенная рысь" {
		t.Fatalf("рысь: %v", got)
	}
	if got := titles("полосатый манул"); len(got) != 1 || got[0] != "Манул" {
		t.Fatalf("полосатый манул: %v", got)
	}
	if got := titles("дикий кот"); len(got) != 1 || got[0] != "Лесной кот" {
		t.Fatalf("кот: %v", got)
	}
	if got := titles("единорог"); len(got) != 0 {
		t.Fatalf("единорог: %v", got)
	}
}

// Чтение: перенаправление, статья и отсутствующая статья.
func TestWikiExtracts(t *testing.T) {
	w := NewWiki()
	defer w.Close()
	_, body := get(t, w.URL+"/w/api.php?prop=extracts&titles="+url.QueryEscape("Рысь"), true)
	if !strings.Contains(body, `"redirects"`) || !strings.Contains(body, "Lynx lynx") {
		t.Fatalf("перенаправление: %s", body)
	}
	_, body = get(t, w.URL+"/w/api.php?prop=extracts&titles="+url.QueryEscape("Единорог"), true)
	if !strings.Contains(body, `"missing":true`) {
		t.Fatalf("нет статьи: %s", body)
	}
}

// Подставной GBIF: сверка, дерево, народные названия, соседи и сам таксон.
func TestGBIFRoutes(t *testing.T) {
	g := NewGBIF()
	defer g.Close()
	cases := []struct{ path, want string }{
		{"/species/match?name=" + url.QueryEscape(" Lynx Lynx "), `"usageKey":2435240`},
		{"/species/match?name=Felis+venenosa", `"matchType":"HIGHERRANK"`},
		{"/species/match?name=Unicornis", `"matchType":"NONE"`},
		{"/species/2435240/parents", `"Felidae"`},
		{"/species/1/parents", `[]`},
		{"/species/2435270/vernacularNames", `"Манул"`},
		{"/species/1/vernacularNames", `{"results":[]}`},
		{"/species/9703/children", `"Catolynx"`},
		{"/species/1/children", `"count":0`},
		{"/species/9703", `"FAMILY"`},
		{"/species/1", `{}`},
	}
	for _, c := range cases {
		code, body := get(t, g.URL+c.path, false)
		if code != http.StatusOK || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %s", c.path, code, body)
		}
	}
	if code, _ := get(t, g.URL+"/species/1/2/3", false); code != http.StatusNotFound {
		t.Fatalf("незнакомый путь: %d", code)
	}
	if g.Calls.Load() != int64(len(cases)+1) {
		t.Fatalf("запросов %d", g.Calls.Load())
	}
}
