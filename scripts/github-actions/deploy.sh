#!/usr/bin/env bash
set -euo pipefail

TARGET_HOST="${1:?target host is required}"
RUN_APPLY="${2:?run_apply flag is required}"
PLAYBOOK_DIR="${3:-playbooks}"

cd "${PLAYBOOK_DIR}"

args=(
  ansible-playbook
  -i inventory.ini
  deploy_xworkmate_bridge_vhosts.yml
  --tags xworkmate_bridge
  -l "${TARGET_HOST}"
)

if [[ "${RUN_APPLY}" != "true" ]]; then
  args+=(-C)
fi

ANSIBLE_CONFIG="${PWD}/ansible.cfg" \
BRIDGE_AUTH_TOKEN="${INTERNAL_SERVICE_TOKEN:-}" \
BRIDGE_REVIEW_AUTH_TOKEN="${BRIDGE_REVIEW_AUTH_TOKEN:-}" \
"${args[@]}"
