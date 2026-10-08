# 角色模式、自动强度与 AutoBot 中心

设计稿 · 2026-10-08 · 状态：待评审，评审通过后按 §9 的步骤编码

承接 [role-routing-engineer-loop.md](role-routing-engineer-loop.md)（已合入 bridge #30）。本文只写这次新增与改变的部分。

## 1. 已确认的决策

| 问题 | 决定 |
|---|---|
| Gateway chip | 只选 Chat / Work / Coding / AutoBot（#273 的产品模式） |
| 角色模式入口 | 对话框里 Gateway chip 旁边的独立「模式」chip，背后自动路由模型、自动调节强度；用户不选模型和强度 |
| 模式表 | Chat 不显示；Work 只允许 Architect / Researcher / Specialist；Coding 允许四个；AutoBot 不显示 |
| Engineer 执行器 | 默认走 OpenClaw；只有在授权工作区时才用 agent 执行器 |
| 强度起点与范围 | Engineer high（medium–max）、Architect max（high–max）、Researcher high（medium–max）、Specialist high（high–max） |
| AutoBot 入口 | 侧边栏 🤖 按钮打开 AutoBot 中心；Gateway chip 的 AutoBot 改为「把当前草稿变成委派任务」 |
| 后台任务授权 | 创建时预授权范围；越权时暂停，进入审批队列 |
| 顺序 | 先写本文档，评审后编码 |

## 2. 现状与依赖（2026-10-08）

| 仓库 / PR | 状态 | 与本设计的关系 |
|---|---|---|
| bridge `main`（含 #30） | 已合并 | 角色策略、选择器、acp-agent 适配器（opencode-acp / deepseek-harness）、授权回传、取消、任务事件。角色路由目前只走 agent 执行器 |
| bridge #29（`codex/four-capabilities-20261003`） | draft，未合并；与 `main` 在 `internal/acp/types.go` 冲突 | `internal/acp/product_capability.go` 校验 `metadata.xworkmateProductCapability` v1；`chat.send` 前对映射后的 OpenClaw session 调 `sessions.patch` 切换 `xworkmate/<id>` 模型，被拒则不发送。**Gateway 路径的角色选模型必须复用这一步** |
| app `main`（含 #275） | 已合并 | 角色路由状态、授权面板、状态条、断流恢复。#275 的手动选模型 UI 依赖 agent 目标 |
| app #273（`feature/four-capabilities-20261003`） | 未合并；已并入 `main`（`f33f0d4`） | 四个产品模式；去掉 Gateway/Agent 与 Provider 菜单；AutoBot 用 `GatewayBotService`（`cron.add / update / runs / remove`）。合并后角色路由在 app 里**没有入口** |
| Gateway 插件 #9、playbooks #556 | 未检查（不在本会话可访问范围） | 后台任务暂停待审批需要 Gateway 侧支持，见 §8.4 |

## 3. 两个 chip，各管一件事

| | Gateway chip（产品模式） | 模式 chip（角色） |
|---|---|---|
| 回答 | 这是什么任务 | 用什么方式完成、怎样验收 |
| 选项 | Chat / Work / Coding / AutoBot | 默认 / Engineer / Architect / Researcher / Specialist（按 §4 过滤） |
| 请求字段 | `metadata.xworkmateProductCapability.mode`（`chat` / `work` / `code`，沿用 #273 的线上名） | `routing.role`，`routing.roleMode = "auto"` |
| 模型 | 「默认」时：沿用中央目录选择（#273） | 选了角色：由 bridge 角色策略决定，app 不发 `model` |
| 强度 | 「默认」时：沿用现有强度选择（`thinking`） | 选了角色：由 bridge 按 §6 计算，app 不发 `thinking` |

「默认」= 不按角色，行为与 #273 完全一致。

## 4. 模式表（app 与 bridge 同时执行）

| 产品模式（线上名） | 模式 chip | 允许的角色 |
|---|---|---|
| Chat（`chat`） | 隐藏 | — |
| Work（`work`） | 显示 | Architect、Researcher、Specialist |
| Coding（`code`） | 显示 | Engineer、Architect、Researcher、Specialist |
| AutoBot | 隐藏 | —（不是对话轮次） |

- app：不允许的组合不会出现在菜单里；切换产品模式时，如果当前角色不再被允许，模式 chip 回到「默认」，并提示一次。
- bridge：同一张表写在策略里（`roles.<role>.product_modes`），收到不允许的组合直接拒绝，返回 `role_not_allowed_for_mode`，不派发。这样旧客户端也绕不过去。
- Specialist 仍要求 `specialty`；缺少时返回 `specialty_required`（已有）。

## 5. 路由与执行器

### 5.1 请求

app 仍只发一个请求到 bridge（#273 的固定路由），选了角色时：

```json
{
  "method": "session.start",
  "params": {
    "metadata": { "xworkmateProductCapability": { "schemaVersion": 1, "mode": "code" } },
    "routing": { "role": "engineer", "roleMode": "auto" },
    "workingDirectory": "<线程已绑定且用户授权的工作区；没有就不发>"
  }
}
```

### 5.2 执行器选择

在派发前一次性决定，派发后不切换、不自动 fallback（沿用 #30 的规则）。

| 角色 | 执行器顺序 |
|---|---|
| Engineer，**有授权工作区** | `opencode-acp` → `deepseek-harness` → OpenClaw。前两个任一通过全部硬条件即选中；都不通过才选 OpenClaw |
| Engineer，没有授权工作区 | 只用 OpenClaw |
| Architect / Researcher / Specialist | 只用 OpenClaw |

「授权工作区」= 线程已绑定、由用户授权的工作区（现有的工作区绑定），请求里带绝对路径的 `workingDirectory`。agent 执行器另外要求自身的 `workspace_edit`、`test_runner`、`permission_relay` 能力证据为 verified（#30 已有）。

> 待你确认的解读：「默认走 OpenClaw，仅在有授权工作区时才用 agent 执行器」我理解为「有授权工作区时优先 agent 执行器」。如果你的意思是「有授权工作区时也默认 OpenClaw，agent 执行器只作备选」，只需把上表第一行的顺序反过来，其余不变。

### 5.3 OpenClaw（Gateway）执行器

策略里新增执行器类型 `gateway`：

```json
"executors": {
  "openclaw": { "kind": "gateway", "provider_id": "openclaw", "connection": "xworkmate-central",
                "model_option_template": "xworkmate/{model_id}", "capabilities": { ... } }
}
```

流程：

1. 角色选择器选出模型 → bridge 把 `xworkmateProductCapability.model` 设为 `xworkmate/<gateway_model_id>`（覆盖 app 发来的值；选了角色时 app 本就不发）。
2. 复用 #29 的步骤：对映射后的 OpenClaw session 调 `sessions.patch`。被拒 → 本轮失败（`MODEL_BINDING_REJECTED`），不发送、不换模型。
3. `chat.send` 带 `thinking = <§6 算出的强度>`（现有 `openClawChatSendParams…` 已转发 `thinking`）。
4. 结果附 `resolvedRole`、`resolvedExecutor`、`resolvedModelId`、`resolvedEffort`、`modelBindingVerified`（`sessions.patch` 成功即 true）、`effortBindingVerified`（见 §6.3）。

模型候选仍走 #30 的全部硬条件：live 目录精确 ID、能力证据、上下文、外发、预算。Gateway 的 live 目录就是 #273 使用的中央 `xworkmate` 目录。

## 6. 自动强度

### 6.1 档位与角色范围

统一档位：`off < low < medium < high < max`（app 现有强度选择是 `low`–`max`）。

| 角色 | 起点 | 范围 |
|---|---|---|
| Engineer | high | medium – max |
| Architect | max | high – max |
| Researcher | high | medium – max |
| Specialist | high | high – max |

写在策略里：`roles.<role>.effort = { "start": "high", "min": "medium", "max": "max" }`。

### 6.2 调整规则（确定性，不额外调用模型）

从起点开始，按顺序应用，最后夹到范围内：

| 信号 | 调整 | 默认阈值（策略可改） |
|---|---|---|
| 估算上下文大 | +1 | 估算 token > 32000，或附件 ≥ 3 |
| 同一会话的简短跟进 | −1 | `session.message` 且提示 < 200 字符 |
| 同一会话上一轮失败后重试 | +1 | 上一轮状态为 failed |

阈值放在 `limits.effort_rules`。规则只读请求和 bridge 内存里的会话状态，不持久化。

### 6.3 各执行器如何接收

| 执行器 | 使用的设置 | 映射 | `effortBindingVerified` |
|---|---|---|---|
| OpenClaw | `chat.send` 的 `thinking` | 原样传 | Gateway 不回读，记为 `false`，原因 `not_reported` |
| DeepSeek Harness | ACP `reasoning_effort`（`off` / `low` / `high` / `max`） | `medium` 向上取 `high`；只在当前模型的 `configOptions` 广告了该选项时设置，再读回核对 | 读回一致才是 `true` |
| OpenCode | 没有强度选项；有 `mode`（`build` / `plan`） | Architect → `plan`，其他 → `build`；强度记为 `not_applicable` | `false`，原因 `not_applicable` |

执行器没确认的强度，app 不会显示成已生效（状态条写「强度：high（未确认）」）。

## 7. App UI

### 7.1 对话框

- 模式 chip 放在 Gateway chip 右侧，复用 `_TaskDialogSelectorChipInternal`（`lib/features/assistant/assistant_page_task_dialog_controls.dart`）。可见性与选项按 §4。
- 所选角色按线程保存在 `ThreadContextState`，与 #273 的 `productMode` 放在一起；新线程默认「默认」。
- 选了角色时：模型 chip 与强度 chip 变为只读，发送前显示「自动」，发送后显示 bridge 实际选择（来自任务事件或结果）。
- 删除 #275 的手动选模型（`RoleRoutingSelection.manual` + `roleModel`），只保留自动。
- `roleRoutingForTurnInternal` 的条件从「agent 目标」改为「产品模式为 Work / Coding 且角色已选」。
- 状态条、授权面板、断流恢复沿用 #275（`assistant_page_role_task_panel.dart`、`_recoverRoleTaskAfterStreamClosure`），状态条增加强度与执行器显示。
- 移动端：同样的模式 chip 放在 #273 的移动端模式配置里。

### 7.2 Gateway chip 的 AutoBot

选 AutoBot = 「把当前草稿变成委派任务」：打开 AutoBot 中心的新建委派表单，用当前草稿和附件预填；原草稿保留不动。取代 #273 的 `showAssistantBotDialog`。

## 8. AutoBot 中心

### 8.1 侧边栏入口

- 位置：`lib/widgets/sidebar_navigation_task_section.dart`，在「新对话」按钮与「任务列表」标题之间，新增 `FilledButton.tonalIcon`，图标 🤖，文字 AutoBot。
- 徽标：● 已连接 / ○ 离线、运行中 n、未读结果 n、待审批 n。
- 点击：主区打开 AutoBot 页（新增 `WorkspaceDestination.autoBot`），不是弹窗。

### 8.2 页面

| 区块 | 内容 | 现有可复用 |
|---|---|---|
| 连接 | 当前 AI Workspace、Gateway 状态、上次同步时间、重连 / 同步 | 现有远程工作区重连 |
| 委派任务 | 定时任务 + 后台一次性任务的统一列表；新建、暂停 / 恢复、立即运行、历史、删除、打开结果 | 定时：#273 `GatewayBotService` 的 `cron.*`；一次性：现有 OpenClaw 后台任务（`runId` 关联，App 关闭后继续，重开后恢复） |
| 审批队列 | 越权暂停的后台任务；每项可单次批准、拒绝、扩大范围 | 依赖 §8.4 |

### 8.3 委派合同

创建时确定，之后无人值守：

| 字段 | 内容 |
|---|---|
| 任务 | 提示词、附件、目标线程或项目 |
| 角色 | 默认 / Engineer / Architect / Researcher / Specialist（按 §4，以委派时选的产品模式过滤） |
| 模型与强度 | 创建时由 bridge 按角色策略选定，记录精确模型 ID、强度与策略版本 |
| 预授权范围 | 只读 / 指定工作区目录内可写 / 禁止外部副作用（发送、发布、删除）；越权 → 暂停进审批队列 |
| 计划 | 一次 / 每 N 分钟 / 每天某时 |
| 预算 | 创建时须通过角色策略的预算检查 |
| 通知 | 无 / 已配置渠道（沿用 #273 的 delivery） |

- 策略变化：定时任务在 Gateway 上触发时不经过 bridge 重新检查。同步时若策略版本或 live 目录变了，受影响任务标为「策略已变，需重新确认」，不会悄悄换模型。
- 离线：离线时新建或修改的委派先存本地队列（每项带幂等键），恢复连接后按顺序提交；服务端与本地冲突时以服务端为准并显示差异。设备上不执行任何任务。
- 同步：Gateway（`cron.list` / `cron.runs`、后台任务状态）与 Accounts（跨端会话事件）是事实来源；app 只保存缓存和同步游标。启动、网络恢复、打开中心时拉取增量；结果写回对应线程。

### 8.4 越权暂停的前提（未验证）

后台运行没有人实时在线，#30 的实时授权回传（10 分钟超时）不适用。需要：

1. Gateway 在后台运行遇到越权动作时**暂停**该运行，并把待审批项**持久化**；
2. 这些待审批项可以在之后被查询；
3. 之后提交的决定能让运行继续（批准）或结束（拒绝）。

Bridge 的授权状态只在内存里，不能承担持久化。以上能力是否已在 OpenClaw Gateway / 插件 #9 中提供，本会话无法查看，**列为第 0 步验证**。如果不支持，需要在 Gateway 插件仓库补齐；在补齐之前，越权动作按「拒绝」处理，并在委派详情里显示被拒的动作。

## 9. 实施步骤

| # | 工作项 | 输入 | 改动位置 | 验收条件 | 依赖 |
|---|---|---|---|---|---|
| 0a | 合并 bridge #29 到当前 `main` | #29 分支 | 解决 `internal/acp/types.go` 冲突 | `go test ./...` 通过；#29 的产品能力测试与 #30 的角色测试同时通过 | — |
| 0b | 验证 Gateway 暂停待审批能力 | Gateway / 插件 #9 文档或代码 | 只读调研 | 写明支持与否、接口名；不支持则开插件仓库任务 | 需要访问对应仓库 |
| B1 | 策略与选择器 | §4–§6 | `internal/rolepolicy/`（`product_modes`、执行器 `kind: gateway`、`effort`、`limits.effort_rules`）、示例策略 | 模式表拒绝组合；Engineer 有 / 无工作区时的执行器顺序；强度三条规则与范围夹取；全部有单测 | 0a |
| B2 | Gateway 执行器路径 | B1 的选择结果 | `internal/acp/role_routing.go`、复用 #29 的 `sessions.patch` 步骤、`chat.send` 的 `thinking` | 角色轮次在 `sessions.patch` 前覆盖模型；被拒不发送；结果含 `resolvedEffort` 等字段；有 fake Gateway 测试 | 0a、B1 |
| B3 | agent 执行器强度 | B1 | `internal/acpagentadapter/`（`reasoning_effort` 设置与读回、OpenCode `mode`） | dsh 广告时设置并读回；未广告不设置并报告；OpenCode 按角色设 `build` / `plan`；有 fake agent 测试 | B1 |
| B4 | 后台任务待审批 | 0b 的结论 | 视 0b 而定 | 越权暂停可查询、可在之后批准 / 拒绝 | 0b |
| A1 | 模式 chip | B1、B2 的合同 | `assistant_page_task_dialog_controls.dart`、`ThreadContextState`、`app_controller_desktop_role_routing.dart`、移动端模式配置 | 按 §4 显示与过滤；按线程保存；只读的模型 / 强度 chip；删除手动选模型；widget 测试 | #273 合入 `main` |
| A2 | AutoBot 中心（定时） | #273 `GatewayBotService` | 侧边栏按钮、`WorkspaceDestination.autoBot` 页面、Gateway chip 的 AutoBot 改为打开新建委派 | 侧边栏入口与徽标；定时任务列表 / 新建 / 暂停 / 历史 / 删除；widget 测试 | #273 |
| A3 | 后台一次性任务、离线队列、同步 | 现有后台任务关联 | AutoBot 页、本地队列与同步游标 | 离线新建在恢复后按序提交且不重复；结果写回线程；徽标正确 | A2 |
| A4 | 委派合同与审批队列 | B4 | AutoBot 页 | 预授权范围随委派保存；越权项出现在审批队列并可决定 | A3、B4 |
| P / G / K | 部署配置与文档 | — | playbooks、gitops、knowledge | 执行器固定版本、适配器服务、SecretRef；角色 / 模式文档 | 仓库访问 |

合并顺序：bridge 0a → B1 → B2 → B3；app #273 → A1 → A2 → A3；B4 与 A4 一起，取决于 0b。

## 10. 验收用例

- Chat 线程没有模式 chip；Work 线程的模式菜单里没有 Engineer；Coding 四个都有。
- 旧客户端发 `mode: chat` + `role: engineer` → bridge 返回 `role_not_allowed_for_mode`，不派发。
- Coding + Engineer，没有授权工作区 → 执行器为 OpenClaw；有授权工作区且 `opencode-acp` 通过硬条件 → 执行器为 `opencode-acp`。
- Gateway 路径：`sessions.patch` 被拒 → 本轮失败，不发送，不换模型。
- Architect 的简短跟进 → 强度仍为 high（不低于下限）；Engineer 大上下文 → 从 high 升到 max。
- DeepSeek Harness 未广告 `reasoning_effort` → 不设置，结果标 `effortBindingVerified: false`，状态条显示「未确认」。
- 切换到 Work 时当前角色为 Engineer → 模式 chip 回到「默认」并提示。
- Gateway chip 选 AutoBot → 打开新建委派表单，草稿与附件预填，原草稿不变。
- 离线新建委派 → 恢复连接后只提交一次（幂等键）。
- 策略版本变化后同步 → 受影响定时任务标「需重新确认」，未自动换模型。

## 11. 风险与未决

1. §5.2 对「仅在有授权工作区时才用 agent 执行器」的解读，请确认。
2. §8.4 的 Gateway 暂停待审批能力未验证；不支持时 A4 只能「越权即拒绝」。
3. bridge 的角色任务状态仍只在内存里；bridge 重启会丢失进行中的实时轮次（与 #30 相同）。
4. 依赖的 bridge #29 与 app #273 尚未合并；#29 与 `main` 有冲突。
5. 定时任务触发不经过 bridge，模型和强度在创建时固定；只能在同步时提示重新确认。
