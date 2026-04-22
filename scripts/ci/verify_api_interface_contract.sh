#!/usr/bin/env bash
# scripts/ci/verify_api_interface_contract.sh
set -euo pipefail

BRIDGE_SERVER_URL="${BRIDGE_SERVER_URL:-https://xworkmate-bridge.svc.plus}"
BRIDGE_AUTH_TOKEN="${BRIDGE_AUTH_TOKEN:-}"

if [[ -z "${BRIDGE_AUTH_TOKEN}" ]]; then
  echo "Error: BRIDGE_AUTH_TOKEN is required" >&2
  exit 1
fi

echo "--- Verifying API Interface Contract for $BRIDGE_SERVER_URL ---"

check_endpoint() {
  local name=$1
  local path=$2
  local expected_status=$3
  local content_type=$4

  echo -n "Checking $name ($path)... "
  local response_info
  response_info=$(curl -s -o /tmp/resp.body -w "%{http_code} %{content_type}" \
    -H "Authorization: Bearer $BRIDGE_AUTH_TOKEN" \
    "$BRIDGE_SERVER_URL$path")
  
  local status=$(echo "$response_info" | cut -d' ' -f1)
  local actual_ct=$(echo "$response_info" | cut -d' ' -f2-)

  if [[ "$status" == "$expected_status" ]]; then
    if [[ "$actual_ct" == *"$content_type"* ]]; then
      # 验证是否为有效的 JSON 且包含 ok: true
      if jq -e '.ok == true' /tmp/resp.body >/dev/null 2>&1; then
        echo "✅ OK ($status, application/json)"
      else
        echo "❌ Failed (Invalid Bridge Response Structure)"
        cat /tmp/resp.body
        return 1
      fi
    else
      echo "❌ Failed: Wrong Content-Type (Expected $content_type, got $actual_ct)"
      return 1
    fi
  else
    echo "❌ Failed (Expected $expected_status, got $status)"
    return 1
  fi
}

# 现在的架构下，所有路径都应该由 Bridge 统一处理并返回 200 JSON
check_endpoint "OpenClaw" "/gateway/openclaw" "200" "application/json"
check_endpoint "OpenCode" "/acp-server/opencode" "200" "application/json"
check_endpoint "Codex" "/acp-server/codex" "200" "application/json"
check_endpoint "Gemini" "/acp-server/gemini" "200" "application/json"
check_endpoint "Hermes" "/acp-server/hermes" "200" "application/json"

# 6. Aggregate RPC Endpoint
echo -n "Checking Aggregate RPC (/acp/rpc)... "
rpc_status=$(curl -s -o /tmp/rpc.resp -w "%{http_code}" \
  -X POST -H "Authorization: Bearer $BRIDGE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"acp.capabilities","params":{},"id":1}' \
  "$BRIDGE_SERVER_URL/acp/rpc")

if [[ "$rpc_status" == "200" ]]; then
  if jq -e '.ok == true' /tmp/rpc.resp >/dev/null 2>&1; then
    echo "✅ OK (200 + Valid JSON-RPC Result)"
  else
    echo "❌ Failed (Invalid JSON-RPC Response)"
    cat /tmp/rpc.resp
    exit 1
  fi
else
  echo "❌ Failed ($rpc_status)"
  exit 1
fi

echo "Interface contract verification completed."
