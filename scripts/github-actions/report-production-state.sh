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

ping_url="${base_url}/api/ping"
ping_json=""
attempts=6
sleep_seconds=5

for ((attempt = 1; attempt <= attempts; attempt += 1)); do
  if ping_json="$(
    curl \
      --silent \
      --show-error \
      --fail \
      --location \
      --max-time 20 \
      "${ping_url}"
  )"; then
    if [[ -n "${ping_json}" ]]; then
      break
    fi
  fi

  if (( attempt == attempts )); then
    echo "failed to probe bridge ping at ${ping_url} after ${attempts} attempts" >&2
    exit 1
  fi

  echo "bridge ping at ${ping_url} attempt ${attempt}/${attempts} failed; retrying in ${sleep_seconds}s" >&2
  sleep "${sleep_seconds}"
done

PING_JSON="${ping_json}" python3 - <<'PY'
import json
import os
import sys

ping_json = os.environ.get("PING_JSON", "")
if not ping_json:
    print("empty ping response", file=sys.stderr)
    sys.exit(1)

try:
    payload = json.loads(ping_json)
except json.JSONDecodeError as exc:
    print(f"bridge ping returned invalid JSON: {exc}\nBody: {ping_json!r}", file=sys.stderr)
    sys.exit(1)

if payload.get("status") != "ok":
    print("production ping status not ok", file=sys.stderr)
    sys.exit(1)

deployed_image = str(payload.get("image", "")).strip()
deployed_tag = str(payload.get("tag", "")).strip()
deployed_commit = str(payload.get("commit", "")).strip()
deployed_version = str(payload.get("version", "")).strip()

print(f"production_image={deployed_image}")
print(f"production_tag={deployed_tag}")
print(f"production_commit={deployed_commit}")
print(f"production_version={deployed_version}")
PY

