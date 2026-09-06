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
	"aiops/pkg/notify"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TaskEnqueuer 异步任务入队（tasks.Queue 实现）。
type TaskEnqueuer interface {
	Enqueue(ctx context.Context, ticketID, kind, idempotencyKey string, payload json.RawMessage) error
}

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
	queue      TaskEnqueuer
	verifyCfg  VerifyConfig
	notifier   *notify.Notifier
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
	Queue               TaskEnqueuer            // 异步队列（复查任务用），可 nil
	VerifyCfg           *VerifyConfig           // 复查配置，nil 用默认
	Notifier            *notify.Notifier        // 终态通知 webhook，可 nil
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
	verifyCfg := DefaultVerifyConfig()
	if opts.VerifyCfg != nil {
		verifyCfg = *opts.VerifyCfg
	}
	return &Orchestrator{
		store: opts.Store, agents: opts.Agents, tools: opts.Tools,
		llm: opts.LLM, prompts: opts.Prompts, cases: opts.CaseStore, sceneCfgs: opts.SceneCfgs,
		queue: opts.Queue, notifier: opts.Notifier, verifyCfg: verifyCfg,
		maxRounds: opts.MaxRounds, confThresh: opts.ConfidenceThreshold,
		tracer: otel.Tracer("aiops/internal/orchestrator"),
	}
}

// Process 同步处理一个全新工单（submitted）。
func (o *Orchestrator) Process(ctx context.Context, ticketID string) error {
	return o.process(ctx, ticketID, modeFull)
}

// Resume 从 HITL 断点续跑：用户补充信息后重新评估完整性（多轮追问）。
func (o *Orchestrator) Resume(ctx context.Context, ticketID string) error {
	return o.process(ctx, ticketID, modeResume)
}

// Handle 异步任务入口：按工单当前状态分派，幂等且可安全重试。
// 这是 pkg/tasks worker 的调用契约——崩溃后重试不会把工单卡死在中间态。
func (o *Orchestrator) Handle(ctx context.Context, ticketID string, resume bool) error {
	tk, err := o.store.GetTicket(ctx, ticketID)
	if err != nil {
		return fmt.Errorf("加载工单: %w", err)
	}
	switch tk.Status {
	case ticket.StatusSubmitted:
		return o.process(ctx, ticketID, modeFull)
	case ticket.StatusAwaitingInfo:
		// 等待用户补充是稳态（非失败）；若 resume 任务先到也按续跑处理。
		if !resume {
			return nil
		}
		return o.process(ctx, ticketID, modeResume)
	case ticket.StatusInvestigating:
		// 处理中重入：resume 任务重试走续跑语义（意图重评估）；
		// 原始 process 任务重试走崩溃恢复（黑板从已落库观察重建）。
		if resume {
			return o.process(ctx, ticketID, modeResume)
		}
		return o.process(ctx, ticketID, modeRecover)
	default:
		// 终态幂等：任务重复投递直接成功。
		return nil
	}
}

// processMode 处理模式。
type processMode int

const (
	modeFull    processMode = iota // 全新处理（意图→检索→推理→报告）
	modeResume                     // HITL 续跑（意图重评估，多轮追问）
	modeRecover                    // 崩溃恢复（黑板重建，跳过意图直接推理）
)

func (o *Orchestrator) process(ctx context.Context, ticketID string, mode processMode) error {
	ctx, span := o.tracer.Start(ctx, "orchestrator.process",
		trace.WithAttributes(
			attribute.String("ticket.id", ticketID),
			attribute.String("ticket.mode", fmt.Sprintf("%d", mode)),
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
	// 状态门：resume/recover 允许 investigating（恢复语义），full 必须 submitted。
	if mode == modeFull && tk.Status != ticket.StatusSubmitted {
		return fmt.Errorf("工单 %s 状态为 %s，不可处理", ticketID, tk.Status)
	}
	if mode != modeFull && tk.Status != ticket.StatusInvestigating {
		return fmt.Errorf("工单 %s 状态为 %s，不可续跑", ticketID, tk.Status)
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

	// 1. 意图识别（fresh 与 resume 都执行：补充内容需重新评估完整性；recover 跳过——实体已持久化）。
	if mode != modeRecover {
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

	// 1.4 崩溃恢复：从已落库观察重建黑板；已有观察则跳过重复检索。
	if mode == modeRecover {
		if obs, err := o.store.ListObservations(ctx, tk.ID); err == nil && len(obs) > 0 {
			for i := range obs {
				board.Observations = append(board.Observations, *obs[i])
				saved[obs[i].ID] = true
			}
			o.appendMsg(ctx, tk.ID, "system", "",
				fmt.Sprintf("崩溃恢复：从断点重建黑板（%d 条已落库观察），继续根因推理。", len(obs)))
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

	// 2. 指标检索（恢复模式下已有观察则跳过，避免重复查询污染黑板）。
	if mode == modeRecover && len(board.Observations) > 0 {
		// 已重建，直接进入根因推理。
	} else if _, err := o.runAgent(ctx, tk, board, agent.NameMetrics); err != nil {
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
		o.notify(ctx, tk, "ticket.escalated")
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
	o.notify(ctx, tk, "ticket.resolved")

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

// notify 终态 webhook 通知（P4；失败不阻塞）。
func (o *Orchestrator) notify(ctx context.Context, tk *ticket.Ticket, event string) {
	if o.notifier == nil || !o.notifier.Enabled() {
		return
	}
	go o.notifier.Notify(context.WithoutCancel(ctx), notify.Payload{
		Event: event, TicketID: tk.ID, Title: tk.Title, Status: tk.Status,
		Confidence: func() float64 {
			if tk.Report != nil {
				return tk.Report.Confidence
			}
			return 0
		}(),
	})
}

// summarizeCases 案例召回摘要（供黑板与 LLM）。
func summarizeCases(scores []cases.Scored) string {
	var parts []string
	for i, s := range scores {
		if i >= 3 {
			break
		}
		cause := s.RootCause
		if cause == "" {
			cause = s.Summary // Milvus 行 RootCause 未存时回退摘要
		}
		parts = append(parts, fmt.Sprintf("%s[类型=%s 相似度=%.2f] 根因: %s",
			s.TicketID, s.Type, s.Score, firstRunes(cause, 80)))
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
