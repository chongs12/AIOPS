package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"aiops/internal/tool"
	"aiops/internal/tool/promtool"

	"github.com/gin-gonic/gin"
)

// ConsoleTicket 控制台查询的 ticketID 标记（审计区分工单与控制台流量）。
const ConsoleTicket = "console"

// TemplateLister 模板库清单（promtool.Lib 实现，视图为 promtool.APIView）。
type TemplateLister interface {
	ListForAPI() []promtool.APIView
}

// ServiceResolver 服务标签解析（job/instance → 服务名；配置注入，避免平台命名硬编码）。
type ServiceResolver func(metric map[string]string) string

// ObsDeps 观测查询依赖：模板库 + 工具执行管道（控制台查询走同一管道）。
type ObsDeps struct {
	Templates TemplateLister
	Tools     ToolExecutor
	Resolve   ServiceResolver // 可 nil = 缺省规则（service 标签 → job 前缀剥离 → instance）
}

// WithObservability 注入观测查询能力。
func (s *Server) WithObservability(obs *ObsDeps) *Server {
	s.obs = obs
	return s
}

// listTemplates 模板清单。
func (s *Server) listTemplates(c *gin.Context) {
	if s.obs == nil || s.obs.Templates == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "观测查询未装配"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": s.obs.Templates.ListForAPI()})
}

// runQuery 控制台查询（模板或自由模式），统一走工具执行管道。
func (s *Server) runQuery(c *gin.Context) {
	if s.obs == nil || s.obs.Tools == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "观测查询未装配"})
		return
	}
	var req struct {
		Mode        string            `json:"mode"` // "" 模板 / "free"
		Template    string            `json:"template"`
		Expr        string            `json:"expr"`
		Params      map[string]string `json:"params"`
		Start       string            `json:"start"`
		End         string            `json:"end"`
		StepSeconds int               `json:"step_seconds"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法"})
		return
	}
	if req.Start == "" || req.End == "" {
		now := time.Now().UTC()
		req.End = now.Format(time.RFC3339)
		req.Start = now.Add(-time.Hour).Format(time.RFC3339)
	}
	if req.StepSeconds == 0 {
		req.StepSeconds = 60
	}
	if req.Params == nil {
		req.Params = map[string]string{}
	}

	var toolName string
	input := map[string]any{
		"start": req.Start, "end": req.End, "step_seconds": req.StepSeconds,
	}
	if req.Mode == "free" {
		if req.Expr == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "free 模式需要 expr"})
			return
		}
		toolName = "prometheus.free_query"
		input["expr"] = req.Expr
	} else {
		if req.Template == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "template 必填（或 mode=free）"})
			return
		}
		toolName = "prometheus.query_range"
		input["template"] = req.Template
		input["params"] = req.Params
	}
	raw, _ := json.Marshal(input)

	res, err := s.obs.Tools.Execute(c.Request.Context(), ConsoleTicket, toolName, raw)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

// overview 总览聚合：服务 up 状态 + 5xx 速率 + 总速率 + P99（并发执行，全部走管道）。
func (s *Server) overview(c *gin.Context) {
	if s.obs == nil || s.obs.Tools == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "观测查询未装配"})
		return
	}
	ctx := c.Request.Context()

	type qout struct {
		tpl string
		res *tool.Result
		err error
	}
	templates := []string{"up_status", "http_5xx_rate", "http_total_rate", "http_latency_p99"}
	ch := make(chan qout, len(templates))
	for _, tpl := range templates {
		go func(tpl string) {
			input, _ := json.Marshal(map[string]any{
				"template": tpl, "params": map[string]string{"window": "5m"},
			})
			res, err := s.obs.Tools.Execute(ctx, ConsoleTicket, "prometheus.query", input)
			ch <- qout{tpl: tpl, res: res, err: err}
		}(tpl)
	}
	results := map[string]*tool.Result{}
	errs := map[string]error{}
	for i := 0; i < len(templates); i++ {
		o := <-ch
		if o.err != nil {
			errs[o.tpl] = o.err
		} else {
			results[o.tpl] = o.res
		}
	}

	out := gin.H{"generated_at": time.Now().UTC().Format(time.RFC3339)}
	failed := []string{}

	if r := results["up_status"]; r != nil && r.Status == tool.StatusOK {
		out["services"] = parseUpByService(r.Data, s.obs.Resolve)
	} else {
		out["services"] = []map[string]any{}
		failed = append(failed, "up_status")
	}
	for tpl, key := range map[string]string{
		"http_5xx_rate": "err_5xx_rate", "http_total_rate": "total_rate", "http_latency_p99": "p99_seconds",
	} {
		if r := results[tpl]; r != nil && r.Status == tool.StatusOK {
			out[key] = firstFloat(r.Data)
		} else {
			out[key] = nil
			failed = append(failed, tpl)
		}
	}
	out["failed_queries"] = failed
	c.JSON(http.StatusOK, out)
}

// parseUpByService 解析 up 序列 → 按服务聚合 up 状态（同一服务多实例取最小值）。
func parseUpByService(data json.RawMessage, resolve ServiceResolver) []map[string]any {
	var rows []struct {
		Metric map[string]string `json:"metric"`
		Value  []any             `json:"value"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil
	}
	svc := map[string]float64{}
	for _, r := range rows {
		name := r.Metric["service"]
		if name == "" && resolve != nil {
			name = resolve(r.Metric)
		}
		if name == "" {
			name = r.Metric["instance"]
		}
		v := firstNumber(r.Value)
		if cur, ok := svc[name]; !ok || v < cur {
			svc[name] = v
		}
	}
	out := make([]map[string]any, 0, len(svc))
	for name, v := range svc {
		out = append(out, map[string]any{"service": name, "up": v == 1, "value": v})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["service"].(string) < out[j]["service"].(string)
	})
	return out
}

func firstNumber(value []any) float64 {
	if len(value) < 2 {
		return 0
	}
	var f float64
	_, _ = fmt.Sscan(fmt.Sprint(value[1]), &f)
	return f
}

// firstFloat 取首个序列的当前值。
func firstFloat(data json.RawMessage) *float64 {
	var rows []struct {
		Value []any `json:"value"`
	}
	if err := json.Unmarshal(data, &rows); err != nil || len(rows) == 0 {
		return nil
	}
	f := firstNumber(rows[0].Value)
	return &f
}
