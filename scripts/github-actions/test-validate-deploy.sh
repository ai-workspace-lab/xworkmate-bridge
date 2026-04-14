#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT_PATH="${ROOT_DIR}/scripts/github-actions/validate-deploy.sh"
IMAGE_REF="ghcr.io/x-evor/xworkmate-bridge:425a38f1e8076899400d4a858d4678dffd876afb"

RUN_OUTPUT=""
RUN_STATUS=0
RUN_TMP_DIR=""
RUN_STATE_DIR=""

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local haystack="$1"
  local needle="$2"
  if [[ "${haystack}" != *"${needle}"* ]]; then
    fail "expected output to contain: ${needle}"
  fi
}

assert_not_contains() {
  local haystack="$1"
  local needle="$2"
  if [[ "${haystack}" == *"${needle}"* ]]; then
    fail "expected output to not contain: ${needle}"
  fi
}

cleanup_run() {
  if [[ -n "${RUN_TMP_DIR}" && -d "${RUN_TMP_DIR}" ]]; then
    rm -rf "${RUN_TMP_DIR}"
  fi
  RUN_OUTPUT=""
  RUN_STATUS=0
  RUN_TMP_DIR=""
  RUN_STATE_DIR=""
}

create_fake_tools() {
  local scenario="$1"
  local tmp_dir="$2"

  mkdir -p "${tmp_dir}/bin" "${tmp_dir}/state"

  cat >"${tmp_dir}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

state_dir="${FAKE_CURL_STATE_DIR:?}"
scenario="${FAKE_CURL_SCENARIO:?}"

url="${@: -1}"
data=""
write_out=""

for ((i = 1; i <= $#; i += 1)); do
  arg="${!i}"
  case "${arg}" in
    --data)
      next_index=$((i + 1))
      data="${!next_index}"
      ;;
    --write-out)
      next_index=$((i + 1))
      write_out="${!next_index}"
      ;;
  esac
done

counter_file() {
  printf '%s/%s.count\n' "${state_dir}" "$1"
}

read_count() {
  local file
  file="$(counter_file "$1")"
  if [[ -f "${file}" ]]; then
    cat "${file}"
    return
  fi
  printf '0\n'
}

bump_count() {
  local name="$1"
  local file value
  file="$(counter_file "${name}")"
  value=$(( $(read_count "${name}") + 1 ))
  printf '%s\n' "${value}" >"${file}"
  printf '%s\n' "${value}"
}

if [[ -n "${write_out}" ]]; then
  printf '200'
  exit 0
fi

case "${scenario}" in
  bridge-timeout)
    case "${url}" in
      https://xworkmate-bridge.svc.plus/api/ping)
        printf '{"status":"ok","image":"ghcr.io/x-evor/xworkmate-bridge:425a38f1e8076899400d4a858d4678dffd876afb","tag":"425a38f1e8076899400d4a858d4678dffd876afb","commit":"425a38f1e8076899400d4a858d4678dffd876afb","version":"425a38f1e8076899400d4a858d4678dffd876afb"}\n'
        ;;
      https://xworkmate-bridge.svc.plus/)
        printf 'xworkmate-bridge is running\n'
        ;;
      https://acp-server.svc.plus/*/acp/rpc)
        printf '{"jsonrpc":"2.0","result":{"providers":["ok"]}}\n'
        ;;
      https://xworkmate-bridge.svc.plus/acp/rpc)
        printf 'curl: (28) Operation timed out after 20001 milliseconds with 0 bytes received\n' >&2
        exit 1
        ;;
      *)
        printf 'unexpected url in bridge-timeout scenario: %s\n' "${url}" >&2
        exit 1
        ;;
    esac
    ;;
  retry-success)
    case "${url}" in
      https://xworkmate-bridge.svc.plus/api/ping)
        ping_attempt="$(bump_count ping)"
        if (( ping_attempt < 3 )); then
          printf 'curl: (28) Operation timed out after 20001 milliseconds with 0 bytes received\n' >&2
          exit 1
        fi
        printf '{"status":"ok","image":"ghcr.io/x-evor/xworkmate-bridge:425a38f1e8076899400d4a858d4678dffd876afb","tag":"425a38f1e8076899400d4a858d4678dffd876afb","commit":"425a38f1e8076899400d4a858d4678dffd876afb","version":"425a38f1e8076899400d4a858d4678dffd876afb"}\n'
        ;;
      https://xworkmate-bridge.svc.plus/)
        printf 'xworkmate-bridge is running\n'
        ;;
      https://acp-server.svc.plus/*/acp/rpc)
        printf '{"jsonrpc":"2.0","result":{"providers":["ok"]}}\n'
        ;;
      https://xworkmate-bridge.svc.plus/acp/rpc)
        if [[ "${data}" == *'"providerId":"codex"'* ]]; then
          printf '{"jsonrpc":"2.0","result":{"success":true,"providerId":"codex","capabilities":{"providers":["codex"]}}}\n'
          exit 0
        fi
        if [[ "${data}" == *'"providerId":"opencode"'* ]]; then
          printf '{"jsonrpc":"2.0","result":{"success":true,"providerId":"opencode","capabilities":{"providers":["opencode"]}}}\n'
          exit 0
        fi
        if [[ "${data}" == *'"providerId":"gemini"'* ]]; then
          printf '{"jsonrpc":"2.0","result":{"success":true,"providerId":"gemini","capabilities":{"providers":["gemini"]}}}\n'
          exit 0
        fi
        printf 'unexpected bridge probe payload in retry-success scenario: %s\n' "${data}" >&2
        exit 1
        ;;
      *)
        printf 'unexpected url in retry-success scenario: %s\n' "${url}" >&2
        exit 1
        ;;
    esac
    ;;
  *)
    printf 'unsupported fake curl scenario: %s\n' "${scenario}" >&2
    exit 1
    ;;
esac
EOF

  cat >"${tmp_dir}/bin/sleep" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
exit 0
EOF

  chmod +x "${tmp_dir}/bin/curl" "${tmp_dir}/bin/sleep"
}

run_validate_capture() {
  local scenario="$1"
  cleanup_run

  RUN_TMP_DIR="$(mktemp -d)"
  RUN_STATE_DIR="${RUN_TMP_DIR}/state"
  create_fake_tools "${scenario}" "${RUN_TMP_DIR}"

  set +e
  RUN_OUTPUT="$(
    PATH="${RUN_TMP_DIR}/bin:${PATH}" \
    FAKE_CURL_SCENARIO="${scenario}" \
    FAKE_CURL_STATE_DIR="${RUN_STATE_DIR}" \
    BRIDGE_SERVER_URL="https://xworkmate-bridge.svc.plus" \
    OPENCLAW_URL="wss://openclaw.svc.plus" \
    CODEX_RPC_URL="https://acp-server.svc.plus/codex/acp/rpc" \
    OPENCODE_RPC_URL="https://acp-server.svc.plus/opencode/acp/rpc" \
    GEMINI_RPC_URL="https://acp-server.svc.plus/gemini/acp/rpc" \
    INTERNAL_SERVICE_TOKEN="test-token" \
    bash "${SCRIPT_PATH}" "${IMAGE_REF}" 2>&1
  )"
  RUN_STATUS=$?
  set -e
}

test_bridge_timeout_stops_without_json_decode_noise() {
  run_validate_capture "bridge-timeout"

  if [[ "${RUN_STATUS}" -eq 0 ]]; then
    fail "expected bridge-timeout scenario to fail"
  fi

  assert_contains "${RUN_OUTPUT}" "bridge rpc https://xworkmate-bridge.svc.plus/acp/rpc request failed"
  assert_not_contains "${RUN_OUTPUT}" "JSONDecodeError"
  cleanup_run
}

test_ping_retry_reaches_successful_release_validation() {
  run_validate_capture "retry-success"

  if [[ "${RUN_STATUS}" -ne 0 ]]; then
    printf '%s\n' "${RUN_OUTPUT}" >&2
    fail "expected retry-success scenario to pass"
  fi

  if [[ ! -f "${RUN_STATE_DIR}/ping.count" ]]; then
    fail "expected ping retry counter to be recorded"
  fi

  ping_attempts="$(tr -d '\n' <"${RUN_STATE_DIR}/ping.count")"
  if [[ "${ping_attempts}" != "3" ]]; then
    fail "expected ping to succeed on third attempt, got ${ping_attempts}"
  fi

  cleanup_run
}

test_bridge_timeout_stops_without_json_decode_noise
test_ping_retry_reaches_successful_release_validation

printf 'validate-deploy regression tests passed\n'
