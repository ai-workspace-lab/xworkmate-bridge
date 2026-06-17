#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT_PATH="${ROOT_DIR}/scripts/github-actions/deploy-native-binary.sh"
EXPECTED_COMMIT="425a38f"

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

assert_file_contains() {
  local file="$1"
  local needle="$2"
  if [[ ! -f "${file}" ]]; then
    fail "expected file to exist: ${file}"
  fi
  if ! grep -qF -- "${needle}" "${file}"; then
    fail "expected ${file} to contain: ${needle}"
  fi
}

build_fake_bin_dir() {
  local bin_dir="$1"

  cat >"${bin_dir}/scp" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
src="$2"
dest="$3"
printf 'scp %s\n' "${dest}" >>"${FAKE_DEPLOY_LOG}"
dest_path="${dest#*:}"
cp "${src}" "${dest_path}"
EOF

  cat >"${bin_dir}/ssh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [[ "${1:-}" == -* ]]; do
  case "${1}" in
    -o|-i|-p|-l)
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
target="$1"
shift
printf 'ssh %s\n' "${target}" >>"${FAKE_DEPLOY_LOG}"
cmd="$*"
case "${cmd}" in
  cat\ *)
    path="${cmd#cat }"
    path="${path%% 2>/dev/null*}"
    path="${path%% || true*}"
    path="${path%\'}"
    path="${path#\'}"
    cat "${path}" 2>/dev/null || true
    ;;
  *)
    bash -c "${cmd}"
    ;;
esac
EOF

  cat >"${bin_dir}/systemctl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'systemctl %s\n' "$*" >>"${FAKE_DEPLOY_LOG}"
if [[ "${1:-}" == "is-active" ]]; then
  exit 1
fi
if [[ "${1:-}" == "--user" && "${2:-}" == "show" ]]; then
  printf '1\n'
  exit 0
fi
exit 0
EOF

  cat >"${bin_dir}/readlink" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "-f" && "${2:-}" == /proc/*/exe ]]; then
  printf '%s\n' "${REMOTE_BINARY}"
  exit 0
fi
/usr/bin/readlink "$@"
EOF

  cat >"${bin_dir}/sleep" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
exit 0
EOF

  chmod +x "${bin_dir}/"*
}

create_fake_binary() {
  local path="$1"
  cat >"${path}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "version" ]]; then
  printf '{"commit":"425a38f","version":"test"}\n'
  exit 0
fi
sleep 3600
EOF
  chmod +x "${path}"
}

setup_test_env() {
  local label="$1"
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  local bin_dir="${tmp_dir}/bin"
  local remote_dir="${tmp_dir}/remote"
  mkdir -p "${bin_dir}" "${remote_dir}/tmp" "${remote_dir}/opt/cloud-neutral/xworkmate-bridge"

  create_fake_binary "${tmp_dir}/xworkmate-bridge"
  build_fake_bin_dir "${bin_dir}"

  printf '%s\n' "${label}" >&2
  echo "${tmp_dir}"
}

run_deploy() {
  local bin_dir="$1"
  local log_file="$2"
  shift 2
  env \
    PATH="${bin_dir}:${PATH}" \
    FAKE_DEPLOY_LOG="${log_file}" \
    "$@" \
    bash "${SCRIPT_PATH}" "example.test" "${bin_dir}/../xworkmate-bridge" "${EXPECTED_COMMIT}"
}

run_env_token_case() {
  local tmp_dir
  tmp_dir="$(setup_test_env "case: AI_WORKSPACE_AUTH_TOKEN env var drives the unit file")"

  local log_file="${tmp_dir}/deploy.log"
  local unit_file="${tmp_dir}/remote/home/ubuntu/.config/systemd/user/xworkmate-bridge.service"
  run_deploy "${tmp_dir}/bin" "${log_file}" \
    REMOTE_TMP="${tmp_dir}/remote/tmp/xworkmate-bridge-${EXPECTED_COMMIT}" \
    REMOTE_BINARY="${tmp_dir}/remote/home/ubuntu/.local/bin/xworkmate-go-core" \
    REMOTE_WORKING_DIR="${tmp_dir}/remote/opt/cloud-neutral/xworkmate-bridge" \
    BRIDGE_CONFIG_PATH="${tmp_dir}/remote/opt/cloud-neutral/xworkmate-bridge/config.yaml" \
    USER_SYSTEMD_DIR="${tmp_dir}/remote/home/ubuntu/.config/systemd/user" \
    DEPLOY_NATIVE_SKIP_PROC_CHECK=true \
    AI_WORKSPACE_AUTH_TOKEN="test-token"

  local log_output
  log_output="$(cat "${log_file}")"
  assert_contains "${log_output}" "ssh root@example.test"
  assert_contains "${log_output}" "scp ubuntu@example.test:"
  assert_contains "${log_output}" "ssh ubuntu@example.test"
  assert_contains "${log_output}" "systemctl --user restart xworkmate-bridge.service"
  assert_file_contains "${unit_file}" 'Environment="AI_WORKSPACE_AUTH_TOKEN=test-token"'
  assert_file_contains "${unit_file}" "WantedBy=default.target"

  rm -rf "${tmp_dir}"
}

run_unit_fallback_case() {
  local tmp_dir
  tmp_dir="$(setup_test_env "case: AI_WORKSPACE_AUTH_TOKEN recovered from system service unit file")"

  local system_unit_dir="${tmp_dir}/remote/etc/systemd/system"
  mkdir -p "${system_unit_dir}"
  local system_unit_file="${system_unit_dir}/xworkmate-bridge.service"
  cat >"${system_unit_file}" <<'EOF'
[Unit]
Description=Stale system service
[Service]
Environment="AI_WORKSPACE_AUTH_TOKEN=recovered-from-systemd"
Environment="BRIDGE_REVIEW_AUTH_TOKEN=recovered-review-token"
ExecStart=/bin/true
EOF

  local log_file="${tmp_dir}/deploy.log"
  local unit_file="${tmp_dir}/remote/home/ubuntu/.config/systemd/user/xworkmate-bridge.service"
  run_deploy "${tmp_dir}/bin" "${log_file}" \
    REMOTE_TMP="${tmp_dir}/remote/tmp/xworkmate-bridge-${EXPECTED_COMMIT}" \
    REMOTE_BINARY="${tmp_dir}/remote/home/ubuntu/.local/bin/xworkmate-go-core" \
    REMOTE_WORKING_DIR="${tmp_dir}/remote/opt/cloud-neutral/xworkmate-bridge" \
    BRIDGE_CONFIG_PATH="${tmp_dir}/remote/opt/cloud-neutral/xworkmate-bridge/config.yaml" \
    USER_SYSTEMD_DIR="${tmp_dir}/remote/home/ubuntu/.config/systemd/user" \
    SYSTEM_SERVICE_UNIT_PATH="${system_unit_file}" \
    DEPLOY_NATIVE_SKIP_PROC_CHECK=true

  assert_file_contains "${unit_file}" 'Environment="AI_WORKSPACE_AUTH_TOKEN=recovered-from-systemd"'
  assert_file_contains "${unit_file}" 'Environment="BRIDGE_REVIEW_AUTH_TOKEN=recovered-review-token"'

  rm -rf "${tmp_dir}"
}

run_fail_fast_case() {
  local tmp_dir
  tmp_dir="$(setup_test_env "case: missing AI_WORKSPACE_AUTH_TOKEN fails fast with clear error")"

  local log_file="${tmp_dir}/deploy.log"
  local stderr_file="${tmp_dir}/deploy.stderr"
  set +e
  run_deploy "${tmp_dir}/bin" "${log_file}" \
    REMOTE_TMP="${tmp_dir}/remote/tmp/xworkmate-bridge-${EXPECTED_COMMIT}" \
    REMOTE_BINARY="${tmp_dir}/remote/home/ubuntu/.local/bin/xworkmate-go-core" \
    REMOTE_WORKING_DIR="${tmp_dir}/remote/opt/cloud-neutral/xworkmate-bridge" \
    BRIDGE_CONFIG_PATH="${tmp_dir}/remote/opt/cloud-neutral/xworkmate-bridge/config.yaml" \
    USER_SYSTEMD_DIR="${tmp_dir}/remote/home/ubuntu/.config/systemd/user" \
    DEPLOY_NATIVE_SKIP_PROC_CHECK=true \
    2>"${stderr_file}"
  local exit_code=$?
  set -e

  if [[ "${exit_code}" == "0" ]]; then
    fail "expected deploy to fail when AI_WORKSPACE_AUTH_TOKEN is empty and no system service unit exists"
  fi
  assert_contains "$(cat "${stderr_file}")" "AI_WORKSPACE_AUTH_TOKEN is required"

  rm -rf "${tmp_dir}"
}

main() {
  run_env_token_case
  run_unit_fallback_case
  run_fail_fast_case
  printf 'deploy-native-binary regression tests passed\n'
}

main "$@"
