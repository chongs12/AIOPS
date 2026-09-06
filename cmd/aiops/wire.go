package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"aiops/internal/agent"
	"aiops/internal/api"
	"aiops/internal/audit"
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
	"aiops/pkg/ratelimit"

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
func buildApp(ctx context.Context, cfg *config.Config) (*app, error) {
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

	// LLM：无可用 key 时为 nil（工单处理将明确转 failed 而非静默 mock）。
	llmc := buildLLM(cfg)
	a.llmEnabled = llmc != nil
	if llmc == nil {
		slog.Warn("LLM 未配置（设置 LLM_API_KEY），工单处理将提示配置缺失")
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
	if db != nil {
		pgCases, err := cases.NewPG(ctx, db)
		if err != nil {
			slog.Warn("案例库 PG 迁移失败", "err", err)
		} else if ms, errM := cases.NewMilvus(ctx, cfg.CaseStore.MilvusAddr, pgCases); errM == nil {
			caseStore = ms
		} else {
			caseStore = pgCases
			slog.Warn("Milvus 不可用，案例库降级 PG", "err", errM)
		}
	}
	if caseStore != nil {
		a.caseBackend = caseStore.Backend()
	}

	// Prompt 热更新仓库（M4）：需 PG；失败降级内置。
	var promptAdmin api.PromptAdmin
	if db != nil {
		if ps, err := agent.NewDBPromptStore(ctx, db, rdb); err == nil {
			promptAdmin = ps
			slog.Info("Prompt 热更新仓库就绪（≤5s 生效）")
		} else {
			slog.Warn("Prompt 外置失败，使用内置模板", "err", err)
		}
	}

	// Agent + 编排。
	agents := agent.NewAll(agent.Deps{LLM: llmc})
	orch := orchestrator.New(orchestrator.Options{
		Store: a.store, Agents: agents, Tools: executor, LLM: llmc, CaseStore: caseStore,
	})

	srv := api.NewServer(a.store, orch, cfg.Jaeger.BaseURL)
	srv.WithTools(executor, auditReader)
	srv.WithObservability(&api.ObsDeps{Templates: lib, Tools: executor})
	srv.WithAdmin(promptAdmin, caseStore)
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
