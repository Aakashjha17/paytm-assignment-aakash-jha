#!/usr/bin/env bash
set -euo pipefail
BASE_URL="${1:?usage: ./burst.sh <BASE_URL>}"
go run ./cmd/burst -base-url "$BASE_URL"
