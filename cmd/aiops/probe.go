package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"aiops/pkg/config"
	"aiops/pkg/httpx"
	"aiops/pkg/llm"
	"aiops/pkg/telemetry"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetes "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// runProbe 环境连通性自检：逐项探测并汇总，硬检查失败以非零码退出。
func runProbe(args []string) {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	cfgPath := fs.String("config", "configs/config.yaml", "配置文件路径")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		slog.Error("加载配置失败", "err", err)
		os.Exit(1)
	}

	p := &prober{httpc: httpx.NewClient(10 * time.Second)}
	ctx := context.Background()

	// telemetry 全程共享一次初始化：LLM 自检与回环验证的 span 同批上报。
	tel := telemetry.Init(ctx, telemetry.Config{
		Enabled:      cfg.Telemetry.Enabled,
		ServiceName:  cfg.Telemetry.ServiceName,
		OTLPEndpoint: cfg.Telemetry.OTLPEndpoint,
		SampleRatio:  cfg.Telemetry.SampleRatio,
	})

	fmt.Println("== AIOps 环境探测 ==")

	// 硬检查：真实数据源三件套。
	p.checkPrometheus(ctx, cfg.Prometheus)
	p.checkJaeger(cfg.Jaeger)
	p.checkK8s(ctx, cfg.K8s)

	// 软检查：开发依赖（make devup 可启动）与 LLM（需配置 key）。
	p.checkRedis(ctx, cfg.Redis)
	p.checkPostgres(cfg.Postgres)
	p.checkLLM(ctx, cfg)

	// OTLP 回环：flush 后从 Jaeger 查回本服务，验证 trace 全链路。
	p.checkOTLPRoundTrip(ctx, cfg, tel)

	p.report()
}

type checkResult struct {
	name string
	ok   bool
	soft bool // 软检查失败仅告警，不影响退出码
	note string
}

type prober struct {
	httpc   *http.Client
	results []checkResult
}

func (p *prober) record(name string, soft bool, ok bool, format string, a ...any) {
	note := fmt.Sprintf(format, a...)
	status := "OK  "
	if !ok {
		if soft {
			status = "WARN"
		} else {
			status = "FAIL"
		}
	}
	fmt.Printf("[%s] %-16s %s\n", status, name, note)
	p.results = append(p.results, checkResult{name: name, ok: ok, soft: soft, note: note})
}

func (p *prober) report() {
	var hardFail int
	for _, r := range p.results {
		if !r.ok && !r.soft {
			hardFail++
		}
	}
	fmt.Println("== 探测结束 ==")
	if hardFail > 0 {
		fmt.Printf("硬检查失败 %d 项，请先修复环境（集群可能未启动）。\n", hardFail)
		os.Exit(1)
	}
	fmt.Println("环境就绪。")
}

// getJSON GET 并解码 JSON。
func getJSON(httpc *http.Client, url string, out any) error {
	resp, err := httpc.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// checkPrometheus 验证 /api/v1/query 可用并统计 up target 数与业务指标存在性。
func (p *prober) checkPrometheus(ctx context.Context, c config.Prometheus) {
	var upResp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	url := strings.TrimSuffix(c.BaseURL, "/") + "/api/v1/query?query=up"
	if err := getJSON(p.httpc, url, &upResp); err != nil {
		p.record("prometheus", false, false, "查询失败: %v", err)
		return
	}
	targets := len(upResp.Data.Result)
	upCount := 0
	for _, r := range upResp.Data.Result {
		if len(r.Value) > 1 && fmt.Sprint(r.Value[1]) == "1" {
			upCount++
		}
	}

	var bizResp struct {
		Data struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	bizURL := strings.TrimSuffix(c.BaseURL, "/") + "/api/v1/query?query=count(http_requests_total)"
	bizErr := getJSON(p.httpc, bizURL, &bizResp)
	bizSeries := 0
	if bizErr == nil && len(bizResp.Data.Result) > 0 && len(bizResp.Data.Result[0].Value) > 1 {
		fmt.Sscan(fmt.Sprint(bizResp.Data.Result[0].Value[1]), &bizSeries)
	}

	ok := upCount == targets && targets > 0
	if bizErr != nil {
		ok = false
		p.record("prometheus", false, ok, "targets=%d(up=%d)，http_requests_total 查询失败: %v", targets, upCount, bizErr)
		return
	}
	p.record("prometheus", false, ok, "targets=%d(up=%d)，http_requests_total 系列=%d", targets, upCount, bizSeries)
}

// checkJaeger 验证 Jaeger 查询 API 可用。
func (p *prober) checkJaeger(c config.Jaeger) {
	var svcResp struct {
		Data []string `json:"data"`
	}
	url := strings.TrimSuffix(c.BaseURL, "/") + "/api/services"
	if err := getJSON(p.httpc, url, &svcResp); err != nil {
		p.record("jaeger", false, false, "查询失败: %v", err)
		return
	}
	p.record("jaeger", false, true, "已有服务: %s", strings.Join(svcResp.Data, ", "))
}

// k8sRestConfig 依次尝试：显式 kubeconfig → in-cluster → 默认 kubeconfig。
func k8sRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if rc, err := rest.InClusterConfig(); err == nil {
		return rc, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

// checkK8s 验证 client-go 可列出目标命名空间的 pod（AIOps 工具层的前置条件）。
func (p *prober) checkK8s(ctx context.Context, c config.K8s) {
	rc, err := k8sRestConfig(c.Kubeconfig)
	if err != nil {
		p.record("k8s", false, false, "加载访问配置失败: %v", err)
		return
	}
	rc.Timeout = time.Duration(c.TimeoutSeconds) * time.Second
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		p.record("k8s", false, false, "构建 clientset 失败: %v", err)
		return
	}
	pods, err := cs.CoreV1().Pods(c.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		p.record("k8s", false, false, "列出 %s 命名空间 pod 失败: %v", c.Namespace, err)
		return
	}
	p.record("k8s", false, true, "命名空间 %s 共 %d 个 pod", c.Namespace, len(pods.Items))
}

// checkRedis ping 开发依赖 Redis（软检查）。
func (p *prober) checkRedis(ctx context.Context, c config.Redis) {
	rdb := redis.NewClient(&redis.Options{
		Addr: c.Addr, Password: c.Password, DB: c.DB, PoolSize: c.PoolSize,
	})
	defer rdb.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		p.record("redis", true, false, "%s 不可达: %v（make devup 可启动）", c.Addr, err)
		return
	}
	p.record("redis", true, true, "%s 可达", c.Addr)
}

// checkPostgres ping 开发依赖 PostgreSQL（软检查）。
func (p *prober) checkPostgres(c config.Postgres) {
	db, err := sql.Open("pgx", c.DSN)
	if err != nil {
		p.record("postgres", true, false, "打开失败: %v（make devup 可启动）", err)
		return
	}
	defer db.Close()
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		p.record("postgres", true, false, "ping 失败: %v（make devup 可启动）", err)
		return
	}
	p.record("postgres", true, true, "可达")
}

// llmProvidersFromConfig 从配置构造 provider 链（跳过未注入 key 的项）。
// M1 起此装配逻辑移入服务启动装配（cmd/aiops）。
func llmProvidersFromConfig(cfg *config.Config) []llm.Provider {
	var ps []llm.Provider
	for _, pc := range cfg.LLM.Providers {
		if pc.APIKey == "" {
			continue
		}
		ps = append(ps, llm.NewOpenAIProvider(llm.OpenAIOptions{
			Name:        pc.Name,
			BaseURL:     pc.BaseURL,
			APIKey:      pc.APIKey,
			Model:       pc.Model,
			Timeout:     time.Duration(pc.TimeoutSeconds) * time.Second,
			MaxTokens:   pc.MaxTokens,
			Temperature: pc.Temperature,
		}))
	}
	return ps
}

// checkLLM 真实调用一次 LLM（软检查：未配置 key 时跳过）。
// 调用会产生 llm.chat span 与 ai_* 指标，供 Jaeger/回环验证。
func (p *prober) checkLLM(ctx context.Context, cfg *config.Config) {
	providers := llmProvidersFromConfig(cfg)
	if len(providers) == 0 {
		p.record("llm", true, true, "未配置可用 provider（设置 LLM_API_KEY 后启用），跳过")
		return
	}
	c, err := llm.NewClient(providers, llm.ClientOptions{})
	if err != nil {
		p.record("llm", true, false, "客户端构造失败: %v", err)
		return
	}
	resp, err := c.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{{
		Role: llm.RoleUser, Content: "连通性自检：请只回复两个字：正常",
	}}})
	if err != nil {
		p.record("llm", true, false, "调用失败: %v", err)
		return
	}
	p.record("llm", true, true, "provider=%s 回复: %q（tokens %d/%d）",
		resp.Provider, trimRunes(resp.Content, 40), resp.PromptTokens, resp.CompletionTokens)
}

// trimRunes 截取前 n 个 rune。
func trimRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// checkOTLPRoundTrip flush 后从 Jaeger 查回本服务，验证 trace 全链路。
// 端点不可达时临时 kubectl port-forward（探测结束即回收）。
func (p *prober) checkOTLPRoundTrip(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry) {
	if !cfg.Telemetry.Enabled {
		p.record("otel-roundtrip", true, true, "telemetry 未启用，跳过")
		return
	}
	if !tel.Enabled() {
		p.record("otel-roundtrip", false, false, "telemetry 初始化降级（检查 OTLP 端口 %s）", cfg.Telemetry.OTLPEndpoint)
		return
	}
	cleanup, reachable := ensureOTLPEndpoint(cfg.Telemetry.OTLPEndpoint, cfg.Telemetry.PortForwardTarget)
	defer cleanup()
	if !reachable {
		p.record("otel-roundtrip", false, false,
			"OTLP %s 不可达且自动 port-forward 失败（手动执行: make pf-otel）", cfg.Telemetry.OTLPEndpoint)
		return
	}

	tracer := otel.Tracer("aiops/probe")
	_, span := tracer.Start(ctx, "probe.selfcheck")
	span.SetAttributes(attribute.String("component", "env-probe"))
	span.End()
	tel.Shutdown(context.Background()) // flush 全部 span（含 llm.chat）

	url := strings.TrimSuffix(cfg.Jaeger.BaseURL, "/") + "/api/services"
	var found bool
	for i := 0; i < 6; i++ { // 采集入库有延迟，轮询确认
		var svcResp struct {
			Data []string `json:"data"`
		}
		if err := getJSON(p.httpc, url, &svcResp); err == nil {
			for _, s := range svcResp.Data {
				if s == cfg.Telemetry.ServiceName {
					found = true
				}
			}
		}
		if found {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !found {
		p.record("otel-roundtrip", false, false, "span 未在 Jaeger 出现（OTLP %s → Jaeger 链路不通）", cfg.Telemetry.OTLPEndpoint)
		return
	}
	p.record("otel-roundtrip", false, true, "span 已在 Jaeger 出现，trace 链路通")
}

// tcpReachable 探测 TCP 端口是否可连。
func tcpReachable(endpoint string) bool {
	conn, err := net.DialTimeout("tcp", endpoint, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ensureOTLPEndpoint 确保 OTLP 端点可达；不可达时临时 kubectl port-forward 并返回清理函数。
// kind 集群只把 ingress/9090/16686 等端口发布到宿主机，OTLP 4317 需要 port-forward。
func ensureOTLPEndpoint(endpoint, forwardTarget string) (cleanup func(), reachable bool) {
	if forwardTarget == "" {
		forwardTarget = "svc/makejob-jaeger"
	}
	if tcpReachable(endpoint) {
		return func() {}, true
	}
	_, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return func() {}, false
	}
	cmd := exec.Command("kubectl", "port-forward", forwardTarget, port+":4317")
	if err := cmd.Start(); err != nil {
		return func() {}, false
	}
	kill := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if tcpReachable(endpoint) {
			return kill, true
		}
		time.Sleep(500 * time.Millisecond)
	}
	kill()
	return func() {}, false
}
