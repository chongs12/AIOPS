// Package async 实现异步任务状态机（对标 PreHire pkg/mq）：
// queued → running → succeeded / failed（重试梯度 20s/30s/45s）→ dead（3 次失败）。
// 持久化于 PostgreSQL（FOR UPDATE SKIP LOCKED 取任务），AI/长耗时任务不阻塞用户请求。
package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9" // 引入以对齐命名习惯；本包实际仅用 PG
)

// Status 任务状态。
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed" // 中间态（等待重试）
	StatusDead      = "dead"   // 死信
)

// retryGradient 重试延迟梯度（对标 PreHire 20s/30s/45s）。
var retryGradient = []time.Duration{20 * time.Second, 30 * time.Second, 45 * time.Second}

// maxAttempts 最大尝试次数（含首次）。
const maxAttempts = 4

// Handler 任务执行函数（kind 用于分派）；返回 error 触发重试。
type Handler func(ctx context.Context, kind string, payload json.RawMessage) error

// Task 任务记录。
type Task struct {
	ID       int64           `json:"id"`
	TicketID string          `json:"ticket_id"`
	Kind     string          `json:"kind"`
	Status   string          `json:"status"`
	Attempt  int             `json:"attempt"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// Queue PG 任务队列。
type Queue struct {
	db      *sql.DB
	handler Handler
	poll    time.Duration
}

// NewQueue 构造并迁移。
func NewQueue(ctx context.Context, db *sql.DB) (*Queue, error) {
	const schema = `
CREATE TABLE IF NOT EXISTS async_tasks (
    id          BIGSERIAL PRIMARY KEY,
    ticket_id   TEXT NOT NULL,
    kind        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'queued',
    attempt     INT NOT NULL DEFAULT 0,
    payload     JSONB,
    error       TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT UNIQUE,
    run_after   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_async_pick ON async_tasks (status, run_after);
`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("async_tasks 迁移: %w", err)
	}
	return &Queue{db: db, poll: time.Second}, nil
}

// Enqueue 提交任务；idempotencyKey 重复时忽略（幂等）。
func (q *Queue) Enqueue(ctx context.Context, ticketID, kind, idempotencyKey string, payload json.RawMessage) error {
	_, err := q.db.ExecContext(ctx, `
INSERT INTO async_tasks (ticket_id, kind, payload, idempotency_key)
VALUES ($1,$2,$3,$4)
ON CONFLICT (idempotency_key) DO NOTHING`,
		ticketID, kind, payload, idempotencyKey)
	if err != nil {
		return fmt.Errorf("async 入队: %w", err)
	}
	return nil
}

// Run 启动 worker 循环（阻塞）；ctx 取消即退出。
func (q *Queue) Run(ctx context.Context, handler Handler) {
	q.handler = handler
	slog.Info("async worker 启动")
	ticker := time.NewTicker(q.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			q.pickAndRun(ctx)
		}
	}
}

// pickAndRun 取一个可执行任务并同步执行（单实例语义；多实例靠 SKIP LOCKED 竞争）。
func (q *Queue) pickAndRun(ctx context.Context) {
	var id int64
	var payload json.RawMessage
	var attempt int
	var kind string
	err := q.db.QueryRowContext(ctx, `
UPDATE async_tasks SET status='running', attempt=attempt+1, updated_at=now()
WHERE id = (
    SELECT id FROM async_tasks
    WHERE status='queued' AND run_after <= now()
    ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1
) RETURNING id, kind, payload, attempt`).Scan(&id, &kind, &payload, &attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		slog.Warn("async 取任务失败", "err", err)
		return
	}

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	runErr := q.handler(runCtx, kind, payload)

	if runErr == nil {
		_, _ = q.db.ExecContext(ctx,
			`UPDATE async_tasks SET status='succeeded', updated_at=now() WHERE id=$1`, id)
		return
	}
	// 失败：按梯度重试或转死信。
	if attempt >= maxAttempts {
		_, _ = q.db.ExecContext(ctx,
			`UPDATE async_tasks SET status='dead', error=$2, updated_at=now() WHERE id=$1`,
			id, runErr.Error())
		slog.Error("async 任务转死信", "id", id, "kind", kind, "err", runErr)
		return
	}
	delay := retryGradient[(attempt-1)%len(retryGradient)]
	_, _ = q.db.ExecContext(ctx,
		`UPDATE async_tasks SET status='failed', error=$2, run_after=now()+$3::interval, updated_at=now() WHERE id=$1`,
		id, runErr.Error(), fmt.Sprintf("%d seconds", int(delay.Seconds())))
	slog.Warn("async 任务失败，将重试", "id", id, "kind", kind, "attempt", attempt, "delay", delay)
}

// ListDead 死信列表（管理面排查用；返回预序列化 JSON 数组）。
func (q *Queue) ListDead(ctx context.Context, limit int) (json.RawMessage, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := q.db.QueryContext(ctx, `
SELECT id, ticket_id, kind, status, attempt, COALESCE(payload::text,''), error
FROM async_tasks WHERE status='dead' ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.TicketID, &t.Kind, &t.Status, &t.Attempt, &t.Payload, &t.Error); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Task{}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// RedisClient 占位：保持与其它 pkg 相同的依赖形态（限流/缓存可挂接）。
var _ = redis.NewClient
