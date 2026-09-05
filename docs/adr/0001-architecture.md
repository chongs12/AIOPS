# ADR-0001：总体架构与技术决策

- 状态：已接受（2026-09-05）
- 决策人：开发智能体（经用户确认）

## 背景

本项目为独立新仓库的 AIOps 运维工单多智能体系统，工具查询对象指向 PreHire 本地
kind 集群的真实可观测栈（见 docs/03-data-source.md）。可借鉴 PreHire 已踩平的基础设施
设计（见 docs/02-reuse-prehire.md），但代码不并入 PreHire 仓库。

## 决策

| # | 决策点 | 结论 | 理由 |
|---|---|---|---|
| D1 | 编排引擎 | 自写轻量状态机（FSM + 黑板） | 工单 FSM、断点恢复、HITL 挂起/恢复需完全自定义；Agent 走统一接口 + 工厂 |
| D2 | 日志数据源 | 先 client-go（容器日志 + K8s Events），Loki 后评估 | 零集群改动、立即可用；Events 含 OOMKilled/BackOff 等根因关键信号 |
| D3 | 进程拓扑 | 单二进制模块化单体（cmd/aiops） | 开发/部署/演示最简；内部严格分层，后期可沿分层边界拆分 |
| D4 | LLM 接入 | OpenAI 兼容协议 + 可配 base_url | 一个适配器覆盖智谱 GLM / DeepSeek 等；fallback 链 = 多实例 |
| D5 | Web 框架 | gin，不引入 Kratos | 自研核心在编排/工具/证据化，框架增益低 |
| D6 | 查询安全 | PromQL 查询模板库优先，LLM 只选模板+填参 | 防幻觉、防高基数；后续可加受限自由查询 + metric 白名单 |
| D7 | 部署 | dev 本机直连 ingress；交付 Helm 进同一 kind 集群 | "AIOps 运维 PreHire 自身"叙事闭环 |

## 架构分层

```
API（gin，HTTP/SSE）
 └─ Orchestrator（工单 FSM + 黑板 + 断点恢复 + HITL 状态 + 回环上限）
     └─ Agent 层（统一接口 + 工厂 + 场景配置外置热更新）
         └─ Tool 层（Registry + 执行管道：校验→限流→并发闸→缓存→超时→重试→降级→审计/子 span）
             └─ pkg 基础设施（cache / ratelimit / llm / telemetry / config / async）
```

工单状态机（一等状态含 HITL）：

```
submitted → intent_classified → investigating ⇄(回环≤3)
    ├─▶ awaiting_info ─(用户补充)─▶ investigating   # 断点恢复
    ├─▶ escalated_human                              # 置信度低/证据不足/高危处置
    └─▶ resolved                                     # 报告产出 + 处置闭环
```

## 故障语义（继承 PreHire，全项目通用）

1. **缓存层**：任何故障降级直查数据源；L2 读 200ms 超时；空值哨兵 30s；
   TTL ±10% 抖动；singleflight 合并回源；版本号失效免 SCAN。
2. **限流层**：Redis Lua 令牌桶（100ms 超时）→ 本地桶兜底 → fail-open 放行 + 计数告警；
   非法规则 fail-safe 放行；昂贵资源 semaphore 并发上限。
3. **LLM 层**：provider fallback 链；全挂 → 模板化兜底报告 + 转人工。
4. **降级事实进入证据链**：工具降级/缺失的证据必须反映在报告与置信度中——
   缺失的证据不能装作存在（防幻觉的工程表达）。

## 里程碑

- M0 基础设施：config / telemetry / cache / ratelimit / llm + 环境探测脚本
- M1 垂直切片：5xx 突增工单端到端（Intent/Metrics/RootCause/Reply + Prometheus 工具）
- M2 HITL 追问 + 断点恢复 + Log Agent（client-go）+ SSE 聊天页
- M3 低危处置闭环（scale/rollout-undo + 二次确认 + 审计）
- M4 自可观测（全链路 Trace / 指标 / 推理回放）+ 管理面
- M5 扩展：OOM / 慢查询 + Milvus 案例库（可选 Loki）

各阶段验收标准见开发计划（README「开发路线」与项目简报第 9 节）。

## M2-M5 增补决策与验收（2026-09-05）

- **HITL/断点**：`awaiting_info` 一等状态持久化（tickets.pending JSONB），用户回复后
  `POST /tickets/{id}/reply` 合并内容并从意图之后续跑（`Orchestrator.Resume`）。
- **日志数据源**：client-go 直拉容器日志 + Events + Pod 状态（`k8s.logs/k8s.events/k8s.get_pods`）。
- **处置工具**：`k8s.scale` / `k8s.rollout_undo`（RS 历史 revision 回写实现），
  执行器对 Mutating 工具强制 `confirmed=true` 人工确认门 + `tool_audit_log` 审计。
- **Prompt 热更新**：`agent_configs` 表外置，两级缓存（L1 5s/L2 60s）+ 版本号失效，
  管理面 `PUT /api/v1/admin/prompts/{scene}` ≤5s 生效，`DELETE` 恢复内置。
- **案例库**：接口 `cases.Store`；Milvus（19530，AUTOINDEX+COSINE，字符 bigram 哈希
  64 维嵌入——生产可换真实 embedding 模型）优先，PG+内存余弦兜底；已解决工单异步沉淀，
  相似案例（score≥0.35）注入根因黑板。
- **前端**：聊天式工单页以内嵌单页应用（web/static，无构建依赖）随二进制分发，
  覆盖提单/SSE 实时流水/报告/处置确认/案例与观察视图；Vite 工程化留作后续。
- **验收记录**：5xx 处置闭环（工单 resolved→确认 scale→服务 200 恢复→审计 6 条）；
  真实 OOMKilled Pod 工单 resolved（诚实发现内存监控缺失）；案例召回 0.765/0.552。

## 环境探测结论（2026-09-05 实测，`aiops probe` 可随时复验）

- Prometheus：ingress `http://prometheus.localhost:8080` 可达，16 target 全 up，
  `http_requests_total` 存在；kind 同时发布了容器端口 → 宿主机 `localhost:9090` 可直连兜底。
- Jaeger：UI/API `http://jaeger.localhost:8080`（或 `localhost:16686`）可达。
- **OTLP 上报**：kind 仅向宿主机发布 8080(ingress)/9090/16686/3000(Grafana)，
  OTLP gRPC（4317）未发布——dev 模式用 `make pf-otel`
  （`kubectl port-forward svc/makejob-jaeger 4317:4317`），probe 会自动临时
  port-forward 并在结束后回收；集群内部署时直连 `collector:4317`。
  已实测：span 成功上报并在 Jaeger 查询 API 中出现（otel-roundtrip OK）。
- **Go 解析器不解析 `*.localhost`**（浏览器/curl 由系统特殊处理可以）：已用
  pkg/httpx 的拨号映射（`*.localhost` → 127.0.0.1，Host 头保留）解决，ingress
  仍按 Host 路由正常工作。
- K8s：context `kind-makejob`，PreHire 全部服务在 `default` 命名空间（20 pod），
  当前凭证可 get pods / pods/log。
- 集群随宿主机 Docker Desktop 启动而恢复（节点容器重启后需等待工作负载就绪）。
- 已知无关问题：makejob-frontend pod CrashLoopBackOff（PreHire 侧问题，不阻塞本项目）。
