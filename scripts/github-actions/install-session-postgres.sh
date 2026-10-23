#!/usr/bin/env bash
set -euo pipefail

sudo apt-get update
sudo apt-get install -y curl ca-certificates
sudo install -d /usr/share/postgresql-common/pgdg
sudo curl --fail --silent --show-error --location \
  https://www.postgresql.org/media/keys/ACCC4CF8.asc \
  --output /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc
# This workflow explicitly targets Ubuntu 24.04; authenticate the publisher's
# repository instead of assuming runner images include optional PGDG packages.
printf '%s\n' \
  'Types: deb' \
  'URIs: https://apt.postgresql.org/pub/repos/apt' \
  'Suites: noble-pgdg' \
  'Components: main' \
  'Signed-By: /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc' \
  | sudo tee /etc/apt/sources.list.d/bridge-session-pgdg.sources >/dev/null
sudo apt-get update
sudo apt-get install -y postgresql-17
test -x /usr/lib/postgresql/17/bin/initdb
test -x /usr/lib/postgresql/17/bin/pg_ctl
printf '%s\n' /usr/lib/postgresql/17/bin >> "${GITHUB_PATH:?GitHub Actions path required}"
