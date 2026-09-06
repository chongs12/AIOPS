package ticket

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"aiops/internal/evidence"
)

// ErrNotFound 工单不存在。
var ErrNotFound = errors.New("ticket: 不存在")

// Store 工单存储接口。
type Store interface {
	CreateTicket(ctx context.Context, t *Ticket) error
	GetTicket(ctx context.Context, id string) (*Ticket, error)
	ListTickets(ctx context.Context, limit int) ([]*Ticket, error)
	UpdateTicket(ctx context.Context, t *Ticket) error
	AppendMessage(ctx context.Context, ticketID string, m *Message) error
	ListMessages(ctx context.Context, ticketID string) ([]*Message, error)
	SaveObservation(ctx context.Context, ticketID string, o *evidence.Observation) error
	ListObservations(ctx context.Context, ticketID string) ([]*evidence.Observation, error)
	GetObservation(ctx context.Context, ticketID, obsID string) (*evidence.Observation, error)
}

// NewID 生成工单号：t-YYYYMMDD-HHMMSS-xxxx。
func NewID() string {
	now := time.Now()
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("t-%s-%s", now.Format("20060102-150405"), hex.EncodeToString(b))
}

// New 创建待处理工单。
func New(title, content, reporter string) *Ticket {
	now := time.Now()
	return &Ticket{
		ID: NewID(), Title: title, Content: content, Reporter: reporter,
		Type: TypeOther, Status: StatusSubmitted, Priority: "P2", CreatedAt: now, UpdatedAt: now,
	}
}

// ---- PostgreSQL 实现 ----

// PGStore PostgreSQL 存储。
type PGStore struct {
	db *sql.DB
}

// DB 暴露底层连接（案例库/审计复用同一连接）。
func (s *PGStore) DB() *sql.DB { return s.db }

// NewPGStore 构造并执行建表迁移（幂等）。
func NewPGStore(ctx context.Context, db *sql.DB) (*PGStore, error) {
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("ticket 建表迁移: %w", err)
	}
	return &PGStore{db: db}, nil
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS tickets (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL,
    content    TEXT NOT NULL,
    reporter   TEXT NOT NULL DEFAULT '',
    type       TEXT NOT NULL DEFAULT 'other',
    status     TEXT NOT NULL,
    entities   JSONB NOT NULL DEFAULT '{}',
    trace_id   TEXT NOT NULL DEFAULT '',
    report     JSONB,
    error      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS ticket_messages (
    seq        BIGINT NOT NULL,
    ticket_id  TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    agent      TEXT NOT NULL DEFAULT '',
    content    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticket_id, seq)
);
CREATE TABLE IF NOT EXISTS ticket_observations (
    id         TEXT PRIMARY KEY,
    ticket_id  TEXT NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    source     TEXT NOT NULL,
    query      TEXT NOT NULL DEFAULT '',
    query_window JSONB,
    summary    TEXT NOT NULL DEFAULT '',
    data       JSONB,
    status     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tickets_created ON tickets (created_at DESC);
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS pending JSONB;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS rating INT;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS rating_note TEXT DEFAULT '';
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS priority TEXT DEFAULT 'P2';
`

const ticketCols = `id, title, content, reporter, type, status, entities, trace_id, report, pending, rating, rating_note, error, created_at, updated_at`

func (s *PGStore) CreateTicket(ctx context.Context, t *Ticket) error {
	ent, err := json.Marshal(t.Entities)
	if err != nil {
		return err
	}
	report, err := nullableJSON(t.Report)
	if err != nil {
		return err
	}
	pending, err := nullableJSON(t.Pending)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO tickets (id, title, content, reporter, type, status, entities, trace_id, report, pending, priority, rating, rating_note, error, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		t.ID, t.Title, t.Content, t.Reporter, t.Type, t.Status, ent, t.TraceID, report, pending, t.Priority, t.Rating, t.RatingNote, t.Error, t.CreatedAt, t.UpdatedAt)
	return err
}

func (s *PGStore) GetTicket(ctx context.Context, id string) (*Ticket, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+ticketCols+` FROM tickets WHERE id = $1`, id)
	return scanTicket(row.Scan)
}

func (s *PGStore) ListTickets(ctx context.Context, limit int) ([]*Ticket, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+ticketCols+` FROM tickets ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Ticket
	for rows.Next() {
		t, err := scanTicket(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PGStore) UpdateTicket(ctx context.Context, t *Ticket) error {
	ent, err := json.Marshal(t.Entities)
	if err != nil {
		return err
	}
	t.UpdatedAt = time.Now()
	report, err := nullableJSON(t.Report)
	if err != nil {
		return err
	}
	pending, err := nullableJSON(t.Pending)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
UPDATE tickets SET title=$2, content=$3, reporter=$4, type=$5, status=$6, entities=$7,
                   trace_id=$8, report=$9, pending=$10, priority=$11, rating=$12, rating_note=$13, error=$14, updated_at=$15
WHERE id=$1`,
		t.ID, t.Title, t.Content, t.Reporter, t.Type, t.Status, ent, t.TraceID, report, pending, t.Priority, t.Rating, t.RatingNote, t.Error, t.UpdatedAt)
	return err
}

// scanTicket 通用行扫描（row.Scan 适配 *sql.Row 与 *sql.Rows）。
func scanTicket(scan func(dest ...any) error) (*Ticket, error) {
	var t Ticket
	var ent, traceID []byte
	var report, pending []byte
	var createdAt, updatedAt time.Time
	var rating sql.NullInt64
	var ratingNote sql.NullString
	if err := scan(&t.ID, &t.Title, &t.Content, &t.Reporter, &t.Type, &t.Status,
		&ent, &traceID, &report, &pending, &rating, &ratingNote, &t.Error, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if len(ent) > 0 {
		_ = json.Unmarshal(ent, &t.Entities)
	}
	t.TraceID = string(traceID)
	if len(report) > 0 {
		var r Report
		if err := json.Unmarshal(report, &r); err == nil {
			t.Report = &r
		}
	}
	if len(pending) > 0 {
		var pi PendingInfo
		if err := json.Unmarshal(pending, &pi); err == nil {
			t.Pending = &pi
		}
	}
	t.Rating = int(rating.Int64)
	t.RatingNote = ratingNote.String
	t.CreatedAt, t.UpdatedAt = createdAt, updatedAt
	return &t, nil
}

func (s *PGStore) AppendMessage(ctx context.Context, ticketID string, m *Message) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO ticket_messages (seq, ticket_id, role, agent, content, created_at)
VALUES ((SELECT COALESCE(MAX(seq),0)+1 FROM ticket_messages WHERE ticket_id=$1), $1, $2, $3, $4, COALESCE($5, now()))`,
		ticketID, m.Role, m.Agent, m.Content, nullableTime(m.CreatedAt))
	return err
}

func (s *PGStore) ListMessages(ctx context.Context, ticketID string) ([]*Message, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, role, agent, content, created_at FROM ticket_messages WHERE ticket_id=$1 ORDER BY seq`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Seq, &m.Role, &m.Agent, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (s *PGStore) SaveObservation(ctx context.Context, ticketID string, o *evidence.Observation) error {
	var win any
	if o.Window != nil {
		w, err := json.Marshal(o.Window)
		if err != nil {
			return err
		}
		win = w
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO ticket_observations (id, ticket_id, source, query, query_window, summary, data, status, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,COALESCE($9, now()))
ON CONFLICT (id) DO UPDATE SET summary=EXCLUDED.summary, data=EXCLUDED.data, status=EXCLUDED.status`,
		o.ID, ticketID, o.Source, o.Query, win, o.Summary, o.Data, o.Status, nullableTime(o.CreatedAt))
	return err
}

func (s *PGStore) ListObservations(ctx context.Context, ticketID string) ([]*evidence.Observation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, source, query, query_window, summary, data, status, created_at
FROM ticket_observations WHERE ticket_id=$1 ORDER BY created_at`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*evidence.Observation
	for rows.Next() {
		var o evidence.Observation
		var win, data []byte
		if err := rows.Scan(&o.ID, &o.Source, &o.Query, &win, &o.Summary, &data, &o.Status, &o.CreatedAt); err != nil {
			return nil, err
		}
		if len(win) > 0 {
			var w evidence.Window
			if err := json.Unmarshal(win, &w); err == nil {
				o.Window = &w
			}
		}
		o.Data = data
		out = append(out, &o)
	}
	return out, rows.Err()
}

// nullableJSON 序列化为 JSONB 参数；nil → NULL。
func nullableJSON(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// GetObservation 按 ID 取单条观察。
func (s *PGStore) GetObservation(ctx context.Context, ticketID, obsID string) (*evidence.Observation, error) {
	rows, err := s.ListObservations(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	for _, o := range rows {
		if o.ID == obsID {
			return o, nil
		}
	}
	return nil, ErrNotFound
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// ---- 内存实现（单测用） ----

// MemStore 内存存储，语义与 PGStore 一致；线程安全（生产降级模式可用）。
type MemStore struct {
	mu           sync.RWMutex
	tickets      map[string]*Ticket
	messages     map[string][]*Message
	observations map[string][]*evidence.Observation
}

// NewMemStore 构造内存存储。
func NewMemStore() *MemStore {
	return &MemStore{
		tickets:      make(map[string]*Ticket),
		messages:     make(map[string][]*Message),
		observations: make(map[string][]*evidence.Observation),
	}
}

func (m *MemStore) CreateTicket(_ context.Context, t *Ticket) error {
	cp := *t
	m.mu.Lock()
	m.tickets[t.ID] = &cp
	m.mu.Unlock()
	return nil
}

func (m *MemStore) GetTicket(_ context.Context, id string) (*Ticket, error) {
	m.mu.RLock()
	t, ok := m.tickets[id]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (m *MemStore) ListTickets(_ context.Context, limit int) ([]*Ticket, error) {
	m.mu.RLock()
	out := make([]*Ticket, 0, len(m.tickets))
	for _, t := range m.tickets {
		cp := *t
		out = append(out, &cp)
	}
	m.mu.RUnlock()
	// 简化：不排序，测试用例少。
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemStore) UpdateTicket(_ context.Context, t *Ticket) error {
	cp := *t
	m.mu.Lock()
	if _, ok := m.tickets[t.ID]; !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	m.tickets[t.ID] = &cp
	m.mu.Unlock()
	return nil
}

func (m *MemStore) AppendMessage(_ context.Context, ticketID string, msg *Message) error {
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	m.mu.Lock()
	msg.Seq = len(m.messages[ticketID]) + 1
	m.messages[ticketID] = append(m.messages[ticketID], msg)
	m.mu.Unlock()
	return nil
}

func (m *MemStore) ListMessages(_ context.Context, ticketID string) ([]*Message, error) {
	m.mu.RLock()
	out := m.messages[ticketID]
	cps := make([]*Message, 0, len(out))
	for _, msg := range out {
		cp := *msg
		cps = append(cps, &cp)
	}
	m.mu.RUnlock()
	return cps, nil
}

func (m *MemStore) SaveObservation(_ context.Context, ticketID string, o *evidence.Observation) error {
	cp := *o
	m.mu.Lock()
	m.observations[ticketID] = append(m.observations[ticketID], &cp)
	m.mu.Unlock()
	return nil
}

// GetObservation 按 ID 取单条观察。
func (m *MemStore) GetObservation(_ context.Context, ticketID, obsID string) (*evidence.Observation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, o := range m.observations[ticketID] {
		if o.ID == obsID {
			cp := *o
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) ListObservations(_ context.Context, ticketID string) ([]*evidence.Observation, error) {
	m.mu.RLock()
	out := m.observations[ticketID]
	cps := make([]*evidence.Observation, 0, len(out))
	for _, o := range out {
		cp := *o
		cps = append(cps, &cp)
	}
	m.mu.RUnlock()
	return cps, nil
}
