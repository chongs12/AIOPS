// Package orchestrator 实现编排引擎：工单处理状态机 + 黑板 + 回环控制。
// 状态流转（M1）: submitted → investigating → resolved | escalated_human | failed
// （M2 增加 awaiting_info 挂起/恢复）。回环：根因证据不足时补查，上限 maxRounds。
// 可靠性：每步落库（断点信息）、panic 恢复转 failed、全程单条 trace。
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"aiops/internal/agent"
	"aiops/internal/cases"
	"aiops/internal/evidence"
	"aiops/internal/ticket"
	"aiops/pkg/llm"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// CaseStore 案例库能力（M5：相似召回 + 已解决沉淀）。
type CaseStore interface {
	Save(ctx context.Context, c *cases.Case) error
	Search(ctx context.Context, query string, topK int) ([]cases.Scored, error)
	Backend() string
}

// Orchestrator 编排引擎。
type Orchestrator struct {
	store      ticket.Store
	agents     map[string]agent.Agent
	tools      agent.ToolExecutor
	llm        *llm.Client
	prompts    llm.TemplateStore
	cases      CaseStore
	sceneCfgs  agent.SceneConfigSource
	maxRounds  int
	confThresh float64
	tracer     trace.Tracer
}

// Options 编排器参数。
type Options struct {
	Store               ticket.Store
	Agents              map[string]agent.Agent
	Tools               agent.ToolExecutor
	LLM                 *llm.Client
	Prompts             llm.TemplateStore       // nil = 内置
	MaxRounds           int                     // 根因补查回环上限，默认 3
	ConfidenceThreshold float64                 // 低于则转人工，默认 0.6
	CaseStore           CaseStore               // 可 nil
	SceneCfgs           agent.SceneConfigSource // 场景参数热更新来源，可 nil
}

// New 构造编排器。
func New(opts Options) *Orchestrator {
	if opts.MaxRounds <= 0 {
		opts.MaxRounds = 3
	}
	if opts.ConfidenceThreshold <= 0 {
		opts.ConfidenceThreshold = agent.ConfidenceThreshold
	}
	if opts.Prompts == nil {
		opts.Prompts = agent.NewBuiltinPromptStore()
	}
	return &Orchestrator{
		store: opts.Store, agents: opts.Agents, tools: opts.Tools,
		llm: opts.LLM, prompts: opts.Prompts, cases: opts.CaseStore, sceneCfgs: opts.SceneCfgs,
		maxRounds: opts.MaxRounds, confThresh: opts.ConfidenceThreshold,
		tracer: otel.Tracer("aiops/internal/orchestrator"),
	}
}

// Process 同步处理一个工单（API 层负责异步调用；M2 换 pkg/async 任务状态机）。
// 契约：除 ctx 取消外，任何内部错误都不向上抛——工单转 failed 并落库原因。
func (o *Orchestrator) Process(ctx context.Context, ticketID string) error {
	return o.process(ctx, ticketID, false)
}

// Resume 从断点续跑：用户补充信息后跳过意图识别，直接进入检索与推理阶段。
func (o *Orchestrator) Resume(ctx context.Context, ticketID string) error {
	return o.process(ctx, ticketID, true)
}

func (o *Orchestrator) process(ctx context.Context, ticketID string, skipIntent bool) error {
	ctx, span := o.tracer.Start(ctx, "orchestrator.process",
		trace.WithAttributes(
			attribute.String("ticket.id", ticketID),
			attribute.Bool("resume", skipIntent),
		))
	defer span.End()

	defer func() {
		if r := recover(); r != nil {
			slog.Error("编排 panic，工单转 failed", "ticket", ticketID, "panic", r)
			o.markFailed(context.WithoutCancel(ctx), ticketID, fmt.Sprintf("内部异常: %v", r))
			span.SetStatus(codes.Error, fmt.Sprint(r))
		}
	}()

	if o.llm == nil {
		o.markFailed(ctx, ticketID, "LLM 未配置（设置 LLM_API_KEY 后可用）")
		return nil
	}

	tk, err := o.store.GetTicket(ctx, ticketID)
	if err != nil {
		return fmt.Errorf("加载工单: %w", err)
	}
	// 断点续跑：awaiting_info 状态由 API 层转为 investigating 后从意图之后继续；
	// 全新工单必须为 submitted。
	if skipIntent {
		if tk.Status != ticket.StatusInvestigating {
			return fmt.Errorf("工单 %s 状态为 %s，不可续跑", ticketID, tk.Status)
		}
	} else if tk.Status != ticket.StatusSubmitted {
		return fmt.Errorf("工单 %s 状态为 %s，不可处理", ticketID, tk.Status)
	}

	// trace id 落库（一次工单处理 = 一条分布式 trace）。
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		tk.TraceID = sc.TraceID().String()
	}
	tk.Status = ticket.StatusInvestigating
	if err := o.store.UpdateTicket(ctx, tk); err != nil {
		return err
	}

	board := evidence.NewBoard()
	saved := map[string]bool{} // 已落库的观察

	// 1. 意图识别（fresh 与 resume 都执行：补充内容需重新评估完整性）。
	{
		intentOut, err := o.runAgent(ctx, tk, board, agent.NameIntent)
		if err != nil {
			return o.failStep(ctx, tk, "意图识别失败: "+err.Error())
		}
		intent := intentOut.Payload.(*agent.IntentResult)
		o.applyIntent(tk, intent)

		// 多轮 HITL：不完整 → 挂起追问（最多 2 轮）；超限则带默认值继续。
		if intent.NeedMoreInfo && len(intent.Questions) > 0 {
			round := 1
			if tk.Pending != nil {
				round = tk.Pending.Round + 1
			}
			if round <= 2 {
				tk.Status = ticket.StatusAwaitingInfo
				tk.Pending = &ticket.PendingInfo{Questions: intent.Questions, Round: round, AskedAt: time.Now()}
				if err := o.store.UpdateTicket(ctx, tk); err != nil {
					return err
				}
				o.appendMsg(ctx, tk.ID, "agent", agent.NameIntent,
					fmt.Sprintf("第 %d 轮追问，需要补充信息：\n- ", round)+strings.Join(intent.Questions, "\n- "))
				span.SetAttributes(attribute.String("ticket.outcome", "awaiting_info"))
				return nil
			}
			o.appendMsg(ctx, tk.ID, "agent", agent.NameIntent,
				"已达追问上限，带默认参数继续处理。")
		}
		tk.Pending = nil
		if err := o.store.UpdateTicket(ctx, tk); err != nil {
			return err
		}
	}

	// 1.5 相似历史案例召回（M5）：进黑板供根因参考。
	if o.cases != nil {
		if scores, err := o.cases.Search(ctx, tk.Title+" "+tk.Content, 3); err == nil &&
			len(scores) > 0 && scores[0].Score >= 0.35 {
			board.AddObservation(evidence.Observation{
				Source:  "cases.search",
				Query:   "similar resolved cases",
				Status:  "ok",
				Summary: summarizeCases(scores),
			})
		}
	}

	// 2. 指标检索。
	if _, err := o.runAgent(ctx, tk, board, agent.NameMetrics); err != nil {
		return o.failStep(ctx, tk, "指标检索失败: "+err.Error())
	}
	if err := o.persistObservations(ctx, tk.ID, board, saved); err != nil {
		return err
	}

	// 3. 根因推理 + 补查回环（上限 maxRounds）。
	rca, err := o.rootCauseLoop(ctx, tk, board, saved)
	if err != nil {
		return o.failStep(ctx, tk, "根因推理失败: "+err.Error())
	}

	// 4. 证据不足/低置信度 → 转人工。
	if rca.RootCause == "" || rca.Confidence < o.confThresh {
		tk.Status = ticket.StatusEscalated
		o.appendMsg(ctx, tk.ID, "system", "",
			fmt.Sprintf("证据不足或置信度 %.2f 低于阈值 %.2f，已转人工处理。缺失证据: %v",
				rca.Confidence, o.confThresh, rca.MissingEvidence))
		if err := o.store.UpdateTicket(ctx, tk); err != nil {
			return err
		}
		span.SetAttributes(attribute.String("ticket.outcome", "escalated"))
		return nil
	}

	// 5. 事故报告。
	rc := &agent.RunContext{Ticket: tk, Board: board, RootCause: rca,
		Tools: o.tools, LLM: o.llm, Prompts: o.prompts, SceneCfg: o.sceneCfgs, Ctx: ctx}
	replyOut, err := o.runAgentWith(ctx, rc, agent.NameReply)
	if err != nil {
		return o.failStep(ctx, tk, "报告生成失败: "+err.Error())
	}
	tk.Report = replyOut.Payload.(*ticket.Report)
	tk.Status = ticket.StatusResolved
	if err := o.store.UpdateTicket(ctx, tk); err != nil {
		return err
	}
	span.SetAttributes(
		attribute.String("ticket.outcome", "resolved"),
		attribute.Float64("rca.confidence", rca.Confidence),
	)

	// 案例沉淀（M5）：已解决工单异步入库，供后续相似召回。
	if o.cases != nil {
		c := &cases.Case{
			TicketID: tk.ID, Type: tk.Type, Service: tk.Entities.Service,
			RootCause: tk.Report.RootCause, Summary: tk.Report.Summary, CreatedAt: time.Now(),
		}
		go func() {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := o.cases.Save(sctx, c); err != nil {
				slog.Warn("案例沉淀失败", "ticket", tk.ID, "err", err)
			}
		}()
	}
	return nil
}

// summarizeCases 案例召回摘要（供黑板与 LLM）。
func summarizeCases(scores []cases.Scored) string {
	var parts []string
	for i, s := range scores {
		if i >= 3 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s[类型=%s 相似度=%.2f] 根因: %s",
			s.TicketID, s.Type, s.Score, firstRunes(s.RootCause, 80)))
	}
	if len(parts) == 0 {
		return "无相似历史案例"
	}
	return "相似历史案例：" + strings.Join(parts, "；")
}

// firstRunes 取前 n 个 rune。
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// rootCauseLoop 根因推理回环：证据不足 → 执行补查 → 再推理，上限 maxRounds。
func (o *Orchestrator) rootCauseLoop(ctx context.Context, tk *ticket.Ticket, board *evidence.Board, saved map[string]bool) (*agent.RootCauseResult, error) {
	for round := 1; ; round++ {
		out, err := o.runAgent(ctx, tk, board, agent.NameRootCause)
		if err != nil {
			return nil, err
		}
		r := out.Payload.(*agent.RootCauseResult)
		r.Rounds = round
		if err := o.persistObservations(ctx, tk.ID, board, saved); err != nil {
			return nil, err
		}

		if len(r.NeedMoreQueries) == 0 || round >= o.maxRounds {
			if len(r.NeedMoreQueries) > 0 {
				r.MissingEvidence = append(r.MissingEvidence,
					fmt.Sprintf("回环已达上限 %d 轮，仍有未完成的补查请求", o.maxRounds))
			}
			return r, nil
		}

		// 执行补查（失败的查询由工具层转 failed 信封，进入证据链）。
		for _, q := range r.NeedMoreQueries {
			tr, err := o.tools.Execute(ctx, tk.ID, q.Tool, q.Input)
			if err != nil {
				slog.Warn("补查调用失败", "ticket", tk.ID, "tool", q.Tool, "err", err)
				continue
			}
			board.AddObservation(agent.ObservationFromToolResult(templateOf(q), tr))
		}
		o.appendMsg(ctx, tk.ID, "agent", agent.NameRootCause,
			fmt.Sprintf("第 %d 轮补查完成，继续推理", round))
		_ = o.persistObservations(ctx, tk.ID, board, saved)
	}
}

// applyIntent 把意图结果写回工单。
func (o *Orchestrator) applyIntent(tk *ticket.Ticket, intent *agent.IntentResult) {
	tk.Type = intent.Type
	tk.Entities.Service = intent.Service
	tk.Entities.Namespace = intent.Namespace
	tk.Entities.Keywords = intent.Keywords
	if intent.WindowMinutes > 0 {
		tk.Entities.WindowMinutes = intent.WindowMinutes
	}
}

// runAgent 组装 RunContext 并运行 Agent，附 span 与流水消息。
func (o *Orchestrator) runAgent(ctx context.Context, tk *ticket.Ticket, board *evidence.Board, name string) (*agent.Output, error) {
	rc := &agent.RunContext{Ticket: tk, Board: board, Tools: o.tools, LLM: o.llm,
		Prompts: o.prompts, SceneCfg: o.sceneCfgs, Ctx: ctx}
	return o.runAgentWith(ctx, rc, name)
}

func (o *Orchestrator) runAgentWith(ctx context.Context, rc *agent.RunContext, name string) (*agent.Output, error) {
	a, ok := o.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %q 未注册", name)
	}
	sctx, span := o.tracer.Start(ctx, "agent.run",
		trace.WithAttributes(attribute.String("agent.name", name)))
	defer span.End()

	out, err := a.Run(sctx, rc)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	o.appendMsg(sctx, rc.Ticket.ID, "agent", name, out.Note)
	return out, nil
}

// persistObservations 增量落库黑板观察（断点/回放数据基础）。
func (o *Orchestrator) persistObservations(ctx context.Context, ticketID string, board *evidence.Board, saved map[string]bool) error {
	for i := range board.Observations {
		obs := &board.Observations[i]
		if saved[obs.ID] {
			continue
		}
		if err := o.store.SaveObservation(ctx, ticketID, obs); err != nil {
			return fmt.Errorf("观察落库: %w", err)
		}
		saved[obs.ID] = true
	}
	return nil
}

func (o *Orchestrator) appendMsg(ctx context.Context, ticketID, role, agentName, content string) {
	if content == "" {
		return
	}
	if err := o.store.AppendMessage(ctx, ticketID, &ticket.Message{
		Role: role, Agent: agentName, Content: content, CreatedAt: time.Now(),
	}); err != nil {
		slog.Warn("工单消息落库失败", "ticket", ticketID, "err", err)
	}
}

// failStep 步骤失败：工单转 failed 并落库原因；返回 nil（不向调用方抛出）。
func (o *Orchestrator) failStep(ctx context.Context, tk *ticket.Ticket, reason string) error {
	o.markFailed(ctx, tk.ID, reason)
	return nil
}

func (o *Orchestrator) markFailed(ctx context.Context, ticketID, reason string) {
	tk, err := o.store.GetTicket(ctx, ticketID)
	if err != nil {
		return
	}
	tk.Status = ticket.StatusFailed
	tk.Error = reason
	_ = o.store.UpdateTicket(ctx, tk)
	o.appendMsg(ctx, ticketID, "system", "", "处理失败: "+reason)
}

// templateOf 补查请求的模板名（观察 query 摘要用）。
func templateOf(q agent.QueryRequest) string {
	var in struct {
		Template string `json:"template"`
	}
	_ = json.Unmarshal(q.Input, &in)
	return in.Template
}
