# AIOps 运维工单多智能体系统 · 初步开发计划

## 0. 已确认决策点

| # | 决策 | 结论 |
|---|---|---|
| D1 | 编排引擎 | **自写轻量状态机**（FSM + 黑板）；Agent 走统一接口+工厂 |
| D2 | 日志数据源 | **先 client-go**（容器日志 + K8s Events），Loki 留作 M5 可选增强 |
| D3 | 进程拓扑 | **单二进制模块化单体**（cmd/aiops），内部严格分层，后期可拆 |
| D4 | LLM 接入 | **OpenAI 兼容协议 + 可配 base_url**，fallback 链 = 多实例（不同 base_url/model） |
| D5 | Web 框架 | gin + DDD 分层思想，不引入 Kratos（自研核心在编排/工具/证据化） |
| D6 | 查询安全 | **PromQL 查询模板库优先**：LLM 只选模板+填参，不放开自由生成；后期可加"受限自由查询 + metric 白名单" |
| D7 | 部署 | dev 本机直跑（经 ingress 域名连集群）；交付 Helm 装进同一 kind 集群（"AIOps 运维 PreHire 自身"叙事闭环） |

---

## 1. 系统架构图与模块划分

```
                    ┌──────────────────────────────────────────────┐
                    │        聊天式工单页 (React + Vite, web/)        │
                    │ 工单列表/详情 · SSE 聊天 · 追问回复 · 推理回放视图 │
                    └──────────────────┬───────────────────────────┘
                                       │ HTTP / SSE
┌──────────────────────────────────────▼──────────────────────────────────┐
│                        aiops-server（单 Go 二进制）                        │
│ ┌───────────┐  ┌──────────────────────────────────────────────────────┐ │
│ │ API 层     │  │ 编排引擎 internal/orchestrator                        │ │
│ │ 工单 CRUD  │→ │  · 工单 FSM（快照事务落 PG，断点恢复）                 │ │
│ │ SSE 聊天   │  │  · 黑板：观察集/假设集/已排除/待补项                    │ │
│ │ 管理面     │  │  · HITL：awaiting_info / escalated 为一等状态          │ │
│ └───────────┘  │  · 回环控制（上限 3 轮 + token/时长预算）               │ │
│                └──────────────┬───────────────────────────────────┘ │
│ ┌────────────────────────────▼─────────────────────────────────────┐ │
│ │ Agent 层（统一接口 + 工厂 + 场景配置 DB 外置/两级缓存/热更新）          │ │
│ │ Intent │ Metrics │ Log │ RootCause │ Reply   ← 5 个业务 Agent      │ │
│ └────────────────────────────┬─────────────────────────────────────┘ │
│ ┌────────────────────────────▼─────────────────────────────────────┐ │
│ │ 工具层 internal/tool：Registry + 执行管道                             │ │
│ │ 校验→限流(令牌桶100ms/fail-open)→并发闸(semaphore)→缓存(L1/L2三防)   │ │
│ │ →独立超时执行→重试(仅幂等读)→降级→审计/指标/OTel子span               │ │
│ │ impl/: prometheus(模板化PromQL) · jaeger · k8s(logs/events/get)     │ │
│ │        k8s 处置(scale/rollout-undo, 二次确认+审计)                    │ │
│ └──────────────────────────────────────────────────────────────────┘ │
│ pkg/: cache(三防+版本失效) · ratelimit(Redis Lua桶+本地兜底+fail-open)   │
│       llm(provider工厂+fallback+模板+结构化输出) · telemetry · async    │
└───────┬───────────────┬───────────────────────────┬─────────────────┘
        │               │                           │
   PostgreSQL        Redis                PreHire kind 集群（真实数据源）
  工单/状态快照/      缓存/限流/         Prometheus · Jaeger · K8s API
  证据/配置/审计     会话              (prometheus.localhost:8080 等)
  [M5: Milvus 案例库]
```

**模块职责**：`internal/api`（HTTP/SSE/DTO）、`internal/orchestrator`（FSM+黑板+恢复）、`internal/agent`（接口/工厂/5 Agent）、`internal/evidence`（Observation/Hypothesis/Citation 领域模型）、`internal/ticket`（工单模型+存储）、`internal/tool`（注册表/管道/实现）、`pkg/*`（通用基础设施，对标 PreHire 已踩平设计）。

**5xx 垂直切片数据流**：
1. 用户提交工单（可能缺服务名/时间窗）→ IntentAgent 结构化抽取 + 完整性评估
2. 缺信息 → FSM=`awaiting_info` → 生成追问 → SSE 推给用户 → 用户回复 → 合并实体 → 从断点恢复
3. MetricsAgent 用查询模板拉 `sum(rate(http_requests_total{code=~"5.."}[5m]))` 等 → 观察集
4. LogAgent 在故障时间窗拉容器日志 + K8s Events → 异常模式提取 → 观察集
5. RootCauseAgent 提假设 → 按假设补查（回环≤3，可回到 3/4 步）→ 交叉验证 → 结构化结论（根因/证据引用/置信度/建议）
6. 置信度<阈值 → `escalated_human`；否则 ReplyAgent 生成事故报告（时间线/根因/证据/建议）
7. 低危处置（扩缩/回滚）→ 用户二次确认 → 工具执行 + 审计 → 复查错误率回落 → 工单已解决

---

## 2. 分阶段里程碑与验收标准

| 阶段 | 内容 | 验收标准（可演示） |
|---|---|---|
| **M0 基础设施** | 脚手架/config/telemetry；pkg/cache 三防+版本失效；pkg/ratelimit fail-open；pkg/llm 工厂+模板+结构化输出；docker-compose(PG/Redis)；环境探测脚本 | ①探测脚本确认 Prometheus/Jaeger/K8s 可达并打印 ~16 target；②cache 单测覆盖三防/版本失效，ratelimit 单测覆盖 Redis 故障→本地桶→fail-open；③一次 LLM 调用在 Jaeger 出现 span、`:6060/metrics` 有 ai_* 指标 |
| **M1 垂直切片（核心）** | 工具执行管道+Prometheus 工具+查询模板库；Intent/Metrics/RootCause/Reply 4 Agent；简化 FSM（无 HITL）；工单异步任务（pkg/async 状态机+幂等） | ①`kubectl delete pod` 制造真实 5xx → 提工单 → 自动产出带 PromQL 证据+置信度的事故报告；②人为断 Prometheus → 工具降级、报告标注"证据缺失"、流程不崩（演示降级）；③全流程一条 trace |
| **M2 HITL+日志+断点恢复** | 完整 FSM（awaiting_info 挂起/恢复/快照）；LogAgent（client-go logs+events+RBAC）；Jaeger 工具；SSE 聊天页；根因回环 | ①缺时间窗工单触发追问→回复后从断点续跑；②根因 Agent 至少一次主动补查回环；③Jaeger 中可见 Agent=span/工具=子 span 层级 |
| **M3 处置闭环** | k8s 处置工具（scale/rollout-undo）+二次确认+审计；转人工路径；工单生命周期完整（满意度/历史） | 完整演示闭环：注入故障→工单→根因→回滚→错误率回落→已解决；每次处置有审计记录 |
| **M4 自可观测+管理面** | 工具成功率/延迟/LLM token/工单状态指标全集；Grafana 仪表盘；推理回放页；Prompt/Agent 配置热更新（两级缓存 L1 5s/L2 60s）；工具权限 | ①回放页复现一次工单完整推理链（消息序列+trace 链接）；②改 prompt 配置 5s 内生效不重启 |
| **M5 扩展** | pod OOM 工单类型；DB 慢查询；Milvus 案例库（已解决工单向量化+相似召回）；可选 Loki | OOM 工单端到端出根因；相似历史工单秒级召回 |

---

## 3. 核心任务拆解与依赖

**批次 A（基础）**：T01 脚手架+config+telemetry+探测脚本 → T02 pkg/cache、T03 pkg/ratelimit、T04 pkg/llm、T05 pkg/async（并行，仅依赖 T01）。

**批次 B（工具层）**：T06 工具注册表+执行管道（超时/重试/降级/限流/缓存/审计/子span，依赖 T02/T03/T05）→ T07 Prometheus 工具+查询模板库 ∥ T08 K8s 工具+RBAC ∥ T09 Jaeger 工具。

**批次 C（Agent 层）**：T10 Agent 统一接口+工厂+场景配置（依赖 T04）→ T11 Intent / T12 Metrics（依赖 T07）/ T13 Log（依赖 T08）/ T14 RootCause（依赖全部工具）/ T15 Reply。

**批次 D（编排+接入）**：T16 FSM+黑板+持久化 → T17 断点恢复 → T18 HITL 追问/恢复 → T19 API+SSE → T20 React 前端。

**批次 E（闭环+交付）**：T21 处置工具+审计 ∥ T22 指标/仪表盘/回放 ∥ T23 管理面 ∥ T24 Docker/Helm → T25 Milvus 案例库、T26 OOM/慢查询类型（M5）。

关键路径：T01→T02/03/05→T06→T07→T12→T14→T16→T17/18→T19→T20。M1 = 批次 A + B(仅 Prometheus) + C(4 Agent) + D(简化 T16/T19)。

---

## 4. 工具调用协议 / Schema / 超时重试降级

**Tool 接口**（注册表驱动，LLM 侧暴露 name/desc/JSON Schema）：
```go
type Tool interface {
    Name() string
    Desc() string
    Schema() ToolSchema                 // input/output JSON Schema + 权限级别 + 幂等标记
    Execute(ctx context.Context, in json.RawMessage) (*ToolResult, error)
}
```

**执行管道（顺序）**：注册表查找 → 参数 JSON Schema 校验 → 权限检查（只读/变更）→ Redis Lua 令牌桶限流（100ms 超时，Redis 故障→本地桶→fail-open 放行+计数）→ semaphore 并发闸 → L1/L2 缓存读取（Loader 三防：singleflight 防击穿/空值哨兵 30s 防穿透/TTL±10% 抖动防雪崩；L2 读 200ms 超时）→ 独立超时 context 执行 → 失败按策略重试（指数退避，仅幂等读工具）→ 降级 → OTel 子 span + 指标 + 审计落库。

**统一结果信封**（降级事实必须进入证据链）：
```json
{ "status": "ok | degraded | failed | cached",
  "data": { }, "evidence": {"source": "prometheus", "query": "sum(rate(...))", "excerpt": []},
  "degraded_reason": "timeout after 10s, serving stale cache",
  "duration_ms": 812, "from_cache": true }
```

**超时/重试/降级矩阵**：

| 工具 | 超时 | 重试 | 降级策略 |
|---|---|---|---|
| prometheus_query | 10s | 2 次退避 500ms | 旧缓存值(标 stale)→无则"指标不可用"，报告标注证据缺失并压低置信度 |
| jaeger_query | 8s | 1 次 | 跳过链路证据，标注缺失 |
| k8s_logs / k8s_events / k8s_get | 5s | 1 次 | 标注"日志不可得"，根因只依赖指标+events |
| k8s_scale / rollout_undo | 20s | 0（非幂等不自动重试） | 不执行→转人工确认 |
| llm_call | 60s/provider | 切备 provider 1 次 | 全挂→模板化兜底报告+转人工 |

核心原则（继承 PreHire 故障语义）：**故障绝不阻塞主路径**；降级影响置信度——缺失的证据不能装作存在（防幻觉的一部分）。

**黑板消息协议**（Agent 间结构化传递，落 PG）：
`Observation{source, tool, query, excerpt, ts}`、`Hypothesis{statement, evidence_ids[], confidence, status: pending|confirmed|rejected}`、`PendingQuery{tool, params, reason}`。

**结构化输出**：每 Agent 输出强类型 JSON（校验失败自动修复重试 1 次）；RootCauseAgent 输出 `{root_cause, hypotheses[], timeline[], recommendations[], confidence, missing_evidence[]}`；结论必须引用 evidence_id，无证据的结论标记为"未验证假设"。

---

## 5. 关键技术风险与缓解

| 风险 | 缓解 |
|---|---|
| LLM 幻觉 / 结构化输出不稳 | 查询模板白名单（D6）；结论强制绑定 evidence_id；schema 校验+修复重试；置信度阈值转人工 |
| 编排回环死循环 / 成本失控 | 显式 FSM 转移、回环上限 3、每工单 token/时长预算 |
| 断点恢复状态不一致 | 每步幂等 step_id + 快照事务落 PG；恢复从最后完成步开始；工单走异步任务状态机（queued→running→succeeded/failed/dead，幂等 key+死信+延迟重试梯度 20s/30s/45s） |
| 长查询阻塞 | 工具独立超时 + 结果缓存（固定历史时间窗查询可缓存）+ 工单全异步处理 |
| client-go 日志随 pod 重建丢失 | K8s Events 兜底（OOMKilled/BackOff/探针失败均在 events）；M5 可选 Loki |
| 处置类工具误操作 | 变更默认人工二次确认 + 审计 + namespace 白名单限制 |
| kind 集群资源紧张 | 单二进制低占用；有状态(PG/Redis)宿主机 docker-compose（K8s 内经 host.docker.internal 访问） |
| 环境与文档不符 | M0 首个任务即环境探测脚本；探测失败先修环境再开发，绝不静默 mock |

---

## 6. 仓库目录结构与落库顺序

```
sre/
├── docs/                    # 三份交接文档（自根目录移入）+ adr/0001-architecture.md
├── cmd/aiops/main.go
├── internal/
│   ├── conf/                # 配置加载与环境覆盖（ApplyEnvOverrides，零明文凭证）
│   ├── api/                 # 路由/DTO/SSE
│   ├── orchestrator/        # FSM、黑板、断点恢复、回环控制
│   ├── agent/               # 接口+工厂+场景配置；intent/metrics/log/rootcause/reply
│   ├── evidence/            # Observation/Hypothesis/Citation 模型
│   ├── ticket/              # 工单领域模型 + biz/data 存储
│   └── tool/                # registry、执行管道、impl/{prometheus,jaeger,k8s}
├── pkg/{cache,ratelimit,llm,telemetry,async,config}/
├── web/                     # React+Vite 聊天式工单页
├── deploy/                  # docker-compose(dev) + helm/
├── scripts/                 # 环境探测、故障注入、demo 脚本
├── Makefile  ·  go.mod  ·  README.md
```

**落库顺序（每批 = 一个可编译/可测试的提交）**：①docs 移入+ADR-0001+README → ②go.mod+cmd 骨架+pkg/config+pkg/telemetry → ③pkg/cache（三防单测）→ ④pkg/ratelimit（fail-open 单测）→ ⑤pkg/llm（工厂+fallback+模板+结构化输出）→ ⑥internal/tool 管道+Prometheus 工具+模板库（含连通集成测试）→ ⑦Agent 接口+4 Agent+简化 FSM（M1 切片完成）→ ⑧internal/api+最小前端 → ⑨K8s 工具+RBAC+LogAgent+完整 FSM/HITL/断点恢复 → ⑩处置工具+审计+生命周期 → ⑪指标/仪表盘/回放/管理面 → ⑫Docker/Helm → ⑬Milvus+新工单类型。

**DB migration 顺序**：tickets → ticket_messages + state_snapshots → evidence_records → agent_configs(+prompts) → tool_audit_log → (M5) cases。

---

## 7. 批准后立即执行

从 M0 开始：环境探测脚本（验证 Prometheus/Jaeger/K8s 可达与指标名，探测失败先修环境）→ 仓库脚手架与 docs 归位 → pkg 基础设施三件（cache/ratelimit/llm）。每完成一个里程碑做一次可演示的验收自查并记录到 docs/adr/。