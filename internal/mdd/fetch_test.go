package mdd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

// archiveServer — сервер архива с ETag: отдаёт текущий архив или 304,
// если If-None-Match совпал. Архив и метку тест меняет на ходу.
type archiveServer struct {
	mu       sync.Mutex
	data     []byte
	etag     string
	requests []*http.Request
}

func (a *archiveServer) set(data []byte, etag string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.data, a.etag = data, etag
}

func (a *archiveServer) last() *http.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.requests[len(a.requests)-1]
}

func (a *archiveServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.requests = append(a.requests, r)
	data, etag := a.data, a.etag
	a.mu.Unlock()
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Write(data)
}

func TestFetch(t *testing.T) {
	arch := &archiveServer{}
	arch.set([]byte("PK-архив"), `"v1"`)
	srv := httptest.NewServer(arch)
	defer srv.Close()
	ctx := context.Background()

	res, err := Fetch(ctx, srv.Client(), srv.URL, "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(res.Data) != "PK-архив" || res.ETag != `"v1"` || res.NotModified {
		t.Errorf("200: %+v", res)
	}
	req := arch.last()
	if req.Header.Get("User-Agent") != tools.DefaultUserAgent {
		t.Errorf("User-Agent %q", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("If-None-Match") != "" {
		t.Errorf("If-None-Match без etag: %q", req.Header.Get("If-None-Match"))
	}

	res, err = Fetch(ctx, srv.Client(), srv.URL, `"v1"`)
	if err != nil {
		t.Fatalf("Fetch с ETag: %v", err)
	}
	if !res.NotModified || res.Data != nil || res.ETag != `"v1"` {
		t.Errorf("304: %+v", res)
	}
	if got := arch.last().Header.Get("If-None-Match"); got != `"v1"` {
		t.Errorf("If-None-Match %q", got)
	}

	// Устаревший ETag — архив приходит заново.
	res, err = Fetch(ctx, srv.Client(), srv.URL, `"v0"`)
	if err != nil || res.NotModified || res.ETag != `"v1"` {
		t.Errorf("старый ETag: %+v, %v", res, err)
	}
}

func TestFetchErrors(t *testing.T) {
	ctx := context.Background()

	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()
	if _, err := Fetch(ctx, nil, notFound.URL, ""); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: %v", err)
	}

	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.CopyN(w, zeroReader{}, maxArchiveBytes+1)
	}))
	defer huge.Close()
	if _, err := Fetch(ctx, huge.Client(), huge.URL, ""); err == nil || !strings.Contains(err.Error(), "МБ") {
		t.Errorf("слишком большой архив: %v", err)
	}

	// Ровно на пределе — ещё можно.
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.CopyN(w, zeroReader{}, maxArchiveBytes)
	}))
	defer edge.Close()
	if res, err := Fetch(ctx, edge.Client(), edge.URL, ""); err != nil || len(res.Data) != maxArchiveBytes {
		t.Errorf("архив на пределе: %d байт, %v", len(res.Data), err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := Fetch(tctx, slow.Client(), slow.URL, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("таймаут: %v", err)
	}

	if _, err := Fetch(ctx, nil, "://плохой", ""); err == nil {
		t.Error("плохой URL без ошибки")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// fakeStore — хранилище в тесте Sync: помнит релиз и считает замены.
// Остальные методы Store синхронизации не нужны.
type fakeStore struct {
	mu         sync.Mutex
	dataset    *Dataset
	replaces   int
	releaseErr error
}

func (f *fakeStore) Release(context.Context) (Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releaseErr != nil {
		return Release{}, f.releaseErr
	}
	if f.dataset == nil {
		return Release{}, ErrNotFound
	}
	return f.dataset.Release, nil
}

func (f *fakeStore) Replace(_ context.Context, d *Dataset) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dataset = d
	f.replaces++
	return nil
}

func (f *fakeStore) Get(context.Context, int) (Species, error) { return Species{}, ErrNotFound }
func (f *fakeStore) Find(context.Context, string) (Species, error) {
	return Species{}, ErrNotFound
}
func (f *fakeStore) Search(context.Context, Query) ([]Species, int, error) { return nil, 0, nil }
func (f *fakeStore) Changes(context.Context, string, int) ([]Change, error) {
	return nil, nil
}
func (f *fakeStore) IDs(context.Context) ([]int, error) { return nil, nil }

var _ Store = (*fakeStore)(nil)

// loaderNextRelease — мини-архив, выданный за следующий релиз v2.6.
func loaderNextRelease(t *testing.T) []byte {
	t.Helper()
	files := loaderMiniFiles(t)
	files["MDD/release.toml"] = bytes.ReplaceAll(files["MDD/release.toml"], []byte(`version = "v2.5"`), []byte(`version = "v2.6"`))
	files["MDD/Diff_v2.5-v2.6.csv"] = bytes.ReplaceAll(files["MDD/Diff_v2.4-v2.5.csv"], []byte("MDDv2.4_Name,MDDv2.5_Name"), []byte("MDDv2.5_Name,MDDv2.6_Name"))
	delete(files, "MDD/Diff_v2.4-v2.5.csv")
	files["MDD/MDD_v2.6_12species.csv"] = files["MDD/MDD_v2.5_12species.csv"]
	delete(files, "MDD/MDD_v2.5_12species.csv")
	return loaderZip(t, files)
}

func TestSync(t *testing.T) {
	arch := &archiveServer{}
	arch.set(loaderMiniZip(t), `"a"`)
	srv := httptest.NewServer(arch)
	defer srv.Close()

	ctx := context.Background()
	st := &fakeStore{}
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	opts := SyncOptions{URL: srv.URL, HTTP: srv.Client(), Now: func() time.Time { return now }}

	// Первая загрузка: базы нет.
	res, err := Sync(ctx, st, opts)
	if err != nil {
		t.Fatalf("первая загрузка: %v", err)
	}
	if !res.Updated || res.Prev != nil || st.replaces != 1 {
		t.Fatalf("первая загрузка: %+v, замен %d", res, st.replaces)
	}
	r := res.Release
	if r.Version != "v2.5" || r.ETag != `"a"` || r.SourceURL != srv.URL || !r.LoadedAt.Equal(now) || r.Species != 12 {
		t.Errorf("релиз: %+v", r)
	}
	if st.dataset.Release != r || len(st.dataset.Species) != 12 {
		t.Errorf("в хранилище не то: %+v", st.dataset.Release)
	}
	if got := arch.last().Header.Get("If-None-Match"); got != "" {
		t.Errorf("If-None-Match без базы: %q", got)
	}

	// Повтор: архив не менялся — 304, хранилище не трогаем.
	now = now.Add(time.Hour)
	res, err = Sync(ctx, st, opts)
	if err != nil {
		t.Fatalf("повтор: %v", err)
	}
	if res.Updated || st.replaces != 1 || res.Release != r || res.Prev == nil || *res.Prev != r {
		t.Errorf("повтор: %+v, замен %d", res, st.replaces)
	}
	if got := arch.last().Header.Get("If-None-Match"); got != `"a"` {
		t.Errorf("If-None-Match %q", got)
	}

	// Вышел новый релиз.
	arch.set(loaderNextRelease(t), `"b"`)
	res, err = Sync(ctx, st, opts)
	if err != nil {
		t.Fatalf("новый релиз: %v", err)
	}
	if !res.Updated || st.replaces != 2 || res.Prev == nil {
		t.Fatalf("новый релиз: %+v, замен %d", res, st.replaces)
	}
	if res.Prev.Version != "v2.5" || res.Release.Version != "v2.6" || res.Release.PrevVersion != "v2.5" || res.Release.ETag != `"b"` {
		t.Errorf("v2.5 → v2.6: prev %+v, release %+v", *res.Prev, res.Release)
	}
	if !res.Release.LoadedAt.Equal(now) || len(st.dataset.Changes) != 5 {
		t.Errorf("релиз %+v, изменений %d", res.Release, len(st.dataset.Changes))
	}

	// ETag сменился, а релиз тот же: данные всё равно перезаписываются.
	arch.set(loaderNextRelease(t), `"c"`)
	res, err = Sync(ctx, st, opts)
	if err != nil {
		t.Fatalf("тот же релиз: %v", err)
	}
	if !res.Updated || st.replaces != 3 || res.Prev.Version != res.Release.Version || res.Release.ETag != `"c"` {
		t.Errorf("тот же релиз: %+v, замен %d", res, st.replaces)
	}
}

func TestSyncErrors(t *testing.T) {
	ctx := context.Background()

	// Битый архив: хранилище остаётся прежним.
	arch := &archiveServer{}
	arch.set([]byte("<html>rate limited</html>"), `"x"`)
	srv := httptest.NewServer(arch)
	defer srv.Close()
	st := &fakeStore{}
	if _, err := Sync(ctx, st, SyncOptions{URL: srv.URL, HTTP: srv.Client()}); err == nil || st.replaces != 0 {
		t.Errorf("битый архив: %v, замен %d", err, st.replaces)
	}

	// Хранилище не отвечает — в сеть не идём.
	broken := &fakeStore{releaseErr: errors.New("база заперта")}
	n := len(arch.requests)
	if _, err := Sync(ctx, broken, SyncOptions{URL: srv.URL, HTTP: srv.Client()}); err == nil || !strings.Contains(err.Error(), "база заперта") {
		t.Errorf("ошибка хранилища: %v", err)
	}
	if len(arch.requests) != n {
		t.Error("при ошибке хранилища ушёл запрос")
	}

	// Сервер ответил ошибкой.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "нет", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	if _, err := Sync(ctx, &fakeStore{}, SyncOptions{URL: down.URL, HTTP: down.Client()}); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("503: %v", err)
	}
}

// TestSyncOtherURLIgnoresETag — архив сменил адрес: ETag старого адреса
// не посылается, иначе чужой 304 оставил бы базу как есть.
func TestSyncOtherURLIgnoresETag(t *testing.T) {
	arch := &archiveServer{}
	arch.set(loaderMiniZip(t), `"a"`)
	srv := httptest.NewServer(arch)
	defer srv.Close()

	st := &fakeStore{dataset: &Dataset{Release: Release{Version: "v2.4", ETag: `"a"`, SourceURL: "https://old.example/MDD.zip"}}}
	res, err := Sync(context.Background(), st, SyncOptions{URL: srv.URL, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.Prev.Version != "v2.4" || res.Release.Version != "v2.5" {
		t.Errorf("смена адреса: %+v", res)
	}
	if got := arch.last().Header.Get("If-None-Match"); got != "" {
		t.Errorf("ушёл ETag старого адреса: %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestSyncDefaultURL — пустой URL означает DefaultURL; в сеть тест не ходит.
func TestSyncDefaultURL(t *testing.T) {
	var got string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.URL.String()
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable",
			Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
	})}
	if _, err := Sync(context.Background(), &fakeStore{}, SyncOptions{HTTP: client}); err == nil {
		t.Error("503 без ошибки")
	}
	if got != DefaultURL {
		t.Errorf("запрос ушёл на %q", got)
	}
}
