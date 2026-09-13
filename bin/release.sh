#!/usr/bin/env bash
#
# 发版：从 app 仓库（repo.url）拉取 ref，执行 build.sh，把 outputs 打成
# package.tar.gz 上传到该服务仓库的 GitHub Release（tag=deployment-<hash>），
# 然后删除本地构建产物（release 作为唯一来源，节省本地存储）。
#
# 用法：DEPLOY_SERVICE_ID=<serviceId> ./bin/release.sh [ref]
#
# 需要环境变量 GITHUB_TOKEN（或已 gh auth login），且对目标仓库有 contents:write。
#
set -euo pipefail

BIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${BIN_DIR}/.." && pwd)"
DEPLOYMENT_HOME="${DEPLOYMENT_HOME:-${HOME}/runtime/agent-control-plane-deployment}"

log() { echo "[release] $*"; }
die() { echo "[release][错误] $*" >&2; exit 1; }

SERVICE_ID="${DEPLOY_SERVICE_ID:-}"
if [ -z "${SERVICE_ID}" ]; then
  die "未设置 DEPLOY_SERVICE_ID（serviceId）。例如: DEPLOY_SERVICE_ID=web-cursor ./bin/release.sh main"
fi

REF_INPUT="${1:-main}"

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
PKGDIR="$(mktemp -d "${TMPDIR:-/tmp}/release-pkg.XXXXXX")"
cleanup() { rm -rf "${WORKDIR}" "${PKGDIR}"; }
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

log "commit=${FULL} hash=${HASH} tag=${TAG}"

# 跳过判断：release asset 已存在则不再构建。
if gh release view "${TAG}" --repo "${GIT_REPO_URL%*.git}" >/dev/null 2>&1; then
  if gh release download "${TAG}" --repo "${GIT_REPO_URL%*.git}" --pattern package.tar.gz --dir "${WORKDIR}" --clobber >/dev/null 2>&1; then
    log "release asset 已存在，跳过构建 ✓"
    log "下一步: curl -sS -X POST http://127.0.0.1:4220/api/deploys \\"
    log "  -H 'content-type: application/json' \\"
    log "  -d '{\"serviceId\":\"${SERVICE_ID}\",\"deployment\":\"${TAG}\"}'"
    exit 0
  fi
fi

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

# 冻结到临时 PKGDIR 并打 tar.gz
rsync -a "${SRC_TREE}/outputs/" "${PKGDIR}/"
printf '%s\n' "${HASH}" > "${PKGDIR}/VERSION"
printf '%s\n' "${FULL}" > "${PKGDIR}/COMMIT"
printf '%s\n' "${GIT_REPO_URL}" > "${PKGDIR}/GIT_REPO_URL"
tar -czf "${WORKDIR}/package.tar.gz" -C "${PKGDIR}" .

REPO_SLUG="${GIT_REPO_URL#https://github.com/}"
REPO_SLUG="${REPO_SLUG#git@github.com:}"
REPO_SLUG="${REPO_SLUG%.git}"
REPO_SLUG="${REPO_SLUG%/}"

# 创建 release（若不存在）并上传 asset（--clobber 覆盖同名）
if ! gh release view "${TAG}" --repo "${REPO_SLUG}" >/dev/null 2>&1; then
  gh release create "${TAG}" --repo "${REPO_SLUG}" --title "${TAG}" --notes "deployment package ${HASH}" --target "${FULL}"
fi
gh release upload "${TAG}" --repo "${REPO_SLUG}" "${WORKDIR}/package.tar.gz#package.tar.gz" --clobber

log "已上传 release ✓ ${REPO_SLUG}@${TAG} (asset package.tar.gz)"
log "本地构建产物已清理（release 为唯一来源）"
log "下一步: curl -sS -X POST http://127.0.0.1:4220/api/deploys \\"
log "  -H 'content-type: application/json' \\"
log "  -d '{\"serviceId\":\"${SERVICE_ID}\",\"deployment\":\"${TAG}\"}'"

