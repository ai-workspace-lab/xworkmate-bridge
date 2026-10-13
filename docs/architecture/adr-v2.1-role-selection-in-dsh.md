# ADR：角色选择与任务编排迁出 Bridge（AI Workspace v2.1）

状态：已接受（2026-10-13）· 取代 [role-routing-engineer-loop.md](role-routing-engineer-loop.md) 中 Bridge 侧的角色选择、授权回传与角色任务快照

## 背景

v2 把 `rolepolicy.Select`、授权回传（`xworkmate.permissions.*`）和角色任务快照（`xworkmate.tasks.get`）都放在 Bridge 内存里。v2.1 目标架构的职责划分不同：

- **App**：任务入口、状态查看、审批、结果呈现。
- **Bridge**：鉴权、协议适配、请求转发、事件/状态转发；不做角色选择和任务编排，也不建第二套持久任务状态。
- **DS Bot + DSH**：角色选择、任务拆解、调度、后台运行、人工介入。
- **Accounts**：namespace、session、taskRun、有序事件、scheduler lease 的唯一持久状态。

## 决定

1. Bridge 删除 `internal/rolepolicy`、`role_routing.go`、授权回传 broker，以及 `acp.capabilities` 里的 `roleRouting` / `permissionRelay`。
2. ACP 的 `session.start` / `session.message` / `xworkmate.routing.resolve` 如果仍带 `routing.role` 或 `routing.roleMode`，返回 `unavailableCode: ROLE_ROUTING_MOVED`。Bridge 不做选择，也不留角色快照，不做兼容转发。
3. 角色任务的链路改为：
   ```text
   App ─(Bridge /api/v1 透传，用户 Accounts 令牌)─▶ Accounts
       POST /api/v1/sessions/{id}/messages {text, routing}
       GET  /api/v1/sessions/{id}/events?after_seq=…      ◀── 事件回放与断线恢复
       POST /api/v1/sessions/{id}/runs/{runId}/cancel     ◀── 新增透传路由
   DSH aiw-orchestrator ─(调度令牌)─▶ Accounts /api/internal/task-scheduler
       claim → 角色/执行器/模型选择 → ctx.subagents（OpenCode / Codex / Claude Code / Grok）
       → renew（观察取消）→ 带 fence 的事件
   ```
   策略、选择器和测试已移植到 `ai-workspace-lab/DeepSeek-Bot` 的 `aiw-orchestrator/`（策略 v0.5：执行器改为 DSH subagent provider 族）。
4. 非角色的 ACP 转发（OpenClaw Gateway、单执行器 provider）保持不变。外部 ACP 执行器的授权请求继续按原有的自动批准回复处理；这条路径不属于本次迁移的范围。

## 影响

- App 需要改为通过 `/api/v1` 下发角色任务、回放事件和取消。Bridge 已透传这些路由，不需要新的 Bridge 状态。
- 角色与模型目录目前不再经 Bridge 通告。App 先提供固定角色列表加“自动”，未启用的角色由编排器以 `run.failed{phase: rejected, roleSelection}` 拒绝。由编排器发布目录是后续工作。
- 执行器授权在 DSH 中按 provider 配置的策略回复（`dsh-subagent-acp` 的 `permission: reject|allow`），不再逐次回传给 App。需要人工审批的任务，由编排器在派发前通过 Accounts 事件请求批准；这是后续工作，不在本 ADR 范围内。

## 契约来源

- Accounts 调度接口与路由提示：`ai-workspace-services/accounts` `docs/architecture/accounts/task-session-durability.md`（2026-10-13 修订）。
- 编排插件、事件序列与配置：`ai-workspace-lab/DeepSeek-Bot` `aiw-orchestrator/README.md`。
