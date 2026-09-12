#!/usr/bin/env bash
#
# 上线：把 deployment-<hash>/ rsync 到 runtime，并调用包内 scripts/restart.sh。
#
# 用法：
#   ./bin/deploy.sh deployment-<hash>
#   ./bin/deploy.sh <8-char-hash>
#
# Agents 应优先走 gateway 异步 API，避免在 agent shell 内同步执行本脚本。
#
set -euo pipefail

BIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_HOME="${DEPLOY_HOME:-$(cd "${BIN_DIR}/.." && pwd)}"
RUNTIME_HOME="${RUNTIME_HOME:-/Users/gaolei/runtime}"
PROJECT_NAME="${PROJECT_NAME:-web-cursor}"

log() { echo "[deploy] $*"; }
die() { echo "[deploy][错误] $*" >&2; exit 1; }

RAW="${1:-}"
[ -n "${RAW}" ] || die "用法: ${BIN_DIR}/deploy.sh deployment-<hash>"

case "${RAW}" in
  deployment-*)
    TAG="${RAW}"
    HASH="${RAW#deployment-}"
    ;;
  *)
    HASH="${RAW}"
    TAG="deployment-${HASH}"
    ;;
esac

if [ "${#HASH}" -gt 8 ]; then
  HASH="${HASH:0:8}"
  TAG="deployment-${HASH}"
fi

SRC="${DEPLOY_HOME}/${TAG}"
RUNTIME_DIR="${RUNTIME_DIR:-${RUNTIME_HOME}/${PROJECT_NAME}}"

[ -d "${SRC}" ] || die "发版包不存在: ${SRC}（先运行 ${BIN_DIR}/release.sh）"
[ -f "${SRC}/VERSION" ] || die "缺少 ${SRC}/VERSION"
[ -f "${SRC}/scripts/restart.sh" ] || die "发版包缺少 scripts/restart.sh"

SNAP_VER="$(tr -d '[:space:]' < "${SRC}/VERSION")"
[ "${SNAP_VER}" = "${HASH}" ] || die "VERSION(${SNAP_VER}) 与目录 hash(${HASH}) 不一致"

mkdir -p "${RUNTIME_DIR}"

log "1/2 rsync ${TAG} → ${RUNTIME_DIR}"
rsync -a --delete \
  --filter='P backend/.env' \
  --filter='P backend/data/' \
  --filter='P backend/runtime.pid' \
  --filter='P backend/server.log' \
  --filter='P backend/.watchdog-paused' \
  --exclude='backend/.env' \
  --exclude='backend/data/' \
  --exclude='backend/runtime.pid' \
  --exclude='backend/server.log' \
  --exclude='backend/.watchdog-paused' \
  --exclude='.git/' \
  "${SRC}/" "${RUNTIME_DIR}/" || die "rsync 失败"

printf '%s\n' "${HASH}" > "${RUNTIME_DIR}/VERSION"
printf '%s\n' "${TAG}" > "${RUNTIME_DIR}/DEPLOYMENT"

[ -f "${RUNTIME_DIR}/scripts/restart.sh" ] || die "同步后缺少 scripts/restart.sh"

log "2/2 重启 runtime（调用包内 scripts/restart.sh）"
export APP_VERSION="${HASH}"
export RUNTIME_DIR
bash "${RUNTIME_DIR}/scripts/restart.sh" || die "重启失败"

log "部署完成 ✓  版本=${HASH}"
