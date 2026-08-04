#!/usr/bin/env bash
# Anthropic Messages → POST {base_url}/v1/messages
#
# Usage:
#   ./scripts/messages.sh <base_url> <api_key> <model> <prompt>
#
# Env:
#   STREAM=0          disable SSE streaming (default: on)
#   MAX_TOKENS=1024   max_tokens (Anthropic required; default 1024)
#
# base_url accepts either:
#   http://127.0.0.1:8317
#   http://127.0.0.1:8317/v1
#
# Example:
#   ./scripts/messages.sh http://127.0.0.1:8317 sk-lite-gateway claude-sonnet-4 "hello"
set -euo pipefail

if [[ $# -lt 4 ]]; then
  echo "Usage: $0 <base_url> <api_key> <model> <prompt>" >&2
  echo "  STREAM=0 MAX_TOKENS=512 $0 http://127.0.0.1:8317 sk-lite-gateway claude-sonnet-4 \"hello\"" >&2
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
MAX_TOKENS="${MAX_TOKENS:-1024}"

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
  --argjson max_tokens "$MAX_TOKENS" \
  '{
    model: $model,
    max_tokens: $max_tokens,
    stream: $stream,
    messages: [{role: "user", content: $prompt}]
  }')"

# Gateway accepts Bearer or x-api-key; Anthropic clients usually send x-api-key.
curl -sS -N \
  "${BASE_URL}/v1/messages" \
  -H "x-api-key: ${API_KEY}" \
  -H "anthropic-version: 2023-06-01" \
  -H "User-Agent: claude-cli/2.1.63 (external, cli)" \
  -H "Content-Type: application/json" \
  -d "$BODY"
echo
