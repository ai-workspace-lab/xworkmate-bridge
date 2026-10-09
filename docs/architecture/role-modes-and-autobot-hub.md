# 角色模式、自动强度与 AutoBot 中心

设计稿 · 2026-10-08（修订：执行器分工、审批默认值、§8.5） · 状态：待评审，评审通过后按 §9 的步骤编码

承接 [role-routing-engineer-loop.md](role-routing-engineer-loop.md)（已合入 bridge #30）。本文只写这次新增与改变的部分。

## 1. 已确认的决策

| 问题 | 决定 |
|---|---|
| Gateway chip | 只选 Chat / Work / Coding / AutoBot（#273 的产品模式） |
| 角色模式入口 | 对话框里 Gateway chip 旁边的独立「模式」chip，背后自动路由模型、自动调节强度；用户不选模型和强度 |
| 模式表 | Chat 不显示；Work 只允许 Architect / Researcher / Specialist；Coding 允许四个；AutoBot 不显示 |
| Engineer 执行器 | 有本地工作区：用 agent 执行器（远端运行，操作本地工作区的远端工作副本，见 §5.4）；没有本地工作区：OpenClaw Gateway 在远端执行，本地目录只同步远端结果 |
| 上架约束 | 必须同时满足 App Store 与 Google Play 上架：任何平台、任何分发渠道都不在设备上执行 agent 或下载的代码；App 只做文件传输与展示 |
| 强度起点与范围 | Engineer high（medium–max）、Architect max（high–max）、Researcher high（medium–max）、Specialist high（high–max） |
| AutoBot 入口 | 侧边栏 🤖 按钮打开 AutoBot 中心；Gateway chip 的 AutoBot 改为「把当前草稿变成委派任务」 |
| 后台任务授权 | 创建时预授权范围；越权时暂停，进入审批队列 |
| 执行器分工 | 在线轮次（用户在 App 里等待）走 bridge #30 的 acp-agent 执行器（授权实时回传、可取消）；离线委派（AutoBot）走 OpenClaw Gateway 与插件 #9 的 worker |
| Gateway 命令审批默认值 | 部署时 `security = allowlist`、`ask = on-miss`、`askFallback = deny`（playbooks #637） |
| 连接地址 | 策略与文档不写真实主机名；连接用 `base_url_env` 由部署注入（bridge #31，已合并） |
| OpenClaw 版本 | 升级到 `2026.9.9`（playbooks #638、插件 #10，均已合并），以使用会话级 `permissionMode`、自动化长期授权等 §8.5 中的能力 |
| 顺序 | 先写本文档，评审后编码 |

## 2. 现状与依赖（2026-10-08）

| 仓库 / PR | 状态 | 与本设计的关系 |
|---|---|---|
| bridge `main`（含 #30） | 已合并 | 角色策略、选择器、acp-agent 适配器（opencode-acp / deepseek-harness）、授权回传、取消、任务事件。角色路由目前只走 agent 执行器 |
| bridge #29（`codex/four-capabilities-20261003`） | 已合并（`bc96e01`） | `internal/acp/product_capability.go` 校验 `metadata.xworkmateProductCapability` v1；`chat.send` 前对映射后的 OpenClaw session 调 `sessions.patch` 切换 `xworkmate/<id>` 模型，被拒则不发送。**Gateway 路径的角色选模型必须复用这一步** |
| app `main`（含 #275） | 已合并 | 角色路由状态、授权面板、状态条、断流恢复。#275 的手动选模型 UI 依赖 agent 目标 |
| app 本地执行现状 | 已有 | 线程有 `workspaceBinding`（`WorkspaceKind.localFs` + `workspacePath`）；远端执行结果按「APP 工作区优先」写回线程本地目录（见 [remote-agent-local-workspace-test-matrix.md](../testing/remote-agent-local-workspace-test-matrix.md)）；`ExternalCodeAgentProvider` 已有 `subprocess` / `websocketJsonRpc` 两种传输定义；App Store 构建按 `shouldBlockEmbeddedAgentLaunch` 禁止本地拉起进程；app 不内置 `xworkmate-go-core` |
| app #273（`feature/four-capabilities-20261003`） | 未合并；已并入 `main`（`f33f0d4`） | 四个产品模式；去掉 Gateway/Agent 与 Provider 菜单；AutoBot 用 `GatewayBotService`（`cron.add / update / runs / remove`）。合并后角色路由在 app 里**没有入口** |
| OpenClaw | 部署固定 `2026.6.1`；升级到 `2026.9.9` 的 playbooks #638 与插件 #10 已合并，升级演练通过：bridge 协议 v4、`sessions.patch`、`chat.send`、插件方法均兼容 | shell 命令审批见 §8.4；按委派任务限定工具见 §8.5 |
| 插件 #9（`openclaw-multi-session-plugins`，未合并） | draft | Gateway 侧 `xworkmate_worker` 工具：Work → DSH ACP worker，Code → OpenCode v2 worker。DSH 的 ACP 授权请求一律拒绝（「直到实现审批回调」）；OpenCode v2 用静态权限规则，shell / git / 构建 / 测试默认拒绝。按 §1 的分工只用于离线委派；接入审批前不扩大其权限 |
| playbooks #556 | 未检查 | `roles/vhosts/xworkmate_workers` 的 worker 部署 |
| playbooks #637 | 已合并 | `gateway_openclaw` 角色写入命令审批默认值（`openclaw.json` 的 `tools.exec` 与主机 `exec-approvals.json` 的 `defaults`，保留运行时允许名单） |
| gitops | 无需改动 | 目前没有 OpenClaw 的声明面（只有 compose 注释提及）；审批默认值属于主机服务配置，按仓库边界归 playbooks |

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
    "workspace": { "kind": "local", "syncId": "<§5.4 上传完成后得到的远端工作副本 ID；没有本地工作区就不发>" }
  }
}
```

### 5.2 执行器选择

在派发前一次性决定，派发后不切换、不自动 fallback（沿用 #30 的规则）。

| 角色 | 执行器顺序 |
|---|---|
| Engineer，**有本地工作区** | 远端 `opencode-acp` → 远端 `deepseek-harness` → OpenClaw。前两个任一通过全部硬条件即选中，在 §5.4 的远端工作副本里执行；都不通过才选 OpenClaw |
| Engineer，没有本地工作区 | 只用 OpenClaw，本地目录只同步远端结果 |
| Architect / Researcher / Specialist | 只用 OpenClaw |
| 任何角色的离线委派（AutoBot） | 只用 OpenClaw（Gateway 上的定时 / 后台任务；Work / Code 由插件 #9 的 worker 执行），见 §8 |

「本地工作区」= 线程的 `workspaceBinding` 为 `localFs`，且是用户选定的项目目录（不是 App 自动建立、只用来接收结果的线程目录；`workspaceBinding` 需要能区分这两种来源，见 A5）。agent 执行器另外要求自身的 `workspace_edit`、`test_runner`、`permission_relay` 能力证据为 verified（#30 已有）。所有执行器都在远端运行。

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

### 5.4 本地工作区与远端工作副本（满足商店上架）

原则：设备只读写和传输文件，不执行任何 agent、脚本、构建或下载的代码；agent、测试、构建全部在远端。这样同时满足 App Store 审核指南 2.5.2 与 Google Play 关于下载可执行代码的限制，并且所有平台、所有分发渠道行为一致，不需要分版本。

**有本地工作区（Engineer 走 agent 执行器）：**

1. **上传（发送前）**：App 计算本地工作区清单（相对路径、大小、SHA-256），经 bridge 新增的 `xworkmate.workspace.sync.push` 只上传远端缺失或变化的文件，得到 `syncId` 与基线清单哈希。远端工作副本放在运行 acp-agent 适配器的 worker 上，按 `syncId` 隔离。
   - 遵循 `.gitignore`；默认排除 `.env*`、私钥 / 证书文件和依赖目录（如 `node_modules`），排除规则可配置。
   - 单文件与总量设上限（初值待定，写进策略 `limits.workspace_sync`）；超限时不派发，提示缩小范围。
   - 首次全量，之后的跟进轮次只传增量。
2. **执行**：agent 的 `session/new` 的 `cwd` 为远端工作副本目录；测试在远端运行；授权回传与取消沿用 #30。
3. **取回（完成后）**：bridge 返回改动集（按文件的新增 / 修改 / 删除，附 diff 与上传时的基线哈希）；App 展示 diff。
4. **应用**：用户确认后才写入本地工作区。写入前逐文件核对本地当前哈希与基线；执行期间本地也改过的文件标为冲突，不覆盖，由用户选择保留本地、采用远端或另存。
5. **保留与回收**：远端工作副本保留一段时间（初值待定）供跟进轮次复用，之后回收；回收后的跟进轮次重新全量上传。

**没有本地工作区（OpenClaw）：** OpenClaw 在远端目录执行，结果按现有「APP 工作区优先」规则单向写回线程本地目录（同名文件版本化为 `.v2`），不需要确认，因为这是 App 自己的结果目录。

**平台文件访问：**

| 平台 | 本地工作区访问方式 |
|---|---|
| macOS（含 App Store 沙盒版） | 用户选择目录 + security-scoped bookmark |
| Windows / Linux | 用户选择的普通目录 |
| iOS | 系统文件选择器授权的目录（security-scoped URL） |
| Android | Storage Access Framework 授权的目录 |

如果某个平台无法稳定持续访问用户目录，该平台只提供「没有本地工作区」路径，不降级为本地执行。

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
| 预授权范围 | 只读 / 指定工作区目录内可写 / 禁止外部副作用（发送、发布、删除）；越权 → 暂停进审批队列。如何映射到 OpenClaw 见 §8.5 |
| 计划 | 一次 / 每 N 分钟 / 每天某时 |
| 预算 | 创建时须通过角色策略的预算检查 |
| 通知 | 无 / 已配置渠道（沿用 #273 的 delivery） |

- 执行位置：委派任务始终在远端执行。有本地工作区的委派，在创建时按 §5.4 上传一次快照；结果在同步时以改动集形式出现，用户确认后才写入本地工作区。
- 策略变化：定时任务在 Gateway 上触发时不经过 bridge 重新检查。同步时若策略版本或 live 目录变了，受影响任务标为「策略已变，需重新确认」，不会悄悄换模型。
- 离线：离线时新建或修改的委派先存本地队列（每项带幂等键），恢复连接后按顺序提交；服务端与本地冲突时以服务端为准并显示差异。设备上不执行任何任务。
- 同步：Gateway（`cron.list` / `cron.runs`、后台任务状态）与 Accounts（跨端会话事件）是事实来源；app 只保存缓存和同步游标。启动、网络恢复、打开中心时拉取增量；结果写回对应线程。

### 8.4 越权暂停：第 0b 步核对结果（2026-10-08）

后台运行没有人实时在线，#30 的实时授权回传（10 分钟超时）不适用。设计要求：越权时暂停、待审批项可在之后查询、之后的决定能让运行继续或结束。

**OpenClaw 已有的能力**（只读核对 `src/gateway/exec-approval-manager.ts`、`src/infra/exec-approvals.ts`、`src/agents/bash-tools.exec-host-*.ts`）：

| 项 | 现状 |
|---|---|
| Gateway 方法 | `exec.approval.request / get / list / waitDecision / resolve`；广播事件 `exec.approval.requested / resolved` |
| 策略 | 按 agent 配置（`~/.openclaw/exec-approvals.json`）：`security` = `deny` / `allowlist` / `full`；`ask` = `off` / `on-miss` / `always`；`askFallback`（无人决定时的结果）；命令允许名单 |
| 超时 | 默认 30 分钟（`DEFAULT_EXEC_APPROVAL_TIMEOUT_MS`）；超时后按 `askFallback` 处理 |
| 默认值 | `security = full`、`ask = off`、`askFallback = full`：**默认不审批，超时也放行** |
| 覆盖范围 | 只覆盖 shell 命令（bash / exec 工具）；文件编辑、发送消息、发布等不经过这套审批 |
| 持久化 | 待审批项存在内存 `Map` 里；Gateway 重启即丢失；等待期间运行一直阻塞 |
| 定时任务 | `src/cron` 里没有审批相关代码；定时触发的 agent 轮次用同样的工具，所以只要配置了策略就同样生效，但不会和 AutoBot 中心自动关联 |

**结论**：设计要求部分满足。

- 可以做到：预授权 = 为委派任务使用的 agent 配置 `security = allowlist`、`ask = on-miss`、`askFallback = deny` 加命令允许名单；越权 shell 命令 → 运行等待 → bridge 订阅 `exec.approval.requested` 并在同步时调用 `exec.approval.list`，放进审批队列 → 用户决定后调用 `exec.approval.resolve`；超时则拒绝。
- 做不到（需要改 OpenClaw 或插件）：
  1. **长时间停放**：等待有超时上限，且运行一直占着资源；用户几小时后才回来时，运行可能已经按拒绝结束。
  2. **持久化**：Gateway 重启后待审批项丢失（按拒绝处理）。
  3. **非 shell 副作用**：文件写入、发送、发布不在命令审批范围内；按任务限定工具见 §8.5。
- 插件 #9 的 worker：DSH ACP 授权请求目前一律拒绝；OpenCode v2 用静态权限规则。两者都没有接入上面的审批流程。

**对 A4 的修订**：第一版按「越权 → 等待（超时可配，默认沿用 30 分钟）→ 审批队列 → 超时拒绝」实现；委派详情明确显示「超时未决定按拒绝处理」。真正的长时间停放与持久化，作为 OpenClaw / 插件仓库的后续工作单独立项。

### 8.5 按委派任务限定工具范围：OpenClaw 最新版核对（2026-10-08）

核对对象：npm `openclaw@2026.9.9`（`latest`，2026-10-08 发布）包内的 `docs/`、`CHANGELOG.md` 与协议 schema，并与 docs.openclaw.ai 对照；部署版本 `2026.6.1` 的情况用本地 `2026.6.2` 源码核对。

| 机制 | 粒度 | 限制什么 | 越权时 |
|---|---|---|---|
| 定时任务 `payload.toolsAllow`（`cron.add / cron.update`，CLI `--tools`） | **每个定时任务**（每次运行都生效） | 工具名（MCP 工具可用通配）；只能在全局 / agent 策略之内收窄 | 直接拒绝（工具不提供给模型），不暂停 |
| 定时任务的 `agentId`、`sessionTarget`、`model`、`thinking`、`timeoutSeconds` | 每个定时任务 | 选用哪个 agent 及其配置 | — |
| agent 级 `tools.allow / deny / profile`、沙箱、`exec-approvals.json` 的 `agents.<id>` 命令允许名单 | 每个 agent | 工具名；shell 命令；沙箱的目录与网络 | 工具：拒绝；命令：`ask = on-miss` 时**暂停等审批** |
| 自动化的长期授权（standing grants，`2026.9.x`） | 每个定时任务 + 精确命令 / 目录 / 环境 | 用户对某次审批选「总是允许」后生成，可设过期、可撤销 | — |
| `sessions.create / patch` 的 `permissionMode`（`read-only / guarded / workspace / full`）、`toolOverrides`、`execSecurity / execAsk`（`2026.9.x`） | 每个会话 | 文件系统边界（`sessionRoot`）、谁来审命令、MCP / 技能 / 网页搜索开关 | `guarded`：命令不在名单时找人审批 |
| 插件 `before_tool_call`（`block` / `requireApproval`） | **每次工具调用**（可读 `sessionKey`、`runId`） | 任何工具，包括文件写入、发送 | 可暂停审批；超时默认 2 分钟、最长 10 分钟，超时 / 无审批渠道即拒绝；「总是允许」需插件自己保存 |
| `agent` / `chat.send` / `sessions_spawn` | 每次运行 | **没有**公开的工具允许名单字段 | — |
| 网络 | 全局，或 agent 沙箱 | 出口代理、沙箱网络 | 拒绝；没有按任务的网络范围 |

**结论**：OpenClaw 原生支持按委派任务限定**工具名**，但只在定时任务上（`payload.toolsAllow`，`2026.6.x` 已有），且越权是直接拒绝而不是暂停。命令、文件目录、网络没有按任务的字段，只能按 agent 或会话设置。

**映射到 §8.3 的三种预授权范围**：

| 预授权范围 | 做法 |
|---|---|
| 只读 | 定时任务 `toolsAllow` 只含读类工具；使用 `exec` 为 `deny` 的范围 agent |
| 指定工作区目录内可写 | 使用该范围的 agent（沙箱 `workspaceRoot` 指向远端工作副本，命令 `allowlist + on-miss + deny`）；`2026.9.x` 可改用 `sessions.create` 的 `permissionMode = guarded` + `cwd`，再用 `sessionTarget = session:<id>` 让定时任务进入该会话（是否沿用会话的 `permissionMode` **未核实**） |
| 禁止外部副作用 | `toolsAllow` 不含发送 / 发布类工具；需要「暂停而非拒绝」时，由插件 #9 增加 `before_tool_call` 检查，按 `sessionKey`（隔离运行为 `cron:<jobId>`）查委派范围，返回 `requireApproval` |

一次性委派也走定时任务：`schedule.kind = at`、`deleteAfterRun = true`、`sessionTarget = isolated`，带 `toolsAllow` 与 `agentId`，再调 `cron.run`。这样所有委派共用一套范围字段。

**仍然存在的缺口**：

1. 只有 shell 命令有内建的暂停审批；其他工具的暂停只能靠插件，且最长 10 分钟。
2. 定时任务触发的审批只发给已连接、具备审批能力的客户端（Control UI、App、API 客户端），**没有客户端连接时立即拒绝**。离线委派要想「暂停进审批队列」，bridge 需要作为常驻审批客户端连接 Gateway，接收并转存待审批项；等待仍受 30 分钟超时限制。
3. 待审批项不跨重启保存。
4. standing grants、`permissionMode`、`toolOverrides` 需要 `2026.9.x`；部署版本 `2026.6.1` 只有 `toolsAllow`、agent 级策略和 `execSecurity / execAsk`。升级 OpenClaw 需单独评估。

## 9. 实施步骤

| # | 工作项 | 输入 | 改动位置 | 验收条件 | 依赖 |
|---|---|---|---|---|---|
| 0a | 合并 bridge #29 到当前 `main` | #29 分支 | 解决 `internal/acp/types.go` 冲突 | ✅ 已合并（`bc96e01`） | — |
| 0b | 验证 Gateway 暂停待审批能力 | Gateway / 插件 #9 文档或代码 | 只读调研 | ✅ 已完成，结论见 §8.4 | — |
| B1 ✅（#32） | 策略与选择器 | §4–§6 | `internal/rolepolicy/`（`product_modes`、执行器 `kind: gateway`、`effort`、`limits.effort_rules`）、示例策略 | 模式表拒绝组合；Engineer 有 / 无工作区时的执行器顺序；强度三条规则与范围夹取；全部有单测 | 0a |
| B2 | Gateway 执行器路径 | B1 的选择结果 | `internal/acp/role_routing.go`、复用 #29 的 `sessions.patch` 步骤、`chat.send` 的 `thinking` | 角色轮次在 `sessions.patch` 前覆盖模型；被拒不发送；结果含 `resolvedEffort` 等字段；有 fake Gateway 测试 | 0a、B1 |
| B3 | agent 执行器强度 | B1 | `internal/acpagentadapter/`（`reasoning_effort` 设置与读回、OpenCode `mode`） | dsh 广告时设置并读回；未广告不设置并报告；OpenCode 按角色设 `build` / `plan`；有 fake agent 测试 | B1 |
| B5 | 工作区同步接口 | §5.4 | 新增 `xworkmate.workspace.sync.push` / `.pull`（清单比对、增量上传、改动集与 diff 返回）、worker 上按 `syncId` 隔离的工作副本目录与回收、`limits.workspace_sync`；acp-agent 会话的 `cwd` 指向工作副本 | 增量只传变化文件；排除规则生效（`.env*`、私钥不出设备）；超限拒绝派发；改动集含基线哈希；路径不能逃出工作副本目录；有测试 | B1 |
| B4 | 后台任务待审批 | §8.4、§8.5 | bridge 作为常驻审批客户端连接 Gateway（订阅 `exec.approval.requested`、调用 `exec.approval.list / resolve`），待审批项转存并在同步时提供给 App；委派创建时写入 `toolsAllow` 与范围 agent | 越权命令进入队列；用户在超时前决定可让运行继续；超时 / 无人决定按拒绝；范围外工具不提供给模型；有 fake Gateway 测试 | 0b、P1 |
| A1 | 模式 chip | B1、B2 的合同 | `assistant_page_task_dialog_controls.dart`、`ThreadContextState`、`app_controller_desktop_role_routing.dart`、移动端模式配置 | 按 §4 显示与过滤；按线程保存；只读的模型 / 强度 chip；删除手动选模型；widget 测试 | #273 合入 `main` |
| A5 | 本地工作区同步与改动确认 | B5 | `workspaceBinding` 区分「用户选定的项目目录」与「线程结果目录」；清单计算与上传；diff 审阅与冲突处理界面；各平台目录授权（§5.4 表） | 设备上不启动任何进程；未确认的改动不写入；冲突文件不被覆盖；排除文件不上传；各平台目录授权可持续使用 | A1、B5 |
| A2 | AutoBot 中心（定时） | #273 `GatewayBotService` | 侧边栏按钮、`WorkspaceDestination.autoBot` 页面、Gateway chip 的 AutoBot 改为打开新建委派 | 侧边栏入口与徽标；定时任务列表 / 新建 / 暂停 / 历史 / 删除；widget 测试 | #273 |
| A3 | 后台一次性任务、离线队列、同步 | 现有后台任务关联 | AutoBot 页、本地队列与同步游标 | 离线新建在恢复后按序提交且不重复；结果写回线程；徽标正确 | A2 |
| A4 | 委派合同与审批队列 | B4 | AutoBot 页 | 预授权范围随委派保存；越权项出现在审批队列并可决定 | A3、B4 |
| P1 | Gateway 命令审批默认值 | §1 | playbooks `roles/vhosts/gateway_openclaw`（#637，✅ 已合并） | `openclaw.json` 与主机 `exec-approvals.json` 均为 `allowlist / on-miss / deny`；保留运行时允许名单；重复执行不重启 | — |
| P2 ✅ | OpenClaw 升级到 2026.9.9（已合并，待部署） | §8.5 | 插件 #10（Tasks API 移除、`session_start` 改 `api.on`）；playbooks #638（Node ≥24.16、升级前停服务 + `doctor --repair` 迁移、规范配置、`plugins install` 安装插件、审批默认值改用 `exec-policy set`） | 演练：2026.6.1 状态升级后网关就绪、断言全过、第三次运行 `changed=0`、bridge 探测全部成功 | P1 |
| P3 | 范围 agent | §8.5 | playbooks `openclaw.json.j2`（范围 agent 的工具与命令名单） | 每种预授权范围有对应 agent；委派绑定到它 | P2 |
| P / G / K | 部署配置与文档 | — | playbooks、gitops、knowledge | 执行器固定版本、适配器服务、SecretRef（含 `XWORKMATE_AI_INTERNAL_BASE_URL`）；角色 / 模式文档 | 仓库访问 |

合并顺序：bridge B1 → B2 → B3 → B5；app #273 → A1 → A5 → A2 → A3；P1 → 插件 #10 → P2（#638）→ P3 → B4 → A4。

## 10. 验收用例

- Chat 线程没有模式 chip；Work 线程的模式菜单里没有 Engineer；Coding 四个都有。
- 旧客户端发 `mode: chat` + `role: engineer` → bridge 返回 `role_not_allowed_for_mode`，不派发。
- Coding + Engineer，没有本地工作区 → 执行器为 OpenClaw，结果写回线程结果目录；有本地工作区且远端 `opencode-acp` 通过硬条件 → 先上传工作区，执行器为 `opencode-acp`，在远端工作副本里执行。
- 任何平台、任何构建（App Store、Google Play、直接分发）都不在设备上启动 agent 或脚本进程。
- 远端改动未经用户确认不写入本地工作区；执行期间本地也改过的文件显示为冲突，不被覆盖。
- `.env`、私钥等排除文件不离开设备；跟进轮次只上传变化的文件。
- Gateway 路径：`sessions.patch` 被拒 → 本轮失败，不发送，不换模型。
- Architect 的简短跟进 → 强度仍为 high（不低于下限）；Engineer 大上下文 → 从 high 升到 max。
- DeepSeek Harness 未广告 `reasoning_effort` → 不设置，结果标 `effortBindingVerified: false`，状态条显示「未确认」。
- 切换到 Work 时当前角色为 Engineer → 模式 chip 回到「默认」并提示。
- Gateway chip 选 AutoBot → 打开新建委派表单，草稿与附件预填，原草稿不变。
- 离线新建委派 → 恢复连接后只提交一次（幂等键）。
- 策略版本变化后同步 → 受影响定时任务标「需重新确认」，未自动换模型。

## 11. 风险与未决

1. §5.4 的同步上限、排除规则默认值、远端工作副本保留期还没定初值；大型工作区的首次上传耗时与流量需要实测。
2. 移动端对用户目录的持续访问（iOS security-scoped URL、Android SAF）需要逐平台验证；不稳定的平台只提供「没有本地工作区」路径。
3. §8.4 / §8.5：内建暂停审批只覆盖 shell 命令、只存在内存、有超时上限，且定时任务的审批在无客户端连接时立即拒绝；按任务只能限定工具名（越权即拒绝）。长时间停放、持久化和非 shell 副作用的暂停需要插件或 OpenClaw 的后续工作。
4. 执行器分工已定（§1）：在线走 bridge #30，离线委派走 Gateway 与插件 #9。插件 #9 的 DSH worker 仍一律拒绝 ACP 授权请求，接入 B4 的审批前，离线 Work 任务遇到授权请求会失败。
5. bridge 的角色任务状态仍只在内存里；bridge 重启会丢失进行中的实时轮次（与 #30 相同）。
6. 依赖的 app #273 尚未合并（bridge #29 已合并）。
7. 定时任务触发不经过 bridge，模型和强度在创建时固定；只能在同步时提示重新确认。
