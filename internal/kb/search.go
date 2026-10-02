package kb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
)

// DefaultK — сколько чанков отдаёт поиск по умолчанию.
const DefaultK = 5

// denseIndex — векторы индекса в памяти. builtAt — версия: индекс
// пересобран — векторы перечитываются.
type denseIndex struct {
	builtAt time.Time
	chunks  []Chunk
	vecs    [][]float32
}

// Search ищет по одному индексу. Mode пусто или Dense — векторный поиск с
// откатом на BM25; причина отката — в SearchInfo.Fallback, и ошибкой она не
// считается: ответ по BM25 лучше, чем никакой. Ошибка — только когда не
// работает сама база (или вызывающий отменил контекст).
func (s *Searcher) Search(ctx context.Context, query string, o SearchOptions) ([]Hit, SearchInfo, error) {
	started := time.Now()
	k := o.K
	if k <= 0 {
		k = DefaultK
	}
	info := SearchInfo{Index: o.Index}
	idx, err := s.Store.Index(ctx, o.Index)
	if err != nil {
		return nil, info, err
	}
	if strings.TrimSpace(query) == "" {
		return nil, info, errors.New("пустой запрос")
	}

	var hits []Hit
	switch {
	case o.Mode == BM25:
		// BM25 попросили явно — это не откат.
	case o.Mode != "" && o.Mode != Dense:
		return nil, info, fmt.Errorf("неизвестный режим поиска %q", o.Mode)
	case s.Embedder == nil:
		info.Fallback = "эмбеддер не задан"
	case idx.Embedder == "" || idx.Dims == 0:
		info.Fallback = "индекс построен без векторов"
	case idx.Embedder != s.Embedder.Model():
		info.Fallback = fmt.Sprintf("индекс построен моделью %s, а эмбеддер — %s", idx.Embedder, s.Embedder.Model())
	default:
		info.Embedder = s.Embedder.Model()
		hits, err = s.searchDense(ctx, idx, query, k)
		switch {
		case err == nil:
			info.Mode = Dense
		case ctx.Err() != nil:
			return nil, info, ctx.Err()
		default:
			info.Fallback = "эмбеддер недоступен: " + err.Error()
		}
	}
	if info.Mode == "" {
		info.Mode = BM25
		hits, err = s.bm25(ctx, idx.ID, query, k)
		if err != nil {
			return nil, info, err
		}
	}
	info.Millis = float64(time.Since(started).Microseconds()) / 1000
	return hits, info, nil
}

// searchDense — перебор косинуса по всем векторам индекса. Вектор вопроса
// кэшируется в kb_embed_cache, как и векторы чанков: повторная оценка
// поиска совпадает побитно и не ходит в модель.
func (s *Searcher) searchDense(ctx context.Context, idx IndexInfo, query string, k int) ([]Hit, error) {
	di, err := s.load(ctx, idx)
	if err != nil {
		return nil, err
	}
	q, err := s.queryVec(ctx, idx, query)
	if err != nil {
		return nil, err
	}
	type scored struct {
		i int
		s float32
	}
	all := make([]scored, 0, len(di.vecs))
	for i, v := range di.vecs {
		if v == nil {
			continue
		}
		all = append(all, scored{i, embed.Dot(q, v)})
	}
	// Стабильная сортировка с разбором ничьих по порядку чанков:
	// одинаковый вопрос — одинаковая выдача.
	sort.SliceStable(all, func(a, b int) bool { return all[a].s > all[b].s })
	if len(all) > k {
		all = all[:k]
	}
	hits := make([]Hit, 0, len(all))
	for r, x := range all {
		hits = append(hits, Hit{Chunk: di.chunks[x.i], Score: float64(x.s), Rank: r + 1})
	}
	return hits, nil
}

// queryVec — вектор вопроса через кэш kb_embed_cache.
func (s *Searcher) queryVec(ctx context.Context, idx IndexInfo, query string) ([]float32, error) {
	var e embed.Embedder = s.Embedder
	if _, ok := e.(*embed.Cached); !ok {
		// Свой Cached на вызов: счётчики Cached не потокобезопасны.
		e = &embed.Cached{E: s.Embedder, C: s.Store}
	}
	qv, err := e.Embed(ctx, embed.Query, []string{query})
	if err != nil {
		return nil, err
	}
	if len(qv) != 1 {
		return nil, fmt.Errorf("на вопрос пришло %d векторов", len(qv))
	}
	if len(qv[0]) != idx.Dims {
		return nil, fmt.Errorf("вектор вопроса размерности %d, у индекса %d", len(qv[0]), idx.Dims)
	}
	return qv[0], nil
}

// Score — косинус запроса с заданными чанками индекса (v23, добавление).
// Нужен второму этапу поиска (internal/retrieve): кандидат, которого нашёл
// только BM25, не имеет косинуса в dense-выдаче, а фильтр релевантности
// судит всех кандидатов одной шкалой. Перебор не нужен — только скалярные
// произведения с векторами из того же кэша в памяти, что у Search.
//
// Чанка нет в индексе или у него нет вектора — его нет и в ответе.
// Векторного поиска нет (эмбеддер не задан или недоступен, индекс другой
// модели) — ошибка: вызывающий откатывается на BM25-шкалу сам.
func (s *Searcher) Score(ctx context.Context, index, query string, chunkIDs []string) (map[string]float64, error) {
	idx, err := s.Store.Index(ctx, index)
	if err != nil {
		return nil, err
	}
	switch {
	case s.Embedder == nil:
		return nil, errors.New("эмбеддер не задан")
	case idx.Embedder == "" || idx.Dims == 0:
		return nil, errors.New("индекс построен без векторов")
	case idx.Embedder != s.Embedder.Model():
		return nil, fmt.Errorf("индекс построен моделью %s, а эмбеддер — %s", idx.Embedder, s.Embedder.Model())
	}
	out := make(map[string]float64, len(chunkIDs))
	if len(chunkIDs) == 0 {
		return out, nil
	}
	di, err := s.load(ctx, idx)
	if err != nil {
		return nil, err
	}
	q, err := s.queryVec(ctx, idx, query)
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(chunkIDs))
	for _, id := range chunkIDs {
		want[id] = true
	}
	for i, c := range di.chunks {
		if want[c.ID] && di.vecs[i] != nil {
			out[c.ID] = float64(embed.Dot(q, di.vecs[i]))
		}
	}
	return out, nil
}

// load — векторы индекса из кэша в памяти или из базы. Под мьютексом:
// два одновременных первых поиска не читают индекс дважды.
func (s *Searcher) load(ctx context.Context, idx IndexInfo) (*denseIndex, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if di, ok := s.dense[idx.ID]; ok && di.builtAt.Equal(idx.BuiltAt) {
		return di, nil
	}
	rows, err := s.Store.db.QueryContext(ctx, `SELECT `+chunkCols+`, vec FROM kb_chunks c WHERE index_id = ?
		ORDER BY (SELECT ord FROM kb_docs d WHERE d.doc_id = c.doc_id), ord`, idx.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	di := &denseIndex{builtAt: idx.BuiltAt}
	for rows.Next() {
		var blob []byte
		c, err := scanChunk(rows, &blob)
		if err != nil {
			return nil, err
		}
		var v []float32
		if len(blob) > 0 {
			if v, err = decodeVec(blob); err != nil {
				return nil, fmt.Errorf("чанк %s: %w", c.ID, err)
			}
		}
		di.chunks = append(di.chunks, c)
		di.vecs = append(di.vecs, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if s.dense == nil {
		s.dense = map[string]*denseIndex{}
	}
	s.dense[idx.ID] = di
	return di, nil
}

// bm25 — лексический поиск FTS5 по таблице индекса. bm25() в SQLite отрицателен и «лучше —
// меньше», поэтому балл — его минус, нормированный к лучшему в выдаче.
func (s *Searcher) bm25(ctx context.Context, index, query string, k int) ([]Hit, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	t, err := ftsTable(index)
	if err != nil {
		return nil, err
	}
	// Таблица FTS своя у индекса: IDF и средняя длина — по его чанкам, и
	// ранги не зависят от того, какие ещё индексы есть в базе.
	rows, err := s.Store.db.QueryContext(ctx, `SELECT chunk_id, bm25(`+t+`) AS score
		FROM `+t+` WHERE `+t+` MATCH ?
		ORDER BY score, chunk_id LIMIT ?`, match, k)
	if err != nil {
		return nil, fmt.Errorf("BM25: %w", err)
	}
	type row struct {
		id    string
		score float64
	}
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.score); err != nil {
			rows.Close()
			return nil, err
		}
		found = append(found, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(found))
	for i, r := range found {
		c, err := s.Store.Chunk(ctx, r.id)
		if err != nil {
			return nil, err
		}
		score := 0.0
		if best := -found[0].score; best > 0 {
			score = -r.score / best
		}
		hits = append(hits, Hit{Chunk: c, Score: score, Rank: i + 1})
	}
	return hits, nil
}

// stopwords — служебные и вопросительные слова: в запросе BM25 они только
// шумят (триграммы «как» есть в каждом третьем чанке).
var stopwords = map[string]bool{
	"как": true, "что": true, "чем": true, "кто": true, "где": true, "когда": true, "какой": true,
	"какая": true, "какие": true, "какое": true, "каких": true, "каков": true, "какова": true,
	"каково": true, "каковы": true, "сколько": true, "почему": true, "зачем": true, "ли": true,
	"для": true, "при": true, "его": true, "она": true, "они": true, "оно": true, "это": true,
	"этот": true, "эта": true, "эти": true, "или": true, "так": true, "также": true, "был": true,
	"была": true, "были": true, "было": true, "есть": true, "все": true, "весь": true, "без": true,
	"над": true, "под": true, "про": true, "чтобы": true, "между": true, "from": true, "the": true,
	"and": true, "них": true, "ему": true, "ней": true, "там": true,
}

// ftsQuery — безопасный запрос FTS5 из вопроса: слова (буквы и цифры),
// нижний регистр, ё → е, без служебных; длинные слова обрезаются до основы
// (до 6 букв, но не короче 4 и минус два знака окончания), чтобы
// подстрочный trigram находил другие падежи. Каждое слово — в двойных
// кавычках: так ни одно слово запроса не станет оператором FTS5 (NEAR, OR,
// «-», «*», «:»), а кавычек внутри нет — их отрезало разбиение на слова.
// Слова соединяются OR: BM25 сам поднимет чанки, где совпало больше.
// Trigram не ищет термы короче трёх символов — такие слова отбрасываются.
func ftsQuery(q string) string {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := map[string]bool{}
	var terms []string
	for _, f := range fields {
		f = strings.ReplaceAll(f, "ё", "е")
		if stopwords[f] {
			continue
		}
		r := []rune(f)
		if len(r) > 4 {
			n := min(max(len(r)-2, 4), 6)
			r = r[:n]
		}
		if len(r) < 3 {
			continue
		}
		t := string(r)
		if !seen[t] {
			seen[t] = true
			terms = append(terms, `"`+t+`"`)
		}
	}
	return strings.Join(terms, " OR ")
}
