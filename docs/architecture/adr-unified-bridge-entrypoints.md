# ADR: Unified Bridge Entry Points for APP Traffic

## Status

Accepted

## Date

2026-04-11

## Last Reviewed

2026-04-13

## Context

`xworkmate-bridge` 代理 app 流量到独立 upstream ACP / gateway 服务，但如果把这些 upstream 直接暴露成 app-facing entrypoint，会带来几个问题：

- app 必须知道 provider-specific 或 gateway-specific hostname
- routing truth 会在 URL 形状与 bridge 逻辑之间分裂
- auth contract 更难保持统一
- upstream implementation detail 会泄漏到 app shell / module 心智

当前 app 主链已经收敛到 `assistant + settings` 两个 surface，因此 bridge 入口也必须保持同一口径：统一公共入口，bridge 内部负责 provider / gateway 路由细节。

## Decision

For APP traffic, the canonical public entry point remains:

- `https://xworkmate-bridge.svc.plus`

Canonical app-facing contract families are:

1. ACP control-plane
   - `POST /acp/rpc`
   - `GET /acp`
2. Gateway runtime methods
   - `xworkmate.gateway.connect`
   - `xworkmate.gateway.request`
   - `xworkmate.gateway.disconnect`

Bridge-owned metadata may still include:

- `providerCatalog`
- `gatewayProviders`
- `resolvedExecutionTarget`
- `resolvedProviderId`
- `resolvedGatewayProviderId`

这些字段属于 bridge 返回给 app 的 routing/capability metadata，不属于 app shell taxonomy。

换句话说：

- app 可以消费这些字段来展示当前可用能力或执行结果
- app 不应该把 provider/gateway 矩阵抬升成新的顶层模块、别名页面、或直接 URL 合同

The APP should not depend on provider-specific public URLs such as:

- `/codex/acp/rpc`
- `/opencode/acp/rpc`
- `/gemini/acp/rpc`
- `/openclaw/`

If the bridge reports execution-target metadata such as `single-agent`,
`multi-agent`, or `gateway`, the app should treat those values as routing
results, not as shell-level surface categories.

If the bridge reports gateway provider IDs such as `openclaw`, the
app should treat them as bridge-owned gateway backend identifiers, not as
independent app entrypoints.

## Consequences

### Positive

- APP integration stays stable behind one public origin
- provider and gateway topology remain bridge concerns
- auth handling remains consistent across ACP and gateway forwarding
- app architecture docs can stay focused on `assistant + settings` instead of a fake module matrix

### Trade-offs

- docs must clearly separate canonical app contracts from independent upstream services
- optional bridge metadata must be documented as metadata, not as surface taxonomy

## Path Naming Guidance

Use these terms consistently:

- `canonical app-facing path`: `/acp/rpc` and `/acp`
- `gateway runtime method family`: `xworkmate.gateway.*`
- `independent upstream service`: `acp-server.svc.plus/*`, `wss://openclaw.svc.plus`
- `bridge-owned routing`: provider / gateway selection performed inside bridge
- `routing metadata`: execution target and resolved provider/gateway identifiers returned to the app

Avoid describing upstream URLs, provider IDs, or gateway mode IDs as if they
were independent app modules or alternate primary entrypoints.
