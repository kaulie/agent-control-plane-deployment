#!/usr/bin/env bash
# 兼容 CLI：转发到 HTTP API（推荐直接调 API）。
set -euo pipefail
API="${DEPLOYMENT_API_URL:-http://127.0.0.1:4220}"
# 部署接口要求身份头（第一阶段）：identity_role=user|agent，identity_id=<id>。
IDENTITY_ROLE="${IDENTITY_ROLE:-agent}"
IDENTITY_ID="${IDENTITY_ID:-deploy-agent}"
SERVICE_ID="${1:-web-cursor}"
DEPLOYMENT="${2:-}"
[ -n "${DEPLOYMENT}" ] || {
  echo "用法: $0 <serviceId> deployment-<hash>" >&2
  exit 1
}
curl -sS -X POST "${API}/api/deploys" \
  -H 'content-type: application/json' \
  -H "identity_role: ${IDENTITY_ROLE}" \
  -H "identity_id: ${IDENTITY_ID}" \
  -d "{\"serviceId\":\"${SERVICE_ID}\",\"deployment\":\"${DEPLOYMENT}\"}"
echo
