package promtool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"aiops/internal/tool"
)

// 常量样本：Prometheus vector 响应 data.result。
const sampleVector = `[{"metric":{"job":"makejob-gateway-http"},"value":[1788607344,"0.42"]}]`

func fakeProm(t *testing.T, status int, body string) *Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, 3*time.Second)
}

func TestLibRenderDefaults(t *testing.T) {
	lib := BuiltinLib()
	tpl, ok := lib.Get("http_5xx_rate")
	if !ok {
		t.Fatal("内置模板缺失")
	}
	expr, err := lib.Render(tpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if expr != `sum(rate(http_requests_total{code=~"5.."}[5m]))` {
		t.Errorf("默认 window 应为 5m: %s", expr)
	}
	expr, err = lib.Render(tpl, map[string]string{"window": "30m"})
	if err != nil || expr != `sum(rate(http_requests_total{code=~"5.."}[30m]))` {
		t.Errorf("自定义 window 失败: %s %v", expr, err)
	}
}

func TestLibRenderRejectsInjection(t *testing.T) {
	lib := BuiltinLib()
	tpl, _ := lib.Get("http_5xx_rate")
	// window 不匹配 pattern → 拒绝。
	if _, err := lib.Render(tpl, map[string]string{"window": "5m\"} or vector(1)"}); err == nil {
		t.Fatal("非法 window 应被 pattern 拒绝")
	}
	// 未声明参数 → 丢弃（表达式只含已声明占位，注入面不放大）。
	expr, err := lib.Render(tpl, map[string]string{"evil": "1"})
	if err != nil || expr != `sum(rate(http_requests_total{code=~"5.."}[5m]))` {
		t.Errorf("未声明参数应被丢弃: %s %v", expr, err)
	}
}

func TestLibRenderForLLMListsAll(t *testing.T) {
	out := BuiltinLib().RenderForLLM()
	for _, want := range []string{"http_5xx_rate", "http_5xx_ratio", "up_status", "http_latency_p99"} {
		if !containsStr(out, want) {
			t.Errorf("清单缺 %s", want)
		}
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestQueryToolHappyPath(t *testing.T) {
	client := fakeProm(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":`+sampleVector+`}}`)
	qt := NewQueryTool(client, BuiltinLib())

	res, err := qt.Execute(context.Background(), json.RawMessage(`{"template":"http_5xx_rate","params":{"window":"5m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != tool.StatusOK || res.FromCache {
		t.Fatalf("res = %+v", res)
	}
	if res.Evidence.Source != "prometheus" {
		t.Errorf("evidence.source = %s", res.Evidence.Source)
	}
	if len(res.Evidence.Excerpt) == 0 {
		t.Error("应有证据摘录")
	}
	var rows []map[string]any
	if err := json.Unmarshal(res.Data, &rows); err != nil || len(rows) != 1 {
		t.Errorf("data 应为 1 条序列: %v", err)
	}
}

func TestQueryToolUnknownTemplate(t *testing.T) {
	client := fakeProm(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	qt := NewQueryTool(client, BuiltinLib())

	res, err := qt.Execute(context.Background(), json.RawMessage(`{"template":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != tool.StatusFailed || res.DegradedReason == "" {
		t.Errorf("未知模板应 failed: %+v", res)
	}
}

func TestQueryToolPrometheusDown(t *testing.T) {
	client := fakeProm(t, http.StatusInternalServerError, `boom`)
	qt := NewQueryTool(client, BuiltinLib())

	res, err := qt.Execute(context.Background(), json.RawMessage(`{"template":"up_status"}`))
	if err != nil {
		t.Fatalf("数据源故障应转 failed 信封而非 error: %v", err)
	}
	if res.Status != tool.StatusFailed || res.Evidence.Query == "" {
		t.Errorf("res = %+v", res)
	}
}

func TestQueryToolBadEndtime(t *testing.T) {
	client := fakeProm(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	qt := NewQueryTool(client, BuiltinLib())
	res, _ := qt.Execute(context.Background(), json.RawMessage(`{"template":"up_status","end_time":"not-a-time"}`))
	if res.Status != tool.StatusFailed {
		t.Errorf("非法 end_time 应 failed: %+v", res)
	}
}

// TestQueryToolAgainstRealCluster 集成测试：设 AIOPS_IT=1 且本地集群可达时运行。
func TestQueryToolAgainstRealCluster(t *testing.T) {
	if !itEnabled() {
		t.Skip("跳过集成测试（设 AIOPS_IT=1 启用）")
	}
	client := NewClient("http://prometheus.localhost:8080", 10*time.Second)
	qt := NewQueryTool(client, BuiltinLib())
	res, err := qt.Execute(context.Background(), json.RawMessage(`{"template":"up_status"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != tool.StatusOK {
		t.Fatalf("真实集群查询应成功: %+v", res)
	}
	t.Logf("真实集群返回 %d 字节数据", len(res.Data))
}

func itEnabled() bool { return envOr("AIOPS_IT", "") == "1" }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
