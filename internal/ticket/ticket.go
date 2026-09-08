// Package ticket 定义工单领域模型与存储（PostgreSQL + 内存实现）。
package ticket

import (
	"time"

	"aiops/internal/evidence"
)

// Status 工单状态（FSM 一等状态，awaiting_info / escalated 为 HITL 状态）。
const (
	StatusSubmitted     = "submitted"
	StatusInvestigating = "investigating"
	StatusAwaitingInfo  = "awaiting_info"
	StatusResolved      = "resolved"
	StatusEscalated     = "escalated_human"
	StatusFailed        = "failed"
)

// TicketType 工单类型（意图识别结果）。
const (
	Type5xxSurge  = "5xx_surge"
	TypePodOOM    = "pod_oom"
	TypeSlowQuery = "slow_query"
	TypeOther     = "other"
)

// Ticket 工单聚合根。
type Ticket struct {
	ID         string       `json:"id"`
	Title      string       `json:"title"`
	Content    string       `json:"content"`
	Reporter   string       `json:"reporter"`
	Type       string       `json:"type"`
	Status     string       `json:"status"`
	Entities   Entities     `json:"entities"`
	TraceID    string       `json:"trace_id,omitempty"` // 一次处理 = 一条 trace
	Report     *Report      `json:"report,omitempty"`
	Priority   string       `json:"priority,omitempty"`    // P0-P3，默认 P2
	Rating     int          `json:"rating,omitempty"`      // 满意度 1-5（resolved 后可评）
	RatingNote string       `json:"rating_note,omitempty"` // 评价备注
	Error      string       `json:"error,omitempty"`       // failed 时的原因
	Pending    *PendingInfo `json:"pending,omitempty"`     // awaiting_info 挂起信息
	CreatedAt  time.Time    `json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
}

// PendingInfo HITL 挂起信息：awaiting_info 状态下等待用户补充的内容。
type PendingInfo struct {
	Questions []string  `json:"questions"`
	Round     int       `json:"round"`
	AskedAt   time.Time `json:"asked_at"`
}

// Entities 意图识别抽取的实体。
type Entities struct {
	Service       string     `json:"service,omitempty"`
	Namespace     string     `json:"namespace,omitempty"`
	WindowMinutes int        `json:"window_minutes,omitempty"` // 故障时间窗（分钟）
	WindowEnd     *time.Time `json:"window_end,omitempty"`     // 缺省 = now
	Keywords      []string   `json:"keywords,omitempty"`
}

// WindowEndOrNow 返回窗口终点（缺省 now）。
func (e Entities) WindowEndOrNow() time.Time {
	if e.WindowEnd != nil {
		return *e.WindowEnd
	}
	return time.Now()
}

// TimelineItem 事故时间线条目。
type TimelineItem struct {
	Time   time.Time `json:"time"`
	Event  string    `json:"event"`
	Source string    `json:"source,omitempty"` // 依据的观察 ID 或工具名
}

// Recommendation 处置建议：低危可一键触发，高危转人工。
type Recommendation struct {
	Action      string `json:"action"`           // 如 scale / rollout_undo
	Target      string `json:"target,omitempty"` // 如 deployment/makejob-question
	Description string `json:"description"`
	Risk        string `json:"risk"` // low|high
}

// Report 结构化事故报告（ReplyAgent 产出）。
type Report struct {
	RootCause       string                `json:"root_cause"`
	Confidence      float64               `json:"confidence"`
	Hypotheses      []evidence.Hypothesis `json:"hypotheses,omitempty"`
	Timeline        []TimelineItem        `json:"timeline,omitempty"`
	Recommendations []Recommendation      `json:"recommendations,omitempty"`
	Citations       []evidence.Citation   `json:"citations,omitempty"`
	MissingEvidence []string              `json:"missing_evidence,omitempty"`
	Summary         string                `json:"summary"` // 面向用户的中文叙述
	GeneratedAt     time.Time             `json:"generated_at"`
}

// Message 工单流水消息（Agent 过程与用户对话共用，落库供推理回放）。
type Message struct {
	Seq              int       `json:"seq"`
	Role             string    `json:"role"` // user|agent|system
	Agent            string    `json:"agent,omitempty"`
	Content          string    `json:"content"`
	PromptTokens     int       `json:"prompt_tokens,omitempty"` // Agent 步骤的 LLM 用量
	CompletionTokens int       `json:"completion_tokens,omitempty"`
	Model            string    `json:"model,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}
