package api

import (
	"context"
	"net/http"

	"aiops/internal/agent"
	"aiops/internal/cases"

	"github.com/gin-gonic/gin"
)

// PromptAdmin Prompt 管理能力（agent.DBPromptStore 实现）。
type PromptAdmin interface {
	ListScenes(ctx context.Context) ([]agent.SceneInfo, error)
	Set(ctx context.Context, scene, prompt string) error
	Get(ctx context.Context, scene string) (string, error)
}

// CaseSearch 案例检索能力（cases.Store 实现）。
type CaseSearch interface {
	Search(ctx context.Context, query string, topK int) ([]cases.Scored, error)
	Backend() string
}

// WithAdmin 注入管理面与案例库能力。
func (s *Server) WithAdmin(p PromptAdmin, cs CaseSearch) *Server {
	s.prompts = p
	s.caseSearch = cs
	return s
}

// listPrompts 列出全部 prompt 场景。
func (s *Server) listPrompts(c *gin.Context) {
	if s.prompts == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "prompt 管理未装配（需 DB）"})
		return
	}
	items, err := s.prompts.ListScenes(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// updatePrompt 热更新 prompt（≤5s 生效，无需重启）。
func (s *Server) updatePrompt(c *gin.Context) {
	if s.prompts == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "prompt 管理未装配（需 DB）"})
		return
	}
	var req struct {
		Prompt string `json:"prompt"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt 必填"})
		return
	}
	if err := s.prompts.Set(c.Request.Context(), c.Param("scene"), req.Prompt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "已更新，≤5s 生效（两级缓存+版本失效）"})
}

// searchCases 相似历史案例检索（M5 验收）。
func (s *Server) searchCases(c *gin.Context) {
	if s.caseSearch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "案例库未装配"})
		return
	}
	q := c.Query("q")
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "q 必填"})
		return
	}
	items, err := s.caseSearch.Search(c.Request.Context(), q, 5)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"backend": s.caseSearch.Backend(), "items": items})
}
