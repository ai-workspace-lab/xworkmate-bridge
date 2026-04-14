#!/usr/bin/env bash
set -euo pipefail

IMAGE_REF="${1:?image_ref is required}"

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

curl_common=(
  --silent
  --show-error
  --fail
  --location
  --max-time 20
)

auth_headers=()
if [[ -n "${AUTH_TOKEN}" ]]; then
  auth_headers+=(-H "Authorization: Bearer ${AUTH_TOKEN}")
fi

probe_jsonrpc_capabilities() {
  local endpoint="$1"
  local response
  local headers=(
    -H 'Content-Type: application/json'
    -H 'Accept: application/json'
  )

  headers+=("${auth_headers[@]}")

  response="$(
    curl "${curl_common[@]}" \
      "${headers[@]}" \
      --data '{"jsonrpc":"2.0","id":"cap-1","method":"acp.capabilities"}' \
      "${endpoint}"
  )"

  RESPONSE_JSON="${response}" python3 - <<'PY'
import json
import os

payload = json.loads(os.environ["RESPONSE_JSON"])
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

  response="$(
    curl "${curl_common[@]}" \
      "${headers[@]}" \
      --data "${payload}" \
      "${BASE_URL}/acp/rpc"
  )"

  printf '%s\n' "${response}"
}

probe_bridge_single_agent_smoke() {
  local provider_id="$1"
  local request_id="smoke-${provider_id}-$(date +%s)"
  local session_id="validate-${provider_id}-$(date +%s)"
  local payload
  local response

  payload="$(cat <<JSON
{"jsonrpc":"2.0","id":"${request_id}","method":"session.start","params":{"sessionId":"${session_id}","threadId":"${session_id}","taskPrompt":"Reply with exactly pong","routing":{"routingMode":"explicit","explicitExecutionTarget":"singleAgent","explicitProviderId":"${provider_id}"}}}
JSON
)"

  response="$(jsonrpc_bridge_call "${payload}")"

  PROVIDER_ID="${provider_id}" RESPONSE_JSON="${response}" python3 - <<'PY'
import json
import os

provider = os.environ["PROVIDER_ID"]
payload = json.loads(os.environ["RESPONSE_JSON"])

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
  status="$(
    curl \
      --silent \
      --show-error \
      --output /dev/null \
      --write-out '%{http_code}' \
      --location \
      --max-time 20 \
      "${auth_headers[@]}" \
      "${endpoint}"
  )"

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

ping_json="$(
  curl \
    "${curl_common[@]}" \
    "${auth_headers[@]}" \
    "${BASE_URL}/api/ping"
)"

PING_JSON="${ping_json}" python3 - "${image_ref}" "${tag}" "${commit}" "${version}" <<'PY'
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

bridge_root="$(curl "${curl_common[@]}" "${auth_headers[@]}" "${BASE_URL}/")"
grep -qi 'xworkmate-bridge' <<<"${bridge_root}"

probe_safe_http_endpoint "${OPENCLAW_HTTP_PROBE_URL}"
probe_jsonrpc_capabilities "${CODEX_RPC_URL}"
probe_jsonrpc_capabilities "${OPENCODE_RPC_URL}"
probe_jsonrpc_capabilities "${GEMINI_RPC_URL}"
probe_bridge_single_agent_smoke "codex"
probe_bridge_single_agent_smoke "opencode"
probe_bridge_single_agent_smoke "gemini"
