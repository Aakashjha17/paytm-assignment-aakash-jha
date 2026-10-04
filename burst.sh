#!/usr/bin/env bash
# One-command on-sale stampede against a running service, local or live.
#   ./burst.sh http://localhost:8080
#   ADMIN_API_KEY=<live key> ./burst.sh https://<app>.up.railway.app
# Extra flags pass through, e.g. ./burst.sh <url> -requests 5000 -concurrency 300
set -euo pipefail
BASE_URL="${1:?usage: ./burst.sh <BASE_URL> [flags]}"
shift
if [[ -z "${ADMIN_API_KEY:-}" && -f .env ]]; then
  ADMIN_API_KEY="$(grep -E '^ADMIN_API_KEY=' .env | cut -d= -f2-)"
fi
: "${ADMIN_API_KEY:?set ADMIN_API_KEY (needed to create the burst show)}"
export ADMIN_API_KEY
exec go run ./cmd/burst -base-url "$BASE_URL" "$@"
