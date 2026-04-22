#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
OUTPUT_DIR="${OUTPUT_DIR:-$ROOT_DIR/build/bin}"
OUTPUT_PATH_BASE="${OUTPUT_DIR}/xworkmate-go-core"

if [[ "$(uname -s)" == *MINGW* || "$(uname -s)" == *MSYS* || "$(uname -s)" == *CYGWIN* ]]; then
  OUTPUT_PATH="${OUTPUT_PATH:-${OUTPUT_PATH_BASE}.exe}"
else
  OUTPUT_PATH="${OUTPUT_PATH:-${OUTPUT_PATH_BASE}}"
fi

if [[ ! -f "$ROOT_DIR/go.mod" ]]; then
  echo "Missing go.mod in $ROOT_DIR" >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "Go toolchain is required to build xworkmate-go-core" >&2
  exit 1
fi

mkdir -p "$OUTPUT_DIR"

echo "Building xworkmate-go-core from xworkmate-bridge..."
(
  cd "$ROOT_DIR"
  build_commit="$(git rev-parse --short HEAD)"
  build_date="$(git log -1 --format=%cI)"
  GO111MODULE=on go build -ldflags "-X main.buildCommit=${build_commit} -X main.buildVersion=v1.0-beta2 -X main.buildDate=${build_date}" -o "$OUTPUT_PATH" .
)

chmod +x "$OUTPUT_PATH"
echo "Built: $OUTPUT_PATH"
