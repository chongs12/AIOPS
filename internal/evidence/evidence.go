// Package evidence 定义证据化推理的领域模型：观察（Observation）、假设（Hypothesis）、
// 引用（Citation）与黑板（Board）。防幻觉的工程表达：
// 所有结论必须引用观察 ID，缺证据的结论只能标记为未验证假设。
package evidence

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Observation 一次工具查询得到的观察记录（黑板核心单元，落库供审计与回放）。
type Observation struct {
	ID        string          `json:"id"`               // obs-<hex>
	Source    string          `json:"source"`           // 工具名，如 prometheus.query
	Query     string          `json:"query"`            // 查询表达式/参数摘要（如 PromQL）
	Window    *Window         `json:"window,omitempty"` // 查询时间窗
	Summary   string          `json:"summary"`          // 结果摘要（供人与 LLM 阅读）
	Data      json.RawMessage `json:"data,omitempty"`   // 原始结果
	Status    string          `json:"status"`           // ok|degraded|failed
	CreatedAt time.Time       `json:"created_at"`
}

// Window 查询时间窗。
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// HypothesisStatus 假设状态。
const (
	HypothesisPending   = "pending"
	HypothesisConfirmed = "confirmed"
	HypothesisRejected  = "rejected"
)

// Hypothesis 根因假设：必须绑定证据，未绑定的视为未验证假设。
type Hypothesis struct {
	ID          string   `json:"id"` // hyp-<hex>
	Statement   string   `json:"statement"`
	EvidenceIDs []string `json:"evidence_ids"`
	Confidence  float64  `json:"confidence"`
	Status      string   `json:"status"`
}

// Citation 报告中的证据引用（指向观察）。
type Citation struct {
	ObservationID string `json:"observation_id"`
	Note          string `json:"note,omitempty"`
}

// NewObservationID 生成观察 ID。
func NewObservationID() string { return "obs-" + randomHex(6) }

// NewHypothesisID 生成假设 ID。
func NewHypothesisID() string { return "hyp-" + randomHex(4) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// Board 黑板：编排器在工单处理过程中维护的观察集与假设集，
// Agent 之间通过它做结构化消息传递（多轮/回环的数据载体）。
// 非并发安全：由编排器单协程驱动，跨协程访问需编排器自行加锁。
type Board struct {
	Observations []Observation
	Hypotheses   []Hypothesis
}

// NewBoard 空黑板。
func NewBoard() *Board { return &Board{} }

// AddObservation 追加观察并返回其 ID。
func (b *Board) AddObservation(o Observation) string {
	if o.ID == "" {
		o.ID = NewObservationID()
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now()
	}
	b.Observations = append(b.Observations, o)
	return o.ID
}

// AddHypothesis 追加假设并返回其 ID。
func (b *Board) AddHypothesis(h Hypothesis) string {
	if h.ID == "" {
		h.ID = NewHypothesisID()
	}
	if h.Status == "" {
		h.Status = HypothesisPending
	}
	b.Hypotheses = append(b.Hypotheses, h)
	return h.ID
}

// ObservationByID 按 ID 查观察。
func (b *Board) ObservationByID(id string) (*Observation, bool) {
	for i := range b.Observations {
		if b.Observations[i].ID == id {
			return &b.Observations[i], true
		}
	}
	return nil, false
}

// RenderForLLM 将黑板渲染为供 LLM 阅读的紧凑文本（含观察 ID 供引用）。
func (b *Board) RenderForLLM() string {
	var sb strings.Builder
	sb.WriteString("## 观察集\n")
	if len(b.Observations) == 0 {
		sb.WriteString("(暂无观察)\n")
	}
	for _, o := range b.Observations {
		sb.WriteString(fmt.Sprintf("- [%s] 来源=%s 状态=%s\n  查询: %s\n  摘要: %s\n",
			o.ID, o.Source, o.Status, o.Query, o.Summary))
	}
	sb.WriteString("\n## 假设集\n")
	if len(b.Hypotheses) == 0 {
		sb.WriteString("(暂无假设)\n")
	}
	for _, h := range b.Hypotheses {
		sb.WriteString(fmt.Sprintf("- [%s] %s（置信度 %.2f，状态 %s，证据 %v）\n",
			h.ID, h.Statement, h.Confidence, h.Status, h.EvidenceIDs))
	}
	return sb.String()
}
