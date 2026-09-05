package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aiops/internal/evidence"
	"aiops/internal/ticket"
	"aiops/internal/tool"
	"aiops/internal/tool/promtool"
	"aiops/pkg/llm"
)

// newFakeLLM 依次返回预设回复的假 OpenAI 后端（最后一条循环使用）。
func newFakeLLM(t *testing.T, replies ...string) *llm.Client {
	t.Helper()
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		i := calls
		calls++
		mu.Unlock()
		if i >= len(replies) {
			i = len(replies) - 1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": replies[i]}}},
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

// constTools 固定返回的假工具执行器。
type constTools struct {
	res *tool.Result
	err error
}

func (f *constTools) Execute(_ context.Context, _, _ string, _ json.RawMessage) (*tool.Result, error) {
	return f.res, f.err
}

func baseRC(t *testing.T, llmc *llm.Client, tools ToolExecutor) *RunContext {
	t.Helper()
	tk := ticket.New("接口 5xx 突增", "gateway 出现大量 5xx", "oncall")
	tk.Type = ticket.Type5xxSurge
	tk.Entities.WindowMinutes = 30
	return &RunContext{
		Ticket:  tk,
		Board:   evidence.NewBoard(),
		Tools:   tools,
		LLM:     llmc,
		Prompts: NewBuiltinPromptStore(),
	}
}

const vectorData = `[{"metric":{"job":"makejob-gateway-http"},"value":[1788607344,"0.42"]}]`

func TestIntentAgent(t *testing.T) {
	llmc := newFakeLLM(t, `{"type":"5xx","service":"gateway","window_minutes":0,"keywords":["5xx"],"reason":"描述明确"}`)
	rc := baseRC(t, llmc, &constTools{})

	out, err := NewIntentAgent().Run(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	r := out.Payload.(*IntentResult)
	if r.Type != ticket.Type5xxSurge {
		t.Errorf("type = %s", r.Type)
	}
	if r.Service != "gateway" {
		t.Errorf("service = %s", r.Service)
	}
	if r.WindowMinutes != 30 {
		t.Errorf("window 应取默认 30, got %d", r.WindowMinutes)
	}
	if out.Note == "" {
		t.Error("应有用户可见叙述")
	}
}

func TestIntentAgentLLMFailure(t *testing.T) {
	llmc := newFakeLLM(t, "not json at all")
	rc := baseRC(t, llmc, &constTools{})
	if _, err := NewIntentAgent().Run(context.Background(), rc); err == nil {
		t.Fatal("LLM 输出非法应报错（含修复重试）")
	}
}

func TestMetricsAgentObservations(t *testing.T) {
	llmc := newFakeLLM(t, "{}")
	okTools := &constTools{res: &tool.Result{
		Status:   tool.StatusOK,
		Data:     json.RawMessage(vectorData),
		Evidence: tool.EvidenceMeta{Source: "prometheus", Query: "sum(rate(...))"},
	}}
	rc := baseRC(t, llmc, okTools)

	out, err := NewMetricsAgent().Run(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	r := out.Payload.(*MetricsResult)
	if len(r.ObservationIDs) != 5 || r.FailedCount != 0 {
		t.Errorf("5xx 模板集应有 5 项观察: %+v", r)
	}
	if len(rc.Board.Observations) != 5 {
		t.Errorf("黑板应有 5 条观察: %d", len(rc.Board.Observations))
	}
	for _, o := range rc.Board.Observations {
		if o.Source != "prometheus.query" || o.Query == "" {
			t.Errorf("观察字段缺失: %+v", o)
		}
	}
	if !strings.Contains(rc.Board.Observations[0].Summary, "1 条序列") {
		t.Errorf("摘要应含序列统计: %s", rc.Board.Observations[0].Summary)
	}
}

func TestMetricsAgentFailedQueriesRecorded(t *testing.T) {
	llmc := newFakeLLM(t, "{}")
	badTools := &constTools{res: &tool.Result{
		Status:         tool.StatusFailed,
		DegradedReason: "prometheus 查询不可用: timeout",
		Evidence:       tool.EvidenceMeta{Source: "prometheus"},
	}}
	rc := baseRC(t, llmc, badTools)

	out, err := NewMetricsAgent().Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("查询失败不应中断 Agent: %v", err)
	}
	r := out.Payload.(*MetricsResult)
	if r.FailedCount != 5 {
		t.Errorf("5 项查询应全部记为失败: %d", r.FailedCount)
	}
	// 失败观察仍进入黑板（降级事实进入证据链）。
	if len(rc.Board.Observations) != 5 {
		t.Fatalf("失败观察也应入黑板: %d", len(rc.Board.Observations))
	}
	if !strings.Contains(rc.Board.Observations[0].Summary, "查询失败") {
		t.Errorf("失败摘要异常: %s", rc.Board.Observations[0].Summary)
	}
}

func TestRootCauseAgentEvidenceBinding(t *testing.T) {
	llmc := newFakeLLM(t, `{
		"hypotheses":[{"statement":"pod 被删导致 5xx","evidence_ids":["obs-1"],"confidence":0.9,"status":"confirmed"},
		              {"statement":"数据库故障","evidence_ids":[],"confidence":0.2,"status":"pending"}],
		"root_cause":"question 服务 pod 被删除，上游网关产生 5xx",
		"confidence":0.88,
		"timeline":[{"time":"2026-09-05T10:00:00Z","event":"pod 删除"}],
		"recommendations":[{"action":"scale","target":"deployment/makejob-question","description":"扩容回副本数","risk":"low"}],
		"missing_evidence":[],
		"need_more_queries":[]}`)
	rc := baseRC(t, llmc, &constTools{})
	rc.Ticket.Entities.Service = "question"
	oid := rc.Board.AddObservation(evidence.Observation{
		Source: "prometheus.query", Query: "sum(rate(...))", Status: "ok", Summary: "5xx 速率 0.42",
	})
	if oid != "obs-1" {
		// 黑板 ID 是随机 hex；为让 LLM 回复中的 obs-1 有效，直接改名。
		rc.Board.Observations[0].ID = "obs-1"
	}

	out, err := NewRootCauseAgent(promtoolLib()).Run(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	r := out.Payload.(*RootCauseResult)
	if r.RootCause == "" {
		t.Fatalf("有效证据应支撑根因: %+v", r)
	}
	if r.Confidence < 0.8 {
		t.Errorf("置信度异常: %.2f", r.Confidence)
	}
	if len(r.Citations) == 0 || r.Citations[0].ObservationID != "obs-1" {
		t.Errorf("引用异常: %+v", r.Citations)
	}
	if len(rc.Board.Hypotheses) != 2 {
		t.Errorf("假设应写入黑板: %d", len(rc.Board.Hypotheses))
	}
	if len(r.Recommendations) != 1 || r.Recommendations[0].Risk != "low" {
		t.Errorf("建议异常: %+v", r.Recommendations)
	}
	if len(r.Timeline) != 1 || r.Timeline[0].Time.IsZero() {
		t.Errorf("时间线异常: %+v", r.Timeline)
	}
}

func TestRootCauseAgentUnbackedConclusionPenalized(t *testing.T) {
	llmc := newFakeLLM(t, `{
		"hypotheses":[{"statement":"幻觉结论","evidence_ids":["obs-fake"],"confidence":0.95,"status":"confirmed"}],
		"root_cause":"数据库被外星人删除",
		"confidence":0.95,
		"need_more_queries":[]}`)
	rc := baseRC(t, llmc, &constTools{})

	out, err := NewRootCauseAgent(promtoolLib()).Run(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	r := out.Payload.(*RootCauseResult)
	if r.RootCause != "" {
		t.Errorf("无证据根因必须被清除, got %q", r.RootCause)
	}
	if r.Confidence > 0.3 {
		t.Errorf("无证据置信度应降权: %.2f", r.Confidence)
	}
	if len(r.MissingEvidence) == 0 {
		t.Error("应记录缺失证据说明")
	}
}

func TestRootCauseAgentSanitizesQueries(t *testing.T) {
	llmc := newFakeLLM(t, `{
		"hypotheses":[],"root_cause":"","confidence":0.1,
		"need_more_queries":[
			{"tool":"prometheus.query","input":{"template":"http_5xx_ratio","params":{"window":"15m"}},"reason":"补充占比"},
			{"tool":"prometheus.query","input":{"template":"evil_template"},"reason":"幻觉模板"},
			{"tool":"shell.exec","input":{},"reason":"幻觉工具"}]}`)
	rc := baseRC(t, llmc, &constTools{})

	out, err := NewRootCauseAgent(promtoolLib()).Run(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	r := out.Payload.(*RootCauseResult)
	if len(r.NeedMoreQueries) != 1 {
		t.Errorf("应只保留 1 个合法补查: %d", len(r.NeedMoreQueries))
	}
	if len(r.NeedMoreQueries) == 1 && !strings.Contains(string(r.NeedMoreQueries[0].Input), "http_5xx_ratio") {
		t.Errorf("保留的补查模板异常: %s", r.NeedMoreQueries[0].Input)
	}
}

func TestReplyAgentReport(t *testing.T) {
	llmc := newFakeLLM(t, `{
		"summary":"question 服务一个 Pod 被删除，网关出现 5xx，已恢复。",
		"timeline":[{"event":"Pod 删除","source":"obs-1"}],
		"recommendations":[{"action":"rollout_undo","target":"deployment/makejob-question","description":"回滚","risk":"low"}],
		"citations":[{"observation_id":"obs-1","note":"5xx 速率"},{"observation_id":"obs-fake","note":"幻觉引用"}]}`)
	rc := baseRC(t, llmc, &constTools{})
	rc.Board.Observations = append(rc.Board.Observations, evidence.Observation{ID: "obs-1", Source: "prometheus.query", Status: "ok"})
	rc.RootCause = &RootCauseResult{
		RootCause:  "pod 被删导致上游 5xx",
		Confidence: 0.88,
		Citations:  []evidence.Citation{{ObservationID: "obs-1"}},
		Recommendations: []ticket.Recommendation{
			{Action: "scale", Target: "deployment/makejob-question", Description: "扩容", Risk: "low"},
		},
	}

	out, err := NewReplyAgent().Run(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	report := out.Payload.(*ticket.Report)
	if report.RootCause != "pod 被删导致上游 5xx" {
		t.Errorf("报告根因应以 RCA 为准: %s", report.RootCause)
	}
	if report.Confidence != 0.88 {
		t.Errorf("置信度应以 RCA 为准: %.2f", report.Confidence)
	}
	if len(report.Citations) != 1 || report.Citations[0].ObservationID != "obs-1" {
		t.Errorf("幻觉引用应被过滤: %+v", report.Citations)
	}
	if len(report.Recommendations) != 2 {
		t.Errorf("建议应含 RCA+报告共 2 条: %+v", report.Recommendations)
	}
	if report.Summary == "" || report.GeneratedAt.IsZero() {
		t.Errorf("报告字段缺失: %+v", report)
	}
}

// promtoolLib 测试用模板库。
func promtoolLib() *promtool.Lib { return promtool.BuiltinLib() }
