#!/usr/bin/env bash
set -euo pipefail

TARGET_HOST="${1:?target host is required}"
RUN_APPLY="${2:?run_apply flag is required}"
PLAYBOOK_DIR="${3:-playbooks}"
XWORKMATE_BRIDGE_IMAGE_ARTIFACT_PATH="${XWORKMATE_BRIDGE_IMAGE_ARTIFACT_PATH:?image artifact path is required}"

if [[ ! -f "${XWORKMATE_BRIDGE_IMAGE_ARTIFACT_PATH}" ]]; then
  echo "image artifact not found at ${XWORKMATE_BRIDGE_IMAGE_ARTIFACT_PATH}" >&2
  exit 1
fi

SERVICE_COMPOSE_IMAGE="$(tr -d '\n' < "${XWORKMATE_BRIDGE_IMAGE_ARTIFACT_PATH}" | xargs)"
if [[ -z "${SERVICE_COMPOSE_IMAGE}" ]]; then
  echo "service compose image is empty" >&2
  exit 1
fi

image_no_digest="${SERVICE_COMPOSE_IMAGE%@*}"
image_tag="${image_no_digest##*:}"
if [[ -z "${image_tag}" || "${image_no_digest}" == "${image_tag}" ]]; then
  echo "invalid service image ref: ${SERVICE_COMPOSE_IMAGE}" >&2
  exit 1
fi

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
SERVICE_COMPOSE_IMAGE="${SERVICE_COMPOSE_IMAGE}" \
GHCR_USERNAME="${GHCR_USERNAME:-}" \
GHCR_PASSWORD="${GHCR_PASSWORD:-}" \
BRIDGE_AUTH_TOKEN="${INTERNAL_SERVICE_TOKEN:-}" \
BRIDGE_REVIEW_AUTH_TOKEN="${BRIDGE_REVIEW_AUTH_TOKEN:-}" \
"${args[@]}"
