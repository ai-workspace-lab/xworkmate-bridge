#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BRIDGE_SERVER_URL:-https://xworkmate-bridge.svc.plus}"
AUTH_TOKEN="${BRIDGE_AUTH_TOKEN:-}"
HTTP_TIMEOUT_SECONDS="${HTTP_TIMEOUT_SECONDS:-30}"
RPC_TIMEOUT_SECONDS="${RPC_TIMEOUT_SECONDS:-90}"

if [[ -z "${AUTH_TOKEN}" ]]; then
  echo "BRIDGE_AUTH_TOKEN is required" >&2
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
    --data '{"jsonrpc":"2.0","id":"cap-unauthorized","method":"acp.capabilities"}' \
    "${resolved_base_url}/acp/rpc"
)"

if [[ "${unauthorized_status}" != "401" ]]; then
  echo "expected unauthorized capabilities request to return 401, got ${unauthorized_status}" >&2
  exit 1
fi

capabilities_json="$(
  json_rpc_with_retry \
    '{"jsonrpc":"2.0","id":"cap-1","method":"acp.capabilities"}' \
    -H "Authorization: Bearer ${AUTH_TOKEN}"
)"

mapfile -t provider_ids < <(
  RESPONSE_JSON="${capabilities_json}" python3 - <<'PY'
import json
import os

payload = json.loads(os.environ["RESPONSE_JSON"])
result = payload.get("result")
if not isinstance(result, dict):
    raise SystemExit("bridge capabilities response missing result payload")

provider_catalog = result.get("providerCatalog")
if not isinstance(provider_catalog, list):
    raise SystemExit("providerCatalog is missing or invalid")

for item in provider_catalog:
    if isinstance(item, dict) and item.get("providerId"):
        print(item["providerId"])

if not any(isinstance(item, dict) and item.get("providerId") for item in provider_catalog):
    raise SystemExit("providerCatalog did not include any provider ids")
PY
)

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

expected_agent_ids = ["codex", "opencode", "gemini"]
expected_agent_labels = ["Codex", "OpenCode", "Gemini"]
if len(provider_catalog) != len(expected_agent_ids):
    raise SystemExit(f"expected 3 agent providers, got {provider_catalog!r}")

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

session_start_success=0
session_start_error=""
matched_provider_id=""

for provider_id in "${provider_ids[@]}"; do
  session_start_json="$(
    json_rpc_with_retry \
      "{\"jsonrpc\":\"2.0\",\"id\":\"task-1\",\"method\":\"session.start\",\"params\":{\"sessionId\":\"public-contract-smoke\",\"threadId\":\"public-contract-smoke\",\"taskPrompt\":\"Reply with exactly pong\",\"workingDirectory\":\"/tmp\",\"routing\":{\"routingMode\":\"explicit\",\"explicitExecutionTarget\":\"singleAgent\",\"explicitProviderId\":\"${provider_id}\"}}}" \
      -H "Authorization: Bearer ${AUTH_TOKEN}" 2>/tmp/xworkmate-bridge-public-contract-session-start.err || true
  )"

  if [[ -z "${session_start_json}" ]]; then
    session_start_error="$(cat /tmp/xworkmate-bridge-public-contract-session-start.err)"
    continue
  fi

  if RESPONSE_JSON="${session_start_json}" SELECTED_PROVIDER_ID="${provider_id}" python3 - <<'PY'
import json
import os

payload = json.loads(os.environ["RESPONSE_JSON"])
if payload.get("jsonrpc") != "2.0":
    raise SystemExit("session.start response missing jsonrpc envelope")

result = payload.get("result")
if not isinstance(result, dict):
    raise SystemExit(f"session.start missing result payload: {payload!r}")

if result.get("success") is not True:
    raise SystemExit(f"session.start did not succeed: {result!r}")

provider = str(result.get("provider", "")).strip()
if provider != os.environ["SELECTED_PROVIDER_ID"]:
    raise SystemExit(f"expected provider {os.environ['SELECTED_PROVIDER_ID']!r}, got {result!r}")

output = str(result.get("output", "")).strip().lower()
if output != "pong":
    raise SystemExit(f"expected output 'pong', got {result!r}")
PY
  then
    session_start_success=1
    matched_provider_id="${provider_id}"
    break
  fi

  session_start_error="$(cat /tmp/xworkmate-bridge-public-contract-session-start.err)"
done

if [[ "${session_start_success}" -ne 1 ]]; then
  echo "session.start failed for all providers in providerCatalog" >&2
  if [[ -n "${session_start_error}" ]]; then
    echo "${session_start_error}" >&2
  fi
  exit 1
fi

printf 'public bridge RPC contract verified via %s using provider %s\n' "${resolved_base_url}" "${matched_provider_id}"
