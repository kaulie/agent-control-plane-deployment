#!/usr/bin/env bash
# Start the independent ACP upgrader (does not build; stop/start only).
set -euo pipefail
HOME_DIR="${DEPLOYMENT_HOME:-${HOME}/runtime/agent-control-plane-deployment}"
PID_FILE="${HOME_DIR}/upgrader.pid"
LOG_FILE="${HOME_DIR}/logs/upgrader.log"
BIN="${HOME_DIR}/bin/acp-upgrader"
mkdir -p "${HOME_DIR}/logs" "${HOME_DIR}/upgrade-requests"

if [ -f "${PID_FILE}" ]; then
  old="$(tr -d '[:space:]' < "${PID_FILE}" || true)"
  if [ -n "${old}" ] && kill -0 "${old}" 2>/dev/null; then
    echo "[upgrader-start] already running pid=${old}"
    exit 0
  fi
  rm -f "${PID_FILE}"
fi

if [ ! -x "${BIN}" ]; then
  echo "[upgrader-start][错误] missing ${BIN}; build with: go build -o bin/acp-upgrader ./upgrader" >&2
  exit 1
fi

cd "${HOME_DIR}"
export DEPLOYMENT_HOME="${HOME_DIR}"
nohup "${BIN}" >>"${LOG_FILE}" 2>&1 &
echo $! > "${PID_FILE}"
sleep 0.3
if kill -0 "$(tr -d '[:space:]' < "${PID_FILE}")" 2>/dev/null; then
  echo "[upgrader-start] ok pid=$(cat "${PID_FILE}") log=${LOG_FILE}"
else
  echo "[upgrader-start][错误] failed; see ${LOG_FILE}" >&2
  exit 1
fi
