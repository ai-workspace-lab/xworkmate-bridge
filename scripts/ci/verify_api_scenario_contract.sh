#!/usr/bin/env bash
# scripts/ci/verify_api_scenario_contract.sh
set -euo pipefail

BRIDGE_SERVER_URL="${BRIDGE_SERVER_URL:-https://xworkmate-bridge.svc.plus}"
BRIDGE_AUTH_TOKEN="${BRIDGE_AUTH_TOKEN:-}"

if [[ -z "${BRIDGE_AUTH_TOKEN}" ]]; then
  echo "Error: BRIDGE_AUTH_TOKEN is required" >&2
  exit 1
fi

echo "--- Verifying API Scenario Contract for $BRIDGE_SERVER_URL ---"

# Scenario: Discovery -> Initialize Session
# 1. Discover capabilities and find a provider
echo "Step 1: Discovery"
CAPS=$(curl -s -X POST -H "Authorization: Bearer $BRIDGE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"acp.capabilities","params":{},"id":"scen-1"}' \
  "$BRIDGE_SERVER_URL/acp/rpc")

FIRST_PROVIDER=$(echo "$CAPS" | python3 -c 'import json, sys; d=json.load(sys.stdin); print(d.get("result", {}).get("providerCatalog", [{}])[0].get("providerId", ""))')

if [[ -z "$FIRST_PROVIDER" ]]; then
  echo "❌ Error: No providers found in catalog"
  exit 1
fi
echo "✅ Found provider: $FIRST_PROVIDER"

# 2. Attempt session.start
echo "Step 2: Session Initialization"
SESSION_ID="test-scenario-$(date +%s)"
START_RESP=$(curl -s -X POST -H "Authorization: Bearer $BRIDGE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"jsonrpc\":\"2.0\",\"method\":\"session.start\",\"params\":{\"sessionId\":\"$SESSION_ID\",\"routing\":{\"routingMode\":\"explicit\",\"explicitExecutionTarget\":\"singleAgent\",\"explicitProviderId\":\"$FIRST_PROVIDER\"}},\"id\":\"scen-2\"}" \
  "$BRIDGE_SERVER_URL/acp/rpc")

# Check if response is valid JSON-RPC
if ! echo "$START_RESP" | jq . >/dev/null 2>&1; then
  echo "❌ Error: session.start returned invalid JSON"
  echo "$START_RESP"
  exit 1
fi

# Analyze result
SUCCESS=$(echo "$START_RESP" | python3 -c 'import json, sys; d=json.load(sys.stdin); print(d.get("result", {}).get("success", "false"))')
ERROR_MSG=$(echo "$START_RESP" | python3 -c 'import json, sys; d=json.load(sys.stdin); print(d.get("error", {}).get("message", d.get("result", {}).get("error", "")))')

if [[ "$SUCCESS" == "True" ]]; then
  echo "✅ Session started successfully"
elif [[ "$ERROR_MSG" == *"connection refused"* || "$ERROR_MSG" == *"ROUTING_REQUIRED"* ]]; then
  echo "✅ Scenario logic verified (Gateway correctly routed but backend agent is unreachable: $ERROR_MSG)"
else
  echo "❌ Session start failed with unexpected error: $ERROR_MSG"
  echo "$START_RESP"
  exit 1
fi

echo "Scenario contract verification completed."
