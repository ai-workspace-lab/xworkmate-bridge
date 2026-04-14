# API Interface Reference

This document organizes the current API surface of `xworkmate-bridge` based on the actual code paths in this repository.

It focuses on:

- the ACP Bridge exposed by `xworkmate-go-core serve`
- the Gemini ACP adapter exposed by `xworkmate-go-core gemini-acp-adapter`
- auxiliary HTTP handlers that exist in code but are not currently mounted by `main.go`

## 1. Runtime Entry Points

The binary currently exposes these runtime modes:

```bash
./build/bin/xworkmate-go-core serve
./build/bin/xworkmate-go-core acp-stdio
./build/bin/xworkmate-go-core gemini-acp-adapter
```

Relevant source:

- [main.go](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/main.go)
- [internal/acp/server.go](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/internal/acp/server.go)
- [internal/geminiadapter/server.go](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/internal/geminiadapter/server.go)

## 2. ACP Bridge HTTP / WebSocket API

Canonical APP-facing public origin:

- `https://xworkmate-bridge.svc.plus`

Canonical APP-facing ACP paths:

- `POST /acp/rpc`
- `GET /acp` for WebSocket ACP

Independent upstream ACP and gateway services may exist behind the bridge, but
they are not the primary APP contract.

### 2.1 Default listen address

`serve` mode reads:

- `ACP_LISTEN_ADDR`, default `127.0.0.1:8787`

### 2.2 Exposed routes

When running `xworkmate-go-core serve`, only these two routes are mounted:

| Path | Protocol | Purpose |
| --- | --- | --- |
| `/acp/rpc` | HTTP POST | JSON-RPC entrypoint |
| `/acp` | WebSocket | ACP over WebSocket |

Any other path returns `404`.

### 2.3 Auth and CORS

Bridge auth is bearer-token based:

- Environment variable: `ACP_AUTH_TOKEN`
- Required header: `Authorization: Bearer <token>`

Origin allowlist comes from:

- `ACP_ALLOWED_ORIGINS`
- default: `https://xworkmate.svc.plus,http://localhost:*,http://127.0.0.1:*`

Behavior summary:

- missing or invalid bearer token: HTTP `401`, JSON-RPC error code `-32001`
- disallowed origin: HTTP `403`, JSON-RPC error code `-32003`
- `OPTIONS /acp/rpc` is supported for CORS preflight

Relevant source:

- [internal/acp/server.go](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/internal/acp/server.go)
- [internal/acp/web_contract.go](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/internal/acp/web_contract.go)

### 2.4 Content negotiation

For `POST /acp/rpc`:

- normal JSON-RPC response: `Content-Type: application/json`
- if `Accept` contains `text/event-stream`, the server streams notifications as SSE and sends the final JSON-RPC result through SSE as well

### 2.5 JSON-RPC methods exposed by the bridge

The bridge currently handles these methods:

| Method | Description |
| --- | --- |
| `acp.capabilities` | Return current bridge capability summary |
| `session.start` | Start a task session |
| `session.message` | Continue an existing session |
| `session.cancel` | Cancel an active session |
| `session.close` | Close session state |
| `xworkmate.dispatch.resolve` | Resolve provider choice from candidate providers and requirements |
| `xworkmate.routing.resolve` | Resolve execution target / provider / skills from routing metadata |
| `xworkmate.mounts.reconcile` | Reconcile managed MCP configuration and mount-related settings |
| `xworkmate.gateway.connect` | Connect bridge runtime to gateway |
| `xworkmate.gateway.request` | Send a request to the connected gateway runtime |
| `xworkmate.gateway.disconnect` | Disconnect gateway runtime |

Unknown methods return JSON-RPC error code `-32601`.

## 3. Bridge Method Details

### 3.1 `acp.capabilities`

Request:

```json
{
  "jsonrpc": "2.0",
  "id": "cap-1",
  "method": "acp.capabilities"
}
```

Response shape:

```json
{
  "jsonrpc": "2.0",
  "id": "cap-1",
  "result": {
    "singleAgent": true,
    "multiAgent": true,
    "availableExecutionTargets": ["agent", "gateway"],
    "providerCatalog": [
      { "providerId": "codex", "label": "Codex", "targets": ["agent"] },
      { "providerId": "opencode", "label": "OpenCode", "targets": ["agent"] },
      { "providerId": "gemini", "label": "Gemini", "targets": ["agent"] }
    ],
    "gatewayProviders": [
      { "providerId": "local", "label": "Local", "targets": ["gateway"] },
      {
        "providerId": "openclaw",
        "label": "OpenClaw",
        "targets": ["gateway"],
        "providerDisplay": { "logoEmoji": "🦞" }
      }
    ],
    "capabilities": {
      "single_agent": true,
      "multi_agent": true,
      "availableExecutionTargets": ["agent", "gateway"],
      "providerCatalog": [
        { "providerId": "codex", "label": "Codex", "targets": ["agent"] },
        { "providerId": "opencode", "label": "OpenCode", "targets": ["agent"] },
        { "providerId": "gemini", "label": "Gemini", "targets": ["agent"] }
      ],
      "gatewayProviders": [
        { "providerId": "local", "label": "Local", "targets": ["gateway"] },
        {
          "providerId": "openclaw",
          "label": "OpenClaw",
          "targets": ["gateway"],
          "providerDisplay": { "logoEmoji": "🦞" }
        }
      ]
    }
  }
}
```

Notes:

- `availableExecutionTargets` is the APP-facing task dialog mode list
- `providerCatalog` is the APP-facing agent provider catalog
- `gatewayProviders` is the APP-facing gateway provider catalog
- provider entries may include target metadata and optional display metadata such as `providerDisplay.logoEmoji`
- production upstream routing map is fixed to:
  - `codex` -> `https://acp-server.svc.plus/codex/acp/rpc`
  - `opencode` -> `https://acp-server.svc.plus/opencode/acp/rpc`
  - `gemini` -> `https://acp-server.svc.plus/gemini/acp/rpc`
- APP traffic reaches those upstreams through the bridge's canonical public
  ACP path, not by depending on upstream URLs directly
- upstream ACP auth uses `Authorization: Bearer $INTERNAL_SERVICE_TOKEN`
- `multiAgent` is controlled by `ACP_MULTI_AGENT_ENABLED`, default `true`

### 3.2 `session.start`

Starts a session and resets any existing state with the same `sessionId`.

Minimum required params:

```json
{
  "sessionId": "session-1"
}
```

Typical request:

```json
{
  "jsonrpc": "2.0",
  "id": "task-1",
  "method": "session.start",
  "params": {
    "sessionId": "session-1",
    "threadId": "thread-1",
    "taskPrompt": "Reply with exactly pong",
    "workingDirectory": "/tmp/demo",
    "routing": {
      "routingMode": "explicit",
      "explicitExecutionTarget": "singleAgent",
      "explicitProviderId": "opencode"
    }
  }
}
```

Rules:

- `sessionId` is required, otherwise `-32602`
- `threadId` defaults to `sessionId`
- work is queued per `threadId`
- for public single-agent multi-turn usage, `routing` should be resent on later turns

### 3.3 `session.message`

Continues an existing session.

Minimum required params:

```json
{
  "sessionId": "session-1"
}
```

Typical request:

```json
{
  "jsonrpc": "2.0",
  "id": "task-2",
  "method": "session.message",
  "params": {
    "sessionId": "session-1",
    "threadId": "thread-1",
    "taskPrompt": "Continue the previous task",
    "workingDirectory": "/tmp/demo",
    "routing": {
      "routingMode": "explicit",
      "explicitExecutionTarget": "singleAgent",
      "explicitProviderId": "opencode"
    }
  }
}
```

### 3.4 `session.cancel`

Request:

```json
{
  "jsonrpc": "2.0",
  "id": "cancel-1",
  "method": "session.cancel",
  "params": {
    "sessionId": "session-1"
  }
}
```

Response:

```json
{
  "accepted": true,
  "cancelled": true
}
```

Notes:

- `accepted` indicates the request envelope was accepted
- `cancelled` is `true` only when an active session was actually cancelled
- if the session does not exist or is already finished, `cancelled` may be
  `false`

### 3.5 `session.close`

Request:

```json
{
  "jsonrpc": "2.0",
  "id": "close-1",
  "method": "session.close",
  "params": {
    "sessionId": "session-1"
  }
}
```

Response:

```json
{
  "accepted": true,
  "closed": true
}
```

Notes:

- `accepted` indicates the request envelope was accepted
- `closed` is `true` only when an existing session state was actually closed
- if the session does not exist or is already gone, `closed` may be `false`

### 3.6 `xworkmate.dispatch.resolve`

Purpose:

- choose a provider from a list of candidates
- apply preferred provider and capability constraints
- optionally consider node state and node info

Key params:

- `providers`: array of provider definitions
- `preferredProviderId`
- `requiredCapabilities`
- `nodeState`
- `nodeInfo`

Provider item shape:

```json
{
  "id": "codex",
  "name": "Codex",
  "defaultArgs": ["app-server"],
  "capabilities": ["singleAgent", "tools"]
}
```

### 3.7 `xworkmate.routing.resolve`

Purpose:

- resolve routing metadata into:
  - execution target
  - provider
  - gateway provider
  - model
  - selected skills
  - install suggestion / unavailable state

Canonical use:

- apps should use this method as the single preflight source for:
  - effective execution target
  - effective provider selection
  - unavailable code / message
- apps should not re-derive provider availability or `auto` resolution from
  `acp.capabilities`

Key input fields:

- `taskPrompt`
- `workingDirectory`
- `routing.routingMode`
- `routing.preferredGatewayProviderId`
- `routing.explicitExecutionTarget`
- `routing.explicitProviderId`
- `routing.explicitModel`
- `routing.explicitSkills`
- `routing.allowSkillInstall`
- `routing.installApproval`
- `routing.availableSkills`
- `aiGatewayBaseUrl`
- `aiGatewayApiKey`

Representative response fields:

- `resolvedExecutionTarget`
- `resolvedProviderId`
- `resolvedGatewayProviderId`
- `resolvedModel`
- `resolvedSkills`
- `skillResolutionSource`
- `needsSkillInstall`
- `unavailable`
- `unavailableCode`
- `unavailableMessage`
- `skillInstallRequestId`
- `skillCandidates`
- `memorySources`

APP-facing interpretation:

- if `resolvedExecutionTarget = single-agent`, use `resolvedProviderId`
- if `resolvedExecutionTarget = gateway`, use `resolvedGatewayProviderId`

### 3.7.1 UI / APP consumption model

Recommended APP-side flow:

1. Call `acp.capabilities` at startup or refresh time.
2. Read:
   - `availableExecutionTargets`
   - `providerCatalog` as the `agent` provider catalog
   - `gatewayProviders` as `gatewayProviders`
3. Before execution, call `xworkmate.routing.resolve`.
4. Use the resolved fields, not local heuristics, to decide which UI state and
   execution branch to use.
5. Send `session.start` or `session.message` through the bridge canonical path.

Suggested APP-side view model:

```json
{
  "executionTargets": ["agent", "gateway"],
  "agentProviders": ["codex", "opencode", "gemini"],
  "gatewayProviders": ["local", "openclaw"]
}
```

Recommended interpretation rules:

- if `resolvedExecutionTarget = single-agent`
  - read `resolvedProviderId`
  - ignore `resolvedGatewayProviderId`
- if `resolvedExecutionTarget = multi-agent`
  - treat the run as bridge-orchestrated multi-agent execution
  - do not force a single provider picker as the primary routing control
- if `resolvedExecutionTarget = gateway`
  - read `resolvedGatewayProviderId`
  - ignore `resolvedProviderId`

UI binding guidance:

- provider picker for `agent` mode should be populated from `providerCatalog`
- gateway picker should be populated from `gatewayProviders`
- task dialog mode visibility should be populated from
  `availableExecutionTargets`
- disabled or unavailable states should come from `xworkmate.routing.resolve`
  response fields such as:
  - `unavailable`
  - `unavailableCode`
  - `unavailableMessage`

Do not:

- infer provider availability from hardcoded lists
- treat `openclaw` as a single-agent provider
- re-derive auto-routing from prompt text on the APP side
- use upstream URLs as the APP-facing contract

### 3.8 `xworkmate.mounts.reconcile`

Purpose:

- reconcile managed MCP server config for supported local tools and homes

Key params:

- `config.autoSync`
- `config.usesAris`
- `config.managedMcpServers`
- `aiGatewayUrl`
- `configuredCodexCliPath`
- `codexHome`
- `opencodeHome`
- `openclawHome`
- `aris`

Managed MCP server item shape:

```json
{
  "id": "xworkmate_server",
  "command": "xworkmate-mcp",
  "args": ["--port", "7777"],
  "enabled": true
}
```

### 3.9 Gateway runtime methods

#### `xworkmate.gateway.connect`

Purpose:

- connect a bridge runtime session to the bridge-owned production gateway route
- from the APP perspective this still enters through the canonical bridge ACP
  path and bridge JSON-RPC methods, not a direct `openclaw` URL contract

Key params:

- `runtimeId`
- `gatewayProviderId`
- `clientId`
- `locale`
- `userAgent`
- `connectAuthMode`
- `connectAuthFields`
- `connectAuthSources`
- `hasSharedAuth`
- `hasDeviceToken`
- `endpoint.host`
- `endpoint.port`
- `endpoint.tls`
- `packageInfo`
- `deviceInfo`
- `identity`
- `auth`

Response fields:

- `ok`
- `snapshot`
- `auth`
- `returnedDeviceToken`
- `error`

Notes:

- for `gatewayProviderId=openclaw`, the bridge overrides runtime endpoint
  selection to `wss://openclaw.svc.plus`
- for `gatewayProviderId=local`, the bridge keeps the caller-provided local
  gateway endpoint configuration
- upstream gateway auth uses `Authorization: Bearer $INTERNAL_SERVICE_TOKEN`
- the app does not provide production openclaw endpoint truth

#### `xworkmate.gateway.request`

Purpose:

- send a gateway runtime RPC-like request through an existing runtime connection

Params:

- `runtimeId`
- `method`
- `params`
- `timeoutMs`

Response fields:

- `ok`
- `payload`
- `error`

#### `xworkmate.gateway.disconnect`

Params:

- `runtimeId`

Response:

```json
{
  "accepted": true
}
```

## 4. WebSocket Contract

`/acp` accepts the same JSON-RPC method set as `/acp/rpc`.

Differences from HTTP:

- each inbound message is a JSON-RPC request frame
- notifications are written back as WebSocket JSON messages
- result envelopes and error envelopes are also written as WebSocket JSON messages

Observed bridge behavior on the public deployment:

- authenticated plain `GET /acp` without WebSocket upgrade headers returns HTTP
  `400 Bad Request`
- authenticated `GET /acp` with valid WebSocket upgrade headers returns HTTP
  `101 Switching Protocols`

## 5. Gemini ACP Adapter API

### 5.1 Default listen address

`gemini-acp-adapter` reads:

- `GEMINI_ADAPTER_LISTEN_ADDR`, default `127.0.0.1:8791`

### 5.2 Exposed routes

When running `xworkmate-go-core gemini-acp-adapter`, only these routes are mounted:

| Path | Protocol | Purpose |
| --- | --- | --- |
| `/acp/rpc` | HTTP POST | Adapter JSON-RPC entrypoint |
| `/acp` | WebSocket | Adapter WebSocket JSON-RPC entrypoint |

### 5.3 Auth and CORS

Adapter auth is separate from bridge auth:

- Environment variable: `GEMINI_ADAPTER_AUTH_TOKEN`
- Required header: `Authorization: Bearer <token>`

Allowed origins come from:

- `GEMINI_ADAPTER_ALLOWED_ORIGINS`
- default: `https://xworkmate.svc.plus,http://localhost:*,http://127.0.0.1:*`

### 5.4 Adapter JSON-RPC methods

| Method | Description |
| --- | --- |
| `acp.capabilities` | Synthesized Gemini single-agent capability response |
| `session.start` | Start adapter-local Gemini session |
| `session.message` | Continue adapter-local Gemini session |
| `session.cancel` | Accept cancel request, currently returns `cancelled: false` |
| `session.close` | Close adapter-local session |
| `gemini.initialize` | Return upstream initialize result |
| `gemini.raw` | Forward raw Gemini-facing payload handling |

Unsupported methods return:

```json
{
  "success": false,
  "error": "unsupported method: <method>"
}
```

### 5.5 Core adapter env vars

- `GEMINI_ADAPTER_LISTEN_ADDR`
- `GEMINI_ADAPTER_BIN`
- `GEMINI_ADAPTER_ARGS`
- `GEMINI_ADAPTER_PROTOCOL_VERSION`
- `GEMINI_ADAPTER_AUTH_TOKEN`
- `GEMINI_ADAPTER_PROVIDER_ID`
- `GEMINI_ADAPTER_PROVIDER_LABEL`
- `GEMINI_ADAPTER_ALLOWED_ORIGINS`
- `GEMINI_ADAPTER_UPSTREAM_METHOD`
- `ACP_GEMINI_BIN`

## 6. Auxiliary HTTP Handlers Present in Code

The repository also contains two plain HTTP handlers:

- [internal/handler/auth_handler.go](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/internal/handler/auth_handler.go)
- [internal/handler/token_auth_handler.go](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/internal/handler/token_auth_handler.go)

These handlers are currently not mounted by `main.go`, so they are not part of the live binary HTTP routing surface unless another embedding program wires them in.

### 6.1 Username/password auth handler

Request body:

```json
{
  "username": "demo",
  "password": "secret"
}
```

Behavior:

- invalid JSON -> `400 invalid json`
- auth failure -> `401 <error message>`
- success -> `200 {"status":"ok"}`

### 6.2 Bearer token auth handler

Behavior:

- no service -> `503 service unavailable`
- invalid bearer token -> `401 unauthorized`
- success -> `200 {"ok":true}`

## 7. Public Deployment Notes

Existing project docs already record the current public ingress convention:

- Bridge-facing public ACP HTTP JSON-RPC path: `.../acp/rpc`
- WebSocket ACP path: `.../acp`

Reference docs:

- [docs/acp-public-validation-2026-04-09.md](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/docs/acp-public-validation-2026-04-09.md)
- [docs/gemini-acp-adapter.md](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/docs/gemini-acp-adapter.md)
- [docs/architecture/acp-forwarding-topology.md](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/docs/architecture/acp-forwarding-topology.md)

## 8. Suggested Maintenance Rule

If a new externally callable method is added to:

- [internal/acp/server.go](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/internal/acp/server.go)
- [internal/geminiadapter/server.go](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/internal/geminiadapter/server.go)

then this document should be updated in the same change.
