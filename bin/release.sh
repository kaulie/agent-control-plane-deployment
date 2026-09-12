#!/usr/bin/env bash
#
# 发版：从 app 仓库（repo.url）拉取 ref，执行 build.sh，冻结到
#   $DEPLOY_HOME/deployment-<hash>/
#
# 用法：
#   ./bin/release.sh [ref]
#   例：./bin/release.sh main
#
set -euo pipefail

BIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_HOME="${DEPLOY_HOME:-$(cd "${BIN_DIR}/.." && pwd)}"
RUNTIME_HOME="${RUNTIME_HOME:-/Users/gaolei/runtime}"
PROJECT_NAME="${PROJECT_NAME:-web-cursor}"

log() { echo "[release] $*"; }
die() { echo "[release][错误] $*" >&2; exit 1; }

REF_INPUT="${1:-main}"

if [ -n "${GIT_REPO_URL:-}" ]; then
  :
elif [ -f "${DEPLOY_HOME}/repo.url" ]; then
  GIT_REPO_URL="$(tr -d '[:space:]' < "${DEPLOY_HOME}/repo.url")"
else
  die "未设置 GIT_REPO_URL，且缺少 ${DEPLOY_HOME}/repo.url"
fi
[ -n "${GIT_REPO_URL}" ] || die "GIT_REPO_URL 为空"

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/release-${PROJECT_NAME}.XXXXXX")"
cleanup() { rm -rf "${WORKDIR}"; }
trap cleanup EXIT

log "deployHome=${DEPLOY_HOME}"
log "仓库: ${GIT_REPO_URL}"
log "解析版本: ${REF_INPUT}"
log "临时目录: ${WORKDIR}"

git init -q "${WORKDIR}"
git -C "${WORKDIR}" remote add origin "${GIT_REPO_URL}"

if git -C "${WORKDIR}" fetch --depth 1 origin "${REF_INPUT}" 2>/dev/null; then
  :
elif git -C "${WORKDIR}" fetch --depth 1 origin "refs/heads/${REF_INPUT}" 2>/dev/null; then
  :
elif git -C "${WORKDIR}" fetch --depth 1 origin "refs/tags/${REF_INPUT}" 2>/dev/null; then
  :
else
  git -C "${WORKDIR}" fetch --depth 1 origin "${REF_INPUT}" \
    || die "无法从 origin 获取 ref: ${REF_INPUT}"
fi

FULL="$(git -C "${WORKDIR}" rev-parse FETCH_HEAD)" || die "无法解析 FETCH_HEAD"
HASH="$(git -C "${WORKDIR}" rev-parse --short=8 "${FULL}")"
TAG="deployment-${HASH}"
DEST="${DEPLOY_HOME}/${TAG}"

log "commit=${FULL}"
log "hash=${HASH}"
log "dest=${DEST}"

GH_REPO=""
case "${GIT_REPO_URL}" in
  https://github.com/*|http://github.com/*|git@github.com:*)
    GH_REPO="$(printf '%s' "${GIT_REPO_URL}" | sed -E 's#^(https?://github.com/|git@github.com:)##; s#\.git$##')"
    ;;
esac

if [ -n "${GH_REPO}" ] && command -v gh >/dev/null 2>&1; then
  if gh api --silent "repos/${GH_REPO}/git/refs/tags/${TAG}" >/dev/null 2>&1; then
    log "远程已有 tag ${TAG}"
  else
    if gh api "repos/${GH_REPO}/git/refs" \
      -f ref="refs/tags/${TAG}" \
      -f sha="${FULL}" >/dev/null 2>&1; then
      log "已创建远程 tag ${TAG}"
    else
      log "警告: 创建远程 tag 失败，继续生成本地发版包"
    fi
  fi
else
  log "跳过远程 tag（无 gh 或非 GitHub URL）"
fi

NEED_BUILD=1
if [ -f "${DEST}/VERSION" ] && [ -f "${DEST}/scripts/restart.sh" ]; then
  OLD="$(tr -d '[:space:]' < "${DEST}/VERSION" || true)"
  if [ "${OLD}" = "${HASH}" ]; then
    log "发版包已存在，跳过重建: ${DEST}"
    NEED_BUILD=0
  else
    die "目录 ${DEST} 已存在但 VERSION=${OLD}，与 ${HASH} 不符；请手动处理后再试"
  fi
fi

if [ "${NEED_BUILD}" -eq 1 ]; then
  SRC_TREE="${WORKDIR}/src"
  mkdir -p "${SRC_TREE}"
  log "导出源码"
  git -C "${WORKDIR}" archive "${FULL}" | tar -x -C "${SRC_TREE}"
  [ -f "${SRC_TREE}/build.sh" ] || die "仓库根目录缺少 build.sh"

  log "执行 ./build.sh（APP_VERSION=${HASH}）"
  (
    cd "${SRC_TREE}"
    export APP_VERSION="${HASH}"
    chmod +x ./build.sh
    ./build.sh
  ) || die "build.sh 失败"

  [ -d "${SRC_TREE}/outputs" ] || die "build.sh 未生成 outputs/"
  [ -f "${SRC_TREE}/outputs/scripts/restart.sh" ] || die "outputs/ 缺少 scripts/restart.sh"

  if [ -e "${DEST}" ]; then
    log "清理旧目录: ${DEST}"
    rm -rf "${DEST}"
  fi
  mkdir -p "${DEST}"
  log "冻结 outputs/ → ${DEST}"
  rsync -a "${SRC_TREE}/outputs/" "${DEST}/"

  printf '%s\n' "${HASH}" > "${DEST}/VERSION"
  printf '%s\n' "${FULL}" > "${DEST}/COMMIT"
  printf '%s\n' "${GIT_REPO_URL}" > "${DEST}/GIT_REPO_URL"
  log "构建完成"
fi

log "发版包就绪 ✓"
log "  目录: ${DEST}"
log "  下一步: ${BIN_DIR}/deploy.sh ${TAG}"
log "  或异步: curl -sS -X POST http://127.0.0.1:4211/api/ops/deploy -H 'content-type: application/json' -d '{\"deployment\":\"${TAG}\"}'"
