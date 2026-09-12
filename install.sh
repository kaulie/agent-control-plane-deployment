#!/usr/bin/env bash
# Install this repo into DEPLOY_HOME and (re)start ops daemons.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_HOME="${DEPLOY_HOME:-/Users/gaolei/deployment/web-cursor}"

echo "[install] → ${DEPLOY_HOME}"
mkdir -p "${DEPLOY_HOME}/bin" "${DEPLOY_HOME}/ops" \
  "${DEPLOY_HOME}/deploy-requests" "${DEPLOY_HOME}/deploy-status" \
  "${DEPLOY_HOME}/deploy-requests/processing"

rsync -a --delete \
  --exclude='.git/' \
  --exclude='deployment-*/' \
  --exclude='deploy-requests/' \
  --exclude='deploy-status/' \
  --exclude='ops/*.log' \
  --exclude='ops/*.pid' \
  --exclude='ops/watchdog-pause-until' \
  "${ROOT}/bin/" "${DEPLOY_HOME}/bin/"

rsync -a \
  "${ROOT}/ops/watchdog.sh" \
  "${ROOT}/ops/deploy-agent.sh" \
  "${ROOT}/ops/start-ops.sh" \
  "${ROOT}/ops/stop-ops.sh" \
  "${ROOT}/ops/install.sh" \
  "${DEPLOY_HOME}/ops/"

chmod +x "${DEPLOY_HOME}/bin/"*.sh "${DEPLOY_HOME}/ops/"*.sh

if [ -f "${ROOT}/repo.url" ]; then
  cp "${ROOT}/repo.url" "${DEPLOY_HOME}/repo.url"
fi

echo "[install] starting ops daemons..."
DEPLOY_HOME="${DEPLOY_HOME}" "${DEPLOY_HOME}/ops/stop-ops.sh" || true
DEPLOY_HOME="${DEPLOY_HOME}" "${DEPLOY_HOME}/ops/start-ops.sh"
echo "[install] done"
