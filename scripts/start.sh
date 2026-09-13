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

# Load GitHub token from data/github-token (survives self-deploy rsync because
# data/ is preserved). Used by release-based package upload/download. Falls
# back to any inherited GITHUB_TOKEN if the file is absent.
if [ -z "${GITHUB_TOKEN:-}" ] && [ -f "${HOME_DIR}/data/github-token" ]; then
  export GITHUB_TOKEN="$(tr -d '[:space:]' < "${HOME_DIR}/data/github-token")"
fi

# Load Aliyun packages (制品仓库) basic-auth credentials for the "aliyun"
# artifact storage backend from data/aliyun-credentials (preserved across
# self-deploy because data/ is). File format: line 1 = username, line 2 =
# password. Inherited ALIYUN_PACKAGES_USER / ALIYUN_PACKAGES_PASSWORD win.
if [ -z "${ALIYUN_PACKAGES_USER:-}" ] && [ -f "${HOME_DIR}/data/aliyun-credentials" ]; then
  export ALIYUN_PACKAGES_USER="$(sed -n '1p' "${HOME_DIR}/data/aliyun-credentials" | tr -d '\r\n')"
  export ALIYUN_PACKAGES_PASSWORD="$(sed -n '2p' "${HOME_DIR}/data/aliyun-credentials" | tr -d '\r\n')"
fi

# Default Go module/toolchain proxy to a reachable mirror. The build env
# (packageFromGit runs each service's build.sh inheriting this process env)
# often needs to download the Go toolchain (when go.mod's go directive exceeds
# the installed go) and modules; the default proxy.golang.org is unreachable
# via IPv6 in some networks. Respect an explicit GOPROXY if already set.
if [ -z "${GOPROXY:-}" ]; then
  export GOPROXY="https://goproxy.cn,direct"
fi

if [ ! -x "${BIN}" ]; then
  echo "[start][错误] missing ${BIN}; run ./install.sh / go build -o bin/deployment-server ./src first" >&2
  exit 1
fi

# Refuse to start if the port is already bound (e.g. a stale pidfile left a
# previous server running). Without this, the new process fails to bind and
# exits immediately, leaving a stale pidfile and a stuck self-upgrade.
if command -v lsof >/dev/null 2>&1; then
  holder="$(lsof -nP -iTCP:"${PORT}" -sTCP:LISTEN -t 2>/dev/null | head -1 || true)"
  if [ -n "${holder}" ]; then
    echo "[start][错误] port ${PORT} already in use by pid=${holder}; run scripts/stop.sh first" >&2
    exit 1
  fi
fi

nohup "${BIN}" >>"${LOG_FILE}" 2>&1 &
echo $! > "${PID_FILE}"
sleep 0.6
if kill -0 "$(tr -d '[:space:]' < "${PID_FILE}")" 2>/dev/null; then
  echo "[start] ok pid=$(cat "${PID_FILE}") log=${LOG_FILE}"
else
  # Process died before becoming healthy — remove the stale pidfile so a retry
  # is not confused into thinking it is already running.
  rm -f "${PID_FILE}"
  echo "[start][错误] failed; see ${LOG_FILE}" >&2
  exit 1
fi
