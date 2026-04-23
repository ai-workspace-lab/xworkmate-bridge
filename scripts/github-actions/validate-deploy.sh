#!/usr/bin/env bash
set -euo pipefail

IMAGE_REF="${1:?image_ref is required}"
RETRYABLE_TRANSPORT=10
RETRYABLE_NOT_READY=11
FAST_HTTP_TIMEOUT_SECONDS=20
BRIDGE_RPC_TIMEOUT_SECONDS=60

normalize_url() {
  local value="$1"
  if [[ "${value}" =~ ^https:([^/].*)$ ]]; then
    printf 'https://%s\n' "${BASH_REMATCH[1]}"
    return
  fi
  if [[ "${value}" =~ ^http:([^/].*)$ ]]; then
    printf 'http://%s\n' "${BASH_REMATCH[1]}"
    return
  fi
  printf '%s\n' "${value}"
}

image_ref="$(printf '%s' "${IMAGE_REF}" | tr -d '\n' | xargs)"
image_no_digest="${image_ref%@*}"
tag="${image_no_digest##*:}"
commit=""
version="${tag}"
if [[ "${tag}" =~ ^[0-9a-f]{40}$ ]]; then
  commit="${tag}"
fi

BASE_URL="$(normalize_url "${BRIDGE_SERVER_URL:-${2:-https://xworkmate-bridge.svc.plus}}")"
RPC_URL="${BASE_URL%/}/acp/rpc"
AUTH_TOKEN="${BRIDGE_AUTH_TOKEN:?BRIDGE_AUTH_TOKEN is required}"

fast_http_curl_common=(
  --silent
  --show-error
  --fail
  --location
  --max-time "${FAST_HTTP_TIMEOUT_SECONDS}"
)

bridge_rpc_curl_common=(
  --silent
  --show-error
  --fail
  --location
  --max-time "${BRIDGE_RPC_TIMEOUT_SECONDS}"
)

capture_http_response() {
  local label="$1"
  shift

  local response
  if ! response="$(curl "$@" 2>&1)"; then
    printf '%s request failed: %s\n' "${label}" "${response}" >&2
    return "${RETRYABLE_TRANSPORT}"
  fi

  if [[ -z "${response}" ]]; then
    printf '%s request returned an empty response\n' "${label}" >&2
    return "${RETRYABLE_TRANSPORT}"
  fi

  printf '%s\n' "${response}"
}

should_retry_exit_code() {
  local exit_code="$1"
  local allowed="$2"
  local candidate

  IFS=',' read -r -a candidates <<<"${allowed}"
  for candidate in "${candidates[@]}"; do
    if [[ "${exit_code}" == "${candidate}" ]]; then
      return 0
    fi
  done
  return 1
}

run_with_retry() {
  local label="$1"
  local attempts="$2"
  local sleep_seconds="$3"
  local retryable_codes="$4"
  shift 4

  local attempt exit_code
  for ((attempt = 1; attempt <= attempts; attempt += 1)); do
    if "$@"; then
      return 0
    else
      exit_code=$?
    fi

    if (( attempt == attempts )) || ! should_retry_exit_code "${exit_code}" "${retryable_codes}"; then
      return "${exit_code}"
    fi

    printf '%s attempt %d/%d failed; retrying in %ss\n' \
      "${label}" \
      "${attempt}" \
      "${attempts}" \
      "${sleep_seconds}" >&2
    sleep "${sleep_seconds}"
  done

  return 1
}

jsonrpc_bridge_call() {
  local payload="$1"
  capture_http_response \
    "bridge rpc ${RPC_URL}" \
    "${bridge_rpc_curl_common[@]}" \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json' \
    -H "Authorization: Bearer ${AUTH_TOKEN}" \
    --data "${payload}" \
    "${RPC_URL}"
}

wait_for_release_ping_once() {
  local ping_json
  if ping_json="$(
    capture_http_response "bridge ping ${BASE_URL}/api/ping" \
      "${fast_http_curl_common[@]}" \
      "${BASE_URL%/}/api/ping"
  )"; then
    :
  else
    local exit_code=$?
    return "${exit_code}"
  fi

  if PING_JSON="${ping_json}" python3 - "${image_ref}" "${tag}" "${commit}" "${version}" <<'PY'
import json
import os
import sys

image_ref, tag, commit, version = sys.argv[1:5]
payload = json.loads(os.environ["PING_JSON"])
if payload.get("status") != "ok":
    raise SystemExit("ping status not ok")
if payload.get("image") != image_ref:
    raise SystemExit(f"expected image {image_ref!r}, got {payload.get('image')!r}")
if tag and payload.get("tag") != tag:
    raise SystemExit(f"expected tag {tag!r}, got {payload.get('tag')!r}")
if commit and payload.get("commit") != commit:
    raise SystemExit(f"expected commit {commit!r}, got {payload.get('commit')!r}")
if version and payload.get("version") != version:
    raise SystemExit(f"expected version {version!r}, got {payload.get('version')!r}")
PY
  then
    return 0
  fi

  return "${RETRYABLE_NOT_READY}"
}

probe_bridge_root() {
  local bridge_root
  if bridge_root="$(
    capture_http_response "bridge root ${BASE_URL}/" \
      "${fast_http_curl_common[@]}" \
      "${BASE_URL%/}/"
  )"; then
    :
  else
    local exit_code=$?
    return "${exit_code}"
  fi

  grep -qi 'xworkmate-bridge' <<<"${bridge_root}"
}

probe_bridge_capabilities_once() {
  local response
  if response="$(jsonrpc_bridge_call '{"jsonrpc":"2.0","id":"cap-1","method":"acp.capabilities","params":{}}')"; then
    :
  else
    local exit_code=$?
    return "${exit_code}"
  fi

  RESPONSE_JSON="${response}" python3 - <<'PY'
import json
import os

payload = json.loads(os.environ["RESPONSE_JSON"])
result = payload.get("result")
if payload.get("jsonrpc") != "2.0" or not isinstance(result, dict):
    raise SystemExit("capabilities response missing result payload")

provider_catalog = result.get("providerCatalog")
gateway_providers = result.get("gatewayProviders")
targets = result.get("availableExecutionTargets")
if not isinstance(provider_catalog, list):
    raise SystemExit("providerCatalog missing")
if not isinstance(gateway_providers, list):
    raise SystemExit("gatewayProviders missing")
if not isinstance(targets, list):
    raise SystemExit("availableExecutionTargets missing")

providers = {item.get("providerId") for item in provider_catalog if isinstance(item, dict)}
if not {"codex", "opencode", "gemini", "hermes"}.issubset(providers):
    raise SystemExit(f"unexpected providerCatalog: {provider_catalog!r}")

gateway_ids = {item.get("providerId") for item in gateway_providers if isinstance(item, dict)}
if "openclaw" not in gateway_ids:
    raise SystemExit(f"unexpected gatewayProviders: {gateway_providers!r}")

if "agent" not in targets or "gateway" not in targets:
    raise SystemExit(f"unexpected availableExecutionTargets: {targets!r}")
PY
}

probe_bridge_routing_once() {
  local response
  local payload='{"jsonrpc":"2.0","id":"route-1","method":"xworkmate.routing.resolve","params":{"taskPrompt":"create a powerpoint deck for launch","workingDirectory":"/tmp/validate-deploy","routing":{"routingMode":"explicit","explicitExecutionTarget":"singleAgent","explicitProviderId":"codex","availableSkills":[{"id":"pptx","label":"PPTX","description":"slides","installed":true}]}}}'

  if response="$(jsonrpc_bridge_call "${payload}")"; then
    :
  else
    local exit_code=$?
    return "${exit_code}"
  fi

  RESPONSE_JSON="${response}" python3 - <<'PY'
import json
import os

payload = json.loads(os.environ["RESPONSE_JSON"])
result = payload.get("result")
if payload.get("jsonrpc") != "2.0" or not isinstance(result, dict):
    raise SystemExit("routing response missing result payload")

if result.get("resolvedExecutionTarget") != "single-agent":
    raise SystemExit(f"unexpected routing target: {result!r}")
if result.get("resolvedProviderId") != "codex":
    raise SystemExit(f"unexpected routing provider: {result!r}")
if result.get("status") != "available":
    raise SystemExit(f"unexpected routing status: {result!r}")
PY
}

probe_bridge_gateway_routing_once() {
  local response
  local payload='{"jsonrpc":"2.0","id":"route-gateway-1","method":"xworkmate.routing.resolve","params":{"taskPrompt":"search latest news","workingDirectory":"/tmp/validate-deploy","routing":{"routingMode":"explicit","explicitExecutionTarget":"gateway","preferredGatewayProviderId":"openclaw"}}}'

  if response="$(jsonrpc_bridge_call "${payload}")"; then
    :
  else
    local exit_code=$?
    return "${exit_code}"
  fi

  RESPONSE_JSON="${response}" python3 - <<'PY'
import json
import os

payload = json.loads(os.environ["RESPONSE_JSON"])
result = payload.get("result")
if payload.get("jsonrpc") != "2.0" or not isinstance(result, dict):
    raise SystemExit("gateway routing response missing result payload")

if result.get("resolvedExecutionTarget") != "gateway":
    raise SystemExit(f"unexpected gateway target: {result!r}")
if result.get("resolvedGatewayProviderId") != "openclaw":
    raise SystemExit(f"unexpected gateway provider: {result!r}")
if result.get("status") != "available":
    raise SystemExit(f"unexpected gateway routing status: {result!r}")
PY
}

run_with_retry "bridge ping ${BASE_URL}/api/ping" 6 5 "${RETRYABLE_TRANSPORT},${RETRYABLE_NOT_READY}" wait_for_release_ping_once
probe_bridge_root
run_with_retry "bridge capabilities ${RPC_URL}" 3 5 "${RETRYABLE_TRANSPORT}" probe_bridge_capabilities_once
run_with_retry "bridge routing ${RPC_URL}" 3 5 "${RETRYABLE_TRANSPORT}" probe_bridge_routing_once
run_with_retry "bridge gateway routing ${RPC_URL}" 3 5 "${RETRYABLE_TRANSPORT}" probe_bridge_gateway_routing_once
