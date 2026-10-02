package kb

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/embed"
)

// component — имя компонента миграций в общем internal/db.
const component = "kb"

// migrations — схема kb.db. Шаги только добавляются в конец.
//
// FTS — обычная (не external content) таблица FTS5: текст в ней — это
// EmbedText чанка (заголовок статьи и путь раздела плюс текст), то есть не
// то, что лежит в kb_chunks.text, и синхронизировать её триггерами было бы
// нечем. Пара мегабайт дубля — дешевле, чем хитрость. Токенизатор trigram
// ищет подстроки, поэтому «манул» находит и «манула», и «манулов» — для
// русского без стеммера это лучшее, что есть в стандартном SQLite.
//
// Шаг 2: общая kb_fts заменяется таблицами kb_fts_<index_id> — своя на
// индекс. bm25() считает IDF и среднюю длину документа по всей таблице, и
// в общей таблице ранги BM25 индекса structure зависели от того, собран ли
// рядом fixed. Таблицы индексов создаёт Build (и Open — для индексов,
// собранных до шага 2: их текст FTS восстанавливается из kb_chunks).
var migrations = []string{`
CREATE TABLE kb_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE kb_docs (
	doc_id  TEXT PRIMARY KEY,
	ord     INTEGER NOT NULL,
	source  TEXT NOT NULL,
	title   TEXT NOT NULL,
	url     TEXT NOT NULL,
	revid   INTEGER NOT NULL DEFAULT 0,
	license TEXT NOT NULL,
	chars   INTEGER NOT NULL,
	sha256  TEXT NOT NULL,
	doc     TEXT NOT NULL
);
CREATE TABLE kb_indexes (
	index_id   TEXT PRIMARY KEY,
	strategy   TEXT NOT NULL,
	params     TEXT NOT NULL,
	embedder   TEXT NOT NULL,
	dims       INTEGER NOT NULL,
	corpus_sha TEXT NOT NULL,
	chunks     INTEGER NOT NULL,
	tokens     INTEGER NOT NULL,
	built_at   TEXT NOT NULL,
	seconds    REAL NOT NULL,
	min_score  REAL NOT NULL DEFAULT 0
);
CREATE TABLE kb_chunks (
	chunk_id TEXT PRIMARY KEY,
	index_id TEXT NOT NULL,
	doc_id   TEXT NOT NULL,
	ord      INTEGER NOT NULL,
	source   TEXT NOT NULL,
	title    TEXT NOT NULL,
	section  TEXT NOT NULL,
	path     TEXT NOT NULL,
	start    INTEGER NOT NULL,
	"end"    INTEGER NOT NULL,
	text     TEXT NOT NULL,
	mixed    INTEGER NOT NULL,
	tokens   INTEGER NOT NULL,
	sha      TEXT NOT NULL,
	url      TEXT NOT NULL,
	revid    INTEGER NOT NULL DEFAULT 0,
	vec      BLOB
);
CREATE INDEX kb_chunks_doc ON kb_chunks (index_id, doc_id, ord);
CREATE TABLE kb_embed_cache (
	model TEXT NOT NULL,
	sha   TEXT NOT NULL,
	vec   BLOB NOT NULL,
	PRIMARY KEY (model, sha)
);
CREATE VIRTUAL TABLE kb_fts USING fts5 (
	text,
	chunk_id UNINDEXED,
	index_id UNINDEXED,
	tokenize = 'trigram'
);
CREATE TABLE kb_reports (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	created TEXT NOT NULL,
	report  TEXT NOT NULL
);
`, `
DROP TABLE kb_fts;
`}

// Open открывает (создаёт) kb.db и применяет миграции компонента "kb".
func Open(ctx context.Context, path string) (*Store, error) {
	d, err := db.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, d, component, migrations); err != nil {
		d.Close()
		return nil, err
	}
	s := &Store{db: d}
	if err := s.restoreFTS(ctx); err != nil {
		d.Close()
		return nil, fmt.Errorf("FTS индексов: %w", err)
	}
	return s, nil
}

// OpenCache открывает чужую kb.db только для чтения — как общий кэш
// эмбеддингов (embed.Layered.Shared): без миграций и без записи, файл
// остаётся как был. Пользоваться можно только GetVec.
func OpenCache(ctx context.Context, path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	d, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	var n int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'kb_embed_cache'`).Scan(&n); err != nil {
		d.Close()
		return nil, fmt.Errorf("кэш %s: %w", path, err)
	}
	if n == 0 {
		d.Close()
		return nil, fmt.Errorf("в %s нет kb_embed_cache — это не база знаний", path)
	}
	return &Store{db: d}, nil
}

// CacheSize — сколько векторов модели в кэше эмбеддингов.
func (s *Store) CacheSize(ctx context.Context, model string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM kb_embed_cache WHERE model = ?`, model).Scan(&n)
	return n, err
}

// ftsTable — имя таблицы FTS индекса. id индекса — имя стратегии; в имя
// таблицы попадают только [a-z0-9_], иначе ошибка (имя таблицы в SQL
// вставляется строкой: параметром его не передать).
func ftsTable(id string) (string, error) {
	if id == "" {
		return "", errors.New("пустой id индекса")
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return "", fmt.Errorf("id индекса %q: допустимы a-z, 0-9 и _", id)
		}
	}
	return "kb_fts_" + id, nil
}

// querier — общее у *sql.DB и *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// fillFTS пересоздаёт таблицу FTS индекса и кладёт в неё EmbedText чанков.
func fillFTS(ctx context.Context, tx querier, id string, chunks []Chunk) error {
	t, err := ftsTable(id)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+t); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE VIRTUAL TABLE `+t+` USING fts5 (
		text,
		chunk_id UNINDEXED,
		tokenize = 'trigram'
	)`); err != nil {
		return err
	}
	for _, c := range chunks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+t+` (text, chunk_id) VALUES (?, ?)`, c.EmbedText(), c.ID); err != nil {
			return fmt.Errorf("чанк %s в FTS: %w", c.ID, err)
		}
	}
	return nil
}

// restoreFTS — таблицы FTS индексов, собранных до шага миграции 2 (или
// потерянных): текст FTS — EmbedText чанка, он целиком восстанавливается
// из kb_chunks, пересобирать индекс и кодировать заново не нужно.
func (s *Store) restoreFTS(ctx context.Context) error {
	ids, err := indexIDs(ctx, s.db)
	if err != nil {
		return err
	}
	for _, id := range ids {
		t, err := ftsTable(id)
		if err != nil {
			return err
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, t).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		chunks, err := s.Chunks(ctx, id, "")
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := fillFTS(ctx, tx, id, chunks); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// indexIDs — id индексов базы.
func indexIDs(ctx context.Context, q querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT index_id FROM kb_indexes ORDER BY index_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Close закрывает базу.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// PutCorpus заменяет документы базы снимком корпуса (одной транзакцией).
// Если corpus_sha сменился, прежние индексы удаляются: их смещения и
// тексты относятся к другому корпусу, и искать по ним — значит цитировать
// то, чего в базе уже нет.
func (s *Store) PutCorpus(ctx context.Context, docs []corpus.Doc, m corpus.Manifest) error {
	old, err := s.meta(ctx, "corpus_sha")
	if err != nil {
		// Не знаем прежний corpus_sha — не знаем, устарели ли индексы:
		// лучше отказать, чем оставить индексы чужого корпуса.
		return fmt.Errorf("прежний corpus_sha: %w", err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_docs`); err != nil {
		return err
	}
	for i, d := range docs {
		body, err := json.Marshal(d)
		if err != nil {
			return err
		}
		sha := ""
		for _, e := range m.Entries {
			if e.ID == d.ID {
				sha = e.SHA256
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_docs
			(doc_id, ord, source, title, url, revid, license, chars, sha256, doc)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.ID, i, d.Source, d.Title, d.URL, d.RevID, d.License, d.Chars(), sha, string(body)); err != nil {
			return fmt.Errorf("документ %s: %w", d.ID, err)
		}
	}
	if old != "" && old != m.CorpusSHA {
		ids, err := indexIDs(ctx, tx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if t, err := ftsTable(id); err == nil {
				if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+t); err != nil {
					return err
				}
			}
		}
		for _, q := range []string{`DELETE FROM kb_chunks`, `DELETE FROM kb_indexes`} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	for k, v := range map[string]string{"corpus_sha": m.CorpusSHA, "manifest": string(raw)} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_meta (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kb_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Manifest — манифест корпуса, из которого собрана база (пустой, если
// корпус ещё не загружен).
func (s *Store) Manifest(ctx context.Context) (corpus.Manifest, error) {
	var m corpus.Manifest
	raw, err := s.meta(ctx, "manifest")
	if err != nil || raw == "" {
		return m, err
	}
	err = json.Unmarshal([]byte(raw), &m)
	return m, err
}

// Docs — документы базы в порядке манифеста.
func (s *Store) Docs(ctx context.Context) ([]DocInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT doc_id, source, title, url, revid, license, chars, sha256
		FROM kb_docs ORDER BY ord`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocInfo
	for rows.Next() {
		var d DocInfo
		if err := rows.Scan(&d.ID, &d.Source, &d.Title, &d.URL, &d.RevID, &d.License, &d.Chars, &d.SHA256); err != nil {
			return nil, err
		}
		d.Pages = math.Round(float64(d.Chars)/corpus.PageChars*10) / 10
		out = append(out, d)
	}
	return out, rows.Err()
}

// Doc — документ целиком.
func (s *Store) Doc(ctx context.Context, id string) (corpus.Doc, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT doc FROM kb_docs WHERE doc_id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return corpus.Doc{}, fmt.Errorf("%w: %s", ErrNoDoc, id)
	}
	if err != nil {
		return corpus.Doc{}, err
	}
	var d corpus.Doc
	err = json.Unmarshal([]byte(raw), &d)
	return d, err
}

// allDocs — все документы целиком, в порядке манифеста.
func (s *Store) allDocs(ctx context.Context) ([]corpus.Doc, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT doc FROM kb_docs ORDER BY ord`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []corpus.Doc
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var d corpus.Doc
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// buildBatch — сколько чанков кодируется между отметками прогресса. Батчи
// по сети режет сам эмбеддер; здесь — только частота отчёта и объём
// потерь при отмене.
const buildBatch = 64

// Build режет документы базы стратегией, кодирует чанки (через кэш
// kb_embed_cache) и заменяет индекс этой стратегии одной транзакцией.
// emb == nil — индекс без векторов (только BM25).
//
// Если emb уже *embed.Cached, он используется как есть (без кэша — с
// кэшем этой базы): так вызывающий видит счётчики попаданий и промахов.
// Кодирование идёт до транзакции: оно длится минуты, а замена индекса —
// доли секунды, и поиск всё это время видит прежний индекс целиком.
func (s *Store) Build(ctx context.Context, ch Chunker, emb embed.Embedder, p Progress) (IndexInfo, error) {
	started := time.Now()
	docs, err := s.allDocs(ctx)
	if err != nil {
		return IndexInfo{}, err
	}
	if len(docs) == 0 {
		return IndexInfo{}, errors.New("в базе нет документов: сначала PutCorpus")
	}
	m, err := s.Manifest(ctx)
	if err != nil {
		return IndexInfo{}, err
	}
	var chunks []Chunk
	for _, d := range docs {
		chunks = append(chunks, ch.Split(d)...)
	}
	info := IndexInfo{
		ID: string(ch.Strategy()), Strategy: ch.Strategy(), Params: ch.Params(),
		CorpusSHA: m.CorpusSHA, Chunks: len(chunks),
	}
	for _, c := range chunks {
		info.Tokens += c.Tokens
	}

	vecs := make([][]float32, len(chunks))
	if emb != nil {
		cached, ok := emb.(*embed.Cached)
		if !ok {
			cached = &embed.Cached{E: emb, C: s}
		} else if cached.C == nil {
			cached.C = s
		}
		if p != nil {
			p(0, len(chunks))
		}
		for start := 0; start < len(chunks); start += buildBatch {
			end := min(start+buildBatch, len(chunks))
			texts := make([]string, 0, end-start)
			for _, c := range chunks[start:end] {
				texts = append(texts, c.EmbedText())
			}
			vs, err := cached.Embed(ctx, embed.Passage, texts)
			if err != nil {
				return IndexInfo{}, fmt.Errorf("индекс %s: кодирование: %w", info.ID, err)
			}
			copy(vecs[start:end], vs)
			if p != nil {
				p(end, len(chunks))
			}
		}
		info.Embedder = cached.Model()
		if len(vecs) > 0 {
			info.Dims = len(vecs[0])
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IndexInfo{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_chunks WHERE index_id = ?`, info.ID); err != nil {
		return IndexInfo{}, err
	}
	insChunk, err := tx.PrepareContext(ctx, `INSERT INTO kb_chunks
		(chunk_id, index_id, doc_id, ord, source, title, section, path, start, "end", text, mixed, tokens, sha, url, revid, vec)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return IndexInfo{}, err
	}
	defer insChunk.Close()
	for i, c := range chunks {
		path, _ := json.Marshal(c.Path)
		var blob []byte
		if vecs[i] != nil {
			blob = encodeVec(vecs[i])
		}
		if _, err := insChunk.ExecContext(ctx, c.ID, info.ID, c.DocID, c.Ord, c.Source, c.Title, c.Section,
			string(path), c.Start, c.End, c.Text, c.Mixed, c.Tokens, c.SHA, c.URL, c.RevID, blob); err != nil {
			return IndexInfo{}, fmt.Errorf("чанк %s: %w", c.ID, err)
		}
	}
	// FTS — своя таблица на индекс, пересоздаётся целиком: статистика BM25
	// (IDF, средняя длина) — только по чанкам этого индекса.
	if err := fillFTS(ctx, tx, info.ID, chunks); err != nil {
		return IndexInfo{}, err
	}
	info.BuiltAt = time.Now().UTC()
	info.Seconds = time.Since(started).Seconds()
	params, _ := json.Marshal(info.Params)
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_indexes
		(index_id, strategy, params, embedder, dims, corpus_sha, chunks, tokens, built_at, seconds, min_score)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT (index_id) DO UPDATE SET strategy = excluded.strategy, params = excluded.params,
			embedder = excluded.embedder, dims = excluded.dims, corpus_sha = excluded.corpus_sha,
			chunks = excluded.chunks, tokens = excluded.tokens, built_at = excluded.built_at,
			seconds = excluded.seconds, min_score = 0`,
		info.ID, string(info.Strategy), string(params), info.Embedder, info.Dims, info.CorpusSHA,
		info.Chunks, info.Tokens, info.BuiltAt.Format(time.RFC3339Nano), info.Seconds); err != nil {
		return IndexInfo{}, err
	}
	if err := tx.Commit(); err != nil {
		return IndexInfo{}, err
	}
	return info, nil
}

const indexCols = `index_id, strategy, params, embedder, dims, corpus_sha, chunks, tokens, built_at, seconds, min_score`

func scanIndex(sc interface{ Scan(...any) error }) (IndexInfo, error) {
	var (
		i             IndexInfo
		strategy, prm string
		builtAt       string
	)
	if err := sc.Scan(&i.ID, &strategy, &prm, &i.Embedder, &i.Dims, &i.CorpusSHA, &i.Chunks, &i.Tokens,
		&builtAt, &i.Seconds, &i.MinScore); err != nil {
		return i, err
	}
	i.Strategy = Strategy(strategy)
	_ = json.Unmarshal([]byte(prm), &i.Params)
	i.BuiltAt, _ = time.Parse(time.RFC3339Nano, builtAt)
	return i, nil
}

// Indexes — индексы базы по id.
func (s *Store) Indexes(ctx context.Context) ([]IndexInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+indexCols+` FROM kb_indexes ORDER BY index_id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IndexInfo
	for rows.Next() {
		i, err := scanIndex(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// Index — один индекс; нет — ErrNoIndex.
func (s *Store) Index(ctx context.Context, id string) (IndexInfo, error) {
	i, err := scanIndex(s.db.QueryRowContext(ctx, `SELECT `+indexCols+` FROM kb_indexes WHERE index_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return IndexInfo{}, fmt.Errorf("%w: %s", ErrNoIndex, id)
	}
	return i, err
}

const chunkCols = `chunk_id, doc_id, ord, source, title, section, path, start, "end", text, mixed, tokens, sha, url, revid, index_id`

func scanChunk(sc interface{ Scan(...any) error }, extra ...any) (Chunk, error) {
	var (
		c     Chunk
		path  string
		index string
	)
	dst := []any{&c.ID, &c.DocID, &c.Ord, &c.Source, &c.Title, &c.Section, &path, &c.Start, &c.End,
		&c.Text, &c.Mixed, &c.Tokens, &c.SHA, &c.URL, &c.RevID, &index}
	if err := sc.Scan(append(dst, extra...)...); err != nil {
		return c, err
	}
	_ = json.Unmarshal([]byte(path), &c.Path)
	if c.Path == nil {
		c.Path = []string{}
	}
	c.Strategy = Strategy(index)
	return c, nil
}

// Chunks — чанки индекса (без векторов) по документам и порядку; docID
// пусто — все. Индекса нет — ErrNoIndex.
func (s *Store) Chunks(ctx context.Context, indexID, docID string) ([]Chunk, error) {
	if _, err := s.Index(ctx, indexID); err != nil {
		return nil, err
	}
	q := `SELECT ` + chunkCols + ` FROM kb_chunks c WHERE index_id = ?`
	args := []any{indexID}
	if docID != "" {
		q += ` AND doc_id = ?`
		args = append(args, docID)
	}
	q += ` ORDER BY (SELECT ord FROM kb_docs d WHERE d.doc_id = c.doc_id), ord`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chunk
	for rows.Next() {
		c, err := scanChunk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Chunk — один чанк по id; нет — ErrNoChunk.
func (s *Store) Chunk(ctx context.Context, chunkID string) (Chunk, error) {
	c, err := scanChunk(s.db.QueryRowContext(ctx, `SELECT `+chunkCols+` FROM kb_chunks WHERE chunk_id = ?`, chunkID))
	if errors.Is(err, sql.ErrNoRows) {
		return Chunk{}, fmt.Errorf("%w: %s", ErrNoChunk, chunkID)
	}
	return c, err
}

// GetVec — вектор из кэша эмбеддингов (embed.Cache).
func (s *Store) GetVec(ctx context.Context, model, sha string) ([]float32, bool, error) {
	var blob []byte
	err := s.db.QueryRowContext(ctx, `SELECT vec FROM kb_embed_cache WHERE model = ? AND sha = ?`, model, sha).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	v, err := decodeVec(blob)
	return v, err == nil, err
}

// PutVec — вектор в кэш эмбеддингов (embed.Cache).
func (s *Store) PutVec(ctx context.Context, model, sha string, v []float32) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kb_embed_cache (model, sha, vec) VALUES (?, ?, ?)
		ON CONFLICT (model, sha) DO UPDATE SET vec = excluded.vec`, model, sha, encodeVec(v))
	return err
}

// encodeVec — float32 little-endian подряд: 768 измерений — 3 КБ.
func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func decodeVec(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("вектор: %d байт не кратно 4", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v, nil
}

// indexBytes — сколько места индекс занимает в базе: тексты чанков,
// векторы и текст FTS (без накладных SQLite — для сравнения стратегий
// между собой этого достаточно).
func (s *Store) indexBytes(ctx context.Context, id string) (int64, error) {
	var a, b sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT SUM(length(CAST(text AS BLOB))) + SUM(COALESCE(length(vec), 0)) FROM kb_chunks WHERE index_id = ?`, id).Scan(&a); err != nil {
		return 0, err
	}
	t, err := ftsTable(id)
	if err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT SUM(length(CAST(text AS BLOB))) FROM `+t).Scan(&b); err != nil {
		return 0, err
	}
	return a.Int64 + b.Int64, nil
}

// clean — пробелы и переводы строк в одну строку (для коротких выдержек).
func clean(s string) string { return strings.Join(strings.Fields(s), " ") }

// SetMinScore записывает порог релевантности индекса (калибровка v23,
// retrieve.Calibrate). Пересборка индекса порог сбрасывает: он подобран для
// этих векторов (Build пишет min_score = 0). Порог — косинус, поэтому
// допустим [0, 1): 0 — «не задан», и поиск берёт умолчание конвейера.
func (s *Store) SetMinScore(ctx context.Context, indexID string, v float64) error {
	if math.IsNaN(v) || v < 0 || v >= 1 {
		return fmt.Errorf("порог %v вне [0, 1)", v)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE kb_indexes SET min_score = ? WHERE index_id = ?`, v, indexID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %s", ErrNoIndex, indexID)
	}
	return nil
}
