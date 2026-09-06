// Package agent 实现 5 个业务 Agent（M1 先落 4 个：Intent/Metrics/RootCause/Reply）
// 的统一接口与工厂（对标 PreHire app/ai_gateway 的多 Agent 抽象）。
// 所有 Agent 输出均为强类型结构化结果（LLM 走 ChatJSON + 修复重试）；
// 根因结论强制绑定证据（防幻觉的工程表达）。
package agent

import (
	"context"
	"encoding/json"

	"aiops/internal/evidence"
	"aiops/internal/ticket"
	"aiops/internal/tool"
	"aiops/pkg/llm"
)

// AgentName Agent 名称常量。
const (
	NameIntent    = "intent"
	NameMetrics   = "metrics"
	NameRootCause = "rootcause"
	NameReply     = "reply"
)

// ToolExecutor 编排器注入的工具执行入口（带工单上下文）。
type ToolExecutor interface {
	Execute(ctx context.Context, ticketID, toolName string, input json.RawMessage) (*tool.Result, error)
}

// SceneConfigSource 场景参数来源（DB 外置热更新；可 nil）。
type SceneConfigSource interface {
	GetSceneConfig(ctx context.Context, scene string) (SceneParams, bool)
}

// applyScene 把场景参数覆盖到请求（仅覆盖显式配置项）。
func applyScene(rc *RunContext, scene string, req *llm.ChatRequest) {
	if rc.SceneCfg == nil {
		return
	}
	if sp, ok := rc.SceneCfg.GetSceneConfig(rc.Ctx, scene); ok {
		if sp.Model != "" {
			req.Model = sp.Model
		}
		if sp.Temperature > 0 {
			req.Temperature = sp.Temperature
		}
		if sp.MaxTokens > 0 {
			req.MaxTokens = sp.MaxTokens
		}
	}
}

// RunContext 一次 Agent 运行的输入。
type RunContext struct {
	Ticket    *ticket.Ticket
	Board     *evidence.Board
	RootCause *RootCauseResult // ReplyAgent 消费
	Tools     ToolExecutor
	LLM       *llm.Client
	Prompts   llm.TemplateStore
	SceneCfg  SceneConfigSource // 可 nil
	Ctx       context.Context   // 场景配置查询用（与 Run 的 ctx 分离）
}

// Output Agent 运行输出：用户可见叙述 + 强类型载荷。
type Output struct {
	Note    string // 写入工单流水的阶段叙述
	Payload any    // 按断言取 *IntentResult / *MetricsResult / *RootCauseResult / *ticket.Report
}

// Agent 统一接口。
type Agent interface {
	Name() string
	Run(ctx context.Context, rc *RunContext) (*Output, error)
}

// IntentResult 意图识别结果。
type IntentResult struct {
	Type          string   `json:"type"`
	Service       string   `json:"service"`
	Namespace     string   `json:"namespace"`
	WindowMinutes int      `json:"window_minutes"`
	Keywords      []string `json:"keywords"`
	NeedMoreInfo  bool     `json:"need_more_info"`
	Questions     []string `json:"questions"`
	Reason        string   `json:"reason"`
}

// MetricsResult 指标检索结果。
type MetricsResult struct {
	ObservationIDs []string `json:"observation_ids"`
	Highlights     []string `json:"highlights"`
	FailedCount    int      `json:"failed_count"`
}

// QueryRequest 根因 Agent 发起的补查请求（回环）。
type QueryRequest struct {
	Tool   string          `json:"tool"`
	Input  json.RawMessage `json:"input"`
	Reason string          `json:"reason"`
}

// RootCauseResult 根因推理结果。
type RootCauseResult struct {
	RootCause       string                  `json:"root_cause"`
	Confidence      float64                 `json:"confidence"`
	Hypotheses      []evidence.Hypothesis   `json:"hypotheses"`
	Citations       []evidence.Citation     `json:"citations"`
	MissingEvidence []string                `json:"missing_evidence"`
	Timeline        []ticket.TimelineItem   `json:"timeline"`
	Recommendations []ticket.Recommendation `json:"recommendations"`
	NeedMoreQueries []QueryRequest          `json:"need_more_queries"`
	// Rounds 实际回环轮次（编排器填充）。
	Rounds int `json:"rounds"`
}

// ConfidenceThreshold 低于该置信度转人工（可配置，M4 管理面接入）。
const ConfidenceThreshold = 0.6
