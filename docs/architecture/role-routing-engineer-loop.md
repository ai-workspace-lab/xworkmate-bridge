# 角色路由：先打通 Engineer 闭环

版本 0.3 实施稿 · 2026-10-07 · 对应草案《Chat 角色与模型自动选型草案 v0.3》

## 1. 范围

- 复用现有 App → Bridge → workers → 模型连接器主链，不新建 AI Workspace 后端，不改 Toolkit / IaC。
- 本轮只打通 **Engineer** 一个角色的完整闭环：选型 → 派发 → 授权回传 → 取消 → 统一任务事件 → 结果恢复。其余角色（Chat / Worker / Architect / Researcher / Specialist）只在策略里登记，`enabled: false`，后续逐个按同一闭环接入。
- 执行器只接官方、固定版本、受支持接口，**不改内核**：
  - DeepSeek Harness `@deepseek-ai/dsh@0.2.0-rc.2`，`dsh --profile acp`（ACP v1 stdio）。
  - OpenCode `opencode-ai@1.18.35`，`opencode acp`（ACP v1 stdio）。
  - 两者都由同一个 bridge 子命令 `xworkmate-go-core adapter acp-agent` 包成 bridge provider（WebSocket）。
- 模型连接复用已验证的 ai-aggregator（new-api）。只读 `GET /v1/models`；只有发现接口缺口才改 new-api。
- Bridge 仍然只做单任务转发（AGENTS.md 规则 6）：一个任务只选一个执行器和一个模型，不做多智能体编排，不自动 fallback。

## 2. Engineer 闭环时序

```
App(Chat 入口)                Bridge                         acp-agent adapter          执行器 (dsh / opencode)
  │ session.start              │                                 │                          │
  │ routing.role=engineer ───► │ rolepolicy.Select               │                          │
  │ routing.roleMode=auto      │  ├ 连接/健康: provider 已连接   │                          │
  │                            │  ├ live /v1/models 精确 ID      │                          │
  │                            │  ├ 能力证据 verified 且未过期   │                          │
  │                            │  ├ 上下文 / 外发 / 预算         │                          │
  │ ◄── task.selected ──────── │  └ 选中 or task.rejected(停止)  │                          │
  │ ◄── task.started ───────── │ ── session.start(model=option)─►│ initialize / session/new │
  │                            │                                 │ set_config_option(model) │
  │                            │                                 │ 读回 currentValue 校验    │
  │                            │                                 │ ── session/prompt ──────►│
  │ ◄── delta (流式文本) ───── │ ◄──────── session/update ────── │ ◄────── session/update ──│
  │ ◄── task.permission_requested ◄── session/request_permission ◄── request_permission ───│
  │ xworkmate.permissions.respond ─►（只接受执行器给出的 optionId）─►│ ─── outcome ────────────►│
  │ ◄── task.permission_resolved│                                 │                          │
  │ session.cancel ──────────► │ 拒绝挂起授权 + 关闭上游连接 ───►│ session/cancel ─────────►│
  │ ◄── task.completed / failed / cancelled + result(resolvedRole, resolvedModelId, modelBindingVerified)
  │ xworkmate.tasks.get ─────► │ 角色任务快照：状态、事件、挂起授权、最后结果（断线恢复）
```

## 3. 角色 Map（当前策略状态）

角色优先级采用用户最新选择；优先级只是选型意图，不代表可调用、能力已认证或价格已核对。所有 `gateway_model_id` 在观察到用户 token 下的 live `/v1/models` 之前保持 `null`。

| 角色 | 用户首选顺序 | 执行器 | 本轮状态 |
|---|---|---|---|
| Chat | claude-opus-5-5 → gpt-6.1-sol | （无工具） | 登记，未启用 |
| Worker | gpt-6-luna → gemini-3.8-flash | 待定 | 登记，未启用 |
| **Engineer** | gpt-6.1-sol → claude-opus-5-5 | opencode-acp → deepseek-harness | **本轮闭环** |
| Architect | gpt-6-astra → Grok4.7 → Fable5.1 | 待定 | 登记，未启用 |
| Researcher | gpt-6-astra → Fable5.1 → Grok4.7 | 需真实检索执行器 | 登记，未启用 |
| Specialist | Mythos5.1 | 按 specialty | 登记，未启用（需 specialty） |

开放权重候选（Qwen3.5-4B、Qwen3.8-27B、Qwen3-Coder-Next、DeepSeek-V4.1-Flash、Kimi-K3、GLM-5.3、MiniMax-M3）在策略里标记 `candidate_only: true`。策略校验禁止它们出现在任何角色的 preferences 中，手动选择也会返回 `model_unknown`，因此不会自行成为 fallback。

配置草案：[`example/role-router-policy.example.json`](../../example/role-router-policy.example.json)（`enabled: false`，`budget.mode: disabled`）。

## 4. 工作项（输入 / 改动位置 / 验收条件）

状态：✅ 本分支已实现并有测试；⏳ 待办（需要对应仓库或线上环境）。

### 4.1 xworkmate-bridge（核心）

| # | 工作项 | 输入 | 改动位置 | 验收条件 | 状态 |
|---|---|---|---|---|---|
| B1 | 角色策略与纯选择器 | 策略 JSON（`BRIDGE_ROLE_POLICY_PATH`）、任务契约（role、roleMode、roleModel、specialty、dataClass、workingDirectory、prompt 长度）、已连接 provider、live catalog | `internal/rolepolicy/policy.go`、`selector.go` | 按“连接 → 目录 → 能力 → 上下文 → 外发 → 预算”硬条件排除；首个通过者按用户顺序选中；拒绝时返回 code、排除理由与下一步；不改标点、大小写；过期证据回到 unknown；`candidate_only` 不可选。`go test ./internal/rolepolicy` | ✅ |
| B2 | Live 模型目录 | 连接 `base_url` 或 `base_url_env`（二选一）+ `token_env`（由 SecretRef 注入） | `internal/rolepolicy/catalog.go`、`internal/acp/role_routing.go`（缓存，`catalog_max_age_seconds/2` 刷新，失败 30s 后重试） | 保留 `data[].id` 原值；401 / 缺 token / 超时都成为 `catalog_unavailable` 而非放行；能力接口不暴露 token | ✅ |
| B3 | 路由接入 | `session.start` / `session.message` / `xworkmate.routing.resolve` 的 `routing.role` / `routing.roleMode` | `internal/acp/routing.go`、`orchestrator.go`、`contract.go` | 有 role 字段才走角色路由，其余请求行为不变；拒绝时不派发并发 `task.rejected`；选中时只把 `model=<executor 选项值>` 交给选中的 provider | ✅ |
| B4 | ACP 执行器适配（DeepSeek Harness / OpenCode） | `dsh --profile acp`、`opencode acp` | `internal/acpagentadapter/`、`main.go`（`adapter acp-agent`）、`internal/acp/config.go`（`deepseek_harness_url` / `opencode_acp_url`）、`provider_compat.go` | 只用 ACP v1 标准方法；模型必须在执行器广告的 `model` 选项中（否则 `MODEL_NOT_ADVERTISED`），设置后读回校验（否则 `MODEL_BINDING_UNVERIFIED`）；工作区必须是绝对路径；无 token 拒绝启动。已对真实 dsh 0.2.0-rc.2 与 opencode 1.18.35 完成 initialize + session/new + 模型广告校验（未发送 prompt、未调用模型） | ✅ |
| B5 | 授权回传 | 执行器 `session/request_permission` | `internal/acp/permission_broker.go`、`provider_compat.go`、`rpc_handler.go`（`xworkmate.permissions.respond` / `.list`） | 角色任务的授权全部等用户决定；只接受执行器给出的 optionId，且 sessionId 必须匹配；超时、取消、断线、未知回复一律按 ACP `cancelled`（拒绝）；适配器不认旧的 `approved:true` 形状 | ✅ |
| B6 | 取消 | `session.cancel` | `rpc_handler.go`、`role_routing.go`、`provider_compat.go`（`context.AfterFunc` 关闭上游连接） | 取消立即拒绝挂起授权、通知上游 `session.cancel`、中断 bridge 等待并返回 `status=cancelled`；不重放任何工具动作 | ✅ |
| B7 | 统一任务事件与恢复 | 运行中各阶段 | `role_routing.go`（`emitTaskEvent`、`roleTaskSnapshot`）、`rpc_handler.go`（`xworkmate.tasks.get`） | 事件 `type=task`，`event=task.<phase>`，phase ∈ selected/started/permission_requested/permission_resolved/completed/failed/cancelled/rejected；每会话保留最近 100 条；`tasks.get` 返回状态、事件、挂起授权与最后结果 | ✅ |
| B8 | 能力广告 | 已加载策略 | `rpc_handler.go`（`acp.capabilities.roleRouting`） | 只给 App 角色、模型 key / 显示名、策略版本与预算模式；不含连接地址和凭据 | ✅ |
| B10 | 双节点会话跟随 | 已转发会话的 `session.cancel`、`xworkmate.permissions.*`、不带 OpenClaw run 标识的 `xworkmate.tasks.get` | `internal/acp/distributed_forwarder.go`（`distributedStickyFollowUp`） | 只沿已有 session 路由转发到持有 run 的节点，自身不创建路由；OpenClaw 查询保持本地路径。`TestDistributedTaskRouterRoutesRoleFollowUpsToSessionOwner` | ✅ |
| B9 | 其他角色接入 | 各角色的执行器与证据 | 只改策略文件；Researcher 需要先有真实检索执行器 | 每个角色复用 B1–B8，无需新代码路径 | ⏳ |

### 4.2 xworkmate-app（保持现有 UI）

| # | 工作项 | 输入 | 改动位置 | 验收条件 | 状态 |
|---|---|---|---|---|---|
| A1 | 自动 / 手动选择 | `acp.capabilities.roleRouting` | `lib/runtime/role_routing.dart`、`lib/features/assistant/assistant_page_task_dialog_controls.dart`（在现有 执行目标 / Provider 选择旁加“角色”下拉）、`lib/app/app_controller_desktop_role_routing.dart` | 只有 bridge 报告策略启用且当前为 agent 目标时才出现；选项：关闭（沿用现有 Provider/模型）、自动、`<角色> · 模型自动`、`<角色> · <模型>`；写入 `routing.role/roleMode/roleModel` | ✅ |
| A2 | 实际角色与模型展示 | `task.*` 事件、结果 `resolvedRole` / `resolvedModelId` / `resolvedProviderId` / `modelBindingVerified` | `lib/features/assistant/assistant_page_role_task_panel.dart`（输入框上方的状态条） | 显示 bridge 实际选中的角色 · 模型 ID · 执行器 · 阶段；拒绝或失败时显示原因 | ✅ |
| A3 | 授权确认 | `task.permission_requested` | 同上面板；`GoTaskServiceClient.respondPermission`；`external_code_agent_acp_desktop_transport.dart` | 按执行器给出的选项渲染按钮，另有“拒绝”；发送失败有提示；`permission_resolved` / 终态后移除 | ✅ |
| A4 | 任务状态与结果恢复 | SSE 断流后 `xworkmate.tasks.get`（按 sessionId） | `external_code_agent_acp_desktop_transport.dart`（`_recoverRoleTaskAfterStreamClosure`） | 断流后最多轮询 10 分钟（等于 bridge 授权超时）；重新显示挂起授权；终态返回结果 | ✅ |
| A5 | 停止 | 现有停止按钮 | 复用 `cancelTask` → `session.cancel` | 角色任务停止后 UI 终止，bridge 拒绝挂起授权 | ✅（复用） |
| A6 | 选择持久化 | 用户偏好 | settings store | 当前选择只在内存中，重启后回到“关闭” | ⏳ |

### 4.3 playbooks（复用 `roles/vhosts/xworkmate_workers`，不另建部署体系）

| # | 工作项 | 输入 | 改动位置 | 验收条件 | 状态 |
|---|---|---|---|---|---|
| P1 | 执行器固定版本 | `@deepseek-ai/dsh@0.2.0-rc.2`、`opencode-ai@1.18.35` | `roles/vhosts/xworkmate_workers` 的变量与安装任务 | 版本固定、可回滚；`dsh --profile acp --help` 与 `opencode --version` 输出匹配 | ⏳ |
| P2 | 适配器服务 | `xworkmate-go-core adapter acp-agent` | 同角色的 systemd user unit 模板（每个执行器一个），环境变量见 §6 | 仅监听 loopback / 私网；`ACP_AGENT_ADAPTER_AUTH_TOKEN` 来自 Vault/SecretRef；bridge `acp.capabilities` 的 `providerProbeSummary` 中 `deepseek-harness` / `opencode-acp` 为 available | ⏳ |
| P3 | 模型连接与健康检查 | ai-aggregator base URL、token SecretRef | 执行器配置模板（dsh profile patch / opencode.json provider `ai-internal`，不写明文 key）；健康检查任务 | 健康检查只做 initialize + session/new（不发 prompt）；`GET /v1/models` 返回 200 | ⏳ |

### 4.4 gitops（只存配置和 SecretRef）

| # | 工作项 | 输入 | 改动位置 | 验收条件 | 状态 |
|---|---|---|---|---|---|
| G1 | 执行器版本声明 | P1 版本 | `topology/prod/selfhost/` 下 xworkmate workers 清单 | 版本与 playbooks 一致；变更走 PR | ⏳ |
| G2 | 角色策略引用 | 本仓库 `example/role-router-policy.example.json` 审核后的副本 | 策略文件 + bridge `BRIDGE_ROLE_POLICY_PATH` | 默认 `enabled: false`、`budget.mode: disabled`；启用需单独 PR 并附证据 | ⏳ |
| G3 | 可选 gateway 连接器 | `ai-internal` base URL | 连接声明 + `XWORKMATE_AI_INTERNAL_BASE_URL` 与 `XWORKMATE_AI_INTERNAL_TOKEN`（后者为 SecretRef） | 仓库中无明文 token | ⏳ |

### 4.5 knowledge

| # | 工作项 | 改动位置 | 验收条件 | 状态 |
|---|---|---|---|---|
| K1 | 架构、角色模型 Map、任务清单与验收说明 | `docs/zh/ai-collaboration-guide/` 下新增一页，链接到本文 | 内容与本文 §3–§8 一致，指向固定 commit | ⏳ |

### 4.6 new-api

| # | 工作项 | 验收条件 | 状态 |
|---|---|---|---|
| N1 | 复用 `/v1/models` 与 token 配额 | 只有当 live catalog 或配额预扣无法满足 B2 / §6 预算前提时才提缺口并改动 | 暂无改动 |

## 5. 契约

### 5.1 请求

```json
{
  "method": "session.start",
  "params": {
    "sessionId": "…", "threadId": "…",
    "taskPrompt": "…",
    "workingDirectory": "/abs/path/authorized/for/this/task",
    "routing": {
      "role": "engineer",
      "roleMode": "auto | manual",
      "roleModel": "gpt-6.1-sol",
      "specialty": "",
      "dataClass": "unclassified"
    }
  }
}
```

- `roleMode=auto` 且未给 `role`：只有一个启用角色时自动取它，否则 `role_required`（先澄清，不猜）。
- `roleMode=manual` 必须给 `role`；给了 `roleModel`（策略模型 key）时只评估该模型，仍过全部硬条件。
- 带 `metadata.xworkmateProductCapability.mode` 时按策略 `roles.<role>.product_modes` 检查：不在表内返回 `role_not_allowed_for_mode`，不派发（模式表见 [role-modes-and-autobot-hub.md](role-modes-and-autobot-hub.md) §4）。不带产品模式的旧客户端不做此检查。
- 执行器有类型 `kind`：`agent`（ACP 执行器，需要绝对路径的工作区，否则排除为 `workspace_required`；`required_executor_capabilities` 只约束这一类）与 `gateway`（OpenClaw Gateway，不需要工作区）。按角色的 `executors` 顺序优先，其次才是模型顺序。当前 bridge 只派发 `agent`，`gateway` 候选会被排除为 `executor_kind_unsupported`，直到 Gateway 路径接入（角色设计 B2）。
- 角色声明 `effort`（`start` / `min` / `max`）时，选中结果带 `roleSelection.effort`：从起点按 `limits.effort_rules` 的三条规则调整（大上下文 +1、短跟进 −1、失败后重试 +1）后夹到范围内。

### 5.2 结果附加字段

`resolvedRole`、`resolvedModelId`（网关精确 ID）、`resolvedModel`（执行器选项值，例如 `ai-internal/gpt-6.1-sol`）、`resolvedProviderId`、`modelBindingVerified`、`roleSelection`（策略版本、执行器与其类型 `executorKind`、强度 `effort`、理由、排除列表、下一步）。拒绝时 `status=unavailable`，`unavailableCode=ROLE_SELECTION_REJECTED`；未配置策略为 `ROLE_POLICY_UNCONFIGURED`，策略无效为 `ROLE_POLICY_INVALID`。

### 5.3 任务事件（`session.update`）

```json
{
  "type": "task", "event": "task.permission_requested",
  "sessionId": "…", "threadId": "…", "turnId": "…", "pending": true, "error": false,
  "task": {
    "taskId": "turn-…", "phase": "permission_requested",
    "role": "engineer", "roleMode": "auto", "executor": "opencode",
    "providerId": "opencode-acp", "modelKey": "gpt-6.1-sol", "modelId": "gpt-6.1-sol",
    "policyVersion": "…", "at": "RFC3339",
    "detail": { "requestId": "perm-…", "toolCall": {…}, "options": [{"optionId": "allow-once", "kind": "allow_once"}, …] }
  }
}
```

### 5.4 新增 RPC

| 方法 | 参数 | 说明 |
|---|---|---|
| `xworkmate.permissions.respond` | `sessionId`、`requestId`、`optionId` 或 `decision: "cancel"` | 只能选执行器给出的选项；会话不匹配、重复决定、未知请求都报错 |
| `xworkmate.permissions.list` | `sessionId`（可选） | 挂起授权列表 |
| `xworkmate.tasks.get` | `sessionId` | 角色任务优先返回角色快照；非角色任务行为不变 |
| `session.cancel` | `sessionId` | 返回值新增 `runCancelled` |

## 6. 部署与激活（Engineer）

1. 执行器（固定版本）与适配器：

   ```bash
   # DeepSeek Harness
   ACP_AGENT_ADAPTER_AUTH_TOKEN=<from SecretRef> \
   xworkmate-go-core adapter acp-agent --listen 127.0.0.1:8795 \
     --provider-id deepseek-harness --command dsh --args "--profile acp"
   # OpenCode
   ACP_AGENT_ADAPTER_AUTH_TOKEN=<from SecretRef> \
   xworkmate-go-core adapter acp-agent --listen 127.0.0.1:8796 \
     --provider-id opencode-acp --command opencode --args acp
   ```

   bridge 端：`upstream.deepseek_harness_url` / `upstream.opencode_acp_url`（或 `DEEPSEEK_HARNESS_RPC_URL` / `OPENCODE_ACP_RPC_URL`），`UPSTREAM_AUTHORIZATION_HEADER` 与适配器 token 一致。
2. 执行器的模型连接：OpenCode 使用 provider `ai-internal`（base URL 由部署注入，例如 `https://<ai-gateway-host>/v1`；凭据存 OpenCode 凭据库，不写明文）；执行器广告的选项值形如 `ai-internal/<网关精确 ID>`，因此 `model_option_template` 为 `ai-internal/{model_id}`。DeepSeek Harness 选项值是 JSON 对 `["<provider>","<model>"]`，需要在 dsh profile 中配置同名 provider 后才能使用模板 `["ai-internal","{model_id}"]`。
3. 证据：用 bridge 所用 token 获取 live `/v1/models`，把观察到的精确 ID 写入 `gateway_model_id`；为执行器能力（`workspace_edit`、`test_runner`、`permission_relay`）和模型能力（`tool_calling`、`context_window`）补 `verified` 证据，填写 `source`、`checked_at`、`valid_until`。
4. 预算：`budget.mode` 只有两种：`disabled`（默认，拒绝所有付费派发）和 `gateway_quota`（以 new-api token 配额的预扣作为硬上限）。只有在该 token 已设置配额上限后才切到 `gateway_quota`。本轮不实现 bridge 内部的金额账本，因为 bridge 不持久化状态，重启会丢失预留。
5. 最后把策略 `enabled` 设为 `true`，用 `xworkmate.routing.resolve` 查看 `roleSelection` 确认排除理由为空，再在 App 中选择“角色：自动”。

## 7. 兼容范围（显式声明）

| 兼容层 | 范围 | 负责人 | 退出条件 | 计划移除 |
|---|---|---|---|---|
| 非角色任务的权限自动批准（`writeExternalPermissionApproval`） | 未带 `routing.role` 的 externalACP / codex 会话，行为与改动前一致 | xworkmate-bridge 维护者 | App 授权面板（A3）上线，且所有 agent 会话改走 relay | Engineer 闭环在生产验收通过后的下一个版本 |
| `opencode`（HTTP 适配器）与 `opencode-acp` 并存 | 现有 Chat 走 `opencode`；Engineer 走 `opencode-acp` | 同上 | `opencode-acp` 在生产稳定一个发布周期 | 同上；移除 `opencode_url` 与 `internal/opencodeadapter` |

## 8. 验收用例与测试映射

| 草案验收用例 | 测试 |
|---|---|
| catalog 返回新 ID 但无能力记录 → 不选 | `TestSelectNewCatalogIDWithoutEvidenceIsNotSelected` |
| 名称标点 / 大小写不同 → 不匹配 | `TestSelectDoesNotNormalizeModelIDPunctuation` |
| Engineer 无授权工作区 → 指出缺项 | `TestSelectEngineerWithoutWorkspaceIsRejected`、`TestEngineerLoopRequiresWorkspace`、adapter `TestWorkspaceAndContinuationGuards` |
| 手动指定不存在 / 不可用 / 能力不符的 ID → 明确拒绝 | `TestSelectManualModelGoesThroughSameGates`、adapter `TestModelMustBeAdvertisedAndVerified` |
| 上下文或价格未知 → 不放行 | `TestSelectContextGate`、`TestSelectBudgetDisabledRefusesDispatch` |
| 外发条件不满足 → 不传上下文 | `TestSelectEgressGate` |
| Specialist 无 specialty → 澄清 | `TestSelectSpecialistRequiresSpecialty` |
| 有副作用工具前断开 → 不自动再执行 | `TestEngineerLoopCancelDeniesPendingPermissionAndStopsRun`、adapter `TestLegacyAutoApproveShapeIsDenied`、`TestSelectedOptionMustBeOffered` |
| 完整闭环（选型 → 授权 → 完成 → 恢复） | `TestEngineerLoopRelaysPermissionAndReportsBinding` |
| 目录缺模型 → 不派发 | `TestEngineerLoopRejectsModelMissingFromLiveCatalog` |
| 能力广告不泄露凭据 | `TestCapabilitiesAdvertiseRoleRouting` |
| App 解析 / 参数 / 授权回传 | xworkmate-app `test/runtime/role_routing_test.dart` |

## 9. 已知限制

- 角色自动分类尚未实现：`auto` 只在只有一个启用角色时自动取用，否则要求选择角色。
- Bridge 状态只在内存中：bridge 重启后，进行中的角色任务、事件与挂起授权丢失（与现有非 OpenClaw 会话一致）。
- DeepSeek Harness 仍是 developer preview（`0.2.0-rc.2`），升级前需重跑 B4 的握手检查。
- 本轮没有调用付费模型、没有访问 live catalog、没有修改线上服务；所有线上证据（§6 第 3 步）需在部署环境补齐。
