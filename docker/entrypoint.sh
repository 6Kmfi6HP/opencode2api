#!/bin/sh
set -eu

: "${OPENCODE2API_PORT:=8000}"
: "${OPENCODE2API_CONFIG:=/data/config.json}"
: "${OPENCODE2API_PASSWORD:=123456}"

config_dir="$(dirname "$OPENCODE2API_CONFIG")"
mkdir -p "$config_dir"

if [ ! -f "$OPENCODE2API_CONFIG" ]; then
  if [ -n "${OPENCODE2API_SOCKS5_ADDR:-}" ]; then
    proxy_name="${OPENCODE2API_SOCKS5_NAME:-proxy}"
    cat > "$OPENCODE2API_CONFIG" <<EOF
{
  "model_alias": {
    "deepseek-v4-flash": "deepseek-v4-flash-free",
    "mimo-v2.5": "mimo-v2.5-free",
    "ling-3.0-flash": "ling-3.0-flash-free",
    "nemotron-3-ultra": "nemotron-3-ultra-free",
    "north-mini-code": "north-mini-code-free",
    "laguna-s-2.1": "laguna-s-2.1-free"
  },
  "reasoning_effort_map": {
    "minimal": "low",
    "medium": "medium",
    "high": "high"
  },
  "force_disable_thinking": false,
  "socks5_proxies": [
    {
      "name": "$proxy_name",
      "addr": "$OPENCODE2API_SOCKS5_ADDR",
      "username": "",
      "password": ""
    }
  ],
  "active_socks5": "$OPENCODE2API_SOCKS5_ADDR"
}
EOF
  else
    cp /app/config.example.json "$OPENCODE2API_CONFIG"
  fi
fi

if [ "$(id -u)" = "0" ]; then
  chown -R opencode2api:opencode2api /data 2>/dev/null || true
  exec su-exec opencode2api:opencode2api /usr/local/bin/opencode2api \
    -port "$OPENCODE2API_PORT" \
    -config "$OPENCODE2API_CONFIG" \
    -password "$OPENCODE2API_PASSWORD" \
    -log-file "${OPENCODE2API_LOG_FILE:-/data/opencode2api.log}" \
    -log-level "${OPENCODE2API_LOG_LEVEL:-info}" \
    -log-stdout="${OPENCODE2API_LOG_STDOUT:-true}" \
    "$@"
fi

exec /usr/local/bin/opencode2api \
  -port "$OPENCODE2API_PORT" \
  -config "$OPENCODE2API_CONFIG" \
  -password "$OPENCODE2API_PASSWORD" \
  -log-file "${OPENCODE2API_LOG_FILE:-/data/opencode2api.log}" \
  -log-level "${OPENCODE2API_LOG_LEVEL:-info}" \
  -log-stdout="${OPENCODE2API_LOG_STDOUT:-true}" \
  "$@"
