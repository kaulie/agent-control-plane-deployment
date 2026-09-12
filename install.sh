#!/usr/bin/env bash
# Install + start the deployment HTTP service under DEPLOYMENT_HOME.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOYMENT_HOME="${DEPLOYMENT_HOME:-${HOME}/runtime/agent-control-plane-deployment}"

echo "[install] → ${DEPLOYMENT_HOME}"
mkdir -p "${DEPLOYMENT_HOME}/packages" "${DEPLOYMENT_HOME}/data" "${DEPLOYMENT_HOME}/logs" "${DEPLOYMENT_HOME}/upgrade-requests"

rsync -a \
  --exclude='.git/' \
  --exclude='node_modules/' \
  --exclude='dist/' \
  --exclude='packages/' \
  --exclude='data/' \
  --exclude='logs/' \
  --exclude='deployment.pid' \
  --exclude='upgrader.pid' \
  --exclude='upgrade-requests/' \
  --exclude='src-tree/' \
  --exclude='ops/*.log' \
  --exclude='ops/*.pid' \
  --exclude='bin/deployment-server' \
  --exclude='bin/acp-upgrader' \
  "${ROOT}/" "${DEPLOYMENT_HOME}/"

chmod +x "${DEPLOYMENT_HOME}/bin/"*.sh "${DEPLOYMENT_HOME}/scripts/"*.sh "${DEPLOYMENT_HOME}/install.sh" 2>/dev/null || true

(
  cd "${DEPLOYMENT_HOME}"
  go build -o bin/deployment-server ./src
  go build -o bin/acp-upgrader ./upgrader
)

echo "[install] starting acp-upgrader..."
DEPLOYMENT_HOME="${DEPLOYMENT_HOME}" "${DEPLOYMENT_HOME}/scripts/upgrader-start.sh" || true

echo "[install] starting deployment service..."
# Force ACP listen port; do not inherit ambient PORT from other apps.
DEPLOYMENT_HOME="${DEPLOYMENT_HOME}" DEPLOYMENT_PORT="${DEPLOYMENT_PORT:-4220}" DEPLOYMENT_HOST="${DEPLOYMENT_HOST:-127.0.0.1}" \
  "${DEPLOYMENT_HOME}/scripts/restart.sh"
echo "[install] done — API http://127.0.0.1:${DEPLOYMENT_PORT:-4220}/health"
