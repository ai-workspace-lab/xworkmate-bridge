#!/usr/bin/env bash
set -euo pipefail

BRIDGE_SERVER_URL="${BRIDGE_SERVER_URL:-https://xworkmate-bridge.svc.plus}"
BRIDGE_AUTH_TOKEN="${AI_WORKSPACE_AUTH_TOKEN:-${BRIDGE_AUTH_TOKEN:-}}"
HERMES_RPC_URL="${HERMES_RPC_URL:-${BRIDGE_SERVER_URL%/}/acp/rpc}"

if [[ -z "${BRIDGE_AUTH_TOKEN}" ]]; then
  echo "Error: AI_WORKSPACE_AUTH_TOKEN or BRIDGE_AUTH_TOKEN is required" >&2
  exit 1
fi

rpc_post() {
  local url="$1"
  local payload="$2"
  curl \
    --silent \
    --show-error \
    --fail \
    --location \
    --max-time 180 \
    -H "Accept: application/json" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${BRIDGE_AUTH_TOKEN}" \
    --data "${payload}" \
    "${url}"
}

json_get() {
  local response_json="$1"
  local expr="$2"
  RESPONSE_JSON="${response_json}" python3 - "$expr" <<'PY'
import json
import os
import sys

expr = sys.argv[1]
payload = json.loads(os.environ["RESPONSE_JSON"])

def get(value, path):
    current = value
    for part in path:
        if isinstance(current, list):
            try:
                current = current[int(part)]
            except Exception:
                return ""
            continue
        if not isinstance(current, dict):
            return ""
        current = current.get(part)
        if current is None:
            return ""
    if current is None:
        return ""
    if isinstance(current, (dict, list)):
        return json.dumps(current, ensure_ascii=False)
    return str(current)

print(get(payload, expr.split(".")))
PY
}

assert_nonempty() {
  local name="$1"
  local value="$2"
  if [[ -z "${value}" ]]; then
    echo "FAIL: ${name} is empty" >&2
    exit 1
  fi
}

assert_contains() {
  local name="$1"
  local haystack="$2"
  local needle="$3"
  if [[ "${haystack}" != *"${needle}"* ]]; then
    echo "FAIL: ${name} does not contain ${needle}: ${haystack}" >&2
    exit 1
  fi
}

echo "--- Verifying Hermes ACP scenarios for ${BRIDGE_SERVER_URL} ---"

echo "Scenario A: long dialogue"
hermes_caps="$(
  rpc_post \
    "${HERMES_RPC_URL}" \
    '{"jsonrpc":"2.0","id":"hermes-capabilities","method":"acp.capabilities","params":{}}'
)"
assert_contains "hermes capabilities" "$(json_get "${hermes_caps}" "result.providerCatalog")" "hermes"

hermes_session_id="hermes-scenario-$(date +%s)"
hermes_thread_id="${hermes_session_id}"

hermes_start="$(
  rpc_post \
    "${HERMES_RPC_URL}" \
    "{\"jsonrpc\":\"2.0\",\"id\":\"hermes-start\",\"method\":\"session.start\",\"params\":{\"sessionId\":\"${hermes_session_id}\",\"threadId\":\"${hermes_thread_id}\",\"taskPrompt\":\"Reply with exactly pong\",\"workingDirectory\":\"/tmp/hermes-long-dialogue\",\"routing\":{\"routingMode\":\"explicit\",\"explicitExecutionTarget\":\"singleAgent\",\"explicitProviderId\":\"hermes\"}}}"
)"
assert_nonempty "hermes start output" "$(json_get "${hermes_start}" "result.output")"
assert_contains "hermes start output" "$(json_get "${hermes_start}" "result.output")" "pong"
assert_contains "hermes start upstream method" "$(json_get "${hermes_start}" "result.upstreamMethod")" "session/prompt"
hermes_upstream_session_id="$(json_get "${hermes_start}" "result.upstreamSessionId")"
assert_nonempty "hermes upstream session id" "${hermes_upstream_session_id}"

hermes_message_1="$(
  rpc_post \
    "${HERMES_RPC_URL}" \
    "{\"jsonrpc\":\"2.0\",\"id\":\"hermes-message-1\",\"method\":\"session.message\",\"params\":{\"sessionId\":\"${hermes_session_id}\",\"threadId\":\"${hermes_thread_id}\",\"taskPrompt\":\"Reply with exactly pong again\",\"workingDirectory\":\"/tmp/hermes-long-dialogue\",\"routing\":{\"routingMode\":\"explicit\",\"explicitExecutionTarget\":\"singleAgent\",\"explicitProviderId\":\"hermes\"}}}"
)"
assert_nonempty "hermes message1 output" "$(json_get "${hermes_message_1}" "result.output")"
assert_contains "hermes message1 upstream method" "$(json_get "${hermes_message_1}" "result.upstreamMethod")" "session/prompt"
assert_nonempty "hermes message1 upstream session id" "$(json_get "${hermes_message_1}" "result.upstreamSessionId")"

hermes_message_2="$(
  rpc_post \
    "${HERMES_RPC_URL}" \
    "{\"jsonrpc\":\"2.0\",\"id\":\"hermes-message-2\",\"method\":\"session.message\",\"params\":{\"sessionId\":\"${hermes_session_id}\",\"threadId\":\"${hermes_thread_id}\",\"taskPrompt\":\"Reply with exactly pong one more time\",\"workingDirectory\":\"/tmp/hermes-long-dialogue\",\"routing\":{\"routingMode\":\"explicit\",\"explicitExecutionTarget\":\"singleAgent\",\"explicitProviderId\":\"hermes\"}}}"
)"
assert_nonempty "hermes message2 output" "$(json_get "${hermes_message_2}" "result.output")"
assert_contains "hermes message2 upstream method" "$(json_get "${hermes_message_2}" "result.upstreamMethod")" "session/prompt"

if [[ "$(json_get "${hermes_message_1}" "result.upstreamSessionId")" != "${hermes_upstream_session_id}" ]]; then
  echo "FAIL: Hermes upstream session id changed on message 1" >&2
  exit 1
fi
if [[ "$(json_get "${hermes_message_2}" "result.upstreamSessionId")" != "${hermes_upstream_session_id}" ]]; then
  echo "FAIL: Hermes upstream session id changed on message 2" >&2
  exit 1
fi

echo "Scenario B: skills install routing"
skills_resolve="$(
  rpc_post \
    "${BRIDGE_SERVER_URL%/}/acp/rpc" \
    '{"jsonrpc":"2.0","id":"skills-resolve","method":"xworkmate.routing.resolve","params":{"taskPrompt":"translate and dub this video with subtitles","workingDirectory":"/tmp/skills-install","routing":{"routingMode":"auto","allowSkillInstall":true,"availableSkills":[{"id":"docx","label":"docx","description":"docs","installed":true}]}}}'
)"
assert_contains "skills resolve source" "$(json_get "${skills_resolve}" "result.skillResolutionSource")" "find_skills"
assert_contains "skills resolve needs install" "$(json_get "${skills_resolve}" "result.needsSkillInstall")" "true"
install_request_id="$(json_get "${skills_resolve}" "result.skillInstallRequestId")"
assert_nonempty "skill install request id" "${install_request_id}"
skill_candidate_id="$(json_get "${skills_resolve}" "result.skillCandidates.0.id")"
assert_nonempty "skill candidate id" "${skill_candidate_id}"

skills_retry="$(
  rpc_post \
    "${BRIDGE_SERVER_URL%/}/acp/rpc" \
    "{\"jsonrpc\":\"2.0\",\"id\":\"skills-retry\",\"method\":\"xworkmate.routing.resolve\",\"params\":{\"taskPrompt\":\"translate and dub this video with subtitles\",\"workingDirectory\":\"/tmp/skills-install\",\"routing\":{\"routingMode\":\"auto\",\"allowSkillInstall\":true,\"installApproval\":{\"requestId\":\"${install_request_id}\",\"approvedSkillKeys\":[\"${skill_candidate_id}\"]},\"availableSkills\":[{\"id\":\"docx\",\"label\":\"docx\",\"description\":\"docs\",\"installed\":true}]}}}"
)"
assert_contains "skills retry needs install" "$(json_get "${skills_retry}" "result.needsSkillInstall")" "false"
assert_nonempty "skills retry resolved skills" "$(json_get "${skills_retry}" "result.resolvedSkills")"

echo "Scenario C: long dialogue + skills task"
task_session_id="hermes-task-$(date +%s)"
task_thread_id="${task_session_id}"
task_start="$(
  rpc_post \
    "${BRIDGE_SERVER_URL%/}/acp/rpc" \
    "{\"jsonrpc\":\"2.0\",\"id\":\"task-start\",\"method\":\"session.start\",\"params\":{\"sessionId\":\"${task_session_id}\",\"threadId\":\"${task_thread_id}\",\"taskPrompt\":\"Create a powerpoint deck outline for a launch plan and use the selected skills.\",\"workingDirectory\":\"/tmp/hermes-task\",\"provider\":\"hermes\",\"routing\":{\"routingMode\":\"explicit\",\"explicitExecutionTarget\":\"singleAgent\",\"explicitProviderId\":\"hermes\",\"explicitSkills\":[\"pptx\",\"docx\"],\"allowSkillInstall\":false,\"availableSkills\":[{\"id\":\"pptx\",\"label\":\"pptx\",\"description\":\"slides\",\"installed\":true},{\"id\":\"docx\",\"label\":\"docx\",\"description\":\"docs\",\"installed\":true}]},\"selectedSkills\":[\"pptx\",\"docx\"],\"executionTarget\":\"agent\"}}"
)"
assert_nonempty "task start output" "$(json_get "${task_start}" "result.output")"
task_resolved_skills="$(json_get "${task_start}" "result.resolvedSkills")"
assert_contains "task start resolved skills" "${task_resolved_skills}" "pptx"

task_followup="$(
  rpc_post \
    "${BRIDGE_SERVER_URL%/}/acp/rpc" \
    "{\"jsonrpc\":\"2.0\",\"id\":\"task-message\",\"method\":\"session.message\",\"params\":{\"sessionId\":\"${task_session_id}\",\"threadId\":\"${task_thread_id}\",\"taskPrompt\":\"Continue the same deck outline and keep the structure short.\",\"workingDirectory\":\"/tmp/hermes-task\",\"provider\":\"hermes\",\"routing\":{\"routingMode\":\"explicit\",\"explicitExecutionTarget\":\"singleAgent\",\"explicitProviderId\":\"hermes\",\"explicitSkills\":[\"pptx\",\"docx\"],\"allowSkillInstall\":false,\"availableSkills\":[{\"id\":\"pptx\",\"label\":\"pptx\",\"description\":\"slides\",\"installed\":true},{\"id\":\"docx\",\"label\":\"docx\",\"description\":\"docs\",\"installed\":true}]},\"selectedSkills\":[\"pptx\",\"docx\"],\"executionTarget\":\"agent\"}}"
)"
assert_nonempty "task followup output" "$(json_get "${task_followup}" "result.output")"
assert_contains "task followup upstream method" "$(json_get "${task_followup}" "result.upstreamMethod")" "session/prompt"

echo "Hermes ACP scenarios verified."
