# ACP Public Validation

Last Updated: 2026-04-23

> Historical validation note. Current APP-facing design is WebSocket-first and
> is defined in [Backend API Design](./backend-api-design.md). Do not use this
> older note as the source of truth for xworkmate-app routing.

本文件记录当前推荐的 public validation 方法。

## Canonical Validation Targets

只验证 bridge canonical surface：

- `GET https://xworkmate-bridge.svc.plus/api/ping`
- `GET wss://xworkmate-bridge.svc.plus/acp`
- `POST https://xworkmate-bridge.svc.plus/acp/rpc`
- `POST https://xworkmate-bridge.svc.plus/gateway/openclaw` for OpenClaw `session.start` / `session.message` only

不再把以下路径视为 public validation target：

- `/acp-server/*`

## Validation Checklist

### 1. Release Ping

验证：

- `status == ok`
- `image / tag / commit / version` 与发布镜像一致

### 2. Capabilities

通过 `/acp/rpc` 调 `acp.capabilities`，验证：

- `providerCatalog` 包含 `codex / opencode / gemini / hermes`
- `gatewayProviders` 包含 `openclaw`
- `availableExecutionTargets` 包含 `agent / gateway`

### 3. Routing Resolve

验证显式 single-agent：

- `resolvedExecutionTarget == single-agent`
- `resolvedProviderId == codex`

验证显式 gateway：

- `resolvedExecutionTarget == gateway`
- `resolvedGatewayProviderId == openclaw`

### 4. Session Contract

验证 `session.start` / `session.message`：

- 返回 JSON-RPC hybrid envelope
- 中间通知使用 `session.update`
- 最终结果包含 `turnId`

OpenClaw 的 `session.start` 与同一 task 的 follow-up `session.message` 使用
`/gateway/openclaw`。`session.cancel` 与 `session.close` 仍使用 `/acp` 或 `/acp/rpc`。

### 5. Gateway Contract

验证：

- `xworkmate.gateway.connect`
- `xworkmate.gateway.request`
- `xworkmate.gateway.disconnect`

## Notes

- App-facing canonical transport 是 `GET /acp` WebSocket upgrade
- `POST /acp/rpc` 仅作为 CI、脚本、调试和兼容 fallback
- app-facing contract 由 bridge control plane 独占，不由 provider alias path 定义
