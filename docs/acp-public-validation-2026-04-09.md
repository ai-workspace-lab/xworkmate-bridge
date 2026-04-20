# ACP Public Validation - 2026-04-09

This document records the post-deployment validation of the bridge public
origin at `xworkmate-bridge.svc.plus` and the independent upstream ACP ingress
at `xworkmate-bridge.svc.plus/acp-server`.

For APP integration, the canonical public contract remains the bridge origin
and the `.../acp/rpc` path on that origin. The direct `xworkmate-bridge.svc.plus/acp-server`
URLs in this document are upstream validation targets, not the preferred APP
entry points.

## Verified Public Endpoints

### Bridge root

- URL: `https://xworkmate-bridge.svc.plus/`
- Result: `200 OK`
- Body: `xworkmate-bridge is running`

### ACP public ingress

The public ACP JSON-RPC endpoint is the `.../acp/rpc` path.

Do not send JSON-RPC requests to `.../acp` for HTTP clients.

Recommended APP-facing endpoint:

- `https://xworkmate-bridge.svc.plus/acp/rpc`

Verified public HTTP JSON-RPC endpoints:

- Codex: `https://xworkmate-bridge.svc.plus/acp-server/codex/acp/rpc`
- OpenCode: `https://xworkmate-bridge.svc.plus/acp-server/opencode/acp/rpc`
- Gemini: `https://xworkmate-bridge.svc.plus/acp-server/gemini/acp/rpc`
- OpenClaw: `https://xworkmate-bridge.svc.plus/gateway/openclaw/`

The `.../acp` path remains reserved for WebSocket ACP.

## Auth Contract

All verified public ACP HTTP requests used:

- header: `Authorization: Bearer <INTERNAL_SERVICE_TOKEN>`
- header: `Content-Type: application/json`

Missing bearer auth returns a JSON-RPC error envelope with code `-32001`.

## Public Validation Results

The ingress returned `200 OK` on all public routes after re-apply, and the deployment response confirmed the active upstream mappings:

- `codex` -> `127.0.0.1:9010`
- `opencode` -> `127.0.0.1:3910`
- `gemini` -> `127.0.0.1:8791`
- `openclaw` -> `127.0.0.1:18789` (Host process)

### Codex

Verified `acp.capabilities` over the public ingress:

```json
{
  "method": "acp.capabilities",
  "result": {
    "providers": ["codex", "gemini", "opencode"],
    "singleAgent": true,
    "multiAgent": true
  }
}
```

Verified end-to-end task execution over the public ingress.

Observed conversation behavior:

- `session.start` succeeded and returned `round1`
- `session.message` also succeeded and returned `round2`
- `session.message` must include the same `routing` payload as `session.start`
- omitting `routing` returns `ROUTING_REQUIRED`

### OpenCode

Verified `acp.capabilities` over the public ingress:

```json
{
  "method": "acp.capabilities",
  "result": {
    "providers": ["opencode"],
    "singleAgent": true,
    "multiAgent": true
  }
}
```

Verified `session.start` end to end with prompt `Reply with exactly pong`.

Observed result:

```json
{
  "success": true,
  "provider": "opencode",
  "output": "pong"
}
```

Observed conversation behavior:

- `session.start` succeeded and returned `round1`
- `session.message` also succeeded and returned `round2`
- `session.message` must include the same `routing` payload as `session.start`
- omitting `routing` returns `ROUTING_REQUIRED`

### Gemini

Verified `acp.capabilities` over the public ingress:

```json
{
  "method": "acp.capabilities",
  "result": {
    "providers": ["gemini"],
    "singleAgent": true,
    "multiAgent": false
  }
}
```

Before the compatibility layer landed, the upstream Gemini ACP returned:

```json
{
  "success": false,
  "error": "\"Method not found\": session.start"
}
```

The adapter has now been updated so `session.start` and `session.message` default to adapter-local prompt compatibility instead of forwarding unsupported upstream methods.

Observed conversation behavior after re-apply:

- `session.start` succeeded and returned `round1`
- `session.message` succeeded and returned `round2`
- long conversation validation passed through the public ingress

## Long Conversation Validation

All three public ACP agent entries now pass a two-turn conversation check:

1. `session.start`
2. `session.message`

Verified result summary:

- `codex` long conversation passed
- `opencode` long conversation passed
- `gemini` long conversation passed

This confirms the upstream ACP baseline. The APP-facing baseline remains
`https://xworkmate-bridge.svc.plus/acp/rpc`.

## App Integration Notes

### Recommended request shape

For APP integration, use JSON-RPC `POST` requests against
`https://xworkmate-bridge.svc.plus/acp/rpc`.

For capability discovery:

```json
{
  "jsonrpc": "2.0",
  "id": "cap-1",
  "method": "acp.capabilities"
}
```

For single-agent task execution:

```json
{
  "jsonrpc": "2.0",
  "id": "task-1",
  "method": "session.start",
  "params": {
    "sessionId": "session-1",
    "threadId": "thread-1",
    "taskPrompt": "Reply with exactly pong",
    "workingDirectory": "/tmp",
    "routing": {
      "routingMode": "explicit",
      "explicitExecutionTarget": "singleAgent",
      "explicitProviderId": "opencode"
    }
  }
}
```

### Provider-specific notes

- `codex`, `opencode`, and `gemini` are all now verified public task paths.
- `gemini` still depends on the adapter compatibility layer, not a native upstream Gemini ACP conversation method.
- For multi-turn flows, apps should preserve and resend `routing` on every `session.message`.
- `codex` and `opencode` currently require explicit `routing` on follow-up turns.
