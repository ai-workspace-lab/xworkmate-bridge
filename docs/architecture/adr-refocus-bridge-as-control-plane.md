# ADR: Refocus xworkmate-bridge as ACP Control Plane

## Status

Accepted

## Context

`xworkmate-bridge` 历史上混入了三类职责：

- app-facing ACP control plane
- provider/gateway alias route
- stdio/process/runtime implementation detail

这些职责混在一起后，bridge 容易退化成：

- 通用 reverse proxy
- ingress path router
- adapter runtime 容器

结果是：

- provider catalog 真源不清晰
- routing 逻辑容易散落
- session 主语义被 provider-specific 分支污染
- app 容易误依赖内部 URL / 端口 / service 名

## Decision

我们将 `xworkmate-bridge` 收敛为：

**APP-facing ACP control plane and provider compatibility layer**

### 1. bridge 不是 just proxy

bridge 的核心资产不是纯转发，而是：

- `acp.capabilities`
- `xworkmate.routing.resolve`
- `session orchestration`
- `xworkmate.gateway.*`

因此 bridge 不再以 alias route 作为 public surface。

### 2. provider catalog 必须是 bridge-owned truth

app 只能通过 `acp.capabilities` 获取：

- `providerCatalog`
- `gatewayProviders`
- `availableExecutionTargets`

app 不应依赖：

- `/acp-server/*`
- `/gateway/openclaw` 作为全局 ACP base endpoint
- 本地端口
- systemd service 名

### 3. stdio runtime 必须留在 bridge core 之外

以下内容不是 bridge core：

- JSON-RPC over stdio framing
- process lifecycle
- restart / stderr
- provider-specific upstream session 映射

这些细节属于 adapter runtime / provider compat。

### 4. routing.resolve 与 session orchestration 是核心资产

bridge 继续保留：

- 独立 routing engine
- 统一 session orchestrator

provider-specific 逻辑只能存在于 compat layer，不得污染公共 handler。

### 5. App-facing WebSocket RPC

App-facing canonical transport 定义为：

- `GET /acp` WebSocket upgrade

`POST /acp/rpc` 作为 HTTP JSON-RPC fallback 保留，用于 CI、脚本、调试和兼容场景，不主导 App 运行时路由。

## Consequences

### 正向结果

- public contract 更清晰
- provider 扩展更便宜
- app 不再依赖内部部署事实
- bridge core 与 adapter runtime 职责清晰

### 有意删除

- `/acp-server/*` public alias
- `/gateway/openclaw` 作为 provider/gateway public alias 或全局 ACP base
- multi-agent 作为 bridge core 路径
- 以 reverse proxy 为中心的 bridge 定位

保留 `/gateway/openclaw` 的精确定义：它只是 OpenClaw `session.start` 与 follow-up
`session.message` 的 task submit endpoint，capabilities、routing、cancel、close 和
gateway control-plane method 继续走 `/acp` 或 `/acp/rpc`。

### 后续规则

新增 JSON-RPC over stdio provider 时：

- 只增加 compat/runtime
- 不改 bridge core contract
- 不把 provider-specific URL 暴露给 app
