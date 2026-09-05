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
	"aiops/internal/orchestrator"
	"aiops/internal/ticket"
	"aiops/internal/tool"
	"aiops/internal/tool/promtool"
	"aiops/pkg/cache"
	"aiops/pkg/config"
	"aiops/pkg/llm"
	"aiops/pkg/ratelimit"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

// app 装配完成的全部运行时依赖。
type app struct {
	store      ticket.Store
	handler    http.Handler
	llmEnabled bool
}

// buildApp 按依赖顺序装配：存储 → 缓存/限流 → LLM → 工具层 → Agent → 编排 → API。
// 基础设施故障按各自降级语义处理（内存存储/无 L2/无 LLM 显式告警），绝不阻塞启动。
func buildApp(ctx context.Context, cfg *config.Config) (*app, error) {
	a := &app{}

	// 存储：PG 优先，不可达降级内存存储（重启丢数据，显式告警）。
	a.store, _ = buildStore(ctx, cfg)

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
	if err := reg.Register(promtool.NewQueryTool(promClient, promtool.BuiltinLib())); err != nil {
		return nil, err
	}
	executor := tool.NewExecutor(tool.ExecutorDeps{
		Registry: reg, Limiter: limiter, Cache: appCache, Auditor: tool.NoopAuditor{},
	})

	// Agent + 编排。
	agents := agent.NewAll(agent.Deps{LLM: llmc})
	orch := orchestrator.New(orchestrator.Options{
		Store: a.store, Agents: agents, Tools: executor, LLM: llmc,
	})

	a.handler = api.NewServer(a.store, orch, cfg.Jaeger.BaseURL).Handler()
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
			slog.Info("PostgreSQL 就绪", "dsn_host", hostOf(cfg.Postgres.DSN))
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
