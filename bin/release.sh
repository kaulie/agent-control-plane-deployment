#!/usr/bin/env bash
#
# 发版：从 app 仓库（repo.url）拉取 ref，执行 build.sh，冻结到
#   $DEPLOYMENT_HOME/packages/<DEPLOY_SERVICE_ID>/deployment-<hash>/
#
# 用法：DEPLOY_SERVICE_ID=<serviceId> ./bin/release.sh [ref]
#
set -euo pipefail

BIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${BIN_DIR}/.." && pwd)"
DEPLOYMENT_HOME="${DEPLOYMENT_HOME:-${HOME}/runtime/agent-control-plane-deployment}"
PACKAGES_DIR="${DEPLOYMENT_HOME}/packages"

log() { echo "[release] $*"; }
die() { echo "[release][错误] $*" >&2; exit 1; }

SERVICE_ID="${DEPLOY_SERVICE_ID:-}"
if [ -z "${SERVICE_ID}" ]; then
  die "未设置 DEPLOY_SERVICE_ID（serviceId，用于 packages 目录隔离）。例如: DEPLOY_SERVICE_ID=web-cursor ./bin/release.sh main"
fi

REF_INPUT="${1:-main}"
mkdir -p "${PACKAGES_DIR}/${SERVICE_ID}"

if [ -n "${GIT_REPO_URL:-}" ]; then
  :
elif [ -f "${ROOT}/repo.url" ]; then
  GIT_REPO_URL="$(tr -d '[:space:]' < "${ROOT}/repo.url")"
elif [ -f "${DEPLOYMENT_HOME}/repo.url" ]; then
  GIT_REPO_URL="$(tr -d '[:space:]' < "${DEPLOYMENT_HOME}/repo.url")"
else
  die "未设置 GIT_REPO_URL，且缺少 repo.url"
fi

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/release-acp.XXXXXX")"
cleanup() { rm -rf "${WORKDIR}"; }
trap cleanup EXIT

log "deploymentHome=${DEPLOYMENT_HOME}"
log "仓库: ${GIT_REPO_URL}"
log "ref: ${REF_INPUT}"

git init -q "${WORKDIR}"
git -C "${WORKDIR}" remote add origin "${GIT_REPO_URL}"

if ! git -C "${WORKDIR}" fetch --depth 1 origin "${REF_INPUT}" 2>/dev/null; then
  git -C "${WORKDIR}" fetch --depth 1 origin "refs/heads/${REF_INPUT}" \
    || die "无法获取 ref: ${REF_INPUT}"
fi

FULL="$(git -C "${WORKDIR}" rev-parse FETCH_HEAD)"
HASH="$(git -C "${WORKDIR}" rev-parse --short=8 "${FULL}")"
TAG="deployment-${HASH}"
DEST="${PACKAGES_DIR}/${SERVICE_ID}/${TAG}"

log "commit=${FULL} hash=${HASH} dest=${DEST}"

NEED_BUILD=1
if [ -f "${DEST}/VERSION" ]; then
  OLD="$(tr -d '[:space:]' < "${DEST}/VERSION" || true)"
  if [ "${OLD}" = "${HASH}" ]; then
    log "发版包已存在，跳过重建"
    NEED_BUILD=0
  fi
fi

if [ "${NEED_BUILD}" -eq 1 ]; then
  SRC_TREE="${WORKDIR}/src"
  mkdir -p "${SRC_TREE}"
  git -C "${WORKDIR}" archive "${FULL}" | tar -x -C "${SRC_TREE}"
  [ -f "${SRC_TREE}/build.sh" ] || die "缺少 build.sh"
  (
    cd "${SRC_TREE}"
    export APP_VERSION="${HASH}"
    chmod +x ./build.sh
    ./build.sh
  ) || die "build.sh 失败"
  [ -d "${SRC_TREE}/outputs" ] || die "未生成 outputs/"
  rm -rf "${DEST}"
  mkdir -p "${DEST}"
  rsync -a "${SRC_TREE}/outputs/" "${DEST}/"
  printf '%s\n' "${HASH}" > "${DEST}/VERSION"
  printf '%s\n' "${FULL}" > "${DEST}/COMMIT"
  printf '%s\n' "${GIT_REPO_URL}" > "${DEST}/GIT_REPO_URL"
fi

log "发版包就绪 ✓ ${DEST}"
log "下一步: curl -sS -X POST http://127.0.0.1:4220/api/deploys \\"
log "  -H 'content-type: application/json' \\"
log "  -d '{\"serviceId\":\"web-cursor\",\"deployment\":\"${TAG}\"}'"
