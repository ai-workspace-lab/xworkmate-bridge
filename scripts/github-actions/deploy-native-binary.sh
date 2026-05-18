#!/usr/bin/env bash
set -euo pipefail

TARGET_HOST="${1:?target host is required}"
BINARY_PATH="${2:?binary path is required}"
EXPECTED_COMMIT="${3:?expected short commit is required}"
REMOTE_TMP="/tmp/xworkmate-bridge-${EXPECTED_COMMIT}"
REMOTE_BINARY="/usr/local/bin/xworkmate-go-core"
STALE_DROPIN="/etc/systemd/system/xworkmate-bridge.service.d/10-hotfix-openclaw-artifacts.conf"
SERVICE_NAME="xworkmate-bridge.service"

if [[ ! "${TARGET_HOST}" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "invalid target host: ${TARGET_HOST}" >&2
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

chmod +x "${BINARY_PATH}"
scp -q "${BINARY_PATH}" "root@${TARGET_HOST}:${REMOTE_TMP}"

ssh "root@${TARGET_HOST}" "EXPECTED_COMMIT='${EXPECTED_COMMIT}' REMOTE_TMP='${REMOTE_TMP}' REMOTE_BINARY='${REMOTE_BINARY}' STALE_DROPIN='${STALE_DROPIN}' SERVICE_NAME='${SERVICE_NAME}' bash -s" <<'REMOTE'
set -euo pipefail

had_immutable=0
restore_immutable() {
  if [[ "${had_immutable}" == "1" ]] && command -v chattr >/dev/null 2>&1 && [[ -e "${REMOTE_BINARY}" ]]; then
    chattr +i "${REMOTE_BINARY}" 2>/dev/null || true
  fi
}
trap restore_immutable EXIT

if command -v lsattr >/dev/null 2>&1 && [[ -e "${REMOTE_BINARY}" ]]; then
  attrs="$(lsattr "${REMOTE_BINARY}" 2>/dev/null || true)"
  if [[ "${attrs}" == *i* ]]; then
    had_immutable=1
    chattr -i "${REMOTE_BINARY}"
  fi
fi

install -o root -g root -m 0755 "${REMOTE_TMP}" "${REMOTE_BINARY}"

restore_immutable

rm -f "${STALE_DROPIN}"
rmdir --ignore-fail-on-non-empty "$(dirname "${STALE_DROPIN}")" 2>/dev/null || true

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

systemctl daemon-reload
systemctl restart "${SERVICE_NAME}"
deadline=$((SECONDS + 20))
actual_exe=""
pid=""
while (( SECONDS < deadline )); do
  pid="$(systemctl show -p MainPID --value "${SERVICE_NAME}")"
  if [[ -n "${pid}" && "${pid}" != "0" && -e "/proc/${pid}/exe" ]]; then
    actual_exe="$(readlink -f "/proc/${pid}/exe" 2>/dev/null || true)"
    if [[ "${actual_exe}" == "${REMOTE_BINARY}" ]]; then
      exit 0
    fi
  fi
  sleep 1
done
if [[ -z "${pid}" || "${pid}" == "0" ]]; then
  echo "${SERVICE_NAME} did not start" >&2
  exit 1
fi
echo "${SERVICE_NAME} is not running ${REMOTE_BINARY}; pid=${pid}; actual=${actual_exe:-unknown}" >&2
exit 1
REMOTE
