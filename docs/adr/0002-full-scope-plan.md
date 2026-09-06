# ADR-0002：全量补齐计划（P0 可观测可视化 → P2 工程完整性）

- 状态：已接受（2026-09-06）
- 背景：M0-M5 主线完成后差距分析显示：前端仅 3 页（参考项目 51 页）、
  无任何图表与可观测视图；后端存在简化实现（异步任务、鉴权、部署、
  慢查询、满意度、多轮追问、模型参数热更新、Grafana）。

## 决策

1. 所有新页面只接真实数据源（Prometheus/Jaeger/K8s/PG），不做空壳页。
2. 控制台观测查询统一走工具执行管道（ticketID=console），享受缓存/限流/审计。
3. 指标查询双模式：模板优先（D6），提供受限自由 PromQL（metric 白名单 + 函数白名单 + 禁注释）。
4. 鉴权默认关闭（auth.enabled），启用后管理面/写操作强制 JWT；查询开放。
5. SSE 维持 DB 轮询（不引入消息总线，避免过度工程）。
6. 部署形态：多阶段 Dockerfile（node 构建 dist → go embed → distroless）+ Helm，
   进同一 kind 集群（"AIOps 运维 PreHire 自身"叙事闭环），RBAC 独立 ServiceAccount。

## 交付批次（每批一个提交，全部完成后整体验收）

| 批次 | 内容 | 验收 |
|---|---|---|
| C1 | 观测 API（templates/overview/range+自由查询）+ 总览/指标页 + 详情趋势图 | 15 服务真实 up 状态 + 真实 5xx/P99 曲线；详情页含故障窗错误率图 |
| C2 | K8s API + 日志/事件/部署/链路四页 | 真实容器日志可查；部署页 scale 落审计；Jaeger 服务可列 |
| C3 | 满意度 + 多轮 HITL + 场景参数热更新 + 慢查询工具 | resolved 可评分；两轮追问；温度 ≤5s 生效；pg_sleep 可定位 |
| C4 | 登录鉴权（默认关） | 开启后 admin/写操作需 JWT，关闭行为不变 |
| C5 | pkg/async 任务状态机 | kill -9 重启后 queued 续跑；重试梯度 20s/30s/45s；死信可查 |
| C6 | Dockerfile/Helm/RBAC/Grafana 仪表盘 | make deploy 后以 Pod 跑在集群，trace 上报 collector:4317 |
| C7 | probe 扩展 + 回归 + 演示脚本 | make probe 全绿；一键 5xx→处置→恢复演示 |

## 关键取舍

- ECharts 按需引入（echarts/core）控制包体。
- 自由查询 step 下限 15s、点数上限 2000，防高基数滥用。
- Helm values 提供数据源端点覆盖（集群内 service DNS vs 宿主机 ingress）。
