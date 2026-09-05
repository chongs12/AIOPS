# AIOps 运维工单多智能体系统

面向 SRE 场景的工单处理系统：用户提交运维工单（接口 5xx 突增 / pod OOM / 数据库慢查询），
多 Agent 协作完成「意图识别 → 数据检索 → 根因推理 → 工单回复」，产出**带证据、带置信度、
带处置建议**的事故报告。信息不足时主动追问（HITL），证据不足或置信度低时转人工。

**工具查询对象是 PreHire 本地 kind 集群的真实可观测栈**（Prometheus / Jaeger / K8s API），
工单、查询、结论全部真实，非 mock。详见 [docs/03-data-source.md](docs/03-data-source.md)。

## 快速开始

```bash
# 1. 环境探测：验证 Prometheus / Jaeger / K8s / Redis / PostgreSQL 可达
make probe

# 2. 启动开发依赖（专用 PostgreSQL/Redis，独立端口不与宿主机既有实例冲突）
make devup

# 3. 启动服务（默认 :8090 API，:6060 metrics）
make run
```

## 系统架构（概览）

```
聊天式工单页(React) ──HTTP/SSE──▶ aiops-server（单 Go 二进制）
   ├─ API 层        工单 CRUD / SSE 聊天 / 管理面
   ├─ 编排引擎      工单 FSM（断点恢复）· 黑板（观察集/假设集）· HITL 一等状态 · 回环上限
   ├─ Agent 层     Intent / Metrics / Log / RootCause / Reply（统一接口 + 工厂 + 热更新配置）
   └─ 工具层       Registry + 执行管道：校验→限流→并发闸→缓存→超时→重试→降级→审计
                    Prometheus(模板化 PromQL) · Jaeger · K8s(logs/events/get) · K8s 处置
存储：PostgreSQL（工单/状态快照/证据/配置/审计）· Redis（缓存/限流）· [M5] Milvus 案例库
```

完整设计见 [docs/adr/0001-architecture.md](docs/adr/0001-architecture.md) 与
[docs/01-brief.md](docs/01-brief.md)（项目简报）。

## 目录结构

```
cmd/aiops/          服务入口（含 probe 子命令：环境连通性自检）
internal/
  api/               HTTP 路由 / DTO / SSE
  orchestrator/      工单状态机、黑板、断点恢复、回环控制
  agent/             Agent 统一接口 + 工厂 + 5 个业务 Agent
  evidence/          Observation / Hypothesis / 证据引用 领域模型
  ticket/            工单领域模型与存储
  tool/              工具注册表、执行管道、各工具实现
pkg/
  cache/             两级缓存：Loader 三防（防击穿/穿透/雪崩）+ 版本号失效
  ratelimit/         Redis Lua 令牌桶 + 本地兜底 + fail-open + 并发闸
  llm/               Provider 工厂 + fallback 链 + Prompt 模板 + 结构化输出
  telemetry/         OTel 初始化（trace/metrics，故障时降级不阻塞主路径）
  config/            YAML + ${VAR} 占位 + 环境变量覆盖
deploy/              docker-compose(开发依赖) + helm
web/                 React + Vite 聊天式工单页
scripts/             环境探测、故障注入、演示脚本
docs/                交接文档 + ADR
```

## 开发路线

| 里程碑 | 内容 | 状态 |
|---|---|---|
| M0 | 基础设施（config/telemetry/cache/ratelimit/llm）+ 环境探测 | 进行中 |
| M1 | 垂直切片：接口 5xx 突增工单端到端（含工具降级演示） | |
| M2 | HITL 追问 + 断点恢复 + 日志解析 Agent + SSE 聊天页 | |
| M3 | 低危处置闭环（回滚/扩缩 + 审计）+ 工单生命周期 | |
| M4 | 自可观测（全链路 Trace/指标/推理回放）+ 管理面 | |
| M5 | pod OOM / 慢查询类型 + Milvus 案例库（可选 Loki） | |

## 关键工程约定

- **复用优先**：缓存三防、限流故障语义（Redis 故障→本地桶→fail-open）、provider 工厂、
  OTel 埋点模式均对标 PreHire 已踩平设计，见 [docs/02-reuse-prehire.md](docs/02-reuse-prehire.md)。
- **故障绝不阻塞主路径**：缓存/限流/遥测任何故障都降级，不影响工单处理。
- **证据化推理**：所有结论必须引用真实证据（PromQL 结果/日志片段/K8s events），缺证据的
  结论只作为"未验证假设"，置信度低于阈值转人工。
- **仓库零明文凭证**：敏感字段在配置中用 `${VAR}` 占位，启动时环境变量注入。
