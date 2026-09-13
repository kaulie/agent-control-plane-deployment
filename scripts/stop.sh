#!/usr/bin/env bash
# Stop the deployment HTTP service, ensuring the port is actually freed.
# Relies on deployment.pid first, then falls back to finding the listener by
# port (lsof). Without the fallback, a stale pidfile (pointing at a dead/wrong
# pid) leaves the real server holding the port, so the subsequent start.sh
# fails to bind ("address already in use") and the self-upgrade deploy job gets
# stuck running forever.
set -euo pipefail
HOME_DIR="${DEPLOYMENT_HOME:-${HOME}/runtime/agent-control-plane-deployment}"
PID_FILE="${HOME_DIR}/deployment.pid"
PORT="${DEPLOYMENT_PORT:-4220}"

# Kill a pid, waiting briefly for it to exit, then SIGKILL if needed.
kill_pid() {
  local p="$1"
  [ -n "${p}" ] || return 0
  kill -0 "${p}" 2>/dev/null || return 0
  kill "${p}" 2>/dev/null || true
  local i
  for i in $(seq 1 10); do
    kill -0 "${p}" 2>/dev/null || return 0
    sleep 0.2
  done
  kill -9 "${p}" 2>/dev/null || true
}

# Best-effort: pid currently listening on PORT (empty if none / lsof absent).
listener_pid() {
  command -v lsof >/dev/null 2>&1 || return 0
  lsof -nP -iTCP:"${PORT}" -sTCP:LISTEN -t 2>/dev/null | head -1 || true
}

stopped=""

if [ -f "${PID_FILE}" ]; then
  pid="$(tr -d '[:space:]' < "${PID_FILE}" || true)"
  if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
    kill_pid "${pid}"
    echo "[stop] stopped pid=${pid}"
    stopped="1"
  fi
  rm -f "${PID_FILE}"
fi

# Fallback: ensure the port is actually free even if the pidfile was stale.
lp="$(listener_pid)"
if [ -n "${lp}" ] && kill -0 "${lp}" 2>/dev/null; then
  echo "[stop] port ${PORT} still held by pid=${lp}; force-stopping" >&2
  kill_pid "${lp}"
  echo "[stop] stopped listener pid=${lp}"
  stopped="1"
fi

[ -n "${stopped}" ] || echo "[stop] not running"
