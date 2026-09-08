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
	GetSceneConfig(ctx context.Context, scene string) (agent.SceneParams, bool)
	SetSceneConfig(ctx context.Context, scene string, sp agent.SceneParams) error
}

// CaseSearch 案例检索能力（cases.Store 实现）。
type CaseSearch interface {
	Search(ctx context.Context, query string, topK int) ([]cases.Scored, error)
	Backend() string
}

// WithAuth 注入鉴权（nil = 关闭，全部接口开放）。
func (s *Server) WithAuth(a AuthBackend) *Server {
	s.authSvc = a
	return s
}

// protectedMiddleware 受保护组的中间件集合（鉴权关闭时为空，行为与关闭前完全一致）。
func (s *Server) protectedMiddleware() []gin.HandlerFunc {
	if s.authSvc == nil {
		return nil
	}
	return []gin.HandlerFunc{s.authSvc.Middleware()}
}

// listDeadTasks 死信任务查询（C5 排查面）。
func (s *Server) listDeadTasks(c *gin.Context) {
	if s.dead == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "任务队列未装配"})
		return
	}
	data, err := s.dead.ListDead(c.Request.Context(), 50)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", data)
}

// CaseDeleter 案例删除能力（cases.Store 实现）。
type CaseDeleter interface {
	Delete(ctx context.Context, ticketID string) error
}

// deleteCase 删除知识库案例（PG/Milvus 双后端）。
func (s *Server) deleteCase(c *gin.Context) {
	if s.caseDel == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "案例库未装配"})
		return
	}
	ticketID := c.Param("ticket_id")
	if err := s.caseDel.Delete(c.Request.Context(), ticketID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "案例已删除", "ticket_id": ticketID})
}

// Login 登录换 JWT。
func (s *Server) Login(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Username == "" || req.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账号与密码必填"})
		return
	}
	token, err := s.authSvc.Login(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "账号或密码错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "token_type": "Bearer"})
}

// Me 当前用户（校验令牌有效性）。
func (s *Server) Me(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"username": c.GetString("username"), "role": c.GetString("role")})
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
		Prompt string             `json:"prompt"`
		Config *agent.SceneParams `json:"config,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法"})
		return
	}
	if req.Config != nil {
		if err := s.prompts.SetSceneConfig(c.Request.Context(), c.Param("scene"), *req.Config); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "场景参数已更新，≤5s 生效"})
		return
	}
	if req.Prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt 必填（或提供 config）"})
		return
	}
	if err := s.prompts.Set(c.Request.Context(), c.Param("scene"), req.Prompt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "已更新，≤5s 生效（两级缓存+版本失效）"})
}

// ProviderAdmin provider 链热插拔能力（agent.DBPromptStore 实现）。
type ProviderAdmin interface {
	GetProviders(ctx context.Context) ([]agent.ProviderCfg, bool)
	SetProviders(ctx context.Context, ps []agent.ProviderCfg) error
}

// WithProviderAdmin 注入 provider 热插拔管理。
func (s *Server) WithProviderAdmin(p ProviderAdmin) *Server { s.providers = p; return s }

// getProviders 读当前 provider 链配置。
func (s *Server) getProviders(c *gin.Context) {
	if s.providers == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provider 热插拔未装配"})
		return
	}
	ps, ok := s.providers.GetProviders(c.Request.Context())
	if !ok {
		c.JSON(http.StatusOK, gin.H{"items": []any{}, "note": "未配置自定义链，使用启动配置"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": ps})
}

// setProviders 热更新 provider 链（≤5s 内 watcher 完成替换）。
func (s *Server) setProviders(c *gin.Context) {
	if s.providers == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provider 热插拔未装配"})
		return
	}
	var req struct {
		Providers []agent.ProviderCfg `json:"providers"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Providers) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "providers 必填（至少一项）"})
		return
	}
	for _, p := range req.Providers {
		if p.Name == "" || p.BaseURL == "" || p.APIKey == "" || p.Model == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name/base_url/api_key/model 必填"})
			return
		}
	}
	if err := s.providers.SetProviders(c.Request.Context(), req.Providers); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "provider 链已更新，≤10s 内热替换生效", "count": len(req.Providers)})
}

// resetPrompt 恢复场景为内置模板（删除 DB 覆盖并失效版本）。
func (s *Server) resetPrompt(c *gin.Context) {
	if s.prompts == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "prompt 管理未装配（需 DB）"})
		return
	}
	type resetter interface {
		Reset(ctx context.Context, scene string) error
	}
	r, ok := s.prompts.(resetter)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "当前实现不支持恢复内置"})
		return
	}
	if err := r.Reset(c.Request.Context(), c.Param("scene")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "已恢复内置模板"})
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
