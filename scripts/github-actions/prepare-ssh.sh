#!/usr/bin/env bash
set -euo pipefail

TARGET_HOST="${1:?target host is required}"
SSH_KNOWN_HOSTS_PAYLOAD="${2:-}"

if [[ -z "${SINGLE_NODE_VPS_SSH_PRIVATE_KEY:-}" && -z "${SINGLE_NODE_VPS_SSH_PRIVATE_KEY_B64:-}" ]]; then
  echo "::error::SINGLE_NODE_VPS_SSH_PRIVATE_KEY or SINGLE_NODE_VPS_SSH_PRIVATE_KEY_B64 is required"
  exit 1
fi

mkdir -p "${HOME}/.ssh"
chmod 700 "${HOME}/.ssh"

if [[ -n "${SINGLE_NODE_VPS_SSH_PRIVATE_KEY_B64:-}" ]]; then
  printf '%s' "${SINGLE_NODE_VPS_SSH_PRIVATE_KEY_B64}" | base64 -d > "${HOME}/.ssh/id_rsa"
else
  python3 .github/scripts/normalize-private-key.py normalize > "${HOME}/.ssh/id_rsa"
fi
chmod 600 "${HOME}/.ssh/id_rsa"
ssh-keygen -y -f "${HOME}/.ssh/id_rsa" >/dev/null

touch "${HOME}/.ssh/known_hosts"
chmod 600 "${HOME}/.ssh/known_hosts"

if [[ -n "${SSH_KNOWN_HOSTS_PAYLOAD}" ]]; then
  printf '%s\n' "${SSH_KNOWN_HOSTS_PAYLOAD}" >> "${HOME}/.ssh/known_hosts"
fi

ssh-keyscan -H "${TARGET_HOST}" >> "${HOME}/.ssh/known_hosts" 2>/dev/null || true
