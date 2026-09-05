// Package tool 实现工具层：统一 Tool 接口 + 注册表 + 执行管道。
// 执行管道（顺序）：输入校验 → 缓存 → 限流(Redis 令牌桶, fail-open) → 并发闸(semaphore)
// → 独立超时执行 → 重试(仅幂等读) → 降级 → trace/指标/审计。
// 降级语义：工具失败/超时/限流一律产出 status=failed 的结果信封（附 degraded_reason），
// 绝不向上抛出中断编排的异常——缺失的证据进入证据链并影响置信度。
package tool

import (
	"context"
	"encoding/json"
	"time"
)

// Status 结果信封状态。
const (
	StatusOK       = "ok"
	StatusDegraded = "degraded"
	StatusFailed   = "failed"
)

// Schema 工具元数据：输入 JSON Schema（简化子集）与可靠性策略。
type Schema struct {
	Input json.RawMessage `json:"input,omitempty"` // {type:object, required:[...], properties:{...}}
	// Idempotent 幂等读：失败允许自动重试；变更类工具必须为 false。
	Idempotent bool `json:"idempotent"`
	// Mutating 变更类工具：M3 起强制人工二次确认 + 审计。
	Mutating bool `json:"mutating"`
	// Timeout 单次执行超时；0 用默认 10s。
	Timeout time.Duration `json:"-"`
	// MaxRetries 失败重试次数（仅 Idempotent 生效）。
	MaxRetries int `json:"-"`
	// RatePerSecond / RateBurst 令牌桶限流参数；0 = 不限。
	RatePerSecond float64 `json:"-"`
	RateBurst     int64   `json:"-"`
	// Concurrency 并发闸上限；0 = 不限。
	Concurrency int `json:"-"`
	// CacheTTL 结果缓存时长；0 = 不缓存。
	CacheTTL time.Duration `json:"-"`
}

// EvidenceMeta 证据元信息：进证据链的最小集合。
type EvidenceMeta struct {
	Source  string          `json:"source"`            // 数据源，如 prometheus
	Query   string          `json:"query,omitempty"`   // 查询表达式（如 PromQL）
	Excerpt json.RawMessage `json:"excerpt,omitempty"` // 关键摘录
}

// Result 统一结果信封（降级事实必须进入证据链）。
type Result struct {
	Status         string          `json:"status"`
	Data           json.RawMessage `json:"data,omitempty"`
	Evidence       EvidenceMeta    `json:"evidence"`
	DegradedReason string          `json:"degraded_reason,omitempty"`
	DurationMS     int64           `json:"duration_ms"`
	FromCache      bool            `json:"from_cache"`
}

// Tool 工具统一接口（LLM 侧暴露 name/desc/schema）。
type Tool interface {
	Name() string
	Desc() string
	Schema() Schema
	Execute(ctx context.Context, input json.RawMessage) (*Result, error)
}
