#!/usr/bin/env bash
set -euo pipefail

go mod download
go mod verify
golangci-lint run ./...
go test ./...
bash scripts/github-actions/test-deploy-native-binary.sh
