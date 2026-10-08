![XWorkmate Bridge promotional poster](./assets/product.png)

[![License](https://img.shields.io/badge/license-not%20declared-6b7280.svg)](https://github.com/ai-workspace-lab/xworkmate-bridge) [![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8.svg?logo=go&logoColor=white)](https://go.dev/) [![Transport](https://img.shields.io/badge/transport-WebSocket%20%7C%20HTTP-1769aa.svg)](./docs/api-reference.md) [![ACP](https://img.shields.io/badge/protocol-ACP-0b7285.svg)](./docs/architecture/adr-unified-bridge-entrypoints.md)

# XWorkmate Bridge

`xworkmate-bridge` is the standalone repository for the XWorkmate ACP Bridge Server and the embedded Go helper previously stored under `xworkmate-app/go/go_core`.

## What lives here

- ACP Bridge HTTP/WebSocket server
- ACP stdio bridge entrypoint
- Go helper runtime packages used by the ACP bridge
- Unit tests for bridge routing, RPC contracts, mounts, runtime dispatch, and provider sync

## ACP Forwarding Topology

The default integration route is App → Bridge → OpenClaw Gateway. The APP-facing
canonical ACP path is WebSocket `/acp`; HTTP `/acp/rpc` supports task submission.
Bridge forwards product capability metadata and native Gateway requests; it does
not own worker execution, cron scheduling or multi-agent orchestration.

Chat / Work / Code retain one protocol and layout. Optional
`metadata.xworkmateProductCapability` v1 conveys mode and an explicit central
`xworkmate/<model-id>` reference. Work uses the Gateway's DSH ACP worker and Code
uses OpenCode v2. Both require the separately deployed opt-in Gateway plugin and
OS worker role. Bot uses OpenClaw's native cron API. See the
[session contract](./docs/api-reference.md#8-sessionstart--sessionmessage).

The new path has Go unit/fixture validation; real-model execution, runtime
isolation and mobile end-to-end acceptance require a configured Gateway and
separate deployment verification. Existing compatibility adapters are not proof
that these workers are installed or operational.

## Shared task sessions

Accounts owns durable cross-terminal task/session state. Bridge keeps the
client-compatible `/api/v1` surface as a stateless, bounded streaming proxy.
Set `BRIDGE_ACCOUNTS_SESSION_API_URL` to the Accounts origin (and optional base
path), for example `https://accounts.svc.plus`. Bridge appends the incoming
`/api/v1/...` path and query verbatim. If the variable is absent or invalid,
ACP forwarding remains available and task-session routes return `503`.

The session API exposes namespace listing, session create/list/snapshot,
ordered event replay, and idempotent message append under `/api/v1`. Every
route requires the existing Bearer credential. Bridge preserves its current
public authentication check, forwards `Authorization` to Accounts, and never
derives or accepts account identity from request data.

Bridge does not store, cache, migrate, or persist session data or artifacts.
Accounts owns snapshots, ordered events, message idempotency, and task-run
state. Scheduler/Accounts callbacks persist ACP execution results; Bridge does
not write execution state locally.

## Agent task context (two-way, via QMD)

Every client — ChatGPT / Claude web and mobile extensions and CLI / APP plugins
(Claude Code, Codex, Antigravity, OpenCode) — syncs shared agent task context
both ways through Bridge. QMD owns the schema and merge rules; Bridge
authenticates the caller, forwards allowlisted routes with QMD's own credential,
and stores nothing.

- Submit session facts: `POST /api/v1/agent/ingest`
- Read tasks, lists and shared memory: `GET /api/v1/agent/catalog`, `/threads`,
  `/threads/{id}/briefing`, `/memory`, `/sync` (paged by QMD)
- Remote mode MCP: `POST|GET|DELETE /api/v1/agent/mcp` passes MCP Streamable HTTP
  through to QMD's `/mcp`

Configuration: `BRIDGE_QMD_INGEST_API_URL` is the QMD HTTP origin (for example
`http://127.0.0.1:8181`); `BRIDGE_QMD_INGEST_TOKEN` must equal QMD's
`QMD_INGEST_TOKEN` (ingest and read routes); `BRIDGE_QMD_MCP_TOKEN` must equal
QMD's `QMD_MCP_TOKEN` (MCP passthrough). Missing configuration returns `503`.

Architecture topology: [docs/architecture/acp-forwarding-topology.md](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/docs/architecture/acp-forwarding-topology.md)

ADR for the unified APP-facing bridge contract: [docs/architecture/adr-unified-bridge-entrypoints.md](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/docs/architecture/adr-unified-bridge-entrypoints.md)

Example provider sync config: [example/config.yaml](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/example/config.yaml)

API reference: [docs/api-reference.md](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/docs/api-reference.md)

Backend API design: [docs/backend-api-design.md](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/docs/backend-api-design.md)

## Compatibility

For compatibility with `xworkmate-app`, the built helper binary name remains `xworkmate-go-core`.

## Commands

```bash
make test
make build
./build/bin/xworkmate-go-core serve --listen 127.0.0.1:8787
```

## GitHub Actions

This repository includes one GitHub Actions pipeline with four stages:

- `prep`: Go static checks
- `build`: build the `linux/amd64` artifact for the x86 target host and upload it
- `deploy`: run Ansible CD with `x-evor/playbooks`
- `validate`: verify the public endpoints after deployment

GitHub Releases are published only after `deploy` and `validate` both succeed.
In this repository, a published Release means the built image has been deployed
to `xworkmate-bridge.svc.plus` and passed post-deploy validation there.

### Deploy stage

The deploy stage checks out:

- this service repository into `xworkmate-bridge/`
- the `x-evor/playbooks` repository into `playbooks/`

Then it installs the native `linux/amd64` bridge binary with
`scripts/github-actions/deploy-native-binary.sh`. The native bridge runs as the
`ubuntu` user's systemd user service:

- binary: `/home/ubuntu/.local/bin/xworkmate-go-core`
- unit: `/home/ubuntu/.config/systemd/user/xworkmate-bridge.service`
- restart: `systemctl --user restart xworkmate-bridge.service`

During migration the script performs a one-time stop/disable of the old system
unit, then deploys and restarts through `ubuntu@<target>`.

### Validate stage

The validate stage proves production alignment against the bridge public
contract:

- bridge root and `/api/ping`
- strict image / tag / commit / version match against the built image ref
- upstream ACP capability probes for `codex`, `opencode`, and `gemini`
- minimal `session.start` smoke tests through the bridge JSON-RPC contract

Required GitHub secrets:

- `SINGLE_NODE_VPS_SSH_PRIVATE_KEY`: private key used by the Actions runner to SSH into the target host
- `WORKSPACE_REPO_TOKEN`: token with access to checkout `x-evor/playbooks`

Optional GitHub secrets:

- `SSH_KNOWN_HOSTS`: pre-seeded known_hosts content for stricter host verification

Optional workflow input:

- `ai_workspace_auth_token`: manual dispatch input that is forwarded as `AI_WORKSPACE_AUTH_TOKEN`

## Environment

- `ACP_LISTEN_ADDR`: listen address for `serve` mode, default `127.0.0.1:8787`
- `BRIDGE_ROLE_POLICY_PATH`: optional role policy JSON (see `example/role-router-policy.example.json` and [docs/architecture/role-routing-engineer-loop.md](docs/architecture/role-routing-engineer-loop.md)); unset means role routing returns `ROLE_POLICY_UNCONFIGURED`
- `BRIDGE_PERMISSION_TIMEOUT_SECONDS`: how long a role-routed task waits for the user's permission decision, default `600`; no decision denies
- `DEEPSEEK_HARNESS_RPC_URL` / `OPENCODE_ACP_RPC_URL`: WebSocket URLs of `xworkmate-go-core adapter acp-agent` in front of `dsh --profile acp` and `opencode acp`
- `OUTPUT_DIR`: optional output directory for `make build`
- `OUTPUT_PATH`: optional explicit build path for `make build`
