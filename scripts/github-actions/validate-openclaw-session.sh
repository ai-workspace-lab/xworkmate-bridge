#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BRIDGE_SERVER_URL:-https://xworkmate-bridge.svc.plus}"
AUTH_TOKEN="${BRIDGE_AUTH_TOKEN:-}"
REQUEST_ORIGIN="${OPENCLAW_SMOKE_ORIGIN:-https://xworkmate.svc.plus}"
RPC_TIMEOUT_SECONDS="${OPENCLAW_SMOKE_RPC_TIMEOUT_SECONDS:-180}"
POLL_TIMEOUT_SECONDS="${OPENCLAW_SMOKE_POLL_TIMEOUT_SECONDS:-120}"
POLL_INTERVAL_SECONDS="${OPENCLAW_SMOKE_POLL_INTERVAL_SECONDS:-2}"

if [[ -z "${AUTH_TOKEN}" ]]; then
  echo "BRIDGE_AUTH_TOKEN is required" >&2
  exit 1
fi

normalize_url() {
  local raw="$1"
  printf '%s\n' "${raw%/}"
}

resolved_base_url="$(normalize_url "${BASE_URL}")"
rpc_url="${resolved_base_url}/acp/rpc"

stream_file="$(mktemp)"
trap 'rm -f "$stream_file"' EXIT

session_id="validate-openclaw-$(date +%s)"

request_body="$(cat <<JSON
{
  "jsonrpc": "2.0",
  "id": "validate-openclaw",
  "method": "session.start",
  "params": {
    "sessionId": "${session_id}",
    "threadId": "${session_id}",
    "appThreadKey": "${session_id}",
    "taskPrompt": "Reply exactly pong.",
    "workingDirectory": "/tmp",
    "routing": {
      "routingMode": "explicit",
      "explicitExecutionTarget": "gateway",
      "preferredGatewayProviderId": "openclaw"
    }
  }
}
JSON
)"

echo "OpenClaw smoke -> POST ${rpc_url} (session=${session_id})"

curl --http1.1 --fail --silent --show-error --no-buffer --max-time "${RPC_TIMEOUT_SECONDS}" \
  -H "Authorization: Bearer ${AUTH_TOKEN}" \
  -H "Origin: ${REQUEST_ORIGIN}" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  --data "${request_body}" \
  "${rpc_url}" > "${stream_file}"

OPENCLAW_AUTH_TOKEN="${AUTH_TOKEN}" \
OPENCLAW_STREAM_FILE="${stream_file}" \
OPENCLAW_POLL_URL="${rpc_url}" \
OPENCLAW_POLL_TIMEOUT_SECONDS="${POLL_TIMEOUT_SECONDS}" \
OPENCLAW_POLL_INTERVAL_SECONDS="${POLL_INTERVAL_SECONDS}" \
python3 - <<'PY'
import json
import os
import sys
import time
import urllib.request

stream_path = os.environ["OPENCLAW_STREAM_FILE"]
poll_url = os.environ["OPENCLAW_POLL_URL"]
auth_token = os.environ["OPENCLAW_AUTH_TOKEN"]
poll_timeout = int(os.environ["OPENCLAW_POLL_TIMEOUT_SECONDS"])
poll_interval = float(os.environ["OPENCLAW_POLL_INTERVAL_SECONDS"])

payloads = []
for block in open(stream_path, encoding="utf-8").read().split("\n\n"):
    data_lines = [
        line[len("data: "):]
        for line in block.splitlines()
        if line.startswith("data: ")
    ]
    if not data_lines:
        continue
    payload = "\n".join(data_lines).strip()
    if payload == "[DONE]":
        payloads.append({"done": True})
        continue
    payloads.append(json.loads(payload))

def terminal_result(payload):
    if not isinstance(payload, dict):
        return {}
    nested = payload.get("result")
    if isinstance(nested, dict) and str(payload.get("status", "")).lower() in {
        "completed",
        "failed",
        "cancelled",
        "canceled",
    }:
        return nested
    return payload


def output_text_from(payload):
    if not isinstance(payload, dict):
        return ""
    candidates = []
    stack = [payload]
    seen = set()
    while stack:
        item = stack.pop(0)
        if not isinstance(item, dict):
            continue
        identity = id(item)
        if identity in seen:
            continue
        seen.add(identity)
        for key in ("output", "message", "summary", "resultSummary", "assistantText", "text"):
            value = item.get(key)
            if isinstance(value, str):
                candidates.append(value)
        for key in ("result", "payload", "data", "task", "progress", "artifacts"):
            value = item.get(key)
            if isinstance(value, dict):
                stack.append(value)
    return " ".join(part for part in candidates if part)


def require_nonempty(payload, key):
    value = payload.get(key)
    if isinstance(value, str) and value.strip():
        return
    raise SystemExit(f"OpenClaw smoke result missing {key}: {json.dumps(payload, ensure_ascii=False, sort_keys=True)[:1000]}")


def is_valid_no_displayable_contract(payload):
    if not isinstance(payload, dict):
        return False
    if payload.get("code") != "OPENCLAW_NO_DISPLAYABLE_OUTPUT":
        return False
    if payload.get("resolvedGatewayProviderId") != "openclaw":
        return False
    for key in ("sessionId", "threadId", "runId", "openclawSessionKey", "artifactScope"):
        require_nonempty(payload, key)
    return True


final = next(
    (item for item in payloads if isinstance(item, dict) and item.get("id") == "validate-openclaw"),
    None,
)
if final is None:
    raise SystemExit("missing final OpenClaw result envelope")
if not payloads or payloads[-1].get("done") is not True:
    raise SystemExit("missing SSE done marker")

result = terminal_result(final.get("result") or final.get("payload") or {})
if result.get("status") == "running":
    session_id = result.get("sessionId")
    thread_id = result.get("threadId")
    turn_id = result.get("turnId")
    run_id = result.get("runId")

    deadline = time.time() + poll_timeout
    while time.time() < deadline:
        req_body = json.dumps({
            "jsonrpc": "2.0",
            "id": "poll-task",
            "method": "xworkmate.tasks.get",
            "params": {
                "sessionId": session_id,
                "threadId": thread_id,
                "turnId": turn_id,
                "runId": run_id,
            },
        }).encode("utf-8")

        req = urllib.request.Request(
            poll_url,
            data=req_body,
            headers={
                "Content-Type": "application/json",
                "Authorization": f"Bearer {auth_token}",
            },
        )
        try:
            with urllib.request.urlopen(req) as resp:
                resp_data = json.loads(resp.read().decode("utf-8"))
                poll_result = resp_data.get("result") or {}
                status = poll_result.get("status")
                if status in ("completed", "failed", "cancelled"):
                    result = terminal_result(poll_result)
                    final["result"] = poll_result
                    break
        except Exception as exc:
            print(f"poll error: {exc}", file=sys.stderr)
        time.sleep(poll_interval)
    else:
        raise SystemExit("timeout waiting for OpenClaw smoke task to complete")

error_text = json.dumps(final.get("error", {}), ensure_ascii=False)
for code in (
    "GATEWAY_PROVIDER_REQUIRED",
    "OPENCLAW_GATEWAY_METHOD_NOT_ALLOWED",
    "OPENCLAW_GATEWAY_CONFLICT",
    "OPENCLAW_TASK_ENDPOINT_REQUIRED",
):
    if code in error_text:
        raise SystemExit(f"legacy OpenClaw routing error remained: {code}")

final_text = json.dumps(final, ensure_ascii=False)
for marker in (
    "Requested agent harness",
    "provider is not one of",
    "Agent failed before reply",
    "ACP_HTTP_",
):
    if marker in final_text:
        raise SystemExit(f"OpenClaw smoke returned runtime error text: {marker}")

output_text = output_text_from(result)
if "pong" not in output_text.lower():
    if is_valid_no_displayable_contract(result):
        print("OpenClaw smoke OK: session contract completed without displayable output")
        sys.exit(0)
    result_preview = json.dumps(result, ensure_ascii=False, sort_keys=True)[:1000]
    raise SystemExit(f"OpenClaw smoke did not return pong: {output_text[:500]}\nresult preview: {result_preview}")

print("OpenClaw smoke OK: pong received from session contract")
PY
