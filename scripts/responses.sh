#!/usr/bin/env bash
# OpenAI Responses → POST {base_url}/v1/responses
#
# Usage:
#   ./scripts/responses.sh <base_url> <api_key> <model> <prompt>
#
# Env:
#   STREAM=0   disable SSE streaming (default: on)
#
# base_url accepts either:
#   http://127.0.0.1:8317
#   http://127.0.0.1:8317/v1
#
# Example:
#   ./scripts/responses.sh http://127.0.0.1:8317 sk-lite-gateway gpt-5 "hello"
set -euo pipefail

if [[ $# -lt 4 ]]; then
  echo "Usage: $0 <base_url> <api_key> <model> <prompt>" >&2
  echo "  STREAM=0 $0 http://127.0.0.1:8317 sk-lite-gateway gpt-5 \"hello\"" >&2
  exit 1
fi

# Accept http://host and http://host/v1 (with optional trailing slash).
BASE_URL="${1%/}"
BASE_URL="${BASE_URL%/v1}"
BASE_URL="${BASE_URL%/}"
API_KEY="$2"
MODEL="$3"
PROMPT="$4"
STREAM="${STREAM:-1}"

if ! command -v jq >/dev/null 2>&1; then
  echo "error: jq is required" >&2
  exit 1
fi

if [[ "$STREAM" == "1" || "$STREAM" == "true" ]]; then
  STREAM_JSON=true
else
  STREAM_JSON=false
fi

BODY="$(jq -n \
  --arg model "$MODEL" \
  --arg prompt "$PROMPT" \
  --argjson stream "$STREAM_JSON" \
  '{
    model: $model,
    stream: $stream,
    input: $prompt
  }')"

curl -sS -N \
  "${BASE_URL}/v1/responses" \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d "$BODY"
echo
