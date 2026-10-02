package mdd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// SyncOptions — параметры синхронизации. Нулевое значение годится: архив
// берётся по DefaultURL клиентом по умолчанию.
type SyncOptions struct {
	URL  string       // пусто — DefaultURL
	HTTP *http.Client // nil — клиент Fetch по умолчанию
	Now  func() time.Time
}

// SyncResult — итог синхронизации.
type SyncResult struct {
	// Updated — набор данных в хранилище заменён.
	Updated bool
	// Release — релиз, который теперь в хранилище.
	Release Release
	// Prev — релиз, который был в хранилище до синхронизации; nil — базы
	// не было. По Prev.Version и Release.Version видно, вышел ли новый
	// релиз («v2.5 → v2.6») или сменился только ETag архива.
	Prev *Release
}

// Sync сверяет хранилище с архивом MDD: спрашивает архив с ETag текущего
// релиза и, если архив изменился, разбирает его и заменяет набор данных
// целиком. Архив, пришедший с той же версией, тоже загружается: сменился
// ETag — значит, поправили данные, даже если номер релиза прежний.
func Sync(ctx context.Context, st Store, o SyncOptions) (SyncResult, error) {
	url := o.URL
	if url == "" {
		url = DefaultURL
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}

	var res SyncResult
	cur, err := st.Release(ctx)
	switch {
	case err == nil:
		res.Prev = &cur
	case errors.Is(err, ErrNotFound):
	default:
		return res, fmt.Errorf("текущий релиз MDD: %w", err)
	}

	// ETag относится к конкретному адресу: если архив теперь берётся из
	// другого места, старая метка ничего не говорит, и нужен полный запрос.
	etag := ""
	if res.Prev != nil && (res.Prev.SourceURL == "" || res.Prev.SourceURL == url) {
		etag = res.Prev.ETag
	}

	fr, err := Fetch(ctx, o.HTTP, url, etag)
	if err != nil {
		return res, err
	}
	if fr.NotModified {
		if res.Prev == nil {
			// Без ETag в запросе 304 прийти не должен — сервер ведёт себя
			// странно, и молча считать базу актуальной нельзя.
			return res, fmt.Errorf("архив MDD %s: ответ 304 без запроса по ETag, а базы нет", url)
		}
		res.Release = *res.Prev
		return res, nil
	}

	d, err := Parse(fr.Data)
	if err != nil {
		return res, err
	}
	d.Release.ETag = fr.ETag
	d.Release.SourceURL = url
	d.Release.LoadedAt = now()
	if err := st.Replace(ctx, d); err != nil {
		return res, fmt.Errorf("запись релиза MDD %s: %w", d.Release.Version, err)
	}
	res.Updated = true
	res.Release = d.Release
	return res, nil
}
