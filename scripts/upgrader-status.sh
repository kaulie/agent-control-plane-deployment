#!/usr/bin/env bash
# Exit 0 if acp-upgrader's pidfile process is alive. Used as watchdog command probe.
set -euo pipefail
HOME_DIR="${DEPLOYMENT_HOME:-${HOME}/runtime/agent-control-plane-deployment}"
PID_FILE="${HOME_DIR}/upgrader.pid"
if [ ! -f "${PID_FILE}" ]; then
  echo "[upgrader-status] not running (no pidfile)"
  exit 1
fi
pid="$(tr -d '[:space:]' < "${PID_FILE}" || true)"
if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
  echo "[upgrader-status] running pid=${pid}"
  exit 0
fi
echo "[upgrader-status] stale pidfile pid=${pid:-?}"
exit 1
