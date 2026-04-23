# ADR: Refocus xworkmate-bridge as ACP Control Plane

## Status
Proposed

## Context
Originally, `xworkmate-bridge` evolved as a hybrid of a reverse proxy, a stdio runtime manager, and a basic routing layer. This led to logic fragmentation, where provider-specific details (like Gemini or Hermes protocol handling) were mixed with core session orchestration. 

## Decision
We decided to refocus `xworkmate-bridge` as an **APP-facing ACP control-plane and provider compatibility layer**. 

Key changes:
1. **Control Plane Identity**: The bridge is the Source of Truth (SSOT) for provider discovery (`acp.capabilities`) and routing resolution (`xworkmate.routing.resolve`).
2. **Session Orchestration**: All sessions are orchestrated by a unified engine that handles turn state, history normalization, and result shaping. Provider-specific differences are absorbed by a dedicated **Compatibility Layer** (Adapters).
3. **Decoupled Runtime**: Stdio and process lifecycle details are pushed down to specific adapters. The bridge only depends on the `ProviderAdapter` contract.
4. **Stripped Non-Core Duties**: TLS, ingress routing, and rate limiting are handed over to edge ingress controllers (e.g., APISIX/Caddy).

## Consequences
- **Pros**: Clear separation of concerns, easier to add new providers, stable contract for App, reduced codebase complexity.
- **Cons**: Requires migration of existing direct-proxy logic to the new adapter pattern.
- **Backward Compatibility**: Dropped legacy direct-proxy paths and internal routing leaks.
