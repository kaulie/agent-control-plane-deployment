#!/usr/bin/env bash
# Install + start the deployment HTTP service under DEPLOYMENT_HOME.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOYMENT_HOME="${DEPLOYMENT_HOME:-${HOME}/runtime/agent-control-plane-deployment}"

echo "[install] → ${DEPLOYMENT_HOME}"
mkdir -p "${DEPLOYMENT_HOME}/packages" "${DEPLOYMENT_HOME}/data" "${DEPLOYMENT_HOME}/logs"

rsync -a \
  --exclude='.git/' \
  --exclude='node_modules/' \
  --exclude='dist/' \
  --exclude='packages/' \
  --exclude='data/' \
  --exclude='logs/' \
  --exclude='deployment.pid' \
  --exclude='src-tree/' \
  --exclude='ops/*.log' \
  --exclude='ops/*.pid' \
  --exclude='bin/deployment-server' \
  "${ROOT}/" "${DEPLOYMENT_HOME}/"

chmod +x "${DEPLOYMENT_HOME}/bin/"*.sh "${DEPLOYMENT_HOME}/scripts/"*.sh "${DEPLOYMENT_HOME}/install.sh" 2>/dev/null || true

(
  cd "${DEPLOYMENT_HOME}"
  go build -o bin/deployment-server ./src
)

echo "[install] starting service..."
DEPLOYMENT_HOME="${DEPLOYMENT_HOME}" "${DEPLOYMENT_HOME}/scripts/restart.sh"
echo "[install] done — API http://127.0.0.1:${PORT:-4220}/health"
