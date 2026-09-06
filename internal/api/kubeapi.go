// Package api K8s 平台 API 与 Jaeger 代理（全部走工具管道/HTTPX）。
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"aiops/pkg/httpx"

	"github.com/gin-gonic/gin"
)

// listK8sDeployments 部署列表。
func (s *Server) listK8sDeployments(c *gin.Context) {
	s.runConsoleTool(c, "k8s.list_deployments", map[string]any{})
}

// listK8sEvents 事件查询（事件墙）。
func (s *Server) listK8sEvents(c *gin.Context) {
	input := map[string]any{}
	if v := c.Query("pod_prefix"); v != "" {
		input["pod_prefix"] = v
	}
	if v := c.Query("since_minutes"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			input["since_minutes"] = n
		}
	}
	s.runConsoleTool(c, "k8s.events", input)
}

// queryK8sLogs 日志查询。
func (s *Server) queryK8sLogs(c *gin.Context) {
	input := map[string]any{}
	if v := c.Query("pod_prefix"); v != "" {
		input["pod_prefix"] = v
	}
	for _, key := range []string{"since_minutes", "tail_lines"} {
		if v := c.Query(key); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				input[key] = n
			}
		}
	}
	s.runConsoleTool(c, "k8s.logs", input)
}

// runConsoleTool 控制台 K8s 查询/动作统一入口（走完整工具管道：限流/缓存/审计）。
func (s *Server) runConsoleTool(c *gin.Context, toolName string, input map[string]any) {
	if s.tools == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "工具层未装配"})
		return
	}
	raw, _ := json.Marshal(input)
	res, err := s.tools.Execute(c.Request.Context(), ConsoleTicket, toolName, raw)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

// k8sAction 部署页处置动作（白名单 + 人工确认 + 审计）。
func (s *Server) k8sAction(c *gin.Context) {
	var req struct {
		Tool  string          `json:"tool"`
		Input json.RawMessage `json:"input"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法"})
		return
	}
	if !mutatingAllowlist[req.Tool] {
		c.JSON(http.StatusForbidden, gin.H{"error": "工具不在处置白名单: " + req.Tool})
		return
	}
	var in map[string]any
	_ = json.Unmarshal(req.Input, &in)
	if in == nil {
		in = map[string]any{}
	}
	in["confirmed"] = true // 部署页对话框的确认即人工确认
	raw, _ := json.Marshal(in)
	res, err := s.tools.Execute(c.Request.Context(), ConsoleTicket, req.Tool, raw)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

// ---- Jaeger 代理 ----

// jaegerServices 服务列表（链路页）。
func (s *Server) jaegerServices(c *gin.Context) {
	if s.jbase == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Jaeger 未配置"})
		return
	}
	data, err := s.jaegerGet("/api/services")
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.Data(http.StatusOK, "application/json", data)
}

// jaegerTraces 最近 traces（服务参数可选）。
func (s *Server) jaegerTraces(c *gin.Context) {
	if s.jbase == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Jaeger 未配置"})
		return
	}
	limit := c.DefaultQuery("limit", "20")
	q := fmt.Sprintf("/api/traces?limit=%s&lookback=1h", limit)
	if svc := c.Query("service"); svc != "" {
		q += "&service=" + svc
	}
	data, err := s.jaegerGet(q)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.Data(http.StatusOK, "application/json", data)
}

// jaegerGet 经 httpx 客户端请求 Jaeger API（透传 JSON；内置 *.localhost 拨号映射）。
func (s *Server) jaegerGet(path string) ([]byte, error) {
	client := httpx.NewClient(10 * time.Second)
	resp, err := client.Get(s.jbase + path)
	if err != nil {
		return nil, fmt.Errorf("jaeger 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jaeger HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}
