# Bridge Runtime Design

本文档描述 `xworkmate-bridge` 当前的收敛后运行模型。

## 1. 角色定义

`xworkmate-bridge` 被明确限定为：

**APP-facing ACP control plane and provider compatibility layer**

它不是：

- 通用 reverse proxy
- ingress gateway
- stdio runtime 本体

## 2. 三层边界

### Bridge Core

bridge core 保留：

- `acp.capabilities`
- `xworkmate.routing.resolve`
- `session.start`
- `session.message`
- `session.cancel`
- `session.close`
- `xworkmate.gateway.connect`
- `xworkmate.gateway.request`
- `xworkmate.gateway.disconnect`

### Provider Compatibility / Adapter Runtime

adapter/runtime 保留：

- codex compat
- opencode compat
- gemini compat
- hermes compat
- JSON-RPC over stdio
- process lifecycle / restart / stderr
- provider-specific framing / upstream session mapping

### Outer Gateway

APISIX / Caddy 这类外层网关负责：

- TLS
- ingress
- path routing
- rate limit
- audit / access logging

## 3. Transport 选择

bridge 当前采用：

- App-facing canonical HTTP transport: `POST /acp/rpc`
- ACP WebSocket transport variant: `GET /acp`

设计含义：

- HTTP RPC 是 App 运行时主链
- WS 作为 ACP transport variant，不反向塑造 App 路由
- provider / gateway alias route 不再属于 public surface

## 4. 模块边界

### `bridge_contract`

负责：

- JSON-RPC request / response envelope
- hybrid mode shaping
- common error mapping
- `session.update` notification envelope

### `capability_catalog`

负责：

- `providerCatalog`
- `gatewayProviders`
- `availableExecutionTargets`
- `providerProbeSummary`

### `routing_engine`

负责：

- routing normalize
- execution target resolve
- provider selection
- unavailable shaping

### `session_orchestrator`

负责：

- session state
- turn state
- history
- notification dispatch
- normalized output

### `provider_compat`

负责：

- codex compat
- opencode compat
- gemini compat
- hermes compat

### `gateway compat`

负责：

- openclaw connect / request / disconnect

## 5. 数据流

1. app 通过 `/acp` 或 `/acp/rpc` 发送 JSON-RPC request
2. bridge contract decode request
3. `acp.capabilities` 从 catalog 读取 bridge-owned truth
4. `xworkmate.routing.resolve` 由 routing engine 计算
5. `session.*` 进入 orchestrator
6. orchestrator 将 provider 差异分发到 compat 层
7. compat 与实际 adapter/runtime 通信
8. bridge 统一归一化结果，并通过 `session.update` 发通知

## 6. 当前约束

- app 不应感知 provider-specific upstream URL
- app 不应感知本地端口或 service 名
- routing 逻辑不得散落到各 provider handler
- session 语义不得被 provider-specific 分支污染
- 新增 JSON-RPC over stdio provider 时，应只增加 compat/runtime，不改 bridge core
