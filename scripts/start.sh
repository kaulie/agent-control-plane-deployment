#!/usr/bin/env bash
set -euo pipefail
HOME_DIR="${DEPLOYMENT_HOME:-${HOME}/runtime/agent-control-plane-deployment}"
PID_FILE="${HOME_DIR}/deployment.pid"
LOG_FILE="${HOME_DIR}/logs/deployment.log"
BIN="${HOME_DIR}/bin/deployment-server"
mkdir -p "${HOME_DIR}/logs"

if [ -f "${PID_FILE}" ]; then
  old="$(tr -d '[:space:]' < "${PID_FILE}" || true)"
  if [ -n "${old}" ] && kill -0 "${old}" 2>/dev/null; then
    echo "[start] already running pid=${old}"
    exit 0
  fi
  rm -f "${PID_FILE}"
fi

cd "${HOME_DIR}"
export DEPLOYMENT_HOME="${HOME_DIR}"
# Never inherit app PORT (e.g. web-cursor 4211). Only DEPLOYMENT_PORT overrides.
export PORT="${DEPLOYMENT_PORT:-4220}"
export HOST="${DEPLOYMENT_HOST:-127.0.0.1}"

if [ ! -x "${BIN}" ]; then
  echo "[start][错误] missing ${BIN}; run ./install.sh / go build -o bin/deployment-server ./src first" >&2
  exit 1
fi

nohup "${BIN}" >>"${LOG_FILE}" 2>&1 &
echo $! > "${PID_FILE}"
sleep 0.4
if kill -0 "$(tr -d '[:space:]' < "${PID_FILE}")" 2>/dev/null; then
  echo "[start] ok pid=$(cat "${PID_FILE}") log=${LOG_FILE}"
else
  echo "[start][错误] failed; see ${LOG_FILE}" >&2
  exit 1
fi
