#!/usr/bin/env bash
# Stop independent ops daemons.
set -euo pipefail

OPS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

stop_one() {
  local name="$1"
  local pid_file="${OPS_DIR}/${name}.pid"
  if [ ! -f "${pid_file}" ]; then
    echo "[ops] ${name} not running"
    return 0
  fi
  local pid
  pid="$(tr -d '[:space:]' < "${pid_file}" || true)"
  rm -f "${pid_file}"
  if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
    kill "${pid}" 2>/dev/null || true
    # Also stop children (bash loops).
    pkill -P "${pid}" 2>/dev/null || true
    echo "[ops] stopped ${name} pid=${pid}"
  else
    echo "[ops] ${name} pid stale"
  fi
}

stop_one watchdog
stop_one deploy-agent
echo "[ops] stop-ops complete"
