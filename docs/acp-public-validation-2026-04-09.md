# ACP Public Validation & Expansion Planning - 2026-04-09

This document records the post-deployment validation of the bridge public
origin at `xworkmate-bridge.svc.plus` and outlines the expansion architecture
for the independent upstream ACP adapters.

## Expansion Modes Planning

To support a diverse set of backend providers, the bridge operates in the following expansion modes:

| Mode ID | Adapter Role | Implementation Type |
| :--- | :--- | :--- |
| `acp-adapter-codex`    | Codex ACP Adapter    | Protocol Translator / Forwarder |
| `acp-adapter-opencode` | OpenCode ACP Adapter | JSON-RPC over stdio |
| `acp-adapter-gemini`   | Gemini ACP Adapter   | JSON-RPC over stdio |
| `acp-adapter-hermes`   | Hermes ACP Adapter   | JSON-RPC over stdio |
| `gateway-adapter-openclaw` | OpenClaw Gateway | Unified Protocol Entry |

## Protocol Entry Points (Public)

The canonical entry points for external integrations are segmented by provider:

*   **Codex**: `https://xworkmate-bridge.svc.plus/acp-server/codex`
*   **Gemini**: `https://xworkmate-bridge.svc.plus/acp-server/gemini`
*   **Hermes**: `https://xworkmate-bridge.svc.plus/acp-server/hermes`
*   **OpenCode**: `https://xworkmate-bridge.svc.plus/acp-server/opencode`
*   **OpenClaw**: `https://xworkmate-bridge.svc.plus/gateway/openclaw`

## Request Chain & Runtime Design

### Traffic Flow
`Caddy (Ingress)` -> `xworkmate-bridge (Dispatcher)` -> `Adapter Service`

Caddy handles SSL termination and forwards requests to the `xworkmate-bridge` process, which performs path-based routing to the respective local adapter services.

### Systemd Services & Local Mappings

Each adapter is managed as a standalone systemd service, mapped to a specific local port/protocol:

| Service Name | Local Endpoint | Adapter Target |
| :--- | :--- | :--- |
| `acp-codex.service`    | `ws://127.0.0.1:9001`  | Codex Engine |
| `acp-opencode.service` | `ws://127.0.0.1:38992` | OpenCode Runtime |
| `acp-gemini.service`   | `ws://127.0.0.1:8791`  | Gemini Bridge |
| `acp-hermes.service`   | `ws://127.0.0.1:3920`  | Hermes Engine |
| `(Host Process)`       | `ws://127.0.0.1:18789` | OpenClaw (Shared Runtime) |

## Auth Contract

All public ACP requests require:

-   header: `Authorization: Bearer <INTERNAL_SERVICE_TOKEN>`
-   header: `Content-Type: application/json`

Missing or invalid bearer auth returns a JSON-RPC error envelope with code `-32001`.

## Validation Results (2026-04-09)

The ingress returned `200 OK` on all public routes after re-apply.

### Codex
- Verified `acp.capabilities`: `["codex", "gemini", "opencode"]`
- Two-turn conversation (`session.start` -> `session.message`) passed.

### OpenCode
- Validated as WebSocket ACP upstream at `ws://127.0.0.1:38992/acp`.
- Two-turn conversation passed.

### Gemini
- Verified `acp.capabilities`: `["gemini"]`
- Adapter-local prompt compatibility layer enables `session.start` / `session.message` despite lack of native upstream support.
- Two-turn conversation passed.

## App Integration Notes

### Recommended request shape

Use JSON-RPC `POST` requests against `https://xworkmate-bridge.svc.plus/acp/rpc` for general usage, or the specific provider endpoints for targeted execution.

**Example Task Execution:**

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
- `codex` and `opencode` require explicit `routing` on follow-up turns.
- `gemini` uses a prompt-compatibility layer for multi-turn support.
- `hermes` is verified as a public task path.
