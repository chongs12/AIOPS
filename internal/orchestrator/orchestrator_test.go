package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aiops/internal/agent"
	"aiops/internal/ticket"
	"aiops/internal/tool"
	"aiops/pkg/llm"
	"aiops/pkg/telemetry"

	"go.opentelemetry.io/otel"
)

const intentReply = `{"type":"5xx_surge","service":"question","window_minutes":30,"reason":"描述明确"}`

// promptAwareLLM 从 prompt 内容判断当前步骤并回复合适 JSON；
// 根因步骤会把黑板渲染文本中的真实观察 ID 回填进证据引用。
func newPromptAwareLLM(t *testing.T, rcaReply string) *llm.Client {
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
			reply = intentReply
		case strings.Contains(content, "根因分析专家"):
			reply = strings.ReplaceAll(rcaReply, "@OBS", firstObsID(content))
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

// firstObsID 从黑板渲染文本里抓取第一个 [obs-xxx]。
func firstObsID(s string) string {
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

// seqLLM 严格按序回复的假 LLM（不识别步骤）。
func newSeqLLM(t *testing.T, replies ...string) *llm.Client {
	var mu sync.Mutex
	calls := 0
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

// scriptTools 按调用次序返回预设结果的假工具执行器。
type scriptTools struct {
	mu    sync.Mutex
	items []*tool.Result
	calls int
}

func (s *scriptTools) Execute(_ context.Context, _, _ string, _ json.RawMessage) (*tool.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if len(s.items) == 0 {
		return &tool.Result{Status: tool.StatusFailed, DegradedReason: "no script"}, nil
	}
	if i >= len(s.items) {
		i = len(s.items) - 1
	}
	return s.items[i], nil
}

func (s *scriptTools) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// newTestOrch 组装编排器。
func newTestOrch(t *testing.T, llmc *llm.Client, tools agent.ToolExecutor, customize func(*Options)) (*Orchestrator, *ticket.MemStore) {
	t.Helper()
	store := ticket.NewMemStore()
	opts := Options{
		Store:  store,
		Agents: agent.NewAll(agent.Deps{LLM: llmc}),
		Tools:  tools,
		LLM:    llmc,
	}
	if customize != nil {
		customize(&opts)
	}
	return New(opts), store
}

const okVector = `[{"metric":{"job":"makejob-gateway-http"},"value":[1788607344,"0.42"]}]`

const rcaOK = `{"hypotheses":[{"statement":"pod 被删","evidence_ids":["@OBS"],"confidence":0.9,"status":"confirmed"}],
 "root_cause":"question 服务 pod 被删除导致上游 5xx","confidence":0.9,
 "recommendations":[{"action":"scale","target":"deployment/makejob-question","description":"扩容恢复","risk":"low"}],
 "need_more_queries":[]}`

func TestProcessHappyPath(t *testing.T) {
	llmc := newPromptAwareLLM(t, rcaOK)
	tools := &scriptTools{items: []*tool.Result{{
		Status:   tool.StatusOK,
		Data:     json.RawMessage(okVector),
		Evidence: tool.EvidenceMeta{Source: "prometheus", Query: `sum(rate(http_requests_total{code=~"5.."}[5m]))`},
	}}}
	o, store := newTestOrch(t, llmc, tools, nil)

	tk := ticket.New("接口 5xx 突增", "gateway 出现大量 5xx，question 服务异常", "oncall")
	if err := store.CreateTicket(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	// 测试环境初始化真实 tracer SDK（不上报，仅生成真实 trace id）。
	tel := telemetry.Init(context.Background(), telemetry.Config{
		Enabled: true, ServiceName: "aiops-test", OTLPEndpoint: "127.0.0.1:1", SampleRatio: 1.0,
	})
	defer tel.Shutdown(context.Background())
	// 模拟 API 层先起 trace（一次工单处理 = 一条分布式 trace）。
	tctx, span := otel.Tracer("test").Start(context.Background(), "test.root")
	defer span.End()
	if err := o.Process(tctx, tk.ID); err != nil {
		t.Fatalf("Process: %v", err)
	}

	got, err := store.GetTicket(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusResolved {
		t.Fatalf("状态 = %s（error=%s），流水: %v", got.Status, got.Error, msgSummary(t, store, tk.ID))
	}
	if got.Report == nil || got.Report.RootCause == "" {
		t.Fatalf("应有报告: %+v", got.Report)
	}
	if got.Report.Confidence < 0.6 {
		t.Errorf("置信度 = %.2f", got.Report.Confidence)
	}
	if len(got.Report.Recommendations) == 0 {
		t.Error("报告应含处置建议")
	}
	if got.TraceID == "" {
		t.Error("trace id 应落库")
	}
	if got.Type != ticket.Type5xxSurge || got.Entities.Service != "question" {
		t.Errorf("意图结果未写回工单: %+v", got.Entities)
	}
	if obs, _ := store.ListObservations(context.Background(), tk.ID); len(obs) < 5 {
		t.Errorf("至少 5 条观察落库: %d", len(obs))
	}
	if msgs, _ := store.ListMessages(context.Background(), tk.ID); len(msgs) < 4 {
		t.Errorf("流水消息过少: %d", len(msgs))
	}
}

const rcaNeedMore = `{"hypotheses":[{"statement":"初步怀疑","evidence_ids":["@OBS"],"confidence":0.4,"status":"pending"}],
 "root_cause":"","confidence":0.3,
 "need_more_queries":[{"tool":"prometheus.query","input":{"template":"http_5xx_ratio","params":{"window":"15m"}},"reason":"补充占比证据"}]}`

func TestProcessLoopWithMoreQueries(t *testing.T) {
	// RCA 第一次要求补查，第二次出结论——根因 prompt 出现两次，
	// promptAwareLLM 只认步骤不认轮次，因此这里用自定义序列：
	// 第 1 次 RCA 用 rcaNeedMore（把 @OBS 替换为真实 ID），第 2 次用 rcaOK。
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			reply = intentReply
		case strings.Contains(content, "根因分析专家"):
			mu.Lock()
			i := calls
			calls++
			mu.Unlock()
			if i == 0 {
				reply = strings.ReplaceAll(rcaNeedMore, "@OBS", firstObsID(content))
			} else {
				reply = strings.ReplaceAll(rcaOK, "@OBS", firstObsID(content))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": reply}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 10},
		})
	}))
	t.Cleanup(srv.Close)
	llmc, err := llm.NewClient([]llm.Provider{
		llm.NewOpenAIProvider(llm.OpenAIOptions{Name: "fake", BaseURL: srv.URL, APIKey: "k", Model: "m"}),
	}, llm.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}

	tools := &scriptTools{items: []*tool.Result{
		{Status: tool.StatusOK, Data: json.RawMessage(okVector), Evidence: tool.EvidenceMeta{Source: "prometheus", Query: "q1"}},
		{Status: tool.StatusOK, Data: json.RawMessage(`[{"metric":{},"value":[1,"0.9"]}]`), Evidence: tool.EvidenceMeta{Source: "prometheus", Query: "q2"}},
	}}
	o, store := newTestOrch(t, llmc, tools, nil)

	tk := ticket.New("接口 5xx 突增", "描述", "oncall")
	_ = store.CreateTicket(context.Background(), tk)
	if err := o.Process(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}

	got, _ := store.GetTicket(context.Background(), tk.ID)
	if got.Status != ticket.StatusResolved {
		t.Fatalf("回环后应解决: %s（%s）", got.Status, got.Error)
	}
	if tools.count() < 6 { // 5(首轮模板) + 1(补查)
		t.Errorf("工具应至少执行 6 次, got %d", tools.count())
	}
	if obs, _ := store.ListObservations(context.Background(), tk.ID); len(obs) < 6 {
		t.Errorf("补查观察应落库: %d", len(obs))
	}
}

func TestProcessEscalatesOnLowConfidence(t *testing.T) {
	llmc := newPromptAwareLLM(t,
		`{"hypotheses":[{"statement":"猜测","confidence":0.3,"status":"pending"}],
		 "root_cause":"","confidence":0.2,"missing_evidence":["日志不可得"],"need_more_queries":[]}`)
	tools := &scriptTools{items: []*tool.Result{{Status: tool.StatusFailed, DegradedReason: "查询失败"}}}
	o, store := newTestOrch(t, llmc, tools, nil)

	tk := ticket.New("5xx", "描述", "oncall")
	_ = store.CreateTicket(context.Background(), tk)
	if err := o.Process(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetTicket(context.Background(), tk.ID)
	if got.Status != ticket.StatusEscalated {
		t.Fatalf("低置信度应转人工: %s（%s）", got.Status, got.Error)
	}
}

func TestProcessPanicRecovered(t *testing.T) {
	llmc := newSeqLLM(t, intentReply)
	store := ticket.NewMemStore()
	agents := agent.NewAll(agent.Deps{LLM: llmc})
	agents[agent.NameMetrics] = panicAgent{}
	o := New(Options{Store: store, Agents: agents, Tools: &scriptTools{}, LLM: llmc})

	tk := ticket.New("t", "c", "r")
	_ = store.CreateTicket(context.Background(), tk)
	_ = o.Process(context.Background(), tk.ID) // 不应向上抛 panic

	got, _ := store.GetTicket(context.Background(), tk.ID)
	if got.Status != ticket.StatusFailed {
		t.Fatalf("panic 应转 failed: %s", got.Status)
	}
	if got.Error == "" {
		t.Error("应记录失败原因")
	}
}

func TestProcessFailsWithoutLLM(t *testing.T) {
	store := ticket.NewMemStore()
	o := New(Options{Store: store, Agents: agent.NewAll(agent.Deps{}), Tools: &scriptTools{}, LLM: nil})
	tk := ticket.New("t", "c", "r")
	_ = store.CreateTicket(context.Background(), tk)
	_ = o.Process(context.Background(), tk.ID)
	got, _ := store.GetTicket(context.Background(), tk.ID)
	if got.Status != ticket.StatusFailed || !strings.Contains(got.Error, "LLM") {
		t.Fatalf("LLM 未配置应 failed: %+v", got)
	}
}

type panicAgent struct{}

func (panicAgent) Name() string { return agent.NameMetrics }
func (panicAgent) Run(context.Context, *agent.RunContext) (*agent.Output, error) {
	panic("boom")
}

func msgSummary(t *testing.T, s ticket.Store, id string) []string {
	t.Helper()
	msgs, err := s.ListMessages(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Agent+": "+m.Content)
	}
	return out
}

const intentNeedMore = `{"type":"5xx_surge","service":"question","window_minutes":0,"need_more_info":true,
 "questions":["故障大约从什么时间开始？","影响哪些接口？"],"reason":"缺少时间窗"}`

// newHITLLLM 意图首轮要求追问；根因步骤回填真实观察 ID。
func newHITLLLM(t *testing.T) *llm.Client {
	var mu sync.Mutex
	intentCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			mu.Lock()
			i := intentCalls
			intentCalls++
			mu.Unlock()
			if i == 0 {
				reply = intentNeedMore // 首轮：信息不全 → 追问
			} else {
				reply = intentReply // 兜底（续跑不经过意图）
			}
		case strings.Contains(content, "根因分析专家"):
			reply = strings.ReplaceAll(rcaOK, "@OBS", firstObsID(content))
		case strings.Contains(content, "事故报告撰写专家"):
			reply = `{"summary":"补充信息后定位完成。","citations":[]}`
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

func TestProcessHITLSuspendAndResume(t *testing.T) {
	tools := &scriptTools{items: []*tool.Result{{
		Status:   tool.StatusOK,
		Data:     json.RawMessage(okVector),
		Evidence: tool.EvidenceMeta{Source: "prometheus", Query: `sum(rate(http_requests_total{code=~"5.."}[5m]))`},
	}}}
	store := ticket.NewMemStore()
	// 动态 LLM：首次意图 → 追问；根因 → 回填真实观察 ID 的结论；报告 → 固定。
	llmc2 := newHITLLLM(t)
	o2 := New(Options{Store: store, Agents: agent.NewAll(agent.Deps{LLM: llmc2}), Tools: tools, LLM: llmc2})

	tk := ticket.New("接口 5xx 突增", "gateway 出现大量 5xx", "oncall")
	if err := store.CreateTicket(context.Background(), tk); err != nil {
		t.Fatal(err)
	}

	// 第一轮：信息不全 → awaiting_info 挂起。
	if err := o2.Process(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetTicket(context.Background(), tk.ID)
	if got.Status != ticket.StatusAwaitingInfo {
		t.Fatalf("应挂起等待补充: %s（%s）", got.Status, got.Error)
	}
	if got.Pending == nil || len(got.Pending.Questions) != 2 {
		t.Fatalf("应记录追问: %+v", got.Pending)
	}

	// 用户回复：内容合并、状态回 investigating、断点续跑（跳过意图）。
	got.Content += "\n[补充回复] 大约 30 分钟前开始，影响全部接口。"
	got.Pending = nil
	got.Status = ticket.StatusInvestigating
	_ = store.UpdateTicket(context.Background(), got)
	if err := o2.Resume(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}

	final, _ := store.GetTicket(context.Background(), tk.ID)
	if final.Status != ticket.StatusResolved {
		t.Fatalf("续跑后应解决: %s（%s）", final.Status, final.Error)
	}
	if final.Report == nil {
		t.Fatal("应有报告")
	}
}

// TestProcessCrashRecover 模拟 investigating 中途崩溃：Handle 重试应从已落库观察重建黑板并完成推理。
func TestProcessCrashRecover(t *testing.T) {
	llmc := newPromptAwareLLM(t, rcaOK)
	tools := &scriptTools{items: []*tool.Result{{
		Status:   tool.StatusOK,
		Data:     json.RawMessage(okVector),
		Evidence: tool.EvidenceMeta{Source: "prometheus", Query: "sum(rate(...))"},
	}}}
	store := ticket.NewMemStore()
	o := New(Options{Store: store, Agents: agent.NewAll(agent.Deps{LLM: llmc}), Tools: tools, LLM: llmc})

	// 第一阶段：跑到 investigating 并落库 5 条观察（然后"崩溃"——直接丢弃内存黑板）。
	tk := ticket.New("接口 5xx 突增", "gateway 出现大量 5xx", "oncall")
	_ = store.CreateTicket(context.Background(), tk)
	var firstObs int
	for i := 0; i < 5 && firstObs == 0; i++ {
		if err := o.Process(context.Background(), tk.ID); err != nil {
			t.Fatal(err)
		}
		got, _ := store.GetTicket(context.Background(), tk.ID)
		obs, _ := store.ListObservations(context.Background(), tk.ID)
		firstObs = len(obs)
		_ = got
		break
	}
	obs, _ := store.ListObservations(context.Background(), tk.ID)
	if len(obs) == 0 {
		t.Fatalf("前置失败：无观察落库，无法模拟崩溃中间态")
	}
	// 强制把工单置回 investigating 模拟崩溃现场（Process 已终态则人为重置场景）。
	got, _ := store.GetTicket(context.Background(), tk.ID)
	if got.Status == ticket.StatusResolved {
		got.Status = ticket.StatusInvestigating
		_ = store.UpdateTicket(context.Background(), got)
	}
	// 崩溃重试：Handle(resume=false) → modeRecover → 黑板重建 → 推理 → resolved。
	if err := o.Handle(context.Background(), tk.ID, false); err != nil {
		t.Fatal(err)
	}
	final, _ := store.GetTicket(context.Background(), tk.ID)
	if final.Status != ticket.StatusResolved {
		t.Fatalf("崩溃恢复后应 resolved: %s（%s）", final.Status, final.Error)
	}
	// 恢复不应重复产生观察（重建的 5 条不重复落库）。
	obsAfter, _ := store.ListObservations(context.Background(), tk.ID)
	if len(obsAfter) != len(obs) {
		t.Errorf("恢复不应新增观察: before=%d after=%d", len(obs), len(obsAfter))
	}
}
