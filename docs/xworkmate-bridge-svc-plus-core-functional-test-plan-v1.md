# xworkmate-bridge.svc.plus 功能测试全景规划 V1

## Summary

本轮核心验收拆成两层：

1. 先验证 `xworkmate-app -> accounts.svc.plus -> xworkmate-bridge.svc.plus` 的路由发现链路。
2. 再验证 6 个典型 case 的端到端产出、线程复用与本地 workspace 回写。

重点不是写死 provider 列表，而是确认 UI 和执行层都从 bridge 动态拿到 ACP / gateway / 扩展路由结果。

## Scope

### 1. 路由发现层

- `acp.capabilities` 返回动态 provider 列表。
- 至少覆盖：
  - `singleAgentProviders`: `opencode / codex / gemini`
  - `gatewayProviders`: `openclaw`
- `xworkmate.routing.resolve` 根据 `taskPrompt`、`executionTarget`、`selectedSkills` 返回正确的：
  - `resolvedExecutionTarget`
  - `resolvedProviderId`
  - `resolvedGatewayProviderId`
- `acp.capabilities` 暴露 bridge 内建的生产 provider catalog，并参与后续路由选择。
- APP 对 gateway 的分区应以 `gatewayProviders` / `resolvedGatewayProviderId`
  为主。

### 2. 典型 Case 层

| Case | 目标能力 | 核心验收点 |
| --- | --- | --- |
| `pptx` | 生成演示稿 + 同线程追问修改 | 首次生成文件；继续追问仍复用同一 thread workspace；workspace 不漂移 |
| `docx` | 周报文档生成 | 生成 `.docx`；内容结构正确；写回当前线程 |
| `xlsx` | 公式表格生成 | 生成 `.xlsx`；公式存在且结果可用；写回当前线程 |
| `pdf` | 合并 / 转换 | 输出 `.pdf`；页数或内容符合预期；写回当前线程 |
| `image-resizer` | 图片处理 | 输出图片尺寸正确；写回当前线程 |
| `browser` | 浏览器自动化 | 返回摘要、截图、日志；都写回当前线程 |

### 3. 线程与 Workspace 层

- single-agent 任务必须自动绑定完整 `workspaceBinding`。
- 二次追问不得漂移到新 workspace。
- prompt 文本不能覆盖已绑定 workspace。
- 远程执行目录只作为 metadata 保留，不替代 APP 本地线程 workspace。

### 4. UI 层

- “智能体模式” provider 选项由 bridge 动态发现结果驱动。
- 不使用固定静态列表做功能判断。
- UI 展示应随 bridge 返回结果动态变化。

## Test Plan

### Phase 1: Core Smoke

先验证发现链路和 bridge 核心 ACP 能力：

```bash
flutter test test/runtime/account_bridge_smoke_suite.dart
go test ./internal/acp
```

### Phase 2: Bridge 真实 E2E

验证 bridge 在线真实执行、结果回包和线程复用：

```bash
flutter test test/runtime/bridge_real_e2e_suite.dart
```

### Phase 3: Single-Agent 回归

验证 APP 本地优先 workspace 语义与线程绑定稳定性：

```bash
flutter test test/runtime/app_controller_ai_gateway_chat_suite.dart
flutter test test/runtime/app_controller_single_agent_workspace_binding_regression_test.dart
```

### Phase 4: 6 个 Case 最小验收

每个 case 至少覆盖两步：

1. 首次执行
2. 一次追问 / 复用线程

建议每个 case 最少断言：

| Case | Step 1 | Step 2 |
| --- | --- | --- |
| `pptx` | 成功生成 `.pptx` | 同线程修改 deck，workspace 不变 |
| `docx` | 成功生成 `.docx` | 同线程补充/改写内容，仍写回当前线程 |
| `xlsx` | 成功生成 `.xlsx` 且带公式 | 同线程修改表格，公式或结果仍有效 |
| `pdf` | 成功输出 `.pdf` | 同线程继续合并/转换，结果仍写回同目录 |
| `image-resizer` | 成功输出变换后图片 | 同线程继续调整尺寸/比例，结果写回同目录 |
| `browser` | 成功返回摘要、截图、日志 | 同线程继续浏览任务，产物继续写回同目录 |

## Recommended Assertions

### 路由发现层断言

- `acp.capabilities` 的 provider 列表来自 bridge 当前环境，而不是本地写死。
- bridge 内建生产 catalog 包含 `codex / opencode / gemini`，且不依赖 app 侧预同步。
- bridge 还会暴露 `gatewayProviders = openclaw`。
- `xworkmate.routing.resolve` 在 skill / prompt / target 组合下，返回合理的
  `resolvedProviderId` 或 `resolvedGatewayProviderId`。

### 执行层断言

- `session.start` 成功时返回首轮结果。
- `session.message` 成功时复用同一 thread 语义。
- bridge 成功结果应包含：
  - `artifacts`
  - `resultSummary`
  - `remoteWorkingDirectory`
  - `remoteWorkspaceRefKind`

### APP / Workspace 断言

- 当前线程 `workspaceBinding` 仍是本地 workspace。
- 远程目录只记录为 metadata。
- 产物文件写回当前线程工作目录。
- 同名文件采用版本化命名，不覆盖旧结果。

### UI 断言

- provider 列表显示来自 bridge 的动态发现结果。
- provider 可选项与 bridge 当前能力一致。
- bridge 能力变化后，UI 展示同步更新。

## Execution Order

建议按以下顺序执行，便于快速定位问题：

1. `flutter test test/runtime/account_bridge_smoke_suite.dart`
2. `go test ./internal/acp`
3. `flutter test test/runtime/bridge_real_e2e_suite.dart`
4. `flutter test test/runtime/app_controller_ai_gateway_chat_suite.dart`
5. `flutter test test/runtime/app_controller_single_agent_workspace_binding_regression_test.dart`
6. 再按 `pptx / docx / xlsx / pdf / image-resizer / browser` 逐项补最小验收

## Assumptions

本次测试默认使用以下环境：

- `https://accounts.svc.plus`
- `review@svc.plus`
- `Review123!`
- `BRIDGE_SERVER_URL=https:xworkmate-bridge.svc.plus`
- `BRIDGE_AUTH_TOKEN=...`

额外约定：

- `openclaw` 作为 `gatewayProviders` 之一，按 bridge 发现结果驱动。
- 如果当前环境没有暴露某个 provider，测试允许 `skip`，但要保留断言入口和记录。
- UI 本轮不改结构，只验证 provider 列表来源与展示结果是否随 bridge 动态变化。

## Deliverable

第一版核心功能测试清单的完成标准：

- 有一份稳定的发现链路 smoke
- 有一份 bridge 在线真实 E2E
- 有一份 single-agent workspace 回归
- 有一套覆盖 6 个 case 的最小验收骨架
- 所有断言都围绕“bridge 动态发现 + APP 本地 workspace 优先 + 同线程复用”展开
