package mdd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/tools"
)

const (
	// fetchTimeout — архив весит 15 МБ, на медленном канале это минуты;
	// обычные 30 секунд инструментов здесь оборвали бы загрузку.
	fetchTimeout = 10 * time.Minute
	// maxArchiveBytes — предел тела ответа. Архив v2.5 — 15 МБ; запас на
	// рост в четыре раза, но не бесконечный: вместо архива может прийти
	// что угодно.
	maxArchiveBytes = 64 << 20
)

// FetchResult — ответ на запрос архива.
type FetchResult struct {
	Data []byte // тело архива; пусто при NotModified
	// ETag — метка полученного архива; при NotModified — та же, что спрашивали.
	ETag        string
	NotModified bool // сервер ответил 304: архив не менялся
}

// Fetch скачивает архив MDD. Непустой etag уходит в If-None-Match: если
// архив не менялся, сервер отвечает 304 без тела, и проверка «не вышел ли
// релиз» стоит одного короткого запроса. client == nil — клиент с таймаутом
// в минуты; таймаут можно задать и через ctx.
func Fetch(ctx context.Context, client *http.Client, url, etag string) (FetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return FetchResult{}, fmt.Errorf("запрос архива MDD: %w", err)
	}
	req.Header.Set("User-Agent", tools.DefaultUserAgent)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return FetchResult{}, fmt.Errorf("загрузка архива MDD %s: %w", url, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		res := FetchResult{ETag: resp.Header.Get("ETag"), NotModified: true}
		if res.ETag == "" {
			res.ETag = etag
		}
		return res, nil
	case http.StatusOK:
	default:
		return FetchResult{}, fmt.Errorf("архив MDD %s: сервер ответил %s", url, resp.Status)
	}

	// Читаем на байт больше предела: так превышение отличается от архива
	// ровно предельного размера, и обрезанный zip не уходит в разбор.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return FetchResult{}, fmt.Errorf("чтение архива MDD %s: %w", url, err)
	}
	if len(data) > maxArchiveBytes {
		return FetchResult{}, fmt.Errorf("архив MDD %s больше %d МБ — это не похоже на MDD", url, maxArchiveBytes>>20)
	}
	return FetchResult{Data: data, ETag: resp.Header.Get("ETag")}, nil
}
