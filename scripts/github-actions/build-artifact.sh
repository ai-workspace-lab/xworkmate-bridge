#!/usr/bin/env bash
set -euo pipefail

ARTIFACT_DIR="${1:-dist}"
mkdir -p "${ARTIFACT_DIR}"

build_commit="$(git rev-parse --short HEAD)"
build_date="$(git log -1 --format=%cI)"

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-X main.buildCommit=${build_commit} -X main.buildVersion=v1.0-beta2 -X main.buildDate=${build_date}" -o "${ARTIFACT_DIR}/xworkmate-bridge" .
