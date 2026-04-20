# Bridge Runtime Design

本文档回答一个核心问题：`xworkmate-bridge` 作为独立仓库，如何把 APP-facing bridge、独立 upstream ACP provider、gateway runtime 和本地支撑能力组织成一个统一运行时。

## 1. 运行模式

二进制入口在 [main.go](../../main.go) 中定义，当前支持四种运行模式：

| 模式 | 入口 | 作用 |
| --- | --- | --- |
| `serve` | `acp.Serve` | 启动 bridge HTTP / WebSocket 服务，对外暴露 `/acp/rpc` 与 `/acp`，并附带健康检查路由。 |
| `acp-stdio` | `acp.RunStdio` | 启动 stdio ACP bridge，适合被宿主进程以 stdin/stdout 驱动。 |
| `gemini-acp-adapter` | `geminiadapter.Serve` | 启动 Gemini 专用 ACP adapter，把 Gemini CLI 包装成 ACP HTTP / WebSocket 服务。 |
| `hermes-acp-adapter` | `hermesadapter.Serve` | 启动 Hermes 专用 ACP adapter，把 Hermes stdio ACP 包装成 ACP HTTP / WebSocket 服务。 |
| 默认模式 | `toolbridge.Run` | 启动 MCP 风格的本地工具桥，暴露 `chat`、`claude_review`、`vault_kv` 等工具。 |

  ｜服务     │ 外部入口 (HTTPS/WSS)                           │ 后端转发目标 (Local)     │ 部署方式    │
  ├──────────┼────────────────────────────────────────────────┼──────────────────────────┼─────────────┤
  │ Bridge   │ xworkmate-bridge.svc.plus/                     │ 127.0.0.1:8787           │ 主机进程    │
  │ OpenClaw │ xworkmate-bridge.svc.plus/gateway/openclaw/    │ 127.0.0.1:18789          │ 独立部署    │
  │ Codex    │ xworkmate-bridge.svc.plus/acp-server/codex/    │ 127.0.0.1:9001           │ 主机进程    │
  │ OpenCode │ xworkmate-bridge.svc.plus/acp-server/opencode/ │ 127.0.0.1:38992          │ 主机进程    │
  │ Gemini   │ xworkmate-bridge.svc.plus/acp-server/gemini/   │ 127.0.0.1:8791           │ 主机进程    │
  │ Hermes   │ xworkmate-bridge.svc.plus/acp-server/hermes/   │ 127.0.0.1:3920           │ 主机进程    │


设计含义：

- `main.go` 不承载业务决策，只做模式分发。
- APP-facing canonical bridge 只由 `serve` 模式提供。
- `gemini-acp-adapter` 是独立 adapter，而不是 bridge 主入口的一部分。
- `hermes-acp-adapter` 也是独立 adapter，不是 bridge 主入口的一部分。
- 默认工具桥是本地工具执行面，不参与 APP-facing canonical path。

## 2. 系统边界

### 2.1 Bridge 自身职责

`internal/acp` 是主控面，负责：

- 挂载 HTTP / WebSocket 路由
- 验证 bridge auth 与 origin
- 维护 session / thread 队列
- 做 routing resolve
- 将任务分发到 single-agent、multi-agent 或 gateway 路径
- 向外部 provider 做 ACP forwarding
- 暴露 bridge-owned metadata，例如 provider catalog、gateway provider catalog、resolved routing metadata

### 2.2 Upstream ACP Provider

single-agent provider 目录由 [internal/acp/provider_catalog.go](../../internal/acp/provider_catalog.go) 内建：

- `codex`
- `opencode`
- `gemini`
- `hermes`

这些 provider 的 endpoint 和 auth 归 bridge 所有。APP 只通过 `/acp/rpc` 和 `/acp` 与 bridge 交互，不直接依赖 provider-specific public URL。

### 2.3 Gateway Runtime

`internal/gatewayruntime` 负责 gateway 连接与请求生命周期，当前 bridge 内建的 gateway provider 是：

- `openclaw`

gateway access 不是单独的 public URL family，而是通过 JSON-RPC 方法：

- `xworkmate.gateway.connect`
- `xworkmate.gateway.request`
- `xworkmate.gateway.disconnect`

### 2.4 本地支撑能力

bridge 在 routing 与执行过程中会调用若干支撑包：

- `internal/router`：根据 prompt、memory、available providers、gateway 偏好做执行目标解析
- `internal/skills`：解析显式技能、本地技能匹配与 fallback skill 安装请求
- `internal/memory`：加载本地 / 项目 memory，记录 auto routing 成功经验
- `internal/mounts`：汇总 Codex / OpenCode / OpenClaw / ARIS 等 mount 状态
- `internal/shared`：RPC envelope、provider command、Vault KV、OpenAI-compatible 请求等公共能力

## 3. 核心主链路

### 3.1 `session.start` / `session.message`

APP-facing 主链路从 `internal/acp.Server.handleRequest` 开始：

1. 校验 `sessionId`
2. 通过 `enqueue` 把任务按 `threadId` 串行排队
3. 在 `executeSessionTask` 中强制要求 `routing`
4. 用 `resolveRoutingMetadataWithProviders` 得到 `router.Result`
5. 将 routing 结果写回执行参数：
   - `resolvedExecutionTarget`
   - `resolvedProviderId`
   - `resolvedGatewayProviderId`
   - `resolvedModel`
   - `resolvedSkills`
6. 根据 `mode` 分流：
   - gateway: `runGateway`
   - multi-agent: `runMultiAgent`
   - single-agent: `runSingleAgent`

bridge 在开始与结束阶段通过 `session.update` notification 回传运行状态。

### 3.2 Routing Resolve

`internal/router.Resolver.Resolve` 的输入包括：

- prompt
- working directory
- routing mode
- explicit execution target / provider / model / skills
- available skills
- available providers
- AI gateway base URL / API key
- memory preferences

输出包括：

- `ResolvedExecutionTarget`
- `ResolvedProviderID`
- `ResolvedGatewayProviderID`
- `ResolvedModel`
- `ResolvedSkills`
- `SkillResolutionSource`
- `NeedsSkillInstall`
- `Unavailable*`

核心规则：

- 显式 routing 优先于自动 routing。
- 本地任务倾向 `single-agent`。
- 在线任务倾向 `gateway`。
- 复杂任务可升级到 `multi-agent`。
- memory 中的 preferred route / model / skills / provider 会参与默认值补全。

### 3.3 External ACP Forwarding

single-agent 主链路优先走 bridge 内建 provider catalog。`runSingleAgentViaExternalProvider` 会：

1. 根据 provider 解析 endpoint
2. 用 `sanitizeExternalACPParams` 去掉内部字段
3. 优先使用 bridge-owned upstream auth；如未配置则复用入站 bearer
4. 根据 endpoint scheme 选择 HTTP 或 WebSocket ACP forwarding
5. 收集 upstream notification，并把结果补全为 bridge 统一返回格式

这里的设计重点是：

- bridge 对外保持统一 contract
- provider-specific endpoint 与 auth 不泄漏给 APP
- nested provider forwarding 仍能沿用 bridge bearer

### 3.4 Gateway Connect / Request / Disconnect

`internal/acp/gateway_runtime.go` 负责把 JSON-RPC 参数转为 `gatewayruntime.ConnectRequest` 和 `RequestResult`。

当前 `openclaw` 路径带有 bridge-owned production routing：

- endpoint 固定到 `openclaw.svc.plus:443`
- auth 优先使用 bridge upstream token
- `connectAuthMode` / `connectAuthFields` / `connectAuthSources` 会被 bridge 统一填充

`internal/gatewayruntime.Manager` 维护 runtime session map，并负责：

- 建连
- 重连
- request / response 配对
- push event 转换
- snapshot / log notification 发射

## 4. 横切关注点

### 4.1 Auth

bridge 主入口使用 `BRIDGE_AUTH_TOKEN` 驱动的 bearer auth：

- 如果配置了 token，则必须完全匹配该 token 或 `Bearer <token>`
- 如果未配置 token，则默认放行

Gemini adapter 的 auth 更宽松：

- `GEMINI_ADAPTER_AUTH_TOKEN` 未配置时默认放行
- 配置后才启用 bearer 校验

### 4.2 CORS / WebSocket

bridge 与 Gemini adapter 都维护独立的 allowed origins：

- bridge: `ACP_ALLOWED_ORIGINS`
- Gemini adapter: `GEMINI_ADAPTER_ALLOWED_ORIGINS`

规则是：

- 空 `Origin` 默认允许
- `http://localhost:*` / `http://127.0.0.1:*` 这类前缀匹配通过 `:*` 语法支持
- `/acp/rpc` 支持 `OPTIONS` 预检
- WebSocket upgrade 也复用 origin + auth 校验

### 4.3 Provider Catalog

provider catalog 是 bridge-owned runtime truth，而不是配置中心 UI 真相：

- `newProductionProviderCatalog()` 直接定义生产 provider
- `availableProviderCatalog()` 产出 APP-facing metadata
- `availableGatewayProviderCatalog()` 产出 gateway-facing metadata

这符合仓库 ADR 的约束：APP 只认 bridge canonical entrypoint，不认 provider-specific public URL。

### 4.4 Session Queue / Thread Queue

bridge 把任务串到 `threadId` 级别队列：

- `ensureQueue` 为每个 thread 创建一个缓冲队列
- `runQueue` 顺序执行同一 thread 的任务
- `session.start` 会 reset 同名 session
- `session.cancel` / `session.close` 只操作 session 级状态，不拆分 canonical contract

这样可以保证多轮对话在同一 thread 内顺序执行，避免状态竞争。

### 4.5 Memory / Mounts / Skills 介入点

- routing 前会加载 `memory.Service.Load`
- auto routing 成功后会调用 `RecordSuccess`
- skill 解析在 `router.Resolve` 内完成
- mount 状态通过 `xworkmate.mounts.reconcile` 暴露给上层

这些能力都是 bridge 的支撑层，而不是 APP shell 顶层 surface。

## 5. 设计约束

本仓库的设计约束必须始终保持一致：

- app-facing canonical path 只有 bridge：
  - `POST /acp/rpc`
  - `GET /acp`
- provider 与 gateway 只是 bridge-owned routing metadata，不是 APP 顶层模块
- runtime truth source 尽量 singular：
  - provider catalog 以内建 bridge contract 为准
  - gateway provider 以内建 production routing 为准
- config-center UI 不是 runtime readiness 的前置真相；当 bridge/runtime contract 已能自解析时，不应引入第二套入口或第二套依赖来源

## 6. 代码定位

建议结合以下文件阅读本设计：

- [main.go](../../main.go)
- [internal/acp/server.go](../../internal/acp/server.go)
- [internal/acp/routing.go](../../internal/acp/routing.go)
- [internal/acp/execution.go](../../internal/acp/execution.go)
- [internal/acp/gateway_runtime.go](../../internal/acp/gateway_runtime.go)
- [internal/acp/provider_catalog.go](../../internal/acp/provider_catalog.go)
- [internal/gatewayruntime/runtime.go](../../internal/gatewayruntime/runtime.go)
- [internal/router/router.go](../../internal/router/router.go)
- [internal/toolbridge/runner.go](../../internal/toolbridge/runner.go)
