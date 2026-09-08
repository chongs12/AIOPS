package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// fakeGatherer 返回预置的 ai_* 指标家族。
type fakeGatherer struct {
	reg *prometheus.Registry
}

func (f *fakeGatherer) Gather() ([]*dto.MetricFamily, error) {
	return f.reg.Gather()
}

func TestAiUsageEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 构造带 ai_* 指标的 registry（模拟一次真实调用后的状态）。
	reg := prometheus.NewRegistry()
	calls := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ai_calls_total"},
		[]string{"provider", "model", "agent", "status"})
	tokens := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ai_tokens_total"},
		[]string{"provider", "model", "agent", "type"})
	reg.MustRegister(calls, tokens)
	calls.WithLabelValues("ark", "deepseek-v4-flash", "rootcause", "ok").Inc()
	tokens.WithLabelValues("ark", "deepseek-v4-flash", "rootcause", "prompt").Add(1500)
	tokens.WithLabelValues("ark", "deepseek-v4-flash", "rootcause", "completion").Add(320)

	// 挂路由（ai_usage 走 ObsDeps.Gatherer）
	s := &Server{}
	s.WithObservability(&ObsDeps{Gatherer: &fakeGatherer{reg: reg}})

	engine := s.Handler().(*gin.Engine)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/observability/ai_usage", nil)
	engine.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("状态码 %d: %s", w.Code, w.Body.String())
	}
	var d struct {
		TotalPromptTokens     float64 `json:"total_prompt_tokens"`
		TotalCompletionTokens float64 `json:"total_completion_tokens"`
		TotalCalls            float64 `json:"total_calls"`
		ByEntry               []struct {
			Agent        string  `json:"agent"`
			Model        string  `json:"model"`
			PromptTokens float64 `json:"prompt_tokens"`
		} `json:"by_entry"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.TotalCalls != 1 || d.TotalPromptTokens != 1500 || d.TotalCompletionTokens != 320 {
		t.Fatalf("聚合值异常: %+v", d)
	}
	if len(d.ByEntry) == 0 || d.ByEntry[0].Agent != "rootcause" {
		t.Fatalf("by_entry 异常: %+v", d.ByEntry)
	}
}
