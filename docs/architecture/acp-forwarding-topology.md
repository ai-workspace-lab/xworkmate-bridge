# ACP Forwarding Topology

This document describes the bridge-only production forwarding model for `xworkmate-bridge.svc.plus`.

See also: [adr-unified-bridge-entrypoints.md](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/docs/architecture/adr-unified-bridge-entrypoints.md)

## Topology

```mermaid
flowchart TD
  U["xworkmate-app"] --> B["https://xworkmate-bridge.svc.plus"]

  B -->|POST /acp/rpc| RPC["ACP RPC handler"]
  B -->|WS /acp| WS["ACP WebSocket handler"]

  RPC --> R{"method"}
  WS --> R

  R -->|acp.capabilities| CAP["built-in provider catalog"]
  R -->|xworkmate.routing.resolve| ROUTE["bridge-owned routing resolve"]
  R -->|session.start / session.message| RUN["bridge-owned execution"]
  R -->|xworkmate.gateway.*| GWAPI["gateway runtime proxy"]
  R -->|session.cancel / session.close| LIFE["session lifecycle"]

  RUN --> ACP1["codex -> https://acp-server.svc.plus/codex/acp/rpc"]
  RUN --> ACP2["opencode -> https://acp-server.svc.plus/opencode/acp/rpc"]
  RUN --> ACP3["gemini -> https://acp-server.svc.plus/gemini/acp/rpc"]

  GWAPI --> GW["wss://openclaw.svc.plus"]
```

## Three-Layer View

This view separates what the app sees, what the bridge owns, and what the
real upstream production targets are. The upstream ACP and gateway services
exist independently, but for the app they are all accessed through the single
public bridge origin: `https://xworkmate-bridge.svc.plus`.

```mermaid
flowchart LR
    subgraph L1["APP 视角"]
        APP["xworkmate-app"]
        APPENTRY["https://xworkmate-bridge.svc.plus<br/>统一代理入口"]
        APPMETHODS["bridge methods<br/>acp.capabilities / session.* / xworkmate.gateway.*"]
        APP --> APPENTRY
        APPENTRY --> APPMETHODS
    end

    subgraph L2["Bridge 视角"]
        BRIDGE["xworkmate-bridge<br/>唯一上游发现真源"]

        CAP["Bridge-owned target-scoped provider catalog"]
        CAP1["codex"]
        CAP2["opencode"]
        CAP3["gemini"]

        GW["Bridge-owned gateway routing"]
        GW1["gatewayProviderId=openclaw"]

        BRIDGE --> CAP
        CAP --> CAP1
        CAP --> CAP2
        CAP --> CAP3

        BRIDGE --> GW
        GW --> GW1
    end

    subgraph L3["上游视角"]
        U1["https://acp-server.svc.plus/codex/acp/rpc"]
        U2["https://acp-server.svc.plus/opencode/acp/rpc"]
        U3["https://acp-server.svc.plus/gemini/acp/rpc"]
        U4["wss://openclaw.svc.plus<br/>reported as openclaw.svc.plus:443"]
    end

    APPMETHODS --> BRIDGE

    CAP1 --> U1
    CAP2 --> U2
    CAP3 --> U3
    GW1 --> U4
```

Important distinction:

- the upstream services are independent production services, not embedded
  inside the bridge
- for the app, ACP discovery, session execution, and gateway runtime traffic
  are all proxied through `https://xworkmate-bridge.svc.plus`
- upstream authentication is unified through
  `Authorization: Bearer $INTERNAL_SERVICE_TOKEN`
- `acp.capabilities` is the single APP-facing source for task dialog modes and
  target-scoped provider catalogs
- `providerCatalog` currently advertises the ACP single-agent providers:
  `codex`, `opencode`, and `gemini`
- `gatewayProviders` currently advertises the gateway-scoped providers, such as
  `openclaw`
- `availableExecutionTargets` tells the app which first-level task dialog modes
  are currently available
- for `gatewayProviderId=openclaw`, the bridge rewrites the upstream target to
  `wss://openclaw.svc.plus`

## Production Truth

The production upstream services exist independently. The bridge owns the
routing map used to proxy app traffic to them:

- `codex` -> `https://acp-server.svc.plus/codex/acp/rpc`
- `opencode` -> `https://acp-server.svc.plus/opencode/acp/rpc`
- `gemini` -> `https://acp-server.svc.plus/gemini/acp/rpc`
- gateway -> `wss://openclaw.svc.plus`

Upstream auth is unified and bridge-internal:

- `Authorization: Bearer $INTERNAL_SERVICE_TOKEN`

Canonical APP-facing paths stay on the bridge origin:

- `POST https://xworkmate-bridge.svc.plus/acp/rpc`
- `GET  https://xworkmate-bridge.svc.plus/acp`

## Invariants

- app-facing cloud entry is only `https://xworkmate-bridge.svc.plus`
- app traffic reaches upstream ACP and gateway services only through the
  bridge proxy
- upstream ACP and gateway routes use the same bearer token contract:
  `Authorization: Bearer $INTERNAL_SERVICE_TOKEN`
- `acp.capabilities` returns the built-in production catalog
- no production `xworkmate.providers.sync`
- no app direct call to `acp-server.svc.plus/*`
- no app direct call to `openclaw.svc.plus`
- openclaw gateway runtime status is reported as `openclaw.svc.plus:443`, but the app still talks only to the bridge
