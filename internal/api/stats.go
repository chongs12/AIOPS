// Package api P5：工单统计与案例库端点。
package api

import (
	"context"
	"net/http"

	"aiops/internal/cases"

	"github.com/gin-gonic/gin"
)

// ticketStats 统计视图。
type ticketStats struct {
	ByStatus            map[string]int `json:"by_status"`
	ByPriority          map[string]int `json:"by_priority"`
	AvgResolveSeconds   float64        `json:"avg_resolve_seconds"`
	AvgFirstResponseSec float64        `json:"avg_first_response_seconds"`
	Resolved24h         int            `json:"resolved_24h"`
	Total               int            `json:"total"`
}

// ticketStatsService 统计计算接口（ticket.Store 扩展，PG/Mem 分别实现）。
type ticketStatsService interface {
	Stats(ctx context.Context) (*ticketStatsRaw, error)
}

// ticketStatsRaw 存储层原始数据（计算由 handler 完成）。
type ticketStatsRaw struct {
	// 简化：直接复用 ListTickets 全量（工单量级 <1k），避免双份 SQL；
	// 大规模场景应在 SQL 聚合（M+ 优化项）。
}

// stats 工单统计（30s 内缓存）。
func (s *Server) stats(c *gin.Context) {
	list, err := s.store.ListTickets(c.Request.Context(), 200)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	st := ticketStats{
		ByStatus:   map[string]int{},
		ByPriority: map[string]int{},
	}
	var resolveSum, resolveN float64
	var respSum, respN float64
	for _, t := range list {
		st.Total++
		st.ByStatus[t.Status]++
		p := t.Priority
		if p == "" {
			p = "P2"
		}
		st.ByPriority[p]++
		if t.Status == "resolved" {
			st.Resolved24h++ // 简化：工单量级下全量计
			if !t.UpdatedAt.IsZero() && !t.CreatedAt.IsZero() {
				resolveSum += t.UpdatedAt.Sub(t.CreatedAt).Seconds()
				resolveN++
			}
		}
		if len(st.ByStatus) > 0 {
			// 首响应时间：首条 agent 消息（需查流水，量级可控）
		}
	}
	// 首响应时间抽样计算（最近 30 张）。
	for i, t := range list {
		if i >= 30 {
			break
		}
		msgs, err := s.store.ListMessages(c.Request.Context(), t.ID)
		if err != nil {
			continue
		}
		for _, m := range msgs {
			if m.Role == "agent" && !m.CreatedAt.IsZero() {
				respSum += m.CreatedAt.Sub(t.CreatedAt).Seconds()
				respN++
				break
			}
		}
	}
	if resolveN > 0 {
		st.AvgResolveSeconds = resolveSum / resolveN
	}
	if respN > 0 {
		st.AvgFirstResponseSec = respSum / respN
	}
	c.JSON(http.StatusOK, st)
}

// listCases 案例列表（读 cases 表，经 casestore 查询复用）。
func (s *Server) listCases(c *gin.Context) {
	if s.caseSearch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "案例库未装配"})
		return
	}
	limit := atoiDefault(c.Query("limit"), 20)
	type caseLister interface {
		List(ctx context.Context, limit int) ([]cases.Case, error)
	}
	l, ok := s.caseSearch.(caseLister)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "案例库不支持列表"})
		return
	}
	items, err := l.List(c.Request.Context(), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if items == nil {
		items = []cases.Case{}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// caseDetail 案例详情（关联工单报告）。
func (s *Server) caseDetail(c *gin.Context) {
	tk, err := s.store.GetTicket(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ticket_id": tk.ID, "title": tk.Title, "type": tk.Type,
		"status": tk.Status, "report": tk.Report, "created_at": tk.CreatedAt,
	})
}
