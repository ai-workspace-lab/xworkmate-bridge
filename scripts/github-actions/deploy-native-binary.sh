#!/usr/bin/env bash
set -euo pipefail

TARGET_HOST="${1:?target host is required}"
BINARY_PATH="${2:?binary path is required}"
EXPECTED_COMMIT="${3:?expected short commit is required}"

DEPLOY_USER="${DEPLOY_USER:-ubuntu}"
REMOTE_TMP="/tmp/xworkmate-bridge-${EXPECTED_COMMIT}"
REMOTE_BINARY="${REMOTE_BINARY:-/home/${DEPLOY_USER}/.local/bin/xworkmate-go-core}"
REMOTE_WORKING_DIR="${REMOTE_WORKING_DIR:-/opt/cloud-neutral/xworkmate-bridge}"
BRIDGE_CONFIG_PATH="${BRIDGE_CONFIG_PATH:-/opt/cloud-neutral/xworkmate-bridge/config.yaml}"
SERVICE_NAME="${SERVICE_NAME:-xworkmate-bridge.service}"
SERVICE_LISTEN_ADDR="${SERVICE_LISTEN_ADDR:-127.0.0.1:8787}"
USER_SYSTEMD_DIR="${USER_SYSTEMD_DIR:-/home/${DEPLOY_USER}/.config/systemd/user}"
SYSTEM_SERVICE_NAME="${SYSTEM_SERVICE_NAME:-xworkmate-bridge.service}"
MIGRATE_SYSTEM_SERVICE="${MIGRATE_SYSTEM_SERVICE:-true}"
SYSTEM_MIGRATION_USER="${SYSTEM_MIGRATION_USER:-root}"
STALE_DROPIN="${STALE_DROPIN:-/etc/systemd/system/xworkmate-bridge.service.d/10-hotfix-openclaw-artifacts.conf}"
DEPLOY_NATIVE_SKIP_PROC_CHECK="${DEPLOY_NATIVE_SKIP_PROC_CHECK:-false}"

if [[ ! "${TARGET_HOST}" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "invalid target host: ${TARGET_HOST}" >&2
  exit 1
fi

if [[ ! "${DEPLOY_USER}" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "invalid deploy user: ${DEPLOY_USER}" >&2
  exit 1
fi

if [[ ! "${EXPECTED_COMMIT}" =~ ^[0-9a-f]{7,40}$ ]]; then
  echo "invalid expected commit: ${EXPECTED_COMMIT}" >&2
  exit 1
fi

if [[ ! -f "${BINARY_PATH}" ]]; then
  echo "native bridge binary not found: ${BINARY_PATH}" >&2
  exit 1
fi

escape_systemd_env() {
  python3 - "$1" <<'PY'
import sys
value = sys.argv[1]
print(value.replace("\\", "\\\\").replace('"', '\\"'))
PY
}

AUTH_TOKEN_LINE=""
if [[ -n "${BRIDGE_AUTH_TOKEN:-}" ]]; then
  AUTH_TOKEN_LINE="Environment=\"BRIDGE_AUTH_TOKEN=$(escape_systemd_env "${BRIDGE_AUTH_TOKEN}")\""
fi

REVIEW_TOKEN_LINE=""
if [[ -n "${BRIDGE_REVIEW_AUTH_TOKEN:-}" ]]; then
  REVIEW_TOKEN_LINE="Environment=\"BRIDGE_REVIEW_AUTH_TOKEN=$(escape_systemd_env "${BRIDGE_REVIEW_AUTH_TOKEN}")\""
fi
UNIT_ENV_LINES_B64="$(printf '%s\n%s\n' "${AUTH_TOKEN_LINE}" "${REVIEW_TOKEN_LINE}" | base64 | tr -d '\n')"

chmod +x "${BINARY_PATH}"

if [[ "${MIGRATE_SYSTEM_SERVICE}" == "true" ]]; then
  ssh "${SYSTEM_MIGRATION_USER}@${TARGET_HOST}" \
    "SYSTEM_SERVICE_NAME='${SYSTEM_SERVICE_NAME}' STALE_DROPIN='${STALE_DROPIN}' bash -s" <<'REMOTE_ROOT'
set -euo pipefail

if systemctl list-unit-files "${SYSTEM_SERVICE_NAME}" >/dev/null 2>&1; then
  systemctl disable --now "${SYSTEM_SERVICE_NAME}" >/dev/null 2>&1 || systemctl stop "${SYSTEM_SERVICE_NAME}" >/dev/null 2>&1 || true
fi

rm -f "${STALE_DROPIN}"
rmdir --ignore-fail-on-non-empty "$(dirname "${STALE_DROPIN}")" 2>/dev/null || true
systemctl daemon-reload
REMOTE_ROOT
fi

scp -q "${BINARY_PATH}" "${DEPLOY_USER}@${TARGET_HOST}:${REMOTE_TMP}"

ssh "${DEPLOY_USER}@${TARGET_HOST}" \
  "EXPECTED_COMMIT='${EXPECTED_COMMIT}' REMOTE_TMP='${REMOTE_TMP}' REMOTE_BINARY='${REMOTE_BINARY}' REMOTE_WORKING_DIR='${REMOTE_WORKING_DIR}' BRIDGE_CONFIG_PATH='${BRIDGE_CONFIG_PATH}' SERVICE_NAME='${SERVICE_NAME}' SERVICE_LISTEN_ADDR='${SERVICE_LISTEN_ADDR}' USER_SYSTEMD_DIR='${USER_SYSTEMD_DIR}' SYSTEM_SERVICE_NAME='${SYSTEM_SERVICE_NAME}' UNIT_ENV_LINES_B64='${UNIT_ENV_LINES_B64}' DEPLOY_NATIVE_SKIP_PROC_CHECK='${DEPLOY_NATIVE_SKIP_PROC_CHECK}' bash -s" <<'REMOTE'
set -euo pipefail

mkdir -p "$(dirname "${REMOTE_BINARY}")" "${USER_SYSTEMD_DIR}"
install -m 0755 "${REMOTE_TMP}" "${REMOTE_BINARY}"
rm -f "${REMOTE_TMP}"

version_json="$("${REMOTE_BINARY}" version)"
actual_commit="$(VERSION_JSON="${version_json}" python3 - <<'PY'
import json
import os
payload = json.loads(os.environ["VERSION_JSON"])
print(str(payload.get("commit", "")))
PY
)"
if [[ "${actual_commit}" != "${EXPECTED_COMMIT}" ]]; then
  echo "deployed binary commit mismatch: expected ${EXPECTED_COMMIT}, got ${actual_commit}" >&2
  exit 1
fi

unit_env_lines="$(printf '%s' "${UNIT_ENV_LINES_B64}" | base64 -d | sed '/^$/d')"
existing_env="$(
  {
    systemctl --user show -p Environment --value "${SERVICE_NAME}" 2>/dev/null || true
    systemctl show -p Environment --value "${SYSTEM_SERVICE_NAME}" 2>/dev/null || true
  } | sed '/^$/d' | head -n 1
)"
unit_env_lines="$(
  UNIT_ENV_LINES="${unit_env_lines}" EXISTING_ENV="${existing_env}" python3 - <<'PY'
import os
import shlex

lines = [line for line in os.environ.get("UNIT_ENV_LINES", "").splitlines() if line.strip()]
present = set()
for line in lines:
    prefix = 'Environment="'
    if line.startswith(prefix):
        key = line[len(prefix):].split("=", 1)[0]
        present.add(key)

for item in shlex.split(os.environ.get("EXISTING_ENV", "")):
    key, sep, value = item.partition("=")
    if sep and key in {"BRIDGE_AUTH_TOKEN", "BRIDGE_REVIEW_AUTH_TOKEN"} and key not in present:
        escaped = value.replace("\\", "\\\\").replace('"', '\\"')
        lines.append(f'Environment="{key}={escaped}"')
        present.add(key)

print("\n".join(lines))
PY
)"

cat >"${USER_SYSTEMD_DIR}/${SERVICE_NAME}" <<UNIT
[Unit]
Description=XWorkmate bridge control plane
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=${REMOTE_WORKING_DIR}
Environment="HOME=/home/${USER}"
Environment="TERM=xterm-256color"
Environment="BRIDGE_CONFIG_PATH=${BRIDGE_CONFIG_PATH}"
${unit_env_lines}
ExecStart=${REMOTE_BINARY} serve --listen ${SERVICE_LISTEN_ADDR}
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
UNIT

systemctl --user daemon-reload
systemctl --user enable "${SERVICE_NAME}" >/dev/null

if systemctl is-active --quiet "${SYSTEM_SERVICE_NAME}" 2>/dev/null; then
  echo "${SYSTEM_SERVICE_NAME} is still active as a system service; disable it before starting the user service" >&2
  exit 1
fi

systemctl --user restart "${SERVICE_NAME}"
deadline=$((SECONDS + 20))
actual_exe=""
pid=""
while (( SECONDS < deadline )); do
  pid="$(systemctl --user show -p MainPID --value "${SERVICE_NAME}")"
  if [[ -n "${pid}" && "${pid}" != "0" ]] && [[ "${DEPLOY_NATIVE_SKIP_PROC_CHECK}" == "true" || -e "/proc/${pid}/exe" ]]; then
    actual_exe="$(readlink -f "/proc/${pid}/exe" 2>/dev/null || true)"
    if [[ "${actual_exe}" == "${REMOTE_BINARY}" ]]; then
      exit 0
    fi
  fi
  sleep 1
done
if [[ -z "${pid}" || "${pid}" == "0" ]]; then
  echo "${SERVICE_NAME} did not start as a user service" >&2
  exit 1
fi
echo "${SERVICE_NAME} is not running ${REMOTE_BINARY}; pid=${pid}; actual=${actual_exe:-unknown}" >&2
exit 1
REMOTE
