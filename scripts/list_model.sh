#!/usr/bin/env bash
# List models → GET {base_url}/v1/models
#
# Usage:
#   ./scripts/list_model.sh <base_url> <api_key>
#
# base_url accepts either:
#   http://127.0.0.1:8317
#   http://127.0.0.1:8317/v1
#
# Example:
#   ./scripts/list_model.sh http://127.0.0.1:8317 sk-lite-gateway
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "Usage: $0 <base_url> <api_key>" >&2
  echo "  $0 http://127.0.0.1:8317 sk-lite-gateway" >&2
  exit 1
fi

# Accept http://host and http://host/v1 (with optional trailing slash).
BASE_URL="${1%/}"
BASE_URL="${BASE_URL%/v1}"
BASE_URL="${BASE_URL%/}"
API_KEY="$2"

curl -sS \
  "${BASE_URL}/v1/models" \
  -H "Authorization: Bearer ${API_KEY}"
echo
