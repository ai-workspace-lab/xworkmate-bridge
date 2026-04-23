#!/usr/bin/env bash
set -euo pipefail

BRIDGE_SERVER_URL="${BRIDGE_SERVER_URL:-https://xworkmate-bridge.svc.plus}"
BRIDGE_AUTH_TOKEN="${BRIDGE_AUTH_TOKEN:-}"

if [[ -z "${BRIDGE_AUTH_TOKEN}" ]]; then
  echo "Error: BRIDGE_AUTH_TOKEN is required" >&2
  exit 1
fi

rpc_call() {
  local payload="$1"
  curl \
    --silent \
    --show-error \
    --fail \
    --location \
    --max-time 60 \
    -H "Authorization: Bearer ${BRIDGE_AUTH_TOKEN}" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    --data "${payload}" \
    "${BRIDGE_SERVER_URL%/}/acp/rpc"
}

echo "--- Verifying canonical bridge contract for ${BRIDGE_SERVER_URL} ---"

echo -n "Checking /api/ping... "
ping_json="$(
  curl \
    --silent \
    --show-error \
    --fail \
    --location \
    --max-time 20 \
    "${BRIDGE_SERVER_URL%/}/api/ping"
)"
PING_JSON="${ping_json}" python3 - <<'PY'
import json
import os

payload = json.loads(os.environ["PING_JSON"])
if payload.get("status") != "ok":
    raise SystemExit("ping status is not ok")
PY
echo "OK"

echo -n "Checking acp.capabilities... "
capabilities_json="$(
  rpc_call '{"jsonrpc":"2.0","id":"cap-1","method":"acp.capabilities","params":{}}'
)"
CAPABILITIES_JSON="${capabilities_json}" python3 - <<'PY'
import json
import os

payload = json.loads(os.environ["CAPABILITIES_JSON"])
result = payload.get("result")
if payload.get("jsonrpc") != "2.0" or not isinstance(result, dict):
    raise SystemExit("invalid capabilities envelope")

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
echo "OK"

echo -n "Checking xworkmate.routing.resolve... "
routing_json="$(
  rpc_call '{"jsonrpc":"2.0","id":"routing-1","method":"xworkmate.routing.resolve","params":{"taskPrompt":"create a powerpoint deck for launch","workingDirectory":"/tmp/bridge-contract","routing":{"routingMode":"explicit","explicitExecutionTarget":"singleAgent","explicitProviderId":"codex","availableSkills":[{"id":"pptx","label":"PPTX","description":"slides","installed":true}]}}}'
)"
ROUTING_JSON="${routing_json}" python3 - <<'PY'
import json
import os

payload = json.loads(os.environ["ROUTING_JSON"])
result = payload.get("result")
if payload.get("jsonrpc") != "2.0" or not isinstance(result, dict):
    raise SystemExit("invalid routing envelope")

if result.get("resolvedExecutionTarget") != "single-agent":
    raise SystemExit(f"unexpected resolvedExecutionTarget: {result!r}")
if result.get("resolvedProviderId") != "codex":
    raise SystemExit(f"unexpected resolvedProviderId: {result!r}")
if result.get("status") != "available":
    raise SystemExit(f"unexpected routing status: {result!r}")
PY
echo "OK"

echo "Canonical bridge contract verification completed."
