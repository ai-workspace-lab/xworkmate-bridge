#!/usr/bin/env bash
set -euo pipefail

IMAGE_REF="${1:?image_ref is required}"
RETRYABLE_TRANSPORT=10
RETRYABLE_NOT_READY=11
FAST_HTTP_TIMEOUT_SECONDS=20
BRIDGE_RPC_TIMEOUT_SECONDS=330

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

websocket_probe_url() {
  local value="$1"
  if [[ "${value}" =~ ^wss://(.*)$ ]]; then
    printf 'https://%s\n' "${BASH_REMATCH[1]}"
    return
  fi
  if [[ "${value}" =~ ^ws://(.*)$ ]]; then
    printf 'http://%s\n' "${BASH_REMATCH[1]}"
    return
  fi
  printf '%s\n' "${value}"
}

image_ref="$(printf '%s' "${IMAGE_REF}" | tr -d '\n' | xargs)"
if [[ -z "${image_ref}" ]]; then
  echo "image_ref is required" >&2
  exit 1
fi

image_no_digest="${image_ref%@*}"
tag="${image_no_digest##*:}"
if [[ "${image_no_digest}" == "${tag}" ]]; then
  tag=""
fi

commit=""
version="${tag}"

if [[ "${tag}" =~ ^[0-9a-f]{40}$ ]]; then
  commit="${tag}"
fi

BASE_URL="$(normalize_url "${BRIDGE_SERVER_URL:-${2:-https://xworkmate-bridge.svc.plus}}")"
OPENCLAW_HTTP_PROBE_URL="$(websocket_probe_url "${OPENCLAW_URL:-${3:-wss://openclaw.svc.plus}}")"
CODEX_RPC_URL="$(normalize_url "${CODEX_RPC_URL:-${4:-https://acp-server.svc.plus/codex/acp/rpc}}")"
OPENCODE_RPC_URL="$(normalize_url "${OPENCODE_RPC_URL:-${5:-https://acp-server.svc.plus/opencode/acp/rpc}}")"
GEMINI_RPC_URL="$(normalize_url "${GEMINI_RPC_URL:-${6:-https://acp-server.svc.plus/gemini/acp/rpc}}")"
AUTH_TOKEN="${BRIDGE_AUTH_TOKEN:-${INTERNAL_SERVICE_TOKEN:-${7:-}}}"

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

auth_headers=()
if [[ -n "${AUTH_TOKEN}" ]]; then
  auth_headers+=(-H "Authorization: Bearer ${AUTH_TOKEN}")
fi

# Use explicit assignment guards so transport failures are not swallowed inside
# nested command substitutions when bash runs without inherit_errexit.
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

probe_jsonrpc_capabilities_once() {
  local endpoint="$1"
  local response
  local headers=(
    -H 'Content-Type: application/json'
    -H 'Accept: application/json'
  )

  headers+=("${auth_headers[@]}")

  if response="$(
    capture_http_response "capabilities ${endpoint}" \
      "${fast_http_curl_common[@]}" \
      "${headers[@]}" \
      --data '{"jsonrpc":"2.0","id":"cap-1","method":"acp.capabilities"}' \
      "${endpoint}"
  )"; then
    :
  else
    local exit_code=$?
    return "${exit_code}"
  fi

  RESPONSE_JSON="${response}" python3 - <<'PY'
import json
import os

try:
    payload = json.loads(os.environ["RESPONSE_JSON"])
except json.JSONDecodeError as exc:
    raise SystemExit(f"capabilities response returned invalid JSON: {exc}") from None

if payload.get("jsonrpc") != "2.0":
    raise SystemExit("capabilities response missing jsonrpc envelope")

result = payload.get("result")
if not isinstance(result, dict):
    raise SystemExit("capabilities response missing result payload")

if not result and "providers" not in payload:
    raise SystemExit("capabilities response missing result/providers data")
PY
}

jsonrpc_bridge_call() {
  local payload="$1"
  local response
  local headers=(
    -H 'Content-Type: application/json'
    -H 'Accept: application/json'
  )

  headers+=("${auth_headers[@]}")

  if response="$(
    capture_http_response "bridge rpc ${BASE_URL}/acp/rpc" \
      "${bridge_rpc_curl_common[@]}" \
      "${headers[@]}" \
      --data "${payload}" \
      "${BASE_URL}/acp/rpc"
  )"; then
    :
  else
    local exit_code=$?
    return "${exit_code}"
  fi

  printf '%s\n' "${response}"
}

probe_bridge_single_agent_smoke_once() {
  local provider_id="$1"
  local request_id="smoke-${provider_id}-$(date +%s)"
  local session_id="validate-${provider_id}-$(date +%s)"
  local payload
  local response

  payload="$(cat <<JSON
{"jsonrpc":"2.0","id":"${request_id}","method":"session.start","params":{"sessionId":"${session_id}","threadId":"${session_id}","taskPrompt":"Reply with exactly pong","routing":{"routingMode":"explicit","explicitExecutionTarget":"singleAgent","explicitProviderId":"${provider_id}"}}}
JSON
)"

  if response="$(jsonrpc_bridge_call "${payload}")"; then
    :
  else
    local exit_code=$?
    return "${exit_code}"
  fi

  PROVIDER_ID="${provider_id}" RESPONSE_JSON="${response}" python3 - <<'PY'
import json
import os

provider = os.environ["PROVIDER_ID"]
try:
    payload = json.loads(os.environ["RESPONSE_JSON"])
except json.JSONDecodeError as exc:
    raise SystemExit(f"{provider}: bridge rpc returned invalid JSON: {exc}") from None

if payload.get("jsonrpc") != "2.0":
    raise SystemExit(f"{provider}: missing jsonrpc envelope")

if payload.get("error"):
    raise SystemExit(f"{provider}: rpc error {payload['error']}")

result = payload.get("result")
if not isinstance(result, dict):
    raise SystemExit(f"{provider}: missing result payload")

if result.get("success") is not True:
    raise SystemExit(f"{provider}: success flag was not true: {result!r}")

def first_text_candidate(data):
    for key in ("output", "resultSummary", "summary", "message"):
        value = data.get(key)
        if isinstance(value, str) and value.strip():
            return value
    return ""

def normalize_text(value):
    normalized = value.strip().strip("`").strip()
    if len(normalized) >= 2 and normalized[0] == normalized[-1] and normalized[0] in {'"', "'"}:
        normalized = normalized[1:-1].strip()
    return normalized.lower()

text = first_text_candidate(result)
if normalize_text(text) != "pong":
    raise SystemExit(f"{provider}: expected normalized pong output, got {text!r} from {result!r}")
PY
}

probe_safe_http_endpoint() {
  local endpoint="$1"
  local status
  if ! status="$(
    curl \
      --silent \
      --show-error \
      --output /dev/null \
      --write-out '%{http_code}' \
      --location \
      --max-time "${FAST_HTTP_TIMEOUT_SECONDS}" \
      "${auth_headers[@]}" \
      "${endpoint}" 2>&1
  )"; then
    printf 'HTTP probe failed for %s: %s\n' "${endpoint}" "${status}" >&2
    return "${RETRYABLE_TRANSPORT}"
  fi

  case "${status}" in
    2*|3*|401|403|404|405|426)
      return 0
      ;;
    *)
      printf 'Unexpected HTTP status %s for %s\n' "${status}" "${endpoint}" >&2
      return 1
      ;;
  esac
}

wait_for_release_ping_once() {
  local ping_json

  if ping_json="$(
    capture_http_response "bridge ping ${BASE_URL}/api/ping" \
      "${fast_http_curl_common[@]}" \
      "${BASE_URL}/api/ping"
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
try:
    payload = json.loads(os.environ["PING_JSON"])
except json.JSONDecodeError as exc:
    raise SystemExit(f"bridge ping returned invalid JSON: {exc}") from None

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
      "${BASE_URL}/"
  )"; then
    :
  else
    local exit_code=$?
    return "${exit_code}"
  fi

  grep -qi 'xworkmate-bridge' <<<"${bridge_root}"
}

run_with_retry "bridge ping ${BASE_URL}/api/ping" 6 5 "${RETRYABLE_TRANSPORT},${RETRYABLE_NOT_READY}" wait_for_release_ping_once
probe_bridge_root

probe_safe_http_endpoint "${OPENCLAW_HTTP_PROBE_URL}"
run_with_retry "capabilities ${CODEX_RPC_URL}" 3 5 "${RETRYABLE_TRANSPORT}" probe_jsonrpc_capabilities_once "${CODEX_RPC_URL}"
run_with_retry "capabilities ${OPENCODE_RPC_URL}" 3 5 "${RETRYABLE_TRANSPORT}" probe_jsonrpc_capabilities_once "${OPENCODE_RPC_URL}"
run_with_retry "capabilities ${GEMINI_RPC_URL}" 3 5 "${RETRYABLE_TRANSPORT}" probe_jsonrpc_capabilities_once "${GEMINI_RPC_URL}"
run_with_retry "bridge single-agent smoke codex" 3 10 "${RETRYABLE_TRANSPORT}" probe_bridge_single_agent_smoke_once "codex"
run_with_retry "bridge single-agent smoke opencode" 3 10 "${RETRYABLE_TRANSPORT}" probe_bridge_single_agent_smoke_once "opencode"
run_with_retry "bridge single-agent smoke gemini" 3 10 "${RETRYABLE_TRANSPORT}" probe_bridge_single_agent_smoke_once "gemini"
