#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${1:?base url is required}"

normalize_url() {
  local value="$1"
  if [[ "${value}" =~ ^https:([^/].*)$ ]]; then
    printf 'https://%s\n' "${BASH_REMATCH[1]}"
    return
  fi
  if [[ "${value}" =~ ^http:([^/].*)$ ]]; then
    printf 'http://%s\n' "${BASH_REMATCH[1]}"
    return
  fi
  printf '%s\n' "${value}"
}

base_url="$(normalize_url "${BASE_URL}")"

ping_json="$(
  curl \
    --silent \
    --show-error \
    --fail \
    --location \
    --max-time 20 \
    "${base_url}/api/ping"
)"

PING_JSON="${ping_json}" python3 - <<'PY'
import json
import os

payload = json.loads(os.environ["PING_JSON"])

if payload.get("status") != "ok":
    raise SystemExit("production ping status not ok")

deployed_image = str(payload.get("image", "")).strip()
deployed_tag = str(payload.get("tag", "")).strip()
deployed_commit = str(payload.get("commit", "")).strip()
deployed_version = str(payload.get("version", "")).strip()

print(f"production_image={deployed_image}")
print(f"production_tag={deployed_tag}")
print(f"production_commit={deployed_commit}")
print(f"production_version={deployed_version}")
PY

