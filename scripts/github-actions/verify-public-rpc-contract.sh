#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BRIDGE_SERVER_URL:-https://xworkmate-bridge.svc.plus}"
AUTH_TOKEN="${AI_WORKSPACE_AUTH_TOKEN:-${BRIDGE_AUTH_TOKEN:-}}"
HTTP_TIMEOUT_SECONDS="${HTTP_TIMEOUT_SECONDS:-30}"
RPC_TIMEOUT_SECONDS="${RPC_TIMEOUT_SECONDS:-90}"

if [[ -z "${AUTH_TOKEN}" ]]; then
  echo "AI_WORKSPACE_AUTH_TOKEN or BRIDGE_AUTH_TOKEN is required" >&2
  exit 1
fi

normalize_url() {
  local raw="$1"
  printf '%s\n' "${raw%/}"
}

json_rpc_call() {
  local payload="$1"
  shift
  curl \
    --silent \
    --show-error \
    --fail \
    --location \
    --max-time "${RPC_TIMEOUT_SECONDS}" \
    -H 'Accept: application/json' \
    -H 'Content-Type: application/json' \
    "$@" \
    --data "${payload}" \
    "${resolved_base_url}/acp/rpc"
}

json_rpc_with_retry() {
  local payload="$1"
  shift
  local attempt
  for attempt in 1 2 3; do
    if json_rpc_call "${payload}" "$@"; then
      return 0
    fi
    if (( attempt == 3 )); then
      return 1
    fi
    sleep 5
  done
}

resolved_base_url="$(normalize_url "${BASE_URL}")"

unauthorized_status="$(
  curl \
    --silent \
    --show-error \
    --output /tmp/xworkmate-bridge-public-contract-unauthorized.json \
    --write-out '%{http_code}' \
    --location \
    --max-time "${HTTP_TIMEOUT_SECONDS}" \
    -H 'Accept: application/json' \
    -H 'Content-Type: application/json' \
    --data '{"jsonrpc":"2.0","id":"cap-public","method":"acp.capabilities"}' \
    "${resolved_base_url}/acp/rpc"
)"

if [[ "${unauthorized_status}" != "401" ]]; then
  echo "expected unauthenticated capabilities request to return 401, got ${unauthorized_status}" >&2
  exit 1
fi

unauthorized_session_status="$(
  curl \
    --silent \
    --show-error \
    --output /dev/null \
    --write-out '%{http_code}' \
    --location \
    --max-time "${HTTP_TIMEOUT_SECONDS}" \
    -H 'Accept: application/json' \
    -H 'Content-Type: application/json' \
    --data '{"jsonrpc":"2.0","id":"session-unauthorized","method":"session.start","params":{"sessionId":"test"}}' \
    "${resolved_base_url}/acp/rpc"
)"

if [[ "${unauthorized_session_status}" != "401" ]]; then
  echo "expected unauthorized session.start request to return 401, got ${unauthorized_session_status}" >&2
  exit 1
fi

capabilities_json="$(
  json_rpc_with_retry \
    '{"jsonrpc":"2.0","id":"cap-1","method":"acp.capabilities"}' \
    -H "Authorization: Bearer ${AUTH_TOKEN}"
)"

RESPONSE_JSON="${capabilities_json}" python3 - <<'PY'
import json
import os

payload = json.loads(os.environ["RESPONSE_JSON"])
if payload.get("jsonrpc") != "2.0":
    raise SystemExit("bridge capabilities response missing jsonrpc envelope")

result = payload.get("result")
if not isinstance(result, dict):
    raise SystemExit("bridge capabilities response missing result payload")

expected_targets = ["agent", "gateway"]
if result.get("availableExecutionTargets") != expected_targets:
    raise SystemExit(
        f"expected availableExecutionTargets {expected_targets!r}, got {result.get('availableExecutionTargets')!r}"
    )

provider_catalog = result.get("providerCatalog")
gateway_providers = result.get("gatewayProviders")
if not isinstance(provider_catalog, list):
    raise SystemExit("providerCatalog is missing or invalid")
if not isinstance(gateway_providers, list):
    raise SystemExit("gatewayProviders is missing or invalid")

expected_agent_ids = ["codex", "opencode", "gemini", "hermes"]
expected_agent_labels = ["Codex", "OpenCode", "Gemini", "Hermes"]
if len(provider_catalog) != len(expected_agent_ids):
    raise SystemExit(f"expected 4 agent providers, got {provider_catalog!r}")

for index, (provider_id, label) in enumerate(zip(expected_agent_ids, expected_agent_labels)):
    item = provider_catalog[index]
    if item.get("providerId") != provider_id:
        raise SystemExit(f"expected providerId {provider_id!r} at index {index}, got {item!r}")
    if item.get("label") != label:
        raise SystemExit(f"expected label {label!r} at index {index}, got {item!r}")
    if item.get("targets") != ["agent"]:
        raise SystemExit(f"expected agent targets for {provider_id!r}, got {item!r}")

if len(gateway_providers) != 1:
    raise SystemExit(f"expected one gateway provider, got {gateway_providers!r}")

gateway = gateway_providers[0]
if gateway.get("providerId") != "openclaw":
    raise SystemExit(f"expected gateway providerId 'openclaw', got {gateway!r}")
if gateway.get("label") != "OpenClaw":
    raise SystemExit(f"expected gateway label 'OpenClaw', got {gateway!r}")
if gateway.get("targets") != ["gateway"]:
    raise SystemExit(f"expected gateway targets ['gateway'], got {gateway!r}")
PY

agent_routing_json="$(
  json_rpc_with_retry \
    '{"jsonrpc":"2.0","id":"route-agent-1","method":"xworkmate.routing.resolve","params":{"taskPrompt":"Reply with exactly pong","workingDirectory":"/tmp","routing":{"routingMode":"explicit","explicitExecutionTarget":"singleAgent","explicitProviderId":"codex"}}}' \
    -H "Authorization: Bearer ${AUTH_TOKEN}"
)"

RESPONSE_JSON="${agent_routing_json}" python3 - <<'PY'
import json
import os

payload = json.loads(os.environ["RESPONSE_JSON"])
if payload.get("jsonrpc") != "2.0":
    raise SystemExit("agent routing response missing jsonrpc envelope")

result = payload.get("result")
if not isinstance(result, dict):
    raise SystemExit(f"agent routing missing result payload: {payload!r}")

if result.get("resolvedExecutionTarget") != "single-agent":
    raise SystemExit(f"unexpected routing target: {result!r}")
if result.get("resolvedProviderId") != "codex":
    raise SystemExit(f"unexpected routing provider: {result!r}")
if result.get("status") != "available":
    raise SystemExit(f"unexpected routing status: {result!r}")
PY

gateway_routing_json="$(
  json_rpc_with_retry \
    '{"jsonrpc":"2.0","id":"route-gateway-1","method":"xworkmate.routing.resolve","params":{"taskPrompt":"search latest news","workingDirectory":"/tmp","routing":{"routingMode":"explicit","explicitExecutionTarget":"gateway","preferredGatewayProviderId":"openclaw"}}}' \
    -H "Authorization: Bearer ${AUTH_TOKEN}"
)"

RESPONSE_JSON="${gateway_routing_json}" python3 - <<'PY'
import json
import os

payload = json.loads(os.environ["RESPONSE_JSON"])
if payload.get("jsonrpc") != "2.0":
    raise SystemExit("gateway routing response missing jsonrpc envelope")

result = payload.get("result")
if not isinstance(result, dict):
    raise SystemExit(f"gateway routing missing result payload: {payload!r}")

if result.get("resolvedExecutionTarget") != "gateway":
    raise SystemExit(f"unexpected gateway routing target: {result!r}")
if result.get("resolvedGatewayProviderId") != "openclaw":
    raise SystemExit(f"unexpected gateway provider: {result!r}")
if result.get("status") != "available":
    raise SystemExit(f"unexpected gateway routing status: {result!r}")
PY

printf 'public bridge RPC contract verified via %s\n' "${resolved_base_url}"
