#!/usr/bin/env bash
# Independent ops watchdog (deployment domain — NOT under runtime).
# Survives app restarts; only pulls the runtime process back up.
#
# Pause signal: ${OPS_DIR}/watchdog-pause-until
#   Plain file containing a unix epoch (seconds). While now < until,
#   do not auto-start. When the pause expires, clear it (and the legacy
#   runtime .watchdog-paused flag) and start if still unhealthy.
#
# Usage:
#   nohup /Users/gaolei/deployment/web-cursor/ops/watchdog.sh \
#     >> /Users/gaolei/deployment/web-cursor/ops/watchdog.log 2>&1 &
set -euo pipefail

OPS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_HOME="${DEPLOY_HOME:-$(cd "${OPS_DIR}/.." && pwd)}"
RUNTIME_DIR="${RUNTIME_DIR:-/Users/gaolei/runtime/web-cursor}"
BACKEND_DIR="${RUNTIME_DIR}/backend"
START_SH="${RUNTIME_DIR}/scripts/start.sh"
# Legacy infinite pause (manual stop.sh). Deploy uses time-bounded pause below.
LEGACY_PAUSED_FLAG="${BACKEND_DIR}/.watchdog-paused"
PAUSE_UNTIL_FILE="${OPS_DIR}/watchdog-pause-until"
PORT="${PORT:-4211}"
HEALTH_URL="http://127.0.0.1:${PORT}/health"
INTERVAL="${WATCHDOG_INTERVAL:-10}"
PID_FILE="${OPS_DIR}/watchdog.pid"

mkdir -p "${OPS_DIR}"
echo $$ > "${PID_FILE}"
trap 'rm -f "${PID_FILE}"' EXIT

echo "[ops-watchdog] start deployHome=${DEPLOY_HOME} runtime=${RUNTIME_DIR} every ${INTERVAL}s → ${HEALTH_URL}"

read_pause_until() {
  if [ ! -f "${PAUSE_UNTIL_FILE}" ]; then
    echo ""
    return 0
  fi
  tr -d '[:space:]' < "${PAUSE_UNTIL_FILE}" || true
}

start_runtime() {
  echo "[ops-watchdog] $(date '+%F %T') unhealthy; starting runtime..."
  if [ -x "${START_SH}" ]; then
    RUNTIME_DIR="${RUNTIME_DIR}" "${START_SH}" \
      || echo "[ops-watchdog] start.sh failed"
  else
    echo "[ops-watchdog] missing ${START_SH}"
  fi
}

while true; do
  now="$(date +%s)"
  until_ts="$(read_pause_until)"

  if [ -n "${until_ts}" ] && [[ "${until_ts}" =~ ^[0-9]+$ ]]; then
    if [ "${now}" -lt "${until_ts}" ]; then
      # Deploy grace window — do not fight restart/rsync.
      sleep "${INTERVAL}"
      continue
    fi
    # Pause expired: take over if still down.
    remaining=$((now - until_ts))
    echo "[ops-watchdog] $(date '+%F %T') pause expired (${remaining}s ago); clearing pause flags"
    rm -f "${PAUSE_UNTIL_FILE}" "${LEGACY_PAUSED_FLAG}"
    if ! curl -s -m 3 "${HEALTH_URL}" >/dev/null 2>&1; then
      start_runtime
    else
      echo "[ops-watchdog] $(date '+%F %T') healthy after pause; resume monitoring"
    fi
    sleep "${INTERVAL}"
    continue
  fi

  # No time-bounded pause: honor legacy manual-stop flag.
  if [ -f "${LEGACY_PAUSED_FLAG}" ]; then
    sleep "${INTERVAL}"
    continue
  fi

  if ! curl -s -m 3 "${HEALTH_URL}" >/dev/null 2>&1; then
    start_runtime
  fi
  sleep "${INTERVAL}"
done
