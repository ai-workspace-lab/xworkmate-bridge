# Workspace authentication gateway

This optional Node.js service provides a loopback-only HTTPS entry point for a
single-user self-hosted workspace. It authenticates the workspace token, maps
requests to a fixed set of service routes, and keeps the New API administrator
credential separate from model API credentials.

The deployment owner supplies `WORKSPACE_AUTH_TOKEN`, `NEWAPI_ADMIN_USERNAME`,
and `NEWAPI_ADMIN_PASSWORD` at runtime. `AI_GATEWAY_API_KEY` is optional; model
routes return `503` when it is absent. The TLS key and certificate are mounted
at `/tls/server.key` and `/tls/server.crt`. No credential is stored in browser
storage, command-line arguments, or this repository.

The executable listens on port `18787`; publish it only on loopback. Its fixed
upstreams use the service names `xworkmate-bridge`, `qmd`, `dsh`, `cpa`, and
`newapi` on a private container network. The gateway rejects other hosts and
paths rather than acting as an open proxy.

Build and run the focused tests from this directory:

```sh
docker build -t workspace-auth-gateway:local .
node --test gateway.test.mjs
```

The browser management page is a small local control surface. Native service
administration remains available separately for emergency recovery. OAuth
provider accounts are still authorized independently in their respective
services; the workspace token does not replace provider authorization.

This gateway does not itself provide Vault initialization, unsealing, secret
rotation, or workspace backup. Those remain deployment-owner operations. The
automated fixtures validate the authentication boundary only; they do not prove
that a model provider is configured or that a live inference succeeds.
