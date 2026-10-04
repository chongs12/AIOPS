package api

// 处置 CR 端点 + 告警联动单测：fake RemediationAPI 注入，验证
// 装配缺失 503、参数校验、冲突映射、审批人身份、告警→CR 联动。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	remediationv1 "aiops/internal/apis/remediation/v1"
	"aiops/internal/remediation"
	"aiops/internal/ticket"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gin-gonic/gin"
)

// fakeRemedy 内存实现 RemediationAPI。
type fakeRemedy struct {
	created  []remediation.CreateInput
	approver string
	rejecter string
	items    []remediationv1.Remediation

	errCreate error
	errDecide error
}

func (f *fakeRemedy) Create(_ context.Context, in remediation.CreateInput) (*remediationv1.Remediation, error) {
	if f.errCreate != nil {
		return nil, f.errCreate
	}
	f.created = append(f.created, in)
	return &remediationv1.Remediation{ObjectMeta: metav1.ObjectMeta{Name: "rem-fake"}}, nil
}

func (f *fakeRemedy) Approve(_ context.Context, _, approver, _ string) (*remediationv1.Remediation, error) {
	if f.errDecide != nil {
		return nil, f.errDecide
	}
	f.approver = approver
	return &remediationv1.Remediation{ObjectMeta: metav1.ObjectMeta{Name: "rem-fake"},
		Status: remediationv1.RemediationStatus{Phase: remediationv1.PhasePendingApproval}}, nil
}

func (f *fakeRemedy) Reject(_ context.Context, _, approver, _ string) (*remediationv1.Remediation, error) {
	if f.errDecide != nil {
		return nil, f.errDecide
	}
	f.rejecter = approver
	return &remediationv1.Remediation{ObjectMeta: metav1.ObjectMeta{Name: "rem-fake"},
		Status: remediationv1.RemediationStatus{Phase: remediationv1.PhasePendingApproval}}, nil
}

func (f *fakeRemedy) ListByTicket(context.Context, string) ([]remediationv1.Remediation, error) {
	return f.items, nil
}

// fakeQueue 空任务队列（避免 alertWebhook goroutine 兜底触到 nil orchestrator）。
type fakeQueue struct{}

func (fakeQueue) Enqueue(context.Context, string, string, string, json.RawMessage) error { return nil }

// ---- 端点用例 ----

func remedyServer(t *testing.T) (*Server, *fakeRemedy, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := NewServer(ticket.NewMemStore(), nil, "")
	f := &fakeRemedy{}
	s.WithRemediation(f)
	s.WithQueue(fakeQueue{})
	return s, f, s.Handler().(*gin.Engine)
}

func doJSON(t *testing.T, e *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func mkTicket(t *testing.T, s *Server) string {
	t.Helper()
	tk := ticket.New("5xx 突增", "错误率飙升", "tester")
	if err := s.store.CreateTicket(context.Background(), tk); err != nil {
		t.Fatalf("建单: %v", err)
	}
	return tk.ID
}

func TestCreateRemediation_Accepted(t *testing.T) {
	s, f, e := remedyServer(t)
	id := mkTicket(t, s)

	w := doJSON(t, e, "POST", "/api/v1/tickets/"+id+"/remediations",
		`{"deployment":"demo-app","action":"k8s.scale","replicas":3,"reason":"5xx 突增"}`)
	if w.Code != 202 {
		t.Fatalf("应 202，got %d: %s", w.Code, w.Body.String())
	}
	if len(f.created) != 1 || f.created[0].TicketRef != id || f.created[0].Action != remediationv1.ActionScale {
		t.Fatalf("Create 参数应透传: %+v", f.created)
	}
	if !strings.Contains(w.Body.String(), "rem-fake") {
		t.Fatalf("响应应含 CR 名: %s", w.Body.String())
	}
}

func TestCreateRemediation_503WhenNotWired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := NewServer(ticket.NewMemStore(), nil, "")
	e := s.Handler().(*gin.Engine)
	w := doJSON(t, e, "POST", "/api/v1/tickets/x/remediations", `{}`)
	if w.Code != 503 {
		t.Fatalf("未装配应 503，got %d", w.Code)
	}
}

func TestCreateRemediation_Validation(t *testing.T) {
	s, f, e := remedyServer(t)
	id := mkTicket(t, s)

	// 未知工单 → 404
	if w := doJSON(t, e, "POST", "/api/v1/tickets/nope/remediations", `{"deployment":"d","action":"k8s.scale"}`); w.Code != 404 {
		t.Fatalf("未知工单应 404，got %d", w.Code)
	}
	// 非法 action → 400
	if w := doJSON(t, e, "POST", "/api/v1/tickets/"+id+"/remediations", `{"deployment":"d","action":"restart-node"}`); w.Code != 400 {
		t.Fatalf("非法 action 应 400，got %d", w.Code)
	}
	// 缺 deployment → 400
	if w := doJSON(t, e, "POST", "/api/v1/tickets/"+id+"/remediations", `{"action":"k8s.scale"}`); w.Code != 400 {
		t.Fatalf("缺 deployment 应 400，got %d", w.Code)
	}
	if len(f.created) != 0 {
		t.Fatalf("校验失败不应触达 Create")
	}
}

func TestApproveReject_IdentityAndConflict(t *testing.T) {
	_, f, e := remedyServer(t)

	w := doJSON(t, e, "POST", "/api/v1/remediations/rem-1/approve", `{"note":"ok"}`)
	if w.Code != 200 {
		t.Fatalf("审批应 200，got %d: %s", w.Code, w.Body.String())
	}
	if f.approver != "console" {
		t.Fatalf("鉴权关闭时审批人应记 console，got %q", f.approver)
	}
	if w := doJSON(t, e, "POST", "/api/v1/remediations/rem-1/reject", `{"note":"不合理"}`); w.Code != 200 {
		t.Fatalf("拒绝应 200，got %d", w.Code)
	}
	if f.rejecter != "console" {
		t.Fatalf("拒绝人应记录，got %q", f.rejecter)
	}

	// 状态冲突 → 409
	f.errDecide = remediation.ErrConflict
	if w := doJSON(t, e, "POST", "/api/v1/remediations/rem-1/approve", `{}`); w.Code != 409 {
		t.Fatalf("冲突应 409，got %d", w.Code)
	}
}

func TestListTicketRemediations(t *testing.T) {
	s, f, e := remedyServer(t)
	id := mkTicket(t, s)
	f.items = []remediationv1.Remediation{{
		ObjectMeta: metav1.ObjectMeta{Name: "rem-1"},
		Spec: remediationv1.RemediationSpec{TicketRef: id, Deployment: "demo-app",
			Action: remediationv1.ActionScale},
		Status: remediationv1.RemediationStatus{Phase: remediationv1.PhaseVerifying},
	}}
	w := doJSON(t, e, "GET", "/api/v1/tickets/"+id+"/remediations", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"phase":"Verifying"`) {
		t.Fatalf("列表应包含 CR 视图: %d %s", w.Code, w.Body.String())
	}
}

// ---- 告警联动 ----

func TestAlertWebhook_CreatesRemediationCR(t *testing.T) {
	_, f, e := remedyServer(t)

	body := `{"alerts":[{"labels":{"alertname":"High5xx","service":"question","severity":"critical",
		"aiops-remediation-action":"k8s.scale","aiops-deployment":"makejob-question","aiops-replicas":"4",
		"aiops-reason":"5xx 突增"},"annotations":{"summary":"5xx 突增"},"fingerprint":"fp-1"}]}`
	w := doJSON(t, e, "POST", "/api/v1/alerts/webhook", body)
	if w.Code != 200 {
		t.Fatalf("告警应 200，got %d: %s", w.Code, w.Body.String())
	}
	if len(f.created) != 1 {
		t.Fatalf("应联动创建 1 个处置 CR，got %d", len(f.created))
	}
	in := f.created[0]
	if in.Action != remediationv1.ActionScale || in.Deployment != "makejob-question" ||
		in.Replicas == nil || *in.Replicas != 4 {
		t.Fatalf("CR 参数应来自告警标签: %+v", in)
	}
	if !strings.HasPrefix(in.TicketRef, "t-") {
		t.Fatalf("CR 应关联新建工单，got %q", in.TicketRef)
	}
	if !strings.Contains(w.Body.String(), `"remediation":"rem-fake"`) {
		t.Fatalf("响应应含处置 CR 名: %s", w.Body.String())
	}
}

func TestAlertWebhook_NoRemediationWithoutLabels(t *testing.T) {
	_, f, e := remedyServer(t)
	w := doJSON(t, e, "POST", "/api/v1/alerts/webhook",
		`{"alerts":[{"labels":{"alertname":"Info"},"annotations":{"summary":"x"}}]}`)
	if w.Code != 200 {
		t.Fatalf("告警应 200，got %d", w.Code)
	}
	if len(f.created) != 0 {
		t.Fatalf("无处置标签不应创建 CR")
	}
}

func TestAlertWebhook_MissingDeploymentSkipsCR(t *testing.T) {
	_, f, e := remedyServer(t)
	w := doJSON(t, e, "POST", "/api/v1/alerts/webhook",
		`{"alerts":[{"labels":{"alertname":"High5xx","aiops-remediation-action":"k8s.scale"},
		  "annotations":{"summary":"x"}}]}`)
	if w.Code != 200 {
		t.Fatalf("告警应 200，got %d", w.Code)
	}
	if len(f.created) != 0 {
		t.Fatalf("缺 aiops-deployment 不应创建 CR")
	}
}
