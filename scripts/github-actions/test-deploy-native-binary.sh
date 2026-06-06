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

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

mkdir -p "${tmp_dir}/bin" "${tmp_dir}/remote/tmp" "${tmp_dir}/remote/opt/cloud-neutral/xworkmate-bridge"

fake_binary="${tmp_dir}/xworkmate-bridge"
cat >"${fake_binary}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "version" ]]; then
  printf '{"commit":"425a38f","version":"test"}\n'
  exit 0
fi
sleep 3600
EOF
chmod +x "${fake_binary}"

cat >"${tmp_dir}/bin/scp" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
src="$2"
dest="$3"
printf 'scp %s\n' "${dest}" >>"${FAKE_DEPLOY_LOG}"
dest_path="${dest#*:}"
cp "${src}" "${dest_path}"
EOF

cat >"${tmp_dir}/bin/ssh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
target="$1"
shift
printf 'ssh %s\n' "${target}" >>"${FAKE_DEPLOY_LOG}"
bash -c "$*"
EOF

cat >"${tmp_dir}/bin/systemctl" <<'EOF'
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

cat >"${tmp_dir}/bin/readlink" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "-f" && "${2:-}" == /proc/*/exe ]]; then
  printf '%s\n' "${REMOTE_BINARY}"
  exit 0
fi
/usr/bin/readlink "$@"
EOF

cat >"${tmp_dir}/bin/sleep" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
exit 0
EOF

chmod +x "${tmp_dir}/bin/"*

log_file="${tmp_dir}/deploy.log"
PATH="${tmp_dir}/bin:${PATH}" \
FAKE_DEPLOY_LOG="${log_file}" \
REMOTE_TMP="${tmp_dir}/remote/tmp/xworkmate-bridge-${EXPECTED_COMMIT}" \
REMOTE_BINARY="${tmp_dir}/remote/home/ubuntu/.local/bin/xworkmate-go-core" \
REMOTE_WORKING_DIR="${tmp_dir}/remote/opt/cloud-neutral/xworkmate-bridge" \
BRIDGE_CONFIG_PATH="${tmp_dir}/remote/opt/cloud-neutral/xworkmate-bridge/config.yaml" \
USER_SYSTEMD_DIR="${tmp_dir}/remote/home/ubuntu/.config/systemd/user" \
DEPLOY_NATIVE_SKIP_PROC_CHECK=true \
BRIDGE_AUTH_TOKEN="test-token" \
bash "${SCRIPT_PATH}" "example.test" "${fake_binary}" "${EXPECTED_COMMIT}"

log_output="$(cat "${log_file}")"
assert_contains "${log_output}" "ssh root@example.test"
assert_contains "${log_output}" "scp ubuntu@example.test:"
assert_contains "${log_output}" "ssh ubuntu@example.test"
assert_contains "${log_output}" "systemctl --user restart xworkmate-bridge.service"

unit_file="${tmp_dir}/remote/home/ubuntu/.config/systemd/user/xworkmate-bridge.service"
if [[ ! -f "${unit_file}" ]]; then
  fail "expected user service unit to be written"
fi

unit_output="$(cat "${unit_file}")"
assert_contains "${unit_output}" "ExecStart=${tmp_dir}/remote/home/ubuntu/.local/bin/xworkmate-go-core serve --listen 127.0.0.1:8787"
assert_contains "${unit_output}" 'Environment="BRIDGE_AUTH_TOKEN=test-token"'
assert_contains "${unit_output}" "WantedBy=default.target"

printf 'deploy-native-binary regression tests passed\n'
