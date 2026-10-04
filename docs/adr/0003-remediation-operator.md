# ADR-0003: 处置闭环平台化 —— Remediation Operator

- 状态：已采纳（2026-09-30）
- 关联：M3（处置闭环）、C5（pkg/tasks 异步任务）、P6（告警接入）

## 背景

M3 的处置闭环（`k8s.scale` / `k8s.rollout_undo` + 人工确认门 + 审计落库 + 恢复回验）
全部发生在 aiops-server 进程内：

1. **审批是"点击"不是状态**：HTTP 端点强制 `confirmed=true` 即视为人工确认，
   没有持久化的待审批记录、没有审批人身份、没有拒绝路径。
2. **回验靠 goroutine sleep**：处置成功后 `ScheduleVerify` 起协程睡 60s 再入队，
   进程在这 60s 内崩溃则回验意图丢失（`async_tasks` 的幂等键按分钟，无法补偿"动作已执行但回验从未入队"的窗口）。
3. **回验硬编码**：无论动作类型，固定查 `http_5xx_rate`。
4. **缺陷**：`k8stool.Rollback` 把成功转成 error，`rollout_undo` 补丁已打上却永远报 failed，
   且因此从不触发回验。

这套语义放在"应用内函数"里已经到头了：审批门、执行原子性、崩溃恢复，都是控制循环的母题，
不是 HTTP handler 的母题。

## 决策

新增 `aiops operator` 子命令（同镜像独立 Deployment，双副本 + leader election），
实现 **Remediation CRD（aiops.io/v1）** 的控制器，处置闭环升级为声明式 API：

```
工单报告/告警标签 ──▶ POST /tickets/:id/remediations ──▶ Remediation CR (PendingApproval)
                                                              │
人工审批（Web UI / API，JWT 身份落 CR）──▶ status.approval ──┤
                                                              ▼
                       operator reconcile：Executing（收敛执行）→ Verifying（Prometheus 回验）
                                                              │
                       Succeeded ──▶ sync 镜像回工单消息流 ──▶ 工单自动 resolved
```

### 关键设计

1. **审批即状态**：`status.approval{approved, approver, at, note}` 由服务端写入
   （JWT 用户名可追溯），拒绝是终态 `Rejected`。controller 拒绝在未审批时执行任何动作。
2. **收敛式幂等执行（本 ADR 的核心）**：命令式 `rollout undo`（回滚到"相对上一版"）不幂等，
   重复执行会连环回滚。改造为：**认领阶段就把目标 revision 固化进 `execution.RevisionTo`**，
   执行 = 把 Deployment 模板收敛到该 revision 的 RS 模板。scale 天然是期望态收敛。
   由此，reconcile 任意次重放（进程崩溃/重启/leader 切换）都收敛到同一结果，
   无需区分"崩溃在动作前还是动作后"——这正是 level-triggered 的本义。
3. **observedGeneration 闸**：`status.observedGeneration == metadata.generation`
   表示本代 spec 已处理，杜绝 spec 变更引发的重复执行。
4. **变更动作失败不自动重试**：k8s API 报错直接终态 `Failed`（安全语义）；
   自动重试只发生在回验阶段（有界，`verify.attempts`，耗尽转 `Escalated` 终态）。
5. **回验可配置**：CR `spec.verify{template,threshold,waitSeconds,attempts}`，
   零值回落 operator 启动默认值——按工单类型定制恢复判据，解决硬编码问题。
6. **最小权限 RBAC**：单 namespace watch（cache 收敛）+ namespaced Role；
   operator 只对 Deployment 有 `get/list/scale/patch`，Lease 仅用于选主，
   Events 仅 `create/patch`（处置时间线在 `kubectl describe remediation` 全程可读）。
7. **告警联动**：`alertWebhook` 收到带 `aiops-remediation-action` 等标签的告警时，
   建单同时自动创建待审批 CR——告警→建单→待审批→执行→回验→关单全链闭环，
   但**执行永远等人工审批**，自动化不越过确认门。

### 不采用/暂缓

- **集群级 watch + ClusterRole**：业务 namespace 固定，无需放大权限面。
- **admission webhook**（校验危险动作）：审批门已覆盖安全诉求，留作后续。
- **finalizer**：CR 无外部资源需要级联清理，终态后靠保留策略。
- **controller 生成器脚手架（kubebuilder）**：类型少且稳定，手写 deepcopy 省 codegen 工具链。

## 后果

- 处置能力成为平台 API：任何系统（工单、告警、CI、人）都能 `kubectl apply` 一个 Remediation。
- 复用与修复：`k8stool` 回滚算法移植进 operator 的收敛式实现；M3 的 Bug（成功转 error）已修复，
  旧端点 `/tickets/:id/actions`、`/k8s/actions` 保留（console 直连路径），两条链路并存。
- 测试：controller-runtime fake client 覆盖审批门/幂等重放/回验通过与耗尽；
  服务端端点与告警联动有独立单测。
- 已知限制：Helm 的 `crds/` 目录只随 install 安装、不随 upgrade 更新（schema 变更需手动 apply）；
  sync 镜像的 seen 表在进程内存，服务重启后每个 CR 会重放一条当前阶段消息（幂等无害）。
