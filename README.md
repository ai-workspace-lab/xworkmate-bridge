# XWorkmate Bridge

`xworkmate-bridge` is the standalone repository for the XWorkmate ACP Bridge Server and the embedded Go helper previously stored under `xworkmate-app/go/go_core`.

## What lives here

- ACP Bridge HTTP/WebSocket server
- ACP stdio bridge entrypoint
- Go helper runtime packages used by the ACP bridge
- Unit tests for bridge routing, RPC contracts, mounts, runtime dispatch, and provider sync

## ACP Forwarding Topology

This repository exposes one APP-facing bridge entrypoint and proxies traffic
to four independent upstream production services. The APP-facing canonical ACP
paths remain `/acp/rpc` and `/acp` under
`https://xworkmate-bridge.svc.plus`.

Architecture topology: [docs/architecture/acp-forwarding-topology.md](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/docs/architecture/acp-forwarding-topology.md)

ADR for the unified APP-facing bridge contract: [docs/architecture/adr-unified-bridge-entrypoints.md](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/docs/architecture/adr-unified-bridge-entrypoints.md)

Example provider sync config: [example/config.yaml](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/example/config.yaml)

API reference: [docs/api-reference.md](/Users/shenlan/workspaces/cloud-neutral-toolkit/xworkmate-bridge/docs/api-reference.md)

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

Then it runs `playbooks/deploy_xworkmate_bridge_vhosts.yml`, which builds the service for `linux/amd64` and deploys it to the target host with Ansible.

### Validate stage

The validate stage proves production alignment against the bridge public
contract:

- bridge root and `/api/ping`
- strict image / tag / commit / version match against the built image ref
- upstream ACP capability probes for `codex`, `opencode`, and `gemini`
- minimal `session.start` smoke tests through `https://xworkmate-bridge.svc.plus/acp/rpc`

Required GitHub secrets:

- `SINGLE_NODE_VPS_SSH_PRIVATE_KEY`: private key used by the Actions runner to SSH into the target host
- `WORKSPACE_REPO_TOKEN`: token with access to checkout `x-evor/playbooks`

Optional GitHub secrets:

- `SSH_KNOWN_HOSTS`: pre-seeded known_hosts content for stricter host verification

Optional workflow input:

- `internal_service_token`: manual dispatch input that is forwarded to Ansible as `INTERNAL_SERVICE_TOKEN`

## Environment

- `ACP_LISTEN_ADDR`: listen address for `serve` mode, default `127.0.0.1:8787`
- `OUTPUT_DIR`: optional output directory for `make build`
- `OUTPUT_PATH`: optional explicit build path for `make build`
