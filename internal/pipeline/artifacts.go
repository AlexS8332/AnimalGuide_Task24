package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/db"
)

// ArtifactTTL — сколько живёт выход шага в хранилище. ref нужен, пока идёт
// цепочка (секунды–минуты) и пока человек может захотеть пересохранить
// факты в другом формате; неделя — с запасом. Дольше держать незачем:
// досье — десятки килобайт, а сам выпуск после save_to_file лежит в файле.
const ArtifactTTL = 7 * 24 * time.Hour

// artifactKinds — что кладётся в хранилище: только выходы search и
// summarize. Выход save_to_file по ref никто не передаёт дальше — файл
// уже на диске, и хранить его конверт второй раз незачем.
func artifactKind(kind string) bool { return kind == KindDossier || kind == KindFacts }

// artifactCheck — общее у Put обеих реализаций: вид из допустимых и
// отпечаток сходится с данными. Испорченный конверт в хранилище — это
// испорченный вход следующему шагу по ref, причём уже без шанса заметить
// порчу: его отпечаток — ключ хранилища.
func artifactCheck(e Envelope) error {
	if !artifactKind(e.Kind) {
		return fmt.Errorf("%w: хранятся только %s и %s, пришло %q", ErrKind, KindDossier, KindFacts, e.Kind)
	}
	return e.Open(e.Kind)
}

// artifactRef — ref как его прислали: модели иногда теряют приставку
// «sha256:» или добавляют пробелы. Голые 64 знака hex — тот же отпечаток.
func artifactRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "sha256:") && len(ref) == 64 && isHex(ref) {
		return "sha256:" + strings.ToLower(ref)
	}
	return ref
}

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func artifactNotFound(ref string) error {
	return fmt.Errorf("%w: %s (выходы шагов хранятся %d дней и пропадают, если ref выдуман или с опечаткой; "+
		"можно передать конверт целиком в input)", ErrRef, ref, int(ArtifactTTL/(24*time.Hour)))
}

// ------------------------------------------------------------------ Memory

// Memory — Artifacts в памяти (тесты, приложение без демона).
type Memory struct {
	Now func() time.Time // nil — time.Now

	mu    sync.Mutex
	items map[string]memoryItem
}

type memoryItem struct {
	env     Envelope
	created time.Time
}

var _ Artifacts = (*Memory)(nil)

// NewMemory — пустое хранилище.
func NewMemory() *Memory { return &Memory{items: map[string]memoryItem{}} }

func (m *Memory) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Put сохраняет конверт; повтор освежает срок жизни. Заодно выбрасывает
// просроченные: отдельного уборщика у хранилища нет.
func (m *Memory) Put(ctx context.Context, e Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := artifactCheck(e); err != nil {
		return err
	}
	now := m.now()
	e.Data = append(json.RawMessage(nil), e.Data...) // вызывающий может переиспользовать буфер
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.items == nil {
		m.items = map[string]memoryItem{}
	}
	m.items[e.Digest] = memoryItem{env: e, created: now}
	for k, it := range m.items {
		if now.Sub(it.created) > ArtifactTTL {
			delete(m.items, k)
		}
	}
	return nil
}

// Get — конверт по ref; просроченный — как отсутствующий.
func (m *Memory) Get(ctx context.Context, ref string) (Envelope, error) {
	if err := ctx.Err(); err != nil {
		return Envelope{}, err
	}
	ref = artifactRef(ref)
	m.mu.Lock()
	it, ok := m.items[ref]
	m.mu.Unlock()
	if !ok || m.now().Sub(it.created) > ArtifactTTL {
		return Envelope{}, artifactNotFound(ref)
	}
	e := it.env
	e.Data = append(json.RawMessage(nil), e.Data...)
	return e, nil
}

// ------------------------------------------------------------------ SQLite

// SQLiteComponent — имя компонента в schema_migrations.
const SQLiteComponent = "pipeline"

// sqliteSteps — миграции схемы pipeline. Только дописывать в конец.
//
// Конверт хранится JSON-ом целиком: достаётся он тоже целиком, а разбирать
// его на столбцы значило бы менять схему вместе с каждым полем Envelope.
// kind вынесен столбцом для глаз (sqlite3 trivia.db), created — для уборки
// просроченных. Время — наносекунды Unix, как в остальных компонентах.
var sqliteSteps = []string{
	`CREATE TABLE pipeline_artifact (
		digest   TEXT PRIMARY KEY,
		kind     TEXT NOT NULL,
		created  INTEGER NOT NULL, -- наносекунды Unix
		envelope TEXT NOT NULL
	);
	CREATE INDEX pipeline_artifact_created ON pipeline_artifact (created);`,
}

// SQLite — Artifacts поверх общей базы демона. Базу открывает и закрывает
// владелец (db.Open).
type SQLite struct {
	db  *sql.DB
	Now func() time.Time // nil — time.Now
}

var _ Artifacts = (*SQLite)(nil)

// NewSQLite доводит схему pipeline до текущей версии.
func NewSQLite(ctx context.Context, conn *sql.DB) (*SQLite, error) {
	if conn == nil {
		return nil, errors.New("pipeline: база не открыта")
	}
	if err := db.Migrate(ctx, conn, SQLiteComponent, sqliteSteps); err != nil {
		return nil, fmt.Errorf("pipeline: схема: %w", err)
	}
	return &SQLite{db: conn}, nil
}

func (s *SQLite) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Put сохраняет конверт (повтор освежает created) и удаляет просроченные.
// Обе операции — одной транзакцией: уборка не должна удалить только что
// освежённую запись чужого Put между ними.
func (s *SQLite) Put(ctx context.Context, e Envelope) error {
	if err := artifactCheck(e); err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("pipeline: конверт не записался в JSON: %w", err)
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pipeline: хранилище: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO pipeline_artifact (digest, kind, created, envelope)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (digest) DO UPDATE SET created = excluded.created, envelope = excluded.envelope`,
		e.Digest, e.Kind, now.UnixNano(), string(raw)); err != nil {
		return fmt.Errorf("pipeline: сохранить %s: %w", Short(e.Digest), err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pipeline_artifact WHERE created < ?`,
		now.Add(-ArtifactTTL).UnixNano()); err != nil {
		return fmt.Errorf("pipeline: уборка просроченных: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pipeline: хранилище: %w", err)
	}
	return nil
}

// Get — конверт по ref; просроченный, но ещё не убранный — как отсутствующий.
func (s *SQLite) Get(ctx context.Context, ref string) (Envelope, error) {
	ref = artifactRef(ref)
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT envelope FROM pipeline_artifact WHERE digest = ? AND created >= ?`,
		ref, s.now().Add(-ArtifactTTL).UnixNano()).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Envelope{}, artifactNotFound(ref)
	}
	if err != nil {
		return Envelope{}, fmt.Errorf("pipeline: прочитать %s: %w", Short(ref), err)
	}
	var e Envelope
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return Envelope{}, fmt.Errorf("pipeline: конверт %s не разобрался: %w", Short(ref), err)
	}
	return e, nil
}
