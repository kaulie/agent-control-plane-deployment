#!/usr/bin/env bash
set -euo pipefail
HOME_DIR="${DEPLOYMENT_HOME:-${HOME}/runtime/agent-control-plane-deployment}"
PID_FILE="${HOME_DIR}/deployment.pid"
if [ -f "${PID_FILE}" ]; then
  pid="$(tr -d '[:space:]' < "${PID_FILE}" || true)"
  if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
    kill "${pid}" 2>/dev/null || true
    sleep 0.5
    kill -9 "${pid}" 2>/dev/null || true
    echo "[stop] stopped pid=${pid}"
  fi
  rm -f "${PID_FILE}"
else
  echo "[stop] not running"
fi
