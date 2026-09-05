# AIOps 工具的真实数据源环境（指向 PreHire 本地集群）

> 开发/演示时，AIOps 各 Agent 的"工具"查询对象就是本文件描述的 PreHire 本地 kind 集群。工单、查询、结论因此都是真实的，而非 mock。

## 1. 环境概况

- PreHire = 15 个 Go/Kratos 微服务 + React 前端，部署在本地 **kind 3 节点 K8s**（`makejob` 集群）。
- 可观测栈 = **OTel Collector + Jaeger + Prometheus + Grafana**（无 Loki，无 Alertmanager→工单连线）。

## 2. 访问地址（ingress 子域名，浏览器自动解析 *.localhost）

| 组件 | 地址 | 用途 |
|---|---|---|
| 前端 | http://localhost:8080 | 平台本身 |
| Prometheus | http://prometheus.localhost:8080 | 指标查询（AIOps「指标检索 Agent」主数据源） |
| Jaeger | http://jaeger.localhost:8080 | 链路追踪（AIOps「链路辅助定位」） |
| Grafana | http://grafana.localhost:8080 | 预置「PreHire 全栈概览」仪表盘（admin / makejob123） |

## 3. 指标来源与真实指标名（可直接写 PromQL）

- 每服务暴露 `:6060/metrics`，Prometheus 抓取 ~16 个 target（15 服务 + 其余）。
- **已确认存在的业务指标**：
  - `http_requests_total{method, path, code}` — gateway 埋点 counter，`path` 为 gin 路由模板（如 `/api/v1/interviews/:id`，非真实 URL，避免高基数）
  - `http_request_duration_seconds{method, path}` — gateway hist同源 histogram
  - `ai_calls_total` / `ai_tokens_total` / `ai_call_duration_seconds` — ai_gateway AI 调用三件套
  - `active_websocket_connections` — realtime 实时面试 WS 连接 gauge
  - 各服务 Go runtime 指标（goroutine / 内存 / GC，Prometheus 默认暴露）

> 例：工单「接口 5xx 突增」的排查 PromQL 起点：
> `sum(rate(http_requests_total{code=~"5.."}[5m]))`、`rate(http_request_duration_seconds{path="/api/v1/questions"}[5m])`

## 4. 服务拓扑（供「意图识别/根因推理」做实体定位与 K8s 工具调用）

| 服务 | gRPC | HTTP | 职责 |
|---|---|---|---|
| gateway | - | 8082 | BFF：认证、路由、WS 代理（唯一 HTTP 入口） |
| user | 9101 | 8001 | 用户/认证 |
| membership | 9002 | 8002 | 会员 |
| question | 9003 | 8003 | 题库、判题 |
| interview | 9004 | 8004 | 面试流程、报告 |
| realtime | 9005 | 8085 | 实时语音 WS + 火山对话 |
| growth | 9006 | 8006 | 成长档案 |
| plan | 9007 | 8007 | 学习计划 |
| companion | 9008 | 8008 | 陪伴对话 + Live2D |
| community | 9009 | 8009 | 社区 |
| learning-archive | 9010 | 8010 | 学习档案 |
| ai-gateway | 9011 | 8011 | AI 统一网关 |
| rag | 9012 | 8012 | RAG（Milvus） |
| coderunner | 9013 | - | 代码执行代理 Piston |
| admin | 9014 | 8014 | 管理后台 |

- 服务间用 K8s Service DNS（`makejob-<svc>:<gRPC端口>`）互访；基础设施跑宿主机（`host.docker.internal:端口`）。

## 5. 缺什么 / 需要新项目补的

| 缺口 | 影响 | 建议 |
|---|---|---|
| **无 Loki** | 日志聚合检索无现成源 | 日志解析 Agent 二选一：① 给集群补 Loki 采集 ② 直接走 `kubectl logs` / client-go 拉容器日志 |
| 无 Alertmanager→工单连线 | 工单不能由告警自动触发 | 演示用「人工提交工单」或「故障注入后主动提工单」即可，暂不强求 |

## 6. 演示用故障注入（制造真实 5xx / OOM）

- `kubectl delete pod <question-service-pod>`（可触发上游 5xx）
- 限制副本内存触发 OOMKilled（ReplicaSet 反复重启）
- 停掉依赖（如临时停 Redis/Piston）制造 500 / 功能异常

> 目标演示闭环：制造故障 → Grafana 观察到错误率上升 → 提交工单 → Agent 查真实 Prometheus/日志 → 根因 + 证据 → 执行回滚恢复 → 错误率回落 → 工单「已解决」。