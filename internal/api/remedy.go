// 处置 CR API：处置能力的平台化入口。
// 创建/审批/拒绝走 Remediation CR（operator 负责执行与回验），
// 审批人身份（JWT 用户名）落 CR status 可追溯。
// 旧端点 /tickets/:id/actions（应用内直接执行）保留不动，两条链路并存。
package api

import (
	"context"
	"errors"
	"net/http"

	remediationv1 "aiops/internal/apis/remediation/v1"
	"aiops/internal/remediation"

	"github.com/gin-gonic/gin"
)

// RemediationAPI 服务端对 Remediation CR 的操作面（consumer-defined，测试注入 fake）。
type RemediationAPI interface {
	Create(ctx context.Context, in remediation.CreateInput) (*remediationv1.Remediation, error)
	Approve(ctx context.Context, name, approver, note string) (*remediationv1.Remediation, error)
	Reject(ctx context.Context, name, approver, note string) (*remediationv1.Remediation, error)
	ListByTicket(ctx context.Context, ticketRef string) ([]remediationv1.Remediation, error)
}

// WithRemediation 注入处置 CR 门面（K8s 不可用时为 nil，端点返回 503）。
func (s *Server) WithRemediation(r RemediationAPI) *Server { s.remedy = r; return s }

// 允许发起的处置动作白名单（与 tool 层 k8stool 变更工具一致）。
var remedyActions = map[string]bool{remediationv1.ActionScale: true, remediationv1.ActionUndo: true}

// createRemediation godoc
// POST /api/v1/tickets/:id/remediations {"deployment","action","replicas","reason"}
// → 202 创建待审批 CR。
func (s *Server) createRemediation(c *gin.Context) {
	if s.remedy == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "处置 CR 能力未装配（K8s 不可用）"})
		return
	}
	ticketID := c.Param("id")
	if _, err := s.store.GetTicket(c.Request.Context(), ticketID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	var req struct {
		Deployment string `json:"deployment"`
		Action     string `json:"action"`
		Replicas   *int32 `json:"replicas"`
		Reason     string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法: " + err.Error()})
		return
	}
	if !remedyActions[req.Action] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "action 仅支持 k8s.scale / k8s.rollout_undo"})
		return
	}
	if req.Deployment == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "deployment 必填"})
		return
	}
	rem, err := s.remedy.Create(c.Request.Context(), remediation.CreateInput{
		TicketRef: ticketID, Deployment: req.Deployment, Action: req.Action,
		Replicas: req.Replicas, Reason: req.Reason,
	})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, remediation.ErrConflict) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"id":      rem.Name,
		"phase":   phaseOf(rem),
		"message": "处置 CR 已创建，等待人工审批（kubectl get remediations 观察全程）",
	})
}

// approveRemediation godoc
// POST /api/v1/remediations/:name/approve {"note"} → 200（审批人身份落 CR）。
func (s *Server) approveRemediation(c *gin.Context) { s.decideRemediation(c, true) }

// rejectRemediation godoc
// POST /api/v1/remediations/:name/reject {"note"} → 200。
func (s *Server) rejectRemediation(c *gin.Context) { s.decideRemediation(c, false) }

func (s *Server) decideRemediation(c *gin.Context, approved bool) {
	if s.remedy == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "处置 CR 能力未装配（K8s 不可用）"})
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&req) // note 可选，空 body 合法
	// 审批人身份：鉴权开启时取 JWT 用户名；关闭时标记 console（本地/演示模式）。
	approver := c.GetString("username")
	if approver == "" {
		approver = "console"
	}
	var (
		rem *remediationv1.Remediation
		err error
	)
	if approved {
		rem, err = s.remedy.Approve(c.Request.Context(), c.Param("name"), approver, req.Note)
	} else {
		rem, err = s.remedy.Reject(c.Request.Context(), c.Param("name"), approver, req.Note)
	}
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, remediation.ErrConflict) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":      rem.Name,
		"phase":   phaseOf(rem),
		"message": map[bool]string{true: "审批通过，operator 将执行处置", false: "已拒绝"}[approved],
	})
}

// listTicketRemediations godoc
// GET /api/v1/tickets/:id/remediations → 工单的处置记录列表。
func (s *Server) listTicketRemediations(c *gin.Context) {
	if s.remedy == nil {
		c.JSON(http.StatusOK, gin.H{"items": []gin.H{}})
		return
	}
	items, err := s.remedy.ListByTicket(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": remedyViews(items)})
}

// listRemediations godoc
// GET /api/v1/remediations?ticket_id= → 处置记录列表（可按工单过滤）。
func (s *Server) listRemediations(c *gin.Context) {
	if s.remedy == nil {
		c.JSON(http.StatusOK, gin.H{"items": []gin.H{}})
		return
	}
	if tid := c.Query("ticket_id"); tid != "" {
		items, err := s.remedy.ListByTicket(c.Request.Context(), tid)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": remedyViews(items)})
		return
	}
	// 不带过滤：返回空提示（全量列表走 kubectl get remediations，避免无限增长端点）
	c.JSON(http.StatusOK, gin.H{"items": []gin.H{}, "hint": "请带 ticket_id 过滤，或 kubectl get remediations -o wide"})
}

// remedyView 处置记录对外视图（JSON 化 CR 的精简投影）。
type remedyView struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	TicketRef  string `json:"ticket_ref"`
	Deployment string `json:"deployment"`
	Action     string `json:"action"`
	Replicas   *int32 `json:"replicas,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Phase      string `json:"phase"`
	Message    string `json:"message,omitempty"`
	Approver   string `json:"approver,omitempty"`
	Approved   bool   `json:"approved"`
}

func remedyViews(items []remediationv1.Remediation) []remedyView {
	out := make([]remedyView, 0, len(items))
	for i := range items {
		out = append(out, remedyViewOf(&items[i]))
	}
	return out
}

func remedyViewOf(rem *remediationv1.Remediation) remedyView {
	v := remedyView{
		Name: rem.Name, Namespace: rem.Namespace,
		TicketRef: rem.Spec.TicketRef, Deployment: rem.Spec.Deployment,
		Action: rem.Spec.Action, Replicas: rem.Spec.Replicas, Reason: rem.Spec.Reason,
		Phase: phaseOf(rem), Message: rem.Status.Message,
	}
	if rem.Status.Approval != nil {
		v.Approver = rem.Status.Approval.Approver
		v.Approved = rem.Status.Approval.Approved
	}
	return v
}

func phaseOf(rem *remediationv1.Remediation) string {
	if rem.Status.Phase == "" {
		return remediationv1.PhasePendingApproval
	}
	return rem.Status.Phase
}
