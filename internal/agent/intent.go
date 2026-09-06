package agent

import (
	"context"
	"fmt"

	"aiops/internal/ticket"
	"aiops/pkg/llm"
)

// KnownServices PreHire 平台服务清单（见 docs/03-data-source.md）。
var KnownServices = []string{
	"gateway", "user", "membership", "question", "interview", "realtime",
	"growth", "plan", "companion", "community", "learning-archive",
	"ai-gateway", "rag", "coderunner", "admin",
}

// IntentAgent 意图识别 Agent：判定工单类型、抽取实体、评估信息完整性。
type IntentAgent struct{}

// NewIntentAgent 构造。
func NewIntentAgent() *IntentAgent { return &IntentAgent{} }

// Name Agent 名。
func (a *IntentAgent) Name() string { return NameIntent }

// Run 运行意图识别（LLM 结构化输出）。
func (a *IntentAgent) Run(ctx context.Context, rc *RunContext) (*Output, error) {
	prompt, err := llm.RenderNamed(rc.Prompts, ctx, "intent", map[string]string{
		"services": joinServices(KnownServices),
		"title":    rc.Ticket.Title,
		"content":  rc.Ticket.Content,
	})
	if err != nil {
		return nil, fmt.Errorf("intent prompt 渲染失败: %w", err)
	}

	req := llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: prompt}}}
	applyScene(rc, NameIntent, &req)
	var out intentLLM
	if err := rc.LLM.ChatJSON(ctx, req, &out); err != nil {
		return nil, fmt.Errorf("intent 结构化输出失败: %w", err)
	}

	r := &IntentResult{
		Type:         ticket.NormalizeType(out.Type),
		Service:      out.Service,
		Namespace:    out.Namespace,
		Keywords:     out.Keywords,
		NeedMoreInfo: out.NeedMoreInfo,
		Questions:    out.Questions,
		Reason:       out.Reason,
	}
	// 缺省与护栏：时间窗非法/缺失时用默认 30 分钟（M2 起改为追问挂起）。
	if out.WindowMinutes <= 0 || out.WindowMinutes > 24*60 {
		r.WindowMinutes = 30
	} else {
		r.WindowMinutes = out.WindowMinutes
	}

	return &Output{
		Note:    fmt.Sprintf("意图识别：类型=%s 服务=%s 时间窗=%d分钟（%s）", r.Type, r.Service, r.WindowMinutes, r.Reason),
		Payload: r,
	}, nil
}

type intentLLM struct {
	Type          string   `json:"type"`
	Service       string   `json:"service"`
	Namespace     string   `json:"namespace"`
	WindowMinutes int      `json:"window_minutes"`
	Keywords      []string `json:"keywords"`
	NeedMoreInfo  bool     `json:"need_more_info"`
	Questions     []string `json:"questions"`
	Reason        string   `json:"reason"`
}

func joinServices(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
