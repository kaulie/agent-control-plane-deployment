#!/usr/bin/env bash
# 兼容 CLI：转发到 HTTP API（推荐直接调 API）。
set -euo pipefail
API="${DEPLOYMENT_API_URL:-http://127.0.0.1:4220}"
SERVICE_ID="${1:-web-cursor}"
DEPLOYMENT="${2:-}"
[ -n "${DEPLOYMENT}" ] || {
  echo "用法: $0 <serviceId> deployment-<hash>" >&2
  exit 1
}
curl -sS -X POST "${API}/api/deploys" \
  -H 'content-type: application/json' \
  -d "{\"serviceId\":\"${SERVICE_ID}\",\"deployment\":\"${DEPLOYMENT}\"}"
echo
