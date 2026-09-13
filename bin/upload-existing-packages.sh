#!/usr/bin/env bash
#
# 一次性迁移：把本地 packages/<serviceId>/deployment-<hash>/ 下的旧制品
# 打成 package.tar.gz 上传到对应服务仓库的 GitHub Release（tag=deployment-<hash>），
# 可选 --purge 上传成功后删除本地包。
#
# 用法: ./bin/upload-existing-packages.sh [--purge] [PACKAGES_DIR]
#
# 依赖: gh（已 auth）或环境变量 GITHUB_TOKEN；对每个服务仓库有 contents:write。
set -euo pipefail

PURGE=0
PACKAGES_DIR=""
for arg in "$@"; do
  case "${arg}" in
    --purge) PURGE=1 ;;
    *) PACKAGES_DIR="${arg}" ;;
  esac
done

DEPLOYMENT_HOME="${DEPLOYMENT_HOME:-${HOME}/runtime/agent-control-plane-deployment}"
if [ -z "${PACKAGES_DIR}" ]; then
  PACKAGES_DIR="${DEPLOYMENT_HOME}/packages"
fi

log() { echo "[migrate] $*"; }
die() { echo "[migrate][错误] $*" >&2; exit 1; }

[ -d "${PACKAGES_DIR}" ] || { log "packages 目录不存在: ${PACKAGES_DIR}，无可迁移"; exit 0; }

count=0
uploaded=0
for svc_dir in "${PACKAGES_DIR}"/*/; do
  [ -d "${svc_dir}" ] || continue
  service_id="$(basename "${svc_dir}")"
  for pkg_dir in "${svc_dir}"deployment-*/; do
    [ -d "${pkg_dir}" ] || continue
    [ -f "${pkg_dir}VERSION" ] || continue
    count=$((count+1))
    hash="$(tr -d '[:space:]' < "${pkg_dir}VERSION")"
    tag="deployment-${hash}"
    repo_url=""
    if [ -f "${pkg_dir}GIT_REPO_URL" ]; then
      repo_url="$(tr -d '[:space:]' < "${pkg_dir}GIT_REPO_URL")"
    fi
    if [ -z "${repo_url}" ]; then
      log "跳过 ${pkg_dir}：缺少 GIT_REPO_URL，无法确定目标仓库"
      continue
    fi
    repo_slug="${repo_url#https://github.com/}"
    repo_slug="${repo_slug#git@github.com:}"
    repo_slug="${repo_slug%.git}"
    repo_slug="${repo_slug%/}"

    workdir="$(mktemp -d "${TMPDIR:-/tmp}/migrate.XXXXXX")"
    tarball="${workdir}/package.tar.gz"
    tar -czf "${tarball}" -C "${pkg_dir}" .
    if ! gh release view "${tag}" --repo "${repo_slug}" >/dev/null 2>&1; then
      gh release create "${tag}" --repo "${repo_slug}" --title "${tag}" --notes "migration: deployment package ${hash}"
    fi
    if gh release upload "${tag}" --repo "${repo_slug}" "${tarball}#package.tar.gz" --clobber; then
      log "上传 ✓ ${repo_slug}@${tag} (service=${service_id})"
      uploaded=$((uploaded+1))
      if [ "${PURGE}" -eq 1 ]; then
        rm -rf "${pkg_dir}"
        log "已删除本地包 ${pkg_dir}"
      fi
    else
      log "上传失败 ✗ ${repo_slug}@${tag}"
    fi
    rm -rf "${workdir}"
  done
  # 删除空的服务目录
  rmdir "${svc_dir}" 2>/dev/null || true
done

log "完成：扫描 ${count} 个包，上传 ${uploaded} 个${PURGE:+（已 purge 本地）}"
