# AGENTS

## Deployment Naming Rules

### Caddy config file naming

Use the following filename format for generated Caddy config files:

```text
<server-name>-<release_id>-<hostname>-<domain>.caddy
```

Example:

```text
console-6ebcdd6-jp-xhttp-contabo-console-svc-plus.caddy
```

Notes:

- `server-name` is the service or site identifier, such as `console`
- `release_id` is the normalized release identifier; use the release tag when available, otherwise use a short git commit id
- `hostname` is the target host name
- `domain` should be encoded in a filesystem-friendly form; replace `.` with `-`

## Coding Standards

### Anti-Fallback Rules

1. **No cascading fallback chains.** Prefer explicit single-path resolution. If a value is missing, fail early with a clear error instead of trying progressively degraded alternatives.

2. **No silent error swallowing.** Every error must be logged or returned. Do not use `_ = fn()` to suppress errors unless the function contract explicitly documents that the error is benign (e.g., `Close()` on a nil-safe receiver). For all other cases, log or propagate.

3. **No stale dead code.** Unused functions, types, constants, and entire files must be removed, not commented out or guarded behind unreachable branches.

4. **No redundant indirection.** One-line wrappers that simply delegate to another function must be removed. Call the canonical function directly.

5. **No hardcoded model defaults** that bypass configuration. Model selection must route through the resolver/catalog, not be baked into library code.

6. **No multi-agent orchestration in the bridge.** The bridge handles forwarding only. Any request that includes orchestration parameters (multiAgent, orchestrationMode, mode=multi-agent) must be rejected or treated as unrecognized.

### Dead Code Elimination

- Run `go vet` and ensure zero warnings before committing.
- Run `go build ./...` and verify compilation succeeds after every refactor.
- After removing a source file, verify that no remaining file imports it or references its exported symbols.
- Do not modify `*_test.go` files. If a refactor breaks a test, adjust the production code to keep the test passing.
