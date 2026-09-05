// Package audit 提供工具调用审计的 PostgreSQL 落库实现（变更类操作必须审计）。
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"aiops/internal/tool"
)

// PGStore PostgreSQL 审计存储。
type PGStore struct {
	db *sql.DB
}

// NewStore 构造（不迁移，供已建库使用）。
func NewStore(db *sql.DB) *PGStore { return &PGStore{db: db} }

// New 构造并迁移。
func New(ctx context.Context, db *sql.DB) (*PGStore, error) {
	const schema = `
CREATE TABLE IF NOT EXISTS tool_audit_log (
    id          BIGSERIAL PRIMARY KEY,
    ticket_id   TEXT NOT NULL,
    tool        TEXT NOT NULL,
    input       JSONB,
    status      TEXT NOT NULL,
    duration_ms BIGINT NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_ticket ON tool_audit_log (ticket_id);
`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("audit 建表迁移: %w", err)
	}
	return &PGStore{db: db}, nil
}

// Record 落库一条审计（实现 tool.Auditor）；失败仅记日志——审计不阻塞主路径，
// 变更操作的结果信封已携带降级信息进入证据链。
func (s *PGStore) Record(ctx context.Context, e *tool.AuditEntry) error {
	if e == nil {
		return nil
	}
	var input any
	if len(e.Input) > 0 {
		input = []byte(e.Input)
	}
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO tool_audit_log (ticket_id, tool, input, status, duration_ms, error, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		e.TicketID, e.Tool, input, e.Status, e.DurationMS, e.Error, at)
	if err != nil {
		slog.Warn("审计落库失败", "tool", e.Tool, "err", err)
	}
	return err
}

// ListByTicket 工单维度查询（回放/管理面）。
func (s *PGStore) ListByTicket(ctx context.Context, ticketID string) ([]*tool.AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT ticket_id, tool, COALESCE(input::text,''), status, duration_ms, error, created_at
FROM tool_audit_log WHERE ticket_id=$1 ORDER BY id`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tool.AuditEntry
	for rows.Next() {
		var e tool.AuditEntry
		var input string
		if err := rows.Scan(&e.TicketID, &e.Tool, &input, &e.Status, &e.DurationMS, &e.Error, &e.At); err != nil {
			return nil, err
		}
		if input != "" {
			e.Input = json.RawMessage(input)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
