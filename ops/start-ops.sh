#!/usr/bin/env bash
# Start independent ops daemons (watchdog + deploy-agent).
set -euo pipefail

OPS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_HOME="${DEPLOY_HOME:-$(cd "${OPS_DIR}/.." && pwd)}"
mkdir -p "${OPS_DIR}"

start_one() {
  local name="$1"
  local script="$2"
  local pid_file="${OPS_DIR}/${name}.pid"
  local log_file="${OPS_DIR}/${name}.log"

  if [ -f "${pid_file}" ]; then
    local old
    old="$(tr -d '[:space:]' < "${pid_file}" || true)"
    if [ -n "${old}" ] && kill -0 "${old}" 2>/dev/null; then
      echo "[ops] ${name} already running pid=${old}"
      return 0
    fi
    rm -f "${pid_file}"
  fi

  nohup env DEPLOY_HOME="${DEPLOY_HOME}" bash "${script}" >>"${log_file}" 2>&1 &
  local pid=$!
  # Script rewrites pid_file to its own $$; give it a moment.
  sleep 0.3
  if kill -0 "${pid}" 2>/dev/null; then
    echo "[ops] started ${name} launcher_pid=${pid} (see ${log_file})"
  else
    echo "[ops][错误] failed to start ${name}; see ${log_file}" >&2
    return 1
  fi
}

start_one watchdog "${OPS_DIR}/watchdog.sh"
start_one deploy-agent "${OPS_DIR}/deploy-agent.sh"
echo "[ops] start-ops complete"
