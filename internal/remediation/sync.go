// Package remediation 服务侧对 Remediation CR 的操作门面：
// 创建（工单/告警联动发起）、审批/拒绝（JWT 身份落 CR 可追溯）、按工单反查；
// 以及把 CR 阶段变化镜像回工单消息流的 Sync 循环（SSE 页面实时可见）。
// 处置的执行与回验由 aiops operator 负责，本包绝不直接操作集群负载。
package remediation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	remediationv1 "aiops/internal/apis/remediation/v1"
	"aiops/internal/ticket"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrConflict CR 当前状态不允许该操作（如非待审批阶段请求审批）。
var ErrConflict = errors.New("remediation 状态冲突")

// Client 服务端 CR 操作门面。
type Client struct {
	c         ctrlclient.Client
	namespace string
}

// NewClient 构造。
func NewClient(c ctrlclient.Client, namespace string) *Client {
	return &Client{c: c, namespace: namespace}
}

// CreateInput 创建参数。
type CreateInput struct {
	TicketRef  string // 来源工单；console/脚本来源可空
	Deployment string
	Action     string
	Replicas   *int32
	Reason     string
}

// Create 创建待审批的 Remediation CR。
// 名称 rem-<ticketRef>[-<n>]：同工单多次处置自动递增后缀。
func (cl *Client) Create(ctx context.Context, in CreateInput) (*remediationv1.Remediation, error) {
	if in.Action != remediationv1.ActionScale && in.Action != remediationv1.ActionUndo {
		return nil, fmt.Errorf("%w: 未知处置动作 %q", ErrConflict, in.Action)
	}
	if in.Action == remediationv1.ActionScale && in.Replicas == nil {
		return nil, fmt.Errorf("%w: scale 动作缺少 replicas", ErrConflict)
	}
	if in.Deployment == "" {
		return nil, fmt.Errorf("%w: 缺少目标 deployment", ErrConflict)
	}

	base := "rem-" + in.TicketRef
	if in.TicketRef == "" {
		base = "rem-" + time.Now().Format("20060102-150405")
	}
	rem := &remediationv1.Remediation{
		ObjectMeta: metav1.ObjectMeta{Namespace: cl.namespace},
		Spec: remediationv1.RemediationSpec{
			TicketRef: in.TicketRef, Deployment: in.Deployment, Action: in.Action,
			Replicas: in.Replicas, Reason: in.Reason,
		},
	}
	if in.TicketRef != "" {
		rem.Labels = map[string]string{remediationv1.LabelTicketRef: in.TicketRef}
	}
	for attempt := 0; attempt < 9; attempt++ {
		if attempt > 0 {
			rem.Name = fmt.Sprintf("%s-%d", base, attempt+1)
		} else {
			rem.Name = base
		}
		rem.ResourceVersion = ""
		err := cl.c.Create(ctx, rem)
		if apierrors.IsAlreadyExists(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("创建处置 CR: %w", err)
		}
		return rem.DeepCopy(), nil
	}
	return nil, fmt.Errorf("处置 CR 名称冲突过多: %s*", base)
}

// Approve 审批通过：approver 身份（JWT 用户名）写入 CR status，可追溯。
func (cl *Client) Approve(ctx context.Context, name, approver, note string) (*remediationv1.Remediation, error) {
	return cl.decide(ctx, name, true, approver, note)
}

// Reject 审批拒绝。
func (cl *Client) Reject(ctx context.Context, name, approver, note string) (*remediationv1.Remediation, error) {
	return cl.decide(ctx, name, false, approver, note)
}

func (cl *Client) decide(ctx context.Context, name string, approved bool, approver, note string) (*remediationv1.Remediation, error) {
	var rem remediationv1.Remediation
	key := ctrlclient.ObjectKey{Namespace: cl.namespace, Name: name}
	if err := cl.c.Get(ctx, key, &rem); err != nil {
		return nil, err
	}
	if rem.Status.Phase != "" && rem.Status.Phase != remediationv1.PhasePendingApproval {
		return nil, fmt.Errorf("%w: 阶段 %s 不允许审批/拒绝", ErrConflict, rem.Status.Phase)
	}
	at := metav1.NewTime(time.Now())
	before := rem.DeepCopy()
	rem.Status.Approval = &remediationv1.ApprovalStatus{Approved: approved, Approver: approver, At: &at, Note: note}
	if err := cl.c.Status().Patch(ctx, &rem, ctrlclient.MergeFrom(before)); err != nil {
		return nil, fmt.Errorf("写入审批状态: %w", err)
	}
	return rem.DeepCopy(), nil
}

// Get 按名读取。
func (cl *Client) Get(ctx context.Context, name string) (*remediationv1.Remediation, error) {
	var rem remediationv1.Remediation
	if err := cl.c.Get(ctx, ctrlclient.ObjectKey{Namespace: cl.namespace, Name: name}, &rem); err != nil {
		return nil, err
	}
	return &rem, nil
}

// ListByTicket 反查某工单的全部处置记录。
func (cl *Client) ListByTicket(ctx context.Context, ticketRef string) ([]remediationv1.Remediation, error) {
	var list remediationv1.RemediationList
	err := cl.c.List(ctx, &list,
		ctrlclient.InNamespace(cl.namespace),
		ctrlclient.MatchingLabels{remediationv1.LabelTicketRef: ticketRef})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ListAll 全量列出（sync 镜像循环用）。
func (cl *Client) ListAll(ctx context.Context) ([]remediationv1.Remediation, error) {
	var list remediationv1.RemediationList
	if err := cl.c.List(ctx, &list, ctrlclient.InNamespace(cl.namespace)); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ---- 工单镜像 ----

// ticketStore Sync 依赖的工单存储最小面（PGStore/MemStore 均满足）。
type ticketStore interface {
	GetTicket(ctx context.Context, id string) (*ticket.Ticket, error)
	UpdateTicket(ctx context.Context, t *ticket.Ticket) error
	AppendMessage(ctx context.Context, ticketID string, m *ticket.Message) error
}

// Sync 把 CR 阶段变化镜像进工单消息流，回验通过自动关单（沿用 M3 verify 语义：
// 仅对 investigating/awaiting_info 的工单关单，不覆盖人工已定的终态）。
type Sync struct {
	client   *Client
	store    ticketStore
	interval time.Duration
	// seen 记录已镜像的 phase（进程内存）。重启后每个 CR 会重放一条当前
	// 阶段消息——重复信息幂等无害，换来零额外存储依赖。
	seen map[string]string
}

// NewSync 构造镜像循环（默认 2s 轮询，SSE 页面体感准实时）。
func NewSync(c *Client, store ticketStore) *Sync {
	return &Sync{client: c, store: store, interval: 2 * time.Second, seen: map[string]string{}}
}

// Run 阻塞运行直到 ctx 取消。
func (s *Sync) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

func (s *Sync) tick(ctx context.Context) {
	items, err := s.client.ListAll(ctx)
	if err != nil {
		return // CR API 暂不可用（如本地无集群），静默等待
	}
	for i := range items {
		rem := &items[i]
		if rem.Spec.TicketRef == "" || rem.Status.Phase == "" {
			continue
		}
		if s.seen[rem.Name] == rem.Status.Phase {
			continue // 无阶段变化
		}
		s.seen[rem.Name] = rem.Status.Phase
		s.mirror(ctx, rem)
	}
}

func (s *Sync) mirror(ctx context.Context, rem *remediationv1.Remediation) {
	tk, err := s.store.GetTicket(ctx, rem.Spec.TicketRef)
	if err != nil {
		return // 工单可能已被删除
	}
	msg := mirrorMessage(rem)
	if err := s.store.AppendMessage(ctx, tk.ID, &ticket.Message{Role: "system", Agent: "remediation", Content: msg}); err != nil {
		slog.Warn("处置消息镜像失败", "ticket", tk.ID, "rem", rem.Name, "err", err)
		return
	}
	if rem.Status.Phase == remediationv1.PhaseSucceeded &&
		(tk.Status == ticket.StatusInvestigating || tk.Status == ticket.StatusAwaitingInfo) {
		tk.Status = ticket.StatusResolved
		if err := s.store.UpdateTicket(ctx, tk); err != nil {
			slog.Warn("处置通过后自动关单失败", "ticket", tk.ID, "err", err)
			return
		}
		_ = s.store.AppendMessage(ctx, tk.ID, &ticket.Message{Role: "system", Agent: "remediation",
			Content: "✅ 处置回验通过，工单自动关闭。"})
	}
}

// mirrorMessage 阶段→工单消息文案。
func mirrorMessage(rem *remediationv1.Remediation) string {
	action := fmt.Sprintf("%s %s", rem.Spec.Action, rem.Spec.Deployment)
	switch rem.Status.Phase {
	case remediationv1.PhasePendingApproval:
		reason := rem.Spec.Reason
		if reason == "" {
			return fmt.Sprintf("🔧 已生成处置方案（%s），等待人工审批。", action)
		}
		return fmt.Sprintf("🔧 已生成处置方案（%s），等待人工审批。根因摘要: %s", action, reason)
	case remediationv1.PhaseExecuting:
		approver := ""
		if rem.Status.Approval != nil {
			approver = rem.Status.Approval.Approver
		}
		return fmt.Sprintf("⚙️ 处置已审批（%s），执行中: %s", approver, action)
	case remediationv1.PhaseVerifying:
		if rem.Status.Message != "" {
			return "✅ " + rem.Status.Message + "，进入恢复回验窗口"
		}
		return fmt.Sprintf("✅ 动作已执行（%s），进入恢复回验窗口", action)
	case remediationv1.PhaseSucceeded:
		if rem.Status.Message != "" {
			return "✅ 处置回验通过: " + rem.Status.Message
		}
		return fmt.Sprintf("✅ 处置回验通过（%s），故障已恢复。", action)
	case remediationv1.PhaseFailed:
		return "❌ " + rem.Status.Message
	case remediationv1.PhaseEscalated:
		return "⚠️ 处置回验未通过，转人工跟进: " + rem.Status.Message
	case remediationv1.PhaseRejected:
		note := ""
		if rem.Status.Approval != nil {
			note = rem.Status.Approval.Note
		}
		return "🚫 处置被拒绝: " + note
	}
	return ""
}
