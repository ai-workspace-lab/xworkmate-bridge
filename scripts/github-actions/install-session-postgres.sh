#!/usr/bin/env bash
set -euo pipefail

sudo apt-get update
sudo apt-get install -y postgresql-17
test -x /usr/lib/postgresql/17/bin/initdb
test -x /usr/lib/postgresql/17/bin/pg_ctl
printf '%s\n' /usr/lib/postgresql/17/bin >> "${GITHUB_PATH:?GitHub Actions path required}"
