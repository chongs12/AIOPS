package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"aiops/internal/agent"
	"aiops/internal/api"
	"aiops/internal/audit"
	"aiops/internal/auth"
	"aiops/internal/cases"
	"aiops/internal/orchestrator"
	"aiops/internal/ticket"
	"aiops/internal/tool"
	"aiops/internal/tool/k8stool"
	"aiops/internal/tool/promtool"
	"aiops/internal/tool/slowquery"
	"aiops/pkg/cache"
	"aiops/pkg/config"
	"aiops/pkg/llm"
	"aiops/pkg/notify"
	"aiops/pkg/ratelimit"
	"aiops/pkg/tasks"
	"aiops/pkg/telemetry"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	kubernetes "k8s.io/client-go/kubernetes"
)

// app 装配完成的全部运行时依赖。
type app struct {
	store       ticket.Store
	handler     http.Handler
	llmEnabled  bool
	caseBackend string
}

// buildApp 按依赖顺序装配：存储 → 缓存/限流 → LLM → 工具层 → 案例库/审计/管理面
// → Agent → 编排 → API。基础设施故障按各自降级语义处理，绝不阻塞启动。
func buildApp(ctx context.Context, cfg *config.Config, tel *telemetry.Telemetry) (*app, error) {
	a := &app{}

	// 存储：PG 优先，不可达降级内存存储（重启丢数据，显式告警）。
	var db *sql.DB
	a.store, db = buildStore(ctx, cfg)

	// Redis：可达则作为缓存 L2 与限流后端；不可达均降级（fail-open / 仅 L1）。
	rdb := buildRedis(ctx, cfg)
	limiter := ratelimit.New(ratelimit.Options{RDB: rdb, Timeout: time.Duration(cfg.RateLimit.RedisTimeoutMS) * time.Millisecond})
	appCache := cache.New(cache.Options{
		L1Size:      cfg.Cache.L1Size,
		L1TTL:       time.Duration(cfg.Cache.L1TTLSeconds) * time.Second,
		L2:          rdb,
		NegativeTTL: time.Duration(cfg.Cache.NegativeTTLSeconds) * time.Second,
	})

	// LLM：有 key 时从配置建链；无 key 时建空链 Client——
	// provider watcher 检测到管理面配置后热注入，工单处理随即恢复（无需重启）。
	llmc := buildLLM(cfg)
	a.llmEnabled = llmc != nil
	if llmc == nil {
		if c0, err := llm.NewClient(nil, llm.ClientOptions{}); err == nil {
			llmc = c0
		}
		slog.Warn("LLM 未配置（设置 LLM_API_KEY），等待管理面热注入")
	}

	// 工具层。
	reg := tool.NewRegistry()
	promClient := promtool.NewClient(cfg.Prometheus.BaseURL, time.Duration(cfg.Prometheus.TimeoutSeconds)*time.Second)
	lib := promtool.BuiltinLib()
	if err := reg.Register(promtool.NewQueryTool(promClient, lib)); err != nil {
		return nil, err
	}
	if err := reg.Register(promtool.NewRangeTool(promClient, lib)); err != nil {
		return nil, err
	}
	if err := reg.Register(promtool.NewFreeTool(promClient)); err != nil {
		return nil, err
	}
	// K8s 工具（日志/事件/Pod 状态只读 + 扩缩容/回滚变更类）。失败不阻塞启动。
	if k8sBase, err := buildK8sBase(cfg); err == nil {
		for _, t := range k8stool.NewReadTools(k8sBase) {
			if err := reg.Register(t); err != nil {
				return nil, err
			}
		}
		for _, t := range k8stool.NewMutatingTools(k8sBase) {
			if err := reg.Register(t); err != nil {
				return nil, err
			}
		}
		slog.Info("K8s 工具就绪", "namespace", cfg.K8s.Namespace)
	} else {
		slog.Warn("K8s 客户端构建失败，K8s 工具不可用", "err", err)
	}

	// 审计（M3）：PG 可用则落库，否则 noop。
	var auditor tool.Auditor = tool.NoopAuditor{}
	var auditReader api.AuditReader
	if db != nil {
		pgAudit, err := audit.New(ctx, db)
		if err == nil {
			auditor = pgAudit
			auditReader = pgAudit
		} else {
			slog.Warn("审计存储迁移失败，降级 noop", "err", err)
		}
	}

	// 慢查询工具（需 PG）。
	if db != nil {
		if err := reg.Register(slowquery.New(db, cfg.SlowQuery.ThresholdSeconds)); err != nil {
			return nil, err
		}
	}

	executor := tool.NewExecutor(tool.ExecutorDeps{
		Registry: reg, Limiter: limiter, Cache: appCache, Auditor: auditor,
	})

	// 案例库（M5）：Milvus 优先，PG 兜底；无 PG 则不启用。
	var caseStore orchestrator.CaseStore
	var caseStoreFull cases.Store // 完整接口（含 List），供知识库列表页
	if db != nil {
		pgCases, err := cases.NewPG(ctx, db)
		if err != nil {
			slog.Warn("案例库 PG 迁移失败", "err", err)
		} else if ms, errM := cases.NewMilvus(ctx, cfg.CaseStore.MilvusAddr, pgCases); errM == nil {
			caseStore = ms
			caseStoreFull = ms
		} else {
			caseStore = pgCases
			caseStoreFull = pgCases
			slog.Warn("Milvus 不可用，案例库降级 PG", "err", errM)
		}
	}
	if caseStore != nil {
		a.caseBackend = caseStore.Backend()
	}

	// Prompt/场景参数热更新仓库（M4）：需 PG；失败降级内置。
	// 同一实例同时实现 PromptAdmin（管理面）与 llm.TemplateStore / SceneConfigSource（运行时），
	// 确保 Agent 处理链真正消费热更新后的模板与参数。
	var promptAdmin api.PromptAdmin
	var runtimePrompts llm.TemplateStore
	var runtimeSceneCfg agent.SceneConfigSource
	if db != nil {
		if ps, err := agent.NewDBPromptStore(ctx, db, rdb); err == nil {
			promptAdmin = ps
			runtimePrompts = ps
			runtimeSceneCfg = ps
			slog.Info("Prompt/场景参数热更新仓库就绪（≤5s 生效，已接入编排链）")
		} else {
			slog.Warn("Prompt 外置失败，使用内置模板", "err", err)
		}
	}

	// 服务命名解析：pod 前缀格式 + job 前缀剥离（配置化，替代 PreHire 硬编码）。
	prefixFmt := cfg.K8s.PodPrefixFmt
	if prefixFmt == "" {
		prefixFmt = "%s"
	}
	namer := func(service string) string { return fmt.Sprintf(prefixFmt, service) }
	resolver := func(metric map[string]string) string {
		if job := metric["job"]; strings.HasPrefix(job, cfg.K8s.JobPrefix) {
			return strings.TrimPrefix(job, cfg.K8s.JobPrefix)
		}
		return ""
	}

	// 异步任务队列（先于编排器创建：编排器需要入队复查任务）。
	taskEnqueuer := orchestrator.TaskEnqueuer(nil)
	var taskQueue *tasks.Queue
	var deadLister api.DeadLister
	if db != nil {
		if q, err := tasks.NewQueue(ctx, db); err == nil {
			taskQueue = q
			deadLister = q
			taskEnqueuer = q
		} else {
			slog.Warn("任务队列迁移失败，降级 goroutine", "err", err)
		}
	}

	// Agent + 编排。
	agents := agent.NewAll(agent.Deps{LLM: llmc, PodNamer: namer, Services: cfg.K8s.Services})
	orch := orchestrator.New(orchestrator.Options{
		Store: a.store, Agents: agents, Tools: executor, LLM: llmc, CaseStore: caseStore,
		Prompts: runtimePrompts, SceneCfgs: runtimeSceneCfg,
		Queue:    taskEnqueuer,
		Notifier: notify.NewNotifier(cfg.Notify.WebhookURL),
		VerifyCfg: &orchestrator.VerifyConfig{
			WaitSeconds: cfg.Verify.WaitSeconds, Threshold: cfg.Verify.Threshold,
			Window: "2m", RateTemplate: "http_5xx_rate",
		},
	})

	// provider 热插拔（P7 增补）：后台监听 DB provider 链配置，变化即 SwapProviders。
	if db != nil && llmc != nil {
		if store, ok := runtimeSceneCfg.(*agent.DBPromptStore); ok {
			go func() {
				last := ""
				for {
					ctx2, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
					ps, _ := store.GetProviders(ctx2)
					cancel()
					if len(ps) > 0 {
						raw, _ := json.Marshal(ps)
						sig := string(raw)
						if sig != last && ps[0].APIKey != "" {
							var chain []llm.Provider
							for _, pc := range ps {
								chain = append(chain, llm.NewOpenAIProvider(llm.OpenAIOptions{
									Name: pc.Name, BaseURL: pc.BaseURL, APIKey: pc.APIKey, Model: pc.Model,
									Timeout:     time.Duration(pc.TimeoutSec) * time.Second,
									MaxTokens:   pc.MaxTokens,
									Temperature: pc.Temperature,
								}))
							}
							if err := llmc.SwapProviders(chain); err == nil {
								slog.Info("provider 链热替换完成", "chain", llmc.ProviderNames())
								last = sig
							}
						}
					} else if last != "" {
						last = "" // 配置被清除，允许下次设置时再次触发
					}
					time.Sleep(5 * time.Second)
				}
			}()
		}
	}

	// 鉴权（默认关闭）。
	var authSvc api.AuthBackend
	if cfg.Auth.Enabled && db != nil {
		if as, err := auth.New(ctx, db, cfg.Auth.JWTSecret, time.Duration(cfg.Auth.TTLHours)*time.Hour); err == nil {
			authSvc = as
			slog.Info("登录鉴权已启用（管理面/写操作需 JWT）")
		} else {
			slog.Warn("鉴权初始化失败，降级为关闭", "err", err)
		}
	}

	// worker 启动（队列已在编排器之前创建）：按 kind 分派 process/resume/verify。
	if taskQueue != nil {
		go taskQueue.Run(context.WithoutCancel(ctx), func(rctx context.Context, kind string, payload json.RawMessage) error {
			switch kind {
			case "verify_recovery":
				return orch.RunVerifyTask(rctx, payload)
			default:
				var p struct {
					TicketID string `json:"ticket_id"`
					Resume   bool   `json:"resume"`
				}
				if err := json.Unmarshal(payload, &p); err != nil {
					return err
				}
				return orch.Handle(rctx, p.TicketID, p.Resume)
			}
		})
	}

	srv := api.NewServer(a.store, orch, cfg.Jaeger.BaseURL)
	srv.WithTools(executor, auditReader)
	srv.WithQueue(taskQueue)
	srv.WithCache(appCache)
	srv.WithVerifier(orch)
	srv.WithDeadLister(deadLister)
	srv.WithAuth(authSvc)
	srv.WithObservability(&api.ObsDeps{Templates: lib, Tools: executor, Resolve: resolver, Gatherer: tel.Gatherer()})
	srv.WithAdmin(promptAdmin, caseStoreFull)
	srv.WithCaseDeleter(caseStoreFull)
	if store, ok := runtimeSceneCfg.(*agent.DBPromptStore); ok {
		srv.WithProviderAdmin(store)
	}
	a.handler = srv.Handler()
	return a, nil
}

// buildStore 优先 PG；不可达降级内存存储。
func buildStore(ctx context.Context, cfg *config.Config) (ticket.Store, *sql.DB) {
	db, err := sql.Open("pgx", cfg.Postgres.DSN)
	if err == nil {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
	}
	if err == nil {
		s, err := ticket.NewPGStore(ctx, db)
		if err == nil {
			slog.Info("PostgreSQL 就绪", "host", hostOf(cfg.Postgres.DSN))
			return s, db
		}
		slog.Warn("ticket 建表失败，降级内存存储", "err", err)
		return ticket.NewMemStore(), nil
	}
	slog.Warn("PostgreSQL 不可达，降级内存存储（重启丢数据）", "err", err)
	return ticket.NewMemStore(), nil
}

// buildRedis 探活失败返回 nil（各组件自行降级）。
func buildRedis(ctx context.Context, cfg *config.Config) redis.UniversalClient {
	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB,
		PoolSize: cfg.Redis.PoolSize,
	})
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		slog.Warn("Redis 不可达，缓存/限流降级运行", "addr", cfg.Redis.Addr, "err", err)
		return nil
	}
	slog.Info("Redis 就绪", "addr", cfg.Redis.Addr)
	return rdb
}

// buildLLM 按配置构造 provider fallback 链；无可用 key 返回 nil。
func buildLLM(cfg *config.Config) *llm.Client {
	var providers []llm.Provider
	for _, pc := range cfg.LLM.Providers {
		if pc.APIKey == "" {
			continue
		}
		providers = append(providers, llm.NewOpenAIProvider(llm.OpenAIOptions{
			Name: pc.Name, BaseURL: pc.BaseURL, APIKey: pc.APIKey, Model: pc.Model,
			Timeout:     time.Duration(pc.TimeoutSeconds) * time.Second,
			MaxTokens:   pc.MaxTokens,
			Temperature: pc.Temperature,
		}))
	}
	if len(providers) == 0 {
		return nil
	}
	c, err := llm.NewClient(providers, llm.ClientOptions{})
	if err != nil {
		return nil
	}
	slog.Info("LLM provider 链就绪", "chain", c.ProviderNames())
	return c
}

// buildK8sBase 构造 K8s 工具依赖（复用 probe 的 kubeconfig 加载逻辑）。
func buildK8sBase(cfg *config.Config) (k8stool.Base, error) {
	rc, err := k8sRestConfig(cfg.K8s.Kubeconfig)
	if err != nil {
		return k8stool.Base{}, err
	}
	rc.Timeout = time.Duration(cfg.K8s.TimeoutSeconds) * time.Second
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return k8stool.Base{}, err
	}
	return k8stool.Base{
		Client:    cs,
		Namespace: cfg.K8s.Namespace,
		Timeout:   time.Duration(cfg.K8s.TimeoutSeconds) * time.Second,
	}, nil
}

// hostOf 从 DSN 提取主机地址（日志脱敏用，绝不输出凭证）。
func hostOf(dsn string) string {
	s := dsn
	if i := strings.Index(s, "//"); i >= 0 {
		s = s[i+2:]
	}
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[i+1:] // 跳过 user:password@
	}
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	return s
}
