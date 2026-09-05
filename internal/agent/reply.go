package agent

import (
	"context"
	"fmt"

	"aiops/internal/evidence"
	"aiops/internal/ticket"
	"aiops/pkg/llm"
)

// ReplyAgent 工单回复 Agent：基于根因结论与黑板生成结构化事故报告。
type ReplyAgent struct{}

// NewReplyAgent 构造。
func NewReplyAgent() *ReplyAgent { return &ReplyAgent{} }

// Name Agent 名。
func (a *ReplyAgent) Name() string { return NameReply }

// Run 生成事故报告。
func (a *ReplyAgent) Run(ctx context.Context, rc *RunContext) (*Output, error) {
	if rc.RootCause == nil {
		return nil, fmt.Errorf("reply: 缺少根因分析结论")
	}
	rcaJSON, err := marshalCompact(rc.RootCause)
	if err != nil {
		return nil, err
	}

	prompt, err := llm.RenderNamed(rc.Prompts, ctx, "reply", map[string]string{
		"ticket": renderTicketBlock(rc.Ticket),
		"board":  rc.Board.RenderForLLM(),
		"rca":    rcaJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("reply prompt 渲染失败: %w", err)
	}

	var out reportLLM
	if err := rc.LLM.ChatJSON(ctx, llm.ChatRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: prompt}},
	}, &out); err != nil {
		return nil, fmt.Errorf("reply 结构化输出失败: %w", err)
	}

	report := a.toReport(rc, &out)

	return &Output{
		Note:    fmt.Sprintf("事故报告已生成（置信度 %.2f，引用 %d 项证据）", report.Confidence, len(report.Citations)),
		Payload: report,
	}, nil
}

// toReport 汇编报告：根因/置信度以 RCA 结论为准（防 LLM 擅改），引用过滤为真实观察。
func (a *ReplyAgent) toReport(rc *RunContext, out *reportLLM) *ticket.Report {
	r := &ticket.Report{
		RootCause:       rc.RootCause.RootCause, // 权威来源是 RCA，不是 Reply 的复述
		Confidence:      rc.RootCause.Confidence,
		Hypotheses:      rc.RootCause.Hypotheses,
		MissingEvidence: rc.RootCause.MissingEvidence,
		Summary:         out.Summary,
		GeneratedAt:     nowUTC(),
	}
	for _, t := range out.Timeline {
		tt := ticket.TimelineItem{Event: t.Event, Source: t.Source}
		if t.Time != "" {
			if parsed, err := parseTimeRFC3339(t.Time); err == nil {
				tt.Time = parsed
			}
		}
		r.Timeline = append(r.Timeline, tt)
	}
	// 建议 = RCA 原始建议 + LLM 报告补充，按键去重（RCA 优先）。
	combined := append([]ticket.Recommendation{}, rc.RootCause.Recommendations...)
	for _, rec := range out.Recommendations {
		risk := rec.Risk
		if risk != "low" && risk != "high" {
			risk = "high" // 未知风险级别一律按高危
		}
		combined = append(combined, ticket.Recommendation{
			Action: rec.Action, Target: rec.Target, Description: rec.Description, Risk: risk,
		})
	}
	seen := map[string]bool{}
	for _, rec := range combined {
		k := rec.Action + "|" + rec.Target + "|" + rec.Description
		if seen[k] {
			continue
		}
		seen[k] = true
		r.Recommendations = append(r.Recommendations, rec)
	}

	for _, c := range out.Citations {
		if _, ok := rc.Board.ObservationByID(c.ObservationID); ok {
			r.Citations = append(r.Citations, evidence.Citation{
				ObservationID: c.ObservationID, Note: c.Note,
			})
		}
	}
	// RCA 中绑定根因的引用兜底并入。
	for _, c := range rc.RootCause.Citations {
		found := false
		for _, have := range r.Citations {
			if have.ObservationID == c.ObservationID {
				found = true
				break
			}
		}
		if !found {
			r.Citations = append(r.Citations, c)
		}
	}
	return r
}

type timelineLLM2 struct {
	Time   string `json:"time"`
	Event  string `json:"event"`
	Source string `json:"source"`
}

type recommendationLLM2 struct {
	Action      string `json:"action"`
	Target      string `json:"target"`
	Description string `json:"description"`
	Risk        string `json:"risk"`
}

type citationLLM struct {
	ObservationID string `json:"observation_id"`
	Note          string `json:"note"`
}

type reportLLM struct {
	Summary         string               `json:"summary"`
	Timeline        []timelineLLM2       `json:"timeline"`
	Recommendations []recommendationLLM2 `json:"recommendations"`
	Citations       []citationLLM        `json:"citations"`
}
