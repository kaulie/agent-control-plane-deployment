#!/usr/bin/env bash
# Install ops scripts into the deployment home and (re)start daemons.
set -euo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_HOME="${DEPLOY_HOME:-/Users/gaolei/deployment/web-cursor}"
OPS_DIR="${DEPLOY_HOME}/ops"

mkdir -p "${OPS_DIR}" "${DEPLOY_HOME}/deploy-requests" "${DEPLOY_HOME}/deploy-status" \
  "${DEPLOY_HOME}/deploy-requests/processing"

for f in watchdog.sh deploy-agent.sh start-ops.sh stop-ops.sh; do
  cp "${SRC_DIR}/${f}" "${OPS_DIR}/${f}"
  chmod +x "${OPS_DIR}/${f}"
done

echo "[ops-install] installed → ${OPS_DIR}"
echo "[ops-install] starting daemons..."
DEPLOY_HOME="${DEPLOY_HOME}" "${OPS_DIR}/stop-ops.sh" || true
DEPLOY_HOME="${DEPLOY_HOME}" "${OPS_DIR}/start-ops.sh"
echo "[ops-install] done"
