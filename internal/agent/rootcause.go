package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aiops/internal/evidence"
	"aiops/internal/ticket"
	"aiops/internal/tool/promtool"
	"aiops/pkg/llm"
)

// RootCauseAgent 根因推理 Agent：提假设 → 补查回环 → 交叉验证 → 置信度结论。
// 防幻觉：结论必须引用真实观察 ID；无证据结论被系统降权并记入缺失证据。
type RootCauseAgent struct {
	templates *promtool.Lib
}

// NewRootCauseAgent 构造（templates 用于把可用模板清单注入 prompt 并校验补查请求）。
func NewRootCauseAgent(templates *promtool.Lib) *RootCauseAgent {
	return &RootCauseAgent{templates: templates}
}

// Name Agent 名。
func (a *RootCauseAgent) Name() string { return NameRootCause }

// Run 执行一轮根因推理。
func (a *RootCauseAgent) Run(ctx context.Context, rc *RunContext) (*Output, error) {
	prompt, err := llm.RenderNamed(rc.Prompts, ctx, "rootcause", map[string]string{
		"ticket": renderTicketBlock(rc.Ticket),
		"board":  rc.Board.RenderForLLM(),
		"tools":  a.templates.RenderForLLM(),
	})
	if err != nil {
		return nil, fmt.Errorf("rootcause prompt 渲染失败: %w", err)
	}

	var out rootCauseLLM
	if err := rc.LLM.ChatJSON(ctx, llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: prompt}},
	}, &out); err != nil {
		return nil, fmt.Errorf("rootcause 结构化输出失败: %w", err)
	}

	r := a.toResult(rc.Board, &out)
	r.NeedMoreQueries = a.sanitizeQueries(rc.Board, out.NeedMoreQueries)

	return &Output{Note: a.noteFor(r), Payload: r}, nil
}

// toResult 结构化校验与证据绑定（防幻觉的硬约束）。
func (a *RootCauseAgent) toResult(b *evidence.Board, out *rootCauseLLM) *RootCauseResult {
	r := &RootCauseResult{
		RootCause:       strings.TrimSpace(out.RootCause),
		Confidence:      clamp01(out.Confidence),
		MissingEvidence: out.MissingEvidence,
	}
	for _, h := range out.Hypotheses {
		hh := evidence.Hypothesis{
			Statement:   h.Statement,
			Confidence:  clamp01(h.Confidence),
			Status:      h.Status,
			EvidenceIDs: validIDs(b, h.EvidenceIDs),
		}
		if hh.Status != evidence.HypothesisConfirmed && hh.Status != evidence.HypothesisRejected {
			hh.Status = evidence.HypothesisPending
		}
		b.AddHypothesis(hh)
		r.Hypotheses = append(r.Hypotheses, hh)
	}

	// 证据绑定校验：root_cause 必须有 confirmed 且带有效证据的假设支撑。
	supported := false
	for _, h := range r.Hypotheses {
		if h.Status == evidence.HypothesisConfirmed && len(h.EvidenceIDs) > 0 {
			supported = true
			r.Citations = append(r.Citations, evidence.Citation{ObservationID: h.EvidenceIDs[0]})
			break
		}
	}
	if r.RootCause != "" && !supported {
		r.MissingEvidence = append(r.MissingEvidence,
			"根因结论缺乏有效证据支撑（引用的观察不存在或无 confirmed 假设），已降权处理")
		r.Confidence *= 0.3
		r.RootCause = ""
	}

	for _, t := range out.Timeline {
		tt := ticket.TimelineItem{Event: t.Event, Source: t.Source}
		if t.Time != "" {
			if parsed, err := time.Parse(time.RFC3339, t.Time); err == nil {
				tt.Time = parsed
			}
		}
		r.Timeline = append(r.Timeline, tt)
	}
	for _, rec := range out.Recommendations {
		risk := rec.Risk
		if risk != "low" && risk != "high" {
			risk = "high" // 未知风险级别一律按高危（必须人工确认）
		}
		r.Recommendations = append(r.Recommendations, ticket.Recommendation{
			Action: rec.Action, Target: rec.Target, Description: rec.Description, Risk: risk,
		})
	}
	return r
}

// sanitizeQueries 剔除不合法的补查请求（未知工具/未知模板/黑板已有同款查询）。
// 已执行过的查询不再重复——倒逼模型基于现有证据收敛。
func (a *RootCauseAgent) sanitizeQueries(b *evidence.Board, reqs []queryRequestLLM) []QueryRequest {
	var out []QueryRequest
	for _, q := range reqs {
		if q.Tool != "prometheus.query" {
			continue
		}
		tpl, _ := q.Input["template"].(string)
		t, ok := a.templates.Get(tpl)
		if !ok {
			continue
		}
		raw, err := json.Marshal(q.Input)
		if err != nil {
			continue
		}
		// 同模板+同参数的查询已在黑板 → 跳过。
		params, _ := q.Input["params"].(map[string]any)
		strParams := make(map[string]string, len(params))
		for k, v := range params {
			strParams[k] = fmt.Sprint(v)
		}
		if expr, err := a.templates.Render(t, strParams); err == nil {
			dup := false
			for _, o := range b.Observations {
				if o.Query == expr {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
		}
		out = append(out, QueryRequest{Tool: q.Tool, Input: raw, Reason: q.Reason})
	}
	return out
}

func (a *RootCauseAgent) noteFor(r *RootCauseResult) string {
	if len(r.NeedMoreQueries) > 0 {
		return fmt.Sprintf("根因推理：证据不足，请求补查 %d 项", len(r.NeedMoreQueries))
	}
	if r.RootCause == "" {
		return fmt.Sprintf("根因推理：证据不足且无可补查项，置信度 %.2f，需人工跟进", r.Confidence)
	}
	return fmt.Sprintf("根因推理：根因已定位，置信度 %.2f", r.Confidence)
}

// validIDs 过滤黑板上不存在的证据引用。
func validIDs(b *evidence.Board, ids []string) []string {
	var out []string
	for _, id := range ids {
		if _, ok := b.ObservationByID(id); ok {
			out = append(out, id)
		}
	}
	return out
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// renderTicketBlock 工单信息块（prompt 用）。
func renderTicketBlock(t *ticket.Ticket) string {
	return fmt.Sprintf("工单 %s | 类型=%s 服务=%s 时间窗=%d分钟\n标题: %s\n描述: %s",
		t.ID, t.Type, t.Entities.Service, t.Entities.WindowMinutes, t.Title, t.Content)
}

// ---- LLM 输出结构 ----

type hypothesisLLM struct {
	Statement   string   `json:"statement"`
	EvidenceIDs []string `json:"evidence_ids"`
	Confidence  float64  `json:"confidence"`
	Status      string   `json:"status"`
}

type timelineLLM struct {
	Time   string `json:"time"`
	Event  string `json:"event"`
	Source string `json:"source"`
}

type recommendationLLM struct {
	Action      string `json:"action"`
	Target      string `json:"target"`
	Description string `json:"description"`
	Risk        string `json:"risk"`
}

type queryRequestLLM struct {
	Tool   string         `json:"tool"`
	Input  map[string]any `json:"input"`
	Reason string         `json:"reason"`
}

type rootCauseLLM struct {
	Hypotheses      []hypothesisLLM     `json:"hypotheses"`
	RootCause       string              `json:"root_cause"`
	Confidence      float64             `json:"confidence"`
	Timeline        []timelineLLM       `json:"timeline"`
	Recommendations []recommendationLLM `json:"recommendations"`
	MissingEvidence []string            `json:"missing_evidence"`
	NeedMoreQueries []queryRequestLLM   `json:"need_more_queries"`
}
