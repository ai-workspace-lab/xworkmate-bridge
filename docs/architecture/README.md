# xworkmate-bridge Architecture

This directory contains architecture documentation for the **xworkmate-bridge** repository -- the Go-based ACP control plane and bridge backend. This repo is the companion to `xworkmate-app` (Flutter frontend) within the Cloud-Neutral Toolkit ecosystem.

## Documents

| Doc | Description |
| --- | --- |
| [ACP Forwarding Topology](acp-forwarding-topology.md) | Canonical ACP forwarding topology for bridge runtime |
| [ADR: Refocus Bridge as Control Plane](adr-refocus-bridge-as-control-plane.md) | Architecture Decision Record for re-focusing the bridge as the ACP control plane |
| [ADR: Unified Bridge Entrypoints](adr-unified-bridge-entrypoints.md) | Architecture Decision Record for unifying APP traffic entry points |
| [Bridge Runtime Design](bridge-runtime-design.md) | Converged runtime model for xworkmate-bridge |

## Related Repos

- [xworkmate-app](https://github.com/cloud-neutral-toolkit/xworkmate-app) -- Flutter AI assistant frontend
- [github-org-cloud-neutral-toolkit](https://github.com/cloud-neutral-toolkit/github-org-cloud-neutral-toolkit) -- cross-repo coordination hub
