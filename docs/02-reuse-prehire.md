# 从 PreHire 可借鉴/复用的设计与实现

> 本清单按「实际代码」编写（非文档描述）。新项目无法读 PreHire 仓库，故此处给出足够落地的细节。开发 AIOps 时优先对标这些已踩平的设计，尤其缓存与限流的"故障语义"。

## 1. AI 工程（对标源：app/ai_gateway）

| 可复用点 | 现状细节 | 对 AIOps 的意义 |
|---|---|---|
| **Agent 统一接口** | 6 种 Agent（面试/计划/陪伴/出题/简历解析/Live2D 导演）走统一接口 + 工厂，业务侧只依赖接口 | 5 个运维 Agent + 编排引擎可直接沿用同一抽象 |
| **AI 配置热更新** | 配置存 DB（`ai_configs` 场景表 + `admin_configs` 通用兜底），按场景 key（interview/plan/companion/quiz）读取，经两级缓存（L1 5s / L2 60s），改配置无需重启、TTL 内生效 | 工单 Agent 的模型/超时/温度按场景外置可调 |
| **provider 工厂 + fallback 链** | 主 provider + 备 provider + 不可用兜底，多 provider 可切 | 外部 LLM 挂了走降级，与"工具降级"同构 |
| **Prompt 模板** | `{{variable}}` 占位 + DB 存储 + builtin 兜底 | 各 Agent 的提示词外置、版本可管理 |
| **结构化输出** | LLM 返回约束为结构化 JSON，强类型反序列化 | **根因推理 Agent 的结论必须结构化**（根因/证据列表/置信度/建议） |
| **AI 指标** | `ai_calls_total` / `ai_tokens_total` / `ai_call_duration_seconds` | 自可观测里量化 LLM 成本与延迟 |

## 2. 缓存基础设施（对标源：pkg/cache）—— 强烈建议直接复用思路

- **两级缓存**：L1 进程内 `golang-lru/v2 expirable`（容量+TTL）+ L2 Redis（go-redis v9，连接池/超时显式调优）。
- **Loader 读穿透 + 三防**（`pkg/cache/loader.go`）：
  - singleflight 合并同 key 并发回源（**防击穿**）
  - 空值哨兵缓存 30s（**防穿透**）
  - TTL 随机抖动 ±10%（**防雪崩**）
- **版本号失效**：写路径 `BumpVersion`（INCR 版本 key），读 key 拼版本号，旧 key 靠 TTL 自然消亡，免 SCAN 前缀删除。
- **key 规范**：`makejob:{service}:{domain}:{id}`，统一构造器，禁止散落拼接。
- **故障语义（核心思想，务必继承）**：缓存层任何故障都降级直查数据源，绝不阻塞主路径；L2 读取 200ms 超时。

> 对 AIOps 的直接映射：**慢指标查询结果缓存**（相同 PromQL 在时间窗内缓存）、**案例库/知识缓存**，都可用这套 Loader 三防。

## 3. 限流基础设施（对标源：pkg/ratelimit）—— 强烈建议直接复用思路

- **算法**：Redis Lua 令牌桶，单次 EVAL 原子完成"读令牌-按时间补充-扣减-续期"，O(1) 内存，桶 TTL 自动回收（根除 IP-map 无限增长问题）。
- **降级语义（核心，务必继承）**：Redis 故障 → 本地令牌桶兜底（同参数）→ 仍失败 `fail-open` 放行并计数告警；**限流判定 100ms 超时**（限流绝不比业务慢）；非法规则 `fail-safe` 放行。
- **执行点分层**（纵深防御）：ingress 粗限流 → 网关中间件（全局总闸 + method/path 规则 + user/IP 维度 + JWT 解身份）→ gRPC 拦截器链尾宽松本地桶 → 昂贵资源 semaphore 并发上限。
- **契约**：统一 `{code:429,message}` + `X-RateLimit-Limit/Remaining` + `Retry-After` 头；`Decision` 含 `Degraded` 标志。

> 对 AIOps 的直接映射：**保护昂贵资源**——LLM 调用、外部工具调用（Prometheus/Loki/K8s API）的并发上限与配额保护，直接复用 semaphore + 令牌桶思路。

## 4. 异步任务与可靠性（对标源：pkg/mq）

- 任务状态机 `queued → running → succeeded / failed / dead` + 业务幂等 key + 死信队列 + 延迟重试梯度（20s/30s/45s）。
- 原则：**AI/长耗时任务不阻塞用户请求**。

> 对 AIOps 的直接映射：长耗时指标查询、"多轮工单处理"、报告生成都走异步；工单的幂等与重试同上。

## 5. 可观测性（对标源：pkg/telemetry）

- OTel 全链路：HTTP/gRPC/MQ 三通道埋点，日志带 trace_id，每服务 `:6060/metrics` 统一暴露。
- Prometheus 四维指标（HTTP / AI / WebSocket / Go runtime）+ Grafana provisioning 预置仪表盘。

> 对 AIOps：**"自可观测"是必做项**——一次工单处理打成一条分布式 Trace（Agent=span、工具调用=子 span），直接复用这套 telemetry 初始化与埋点模式。

## 6. 部署与配置（对标源：deploy/ + Dockerfile + pkg/config）

- 统一多阶段 Dockerfile（`ARG SERVICE_NAME` 参数化 + distroless 无 shell、非 root、`CGO_ENABLED=0` 静态编译）。
- Helm Chart 单一模板 range 遍历服务（95% 统一 + 5% 差异化）、REVISION 版本化可回滚。
- ConfigMap/Secret 分离，敏感字段 `${VAR}` 占位 + `ApplyEnvOverrides` 启动时环境覆盖，仓库零明文凭证。
- 有状态外置（DB/中间件 docker-compose 宿主机）+ 无状态容器化进 K8s；K8s 内访问宿主机用 `host.docker.internal`。

## 7. 安全与认证（对标源：pkg/auth）

- JWT（HS256）+ gRPC interceptor 身份透传；**黑名单**：`pkg/auth/blacklist.go` Redis 检查器 + 5s 本地负缓存（跨服务登出即刻生效）。
- Casbin RBAC + admin 后台独立权限路径。

## 8. 目录与工程规范

- 每个服务 Kratos DDD 四层：`cmd/server → internal/{conf,service,biz,data}`；跨服务通用基础设施下沉到 `pkg/`。
- 技术决策用 ADR（`docs/adr/`）记录（如 0005 缓存/限流、0001 可观测选型）。
- 中文注释统一风格：每个函数带职责说明注释；文件 UTF-8 编码。

---

## 附：本清单与"面试答案库"可能不一致的提醒

PreHire 仓库里的 `docs/interview-prep*.md` 答案库**滞后于代码**（例如其对限流/缓存的描述旧于 ADR 0005 的实现）。本清单以**代码 + ADR** 为准；若新智能体需要引用 PreHire 的能力，以本清单为准。