package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aiops/internal/agent"
	"aiops/internal/orchestrator"
	"aiops/internal/ticket"
	"aiops/internal/tool"
	"aiops/internal/tool/promtool"
	"aiops/pkg/llm"
	"aiops/pkg/telemetry"
)

const e2eIntent = `{"type":"5xx_surge","service":"question","window_minutes":30,"reason":"描述明确"}`
const e2eRCA = `{"hypotheses":[{"statement":"pod 被删","evidence_ids":["@OBS"],"confidence":0.9,"status":"confirmed"}],
 "root_cause":"question 服务 pod 被删除导致上游 5xx","confidence":0.9,
 "recommendations":[{"action":"scale","target":"deployment/makejob-question","description":"扩容恢复","risk":"low"}],
 "need_more_queries":[]}`
const e2eReply = `{"summary":"question 服务 pod 被删导致 5xx。","citations":[]}`

// e2eLLM 与编排器测试相同的 prompt 感知假 LLM。
func e2eLLM(t *testing.T) *llm.Client {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		var body struct {
			Messages []struct {
				Content string
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		content := ""
		if len(body.Messages) > 0 {
			content = body.Messages[len(body.Messages)-1].Content
		}
		reply := `{}`
		switch {
		case strings.Contains(content, "意图识别专家"):
			reply = e2eIntent
		case strings.Contains(content, "根因分析专家"):
			reply = strings.ReplaceAll(e2eRCA, "@OBS", firstObsIDIn(content))
		case strings.Contains(content, "事故报告撰写专家"):
			reply = e2eReply
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": reply}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 10},
		})
	}))
	t.Cleanup(srv.Close)
	c, err := llm.NewClient([]llm.Provider{
		llm.NewOpenAIProvider(llm.OpenAIOptions{Name: "fake", BaseURL: srv.URL, APIKey: "k", Model: "m"}),
	}, llm.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func firstObsIDIn(s string) string {
	i := strings.Index(s, "[obs-")
	if i < 0 {
		return "obs-none"
	}
	j := strings.IndexByte(s[i:], ']')
	if j < 0 {
		return "obs-none"
	}
	return s[i+1 : i+j]
}

// fakePrometheus 假 Prometheus /api/v1/query。
func fakePrometheus(t *testing.T) *promtool.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []any{map[string]any{
					"metric": map[string]string{"job": "makejob-gateway-http"},
					"value":  []any{1788607344, "0.42"},
				}},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return promtool.NewClient(srv.URL, 3*time.Second)
}

// TestEndToEndTicket 通过 HTTP 提交工单 → 异步处理 → 轮询至 resolved → 校验报告与流水。
func TestEndToEndTicket(t *testing.T) {
	// 真实 tracer SDK（不上报），保证 trace id 生成与回放链接。
	tel := telemetry.Init(context.Background(), telemetry.Config{
		Enabled: true, ServiceName: "aiops-e2e-test", OTLPEndpoint: "127.0.0.1:1", SampleRatio: 1.0,
	})
	defer tel.Shutdown(context.Background())

	store := ticket.NewMemStore()
	llmc := e2eLLM(t)

	reg := tool.NewRegistry()
	if err := reg.Register(promtool.NewQueryTool(fakePrometheus(t), promtool.BuiltinLib())); err != nil {
		t.Fatal(err)
	}
	executor := tool.NewExecutor(tool.ExecutorDeps{Registry: reg})
	agents := agent.NewAll(agent.Deps{LLM: llmc})
	orch := orchestrator.New(orchestrator.Options{Store: store, Agents: agents, Tools: executor, LLM: llmc})

	srv := httptest.NewServer(NewServer(store, orch, "http://jaeger.example").Handler())
	t.Cleanup(srv.Close)

	// 1. 提交工单。
	resp, err := http.Post(srv.URL+"/api/v1/tickets", "application/json",
		strings.NewReader(`{"title":"接口 5xx 突增","content":"gateway 出现大量 5xx，question 服务异常","reporter":"oncall"}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || created.ID == "" {
		t.Fatalf("创建响应异常: %d %+v", resp.StatusCode, created)
	}

	// 2. 轮询至终态。
	deadline := time.Now().Add(15 * time.Second)
	var final struct {
		Status   string `json:"status"`
		TraceURL string `json:"trace_url"`
		Error    string `json:"error"`
		Report   *struct {
			RootCause  string  `json:"root_cause"`
			Confidence float64 `json:"confidence"`
			Summary    string  `json:"summary"`
		} `json:"report"`
	}
	for {
		r, err := http.Get(srv.URL + "/api/v1/tickets/" + created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(r.Body).Decode(&final); err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		terminal := final.Status == ticket.StatusResolved || final.Status == ticket.StatusEscalated ||
			final.Status == ticket.StatusFailed
		if terminal || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 3. 校验。
	if final.Status != ticket.StatusResolved {
		t.Fatalf("最终状态 = %s（%s）", final.Status, final.Error)
	}
	if final.Report == nil || final.Report.RootCause == "" || final.Report.Confidence < 0.6 {
		t.Fatalf("报告异常: %+v", final.Report)
	}
	if final.TraceURL == "" {
		t.Error("应返回 trace 链接")
	}

	// 4. 流水与观察接口。
	for _, path := range []string{"/messages", "/observations"} {
		r, err := http.Get(srv.URL + "/api/v1/tickets/" + created.ID + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("%s 状态码 %d", path, r.StatusCode)
		}
		var out struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if len(out.Items) == 0 {
			t.Errorf("%s 不应为空", path)
		}
	}

	// 5. 工单列表。
	r, err := http.Get(srv.URL + "/api/v1/tickets")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil || len(list.Items) != 1 {
		t.Errorf("列表异常: %v %+v", err, list)
	}
}

// TestCreateTicketValidation 提交校验。
func TestCreateTicketValidation(t *testing.T) {
	store := ticket.NewMemStore()
	orch := orchestrator.New(orchestrator.Options{Store: store, Agents: agent.NewAll(agent.Deps{}), LLM: nil})
	srv := httptest.NewServer(NewServer(store, orch, "").Handler())
	t.Cleanup(srv.Close)

	r, err := http.Post(srv.URL+"/api/v1/tickets", "application/json", strings.NewReader(`{"title":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("缺字段应 400, got %d", r.StatusCode)
	}
}
