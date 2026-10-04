package remediation

// Client/Sync 单测：controller-runtime fake client 模拟 API server，
// MemStore 验证镜像循环的消息追加与自动关单。

import (
	"context"
	"errors"
	"strings"
	"testing"

	remediationv1 "aiops/internal/apis/remediation/v1"
	"aiops/internal/apis/scheme"
	"aiops/internal/ticket"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFakeCRClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithStatusSubresource(&remediationv1.Remediation{}).
		WithObjects(objs...).
		Build()
}

func scaleInput() CreateInput {
	replicas := int32(3)
	return CreateInput{TicketRef: "t-1", Deployment: "demo-app", Action: remediationv1.ActionScale, Replicas: &replicas, Reason: "5xx 突增"}
}

func TestCreate_NameAndBackref(t *testing.T) {
	cl := NewClient(newFakeCRClient(t), "default")
	rem, err := cl.Create(context.Background(), scaleInput())
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if rem.Name != "rem-t-1" {
		t.Fatalf("CR 名应为 rem-t-1，got %s", rem.Name)
	}
	items, err := cl.ListByTicket(context.Background(), "t-1")
	if err != nil || len(items) != 1 {
		t.Fatalf("按工单反查应命中 1 条: n=%d err=%v", len(items), err)
	}
}

func TestCreate_DuplicateAutoSuffix(t *testing.T) {
	cl := NewClient(newFakeCRClient(t), "default")
	if _, err := cl.Create(context.Background(), scaleInput()); err != nil {
		t.Fatalf("第一次创建失败: %v", err)
	}
	rem2, err := cl.Create(context.Background(), scaleInput())
	if err != nil {
		t.Fatalf("第二次创建失败: %v", err)
	}
	if rem2.Name != "rem-t-1-2" {
		t.Fatalf("同工单第二次创建应自动加后缀 rem-t-1-2，got %s", rem2.Name)
	}
}

func TestCreate_Validation(t *testing.T) {
	cl := NewClient(newFakeCRClient(t), "default")
	ctx := context.Background()

	if _, err := cl.Create(ctx, CreateInput{TicketRef: "t-1", Deployment: "d", Action: "rm -rf"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("未知动作应 ErrConflict，got %v", err)
	}
	if _, err := cl.Create(ctx, CreateInput{TicketRef: "t-1", Deployment: "d", Action: remediationv1.ActionScale}); !errors.Is(err, ErrConflict) {
		t.Fatalf("scale 缺 replicas 应 ErrConflict，got %v", err)
	}
	if _, err := cl.Create(ctx, CreateInput{TicketRef: "t-1", Action: remediationv1.ActionScale}); !errors.Is(err, ErrConflict) {
		t.Fatalf("缺 deployment 应 ErrConflict，got %v", err)
	}
}

func TestApproveReject_RecordsIdentity(t *testing.T) {
	cl := NewClient(newFakeCRClient(t), "default")
	ctx := context.Background()
	rem, _ := cl.Create(ctx, scaleInput())

	got, err := cl.Approve(ctx, rem.Name, "alice", "同意扩容")
	if err != nil {
		t.Fatalf("审批失败: %v", err)
	}
	if got.Status.Approval == nil || !got.Status.Approval.Approved ||
		got.Status.Approval.Approver != "alice" || got.Status.Approval.Note != "同意扩容" {
		t.Fatalf("审批身份应落 CR status: %+v", got.Status.Approval)
	}
	if got.Status.Approval.At == nil {
		t.Fatalf("审批时间应记录")
	}

	// 拒绝路径
	rem2, _ := cl.Create(ctx, scaleInput())
	got2, err := cl.Reject(ctx, rem2.Name, "bob", "副本数不合理")
	if err != nil {
		t.Fatalf("拒绝失败: %v", err)
	}
	if got2.Status.Approval.Approved {
		t.Fatalf("拒绝后 approved 应为 false")
	}
}

func TestDecide_ConflictOnNonPending(t *testing.T) {
	cl := NewClient(newFakeCRClient(t), "default")
	ctx := context.Background()
	rem, _ := cl.Create(ctx, scaleInput())
	rem.Status.Phase = remediationv1.PhaseExecuting
	if err := cl.c.Status().Update(ctx, rem); err != nil {
		t.Fatalf("构造执行态: %v", err)
	}
	if _, err := cl.Approve(ctx, rem.Name, "alice", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("Executing 阶段审批应 ErrConflict，got %v", err)
	}
	if _, err := cl.Reject(ctx, rem.Name, "alice", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("Executing 阶段拒绝应 ErrConflict，got %v", err)
	}
}

// ---- Sync 镜像循环 ----

// drivePhase 直接把 CR 推进到指定阶段（模拟 operator 的状态迁移）。
func drivePhase(t *testing.T, cl *Client, name, phase, message string) {
	t.Helper()
	rem, err := cl.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("读取 CR: %v", err)
	}
	rem.Status.Phase = phase
	rem.Status.Message = message
	if err := cl.c.Status().Update(context.Background(), rem); err != nil {
		t.Fatalf("推进 CR: %v", err)
	}
}

func msgContents(t *testing.T, store ticket.Store, id string) []string {
	t.Helper()
	msgs, err := store.ListMessages(context.Background(), id)
	if err != nil {
		t.Fatalf("读消息: %v", err)
	}
	out := []string{}
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}

func containsMsg(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestSync_MirrorsPhasesAndAutoResolves(t *testing.T) {
	store := ticket.NewMemStore()
	tk := ticket.New("接口 5xx 突增", "网关错误暴涨", "tester")
	if err := store.CreateTicket(context.Background(), tk); err != nil {
		t.Fatalf("建单: %v", err)
	}
	tk.Status = ticket.StatusInvestigating
	if err := store.UpdateTicket(context.Background(), tk); err != nil {
		t.Fatalf("置 investigating: %v", err)
	}

	cl := NewClient(newFakeCRClient(t), "default")
	replicas := int32(3)
	rem, err := cl.Create(context.Background(), CreateInput{TicketRef: tk.ID, Deployment: "demo-app",
		Action: remediationv1.ActionScale, Replicas: &replicas, Reason: "根因摘要 X"})
	if err != nil {
		t.Fatalf("建 CR: %v", err)
	}

	s := NewSync(cl, store)
	ctx := context.Background()

	// 阶段1：执行中
	drivePhase(t, cl, rem.Name, remediationv1.PhaseExecuting, "动作已执行: 副本数 1 → 3")
	s.tick(ctx)
	msgs := msgContents(t, store, tk.ID)
	if !containsMsg(msgs, "执行中") {
		t.Fatalf("应镜像执行中消息: %v", msgs)
	}

	// 阶段2：回验通过 → 自动关单
	drivePhase(t, cl, rem.Name, remediationv1.PhaseSucceeded, "回验通过: http_5xx_rate=0.000")
	s.tick(ctx)
	tk2, _ := store.GetTicket(context.Background(), tk.ID)
	if tk2.Status != ticket.StatusResolved {
		t.Fatalf("回验通过后工单应自动 resolved，got %s", tk2.Status)
	}
	msgs = msgContents(t, store, tk.ID)
	if !containsMsg(msgs, "自动关闭") {
		t.Fatalf("应有自动关单消息: %v", msgs)
	}

	// 幂等：再次 tick 无新消息
	n := len(msgs)
	s.tick(ctx)
	if len(msgContents(t, store, tk.ID)) != n {
		t.Fatalf("重复 tick 不应产生重复消息")
	}
}

func TestSync_EscalatedDoesNotCloseTicket(t *testing.T) {
	store := ticket.NewMemStore()
	tk := ticket.New("OOM 工单", "内存压力", "tester")
	_ = store.CreateTicket(context.Background(), tk)
	tk.Status = ticket.StatusInvestigating
	_ = store.UpdateTicket(context.Background(), tk)

	cl := NewClient(newFakeCRClient(t), "default")
	rem, _ := cl.Create(context.Background(), CreateInput{TicketRef: tk.ID, Deployment: "demo-app", Action: remediationv1.ActionUndo})
	drivePhase(t, cl, rem.Name, remediationv1.PhaseEscalated, "回验未达标")
	NewSync(cl, store).tick(context.Background())

	tk2, _ := store.GetTicket(context.Background(), tk.ID)
	if tk2.Status != ticket.StatusInvestigating {
		t.Fatalf("Escalated 不应自动关单，got %s", tk2.Status)
	}
	if !containsMsg(msgContents(t, store, tk.ID), "转人工") {
		t.Fatalf("应有转人工提示消息")
	}
}

func TestSync_SkipsConsoleCR(t *testing.T) {
	store := ticket.NewMemStore()
	cl := NewClient(newFakeCRClient(t), "default")
	// console 来源（无 ticketRef）不应触发镜像查询
	if _, err := cl.Create(context.Background(), CreateInput{Deployment: "demo-app", Action: remediationv1.ActionUndo}); err != nil {
		t.Fatalf("console CR 创建失败: %v", err)
	}
	NewSync(cl, store).tick(context.Background()) // 不应 panic / 报错
}
