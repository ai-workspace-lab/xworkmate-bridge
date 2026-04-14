# ADR: Unified Bridge Entry Points for APP Traffic

## Status

Accepted

## Date

2026-04-11

## Context

`xworkmate-bridge` currently proxies app traffic to four independent upstream
production services:

- `codex` -> `https://acp-server.svc.plus/codex/acp/rpc`
- `opencode` -> `https://acp-server.svc.plus/opencode/acp/rpc`
- `gemini` -> `https://acp-server.svc.plus/gemini/acp/rpc`
- `gateway` -> `wss://openclaw.svc.plus`

These upstream services exist independently, but exposing them directly as
APP-facing endpoints creates several problems:

- the APP would need to know provider-specific or gateway-specific hostnames
- routing truth would be split between URL shape and bridge-side routing logic
- auth handling would be harder to keep consistent
- upstream implementation details would leak into client contracts

The bridge already acts as the single public integration surface for ACP
discovery, task execution, and gateway runtime operations.

## Decision

For APP traffic, the canonical public entry point is the bridge origin:

- `https://xworkmate-bridge.svc.plus`

The canonical APP-facing ACP paths are:

- `POST /acp/rpc`
- `GET /acp` for WebSocket ACP

The APP should not depend on provider-specific public URLs such as:

- `/codex/acp/rpc`
- `/opencode/acp/rpc`
- `/gemini/acp/rpc`
- `/openclaw/`

Provider choice remains bridge-owned routing, not URL-owned routing.

APP-facing routing should be modeled in two layers:

- `executionTarget`
  - `agent`
  - `gateway`
- `agentProviders`
  - `codex`
  - `opencode`
  - `gemini`
- `gatewayProviders`
  - `local`
  - `openclaw`

For APP integration, `gatewayProviders` is the stable gateway-facing concept.

APP and UI code should consume bridge state in two phases:

1. `acp.capabilities`
   - discover `availableExecutionTargets`
   - discover `agentProviders`
   - discover `gatewayProviders`
2. `xworkmate.routing.resolve`
   - determine `resolvedExecutionTarget`
   - determine `resolvedProviderId` or `resolvedGatewayProviderId`
   - determine unavailable state

The APP should treat `resolvedProviderId` and `resolvedGatewayProviderId` as
mutually exclusive routing outputs depending on `resolvedExecutionTarget`.

Gateway access remains bridge-owned via JSON-RPC methods:

- `xworkmate.gateway.connect`
- `xworkmate.gateway.request`
- `xworkmate.gateway.disconnect`

Upstream authentication is unified for both ACP and gateway routes:

- `Authorization: Bearer $INTERNAL_SERVICE_TOKEN`

## Consequences

### Positive

- APP integration stays stable behind one public origin
- provider and gateway topology remain internal bridge concerns
- auth contract is consistent across all upstream forwarding
- bridge can change upstream mappings without changing APP contracts

### Trade-offs

- direct provider-specific bridge URLs, if exposed at all, must be treated as
  aliases or operator/debug paths, not primary client contracts
- documentation must clearly distinguish canonical APP paths from independent
  upstream targets

## Path Naming Guidance

Use these terms consistently in docs:

- `canonical APP-facing path`: `/acp/rpc` and `/acp`
- `independent upstream service`: `acp-server.svc.plus/*` and
  `wss://openclaw.svc.plus`
- `bridge-owned routing`: bridge logic that selects and proxies to upstreams
- `gatewayProvider`: the APP-facing identifier for a gateway backend such as
  `local` or `openclaw`

Avoid describing upstream URLs as if the APP should call them directly.

If provider-specific public bridge paths are ever introduced, they should be
documented as optional aliases only. They should not replace `/acp/rpc` as the
canonical APP-facing contract.
