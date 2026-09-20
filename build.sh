#!/usr/bin/env bash
# Release build for the deployment control plane itself.
#
# Invoked by packageFromGit (POST /api/deploy-notify pipeline) and bin/release.sh
# with:
#   cwd = repo root, APP_VERSION = <short hash>
# Must produce outputs/ containing the runtime fileset that a self-deploy
# rsyncs into DEPLOYMENT_HOME:
#   bin/deployment-server, bin/acp-upgrader, scripts/*.sh, web/*
# VERSION / COMMIT / GIT_REPO_URL are written by the caller into the package.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${ROOT}"

VERSION="${APP_VERSION:-local}"
OUT="${ROOT}/outputs"
rm -rf "${OUT}"
mkdir -p "${OUT}/bin" "${OUT}/scripts"

# Isolate Go caches inside the (temporary) build tree so they never land in
# the runtime / DEPLOYMENT_HOME — regardless of whatever HOME the caller
# (packageFromGit, inheriting the deployment-server env) has. Without this,
# a wrong HOME pointed at the runtime dir caused `go build` to download the
# module cache into <runtime>/go (read-only), which then broke the
# self-deploy rsync (--delete could not remove it).
export GOMODCACHE="${ROOT}/.gomodcache"
export GOCACHE="${ROOT}/.gocache"
export GOPATH="${ROOT}/.gopath"
# Keep build output out of the package too.
export GOFLAGS="${GOFLAGS:-}"
# Default to a reachable module/toolchain proxy if the caller didn't set one
# (proxy.golang.org is unreachable via IPv6 on some networks; the Go toolchain
# auto-download triggered by go.mod's go directive needs a working proxy).
if [ -z "${GOPROXY:-}" ]; then
  export GOPROXY="https://goproxy.cn,direct"
fi

echo "[build] version=${VERSION}"

go build -o "${OUT}/bin/deployment-server" ./src
go build -o "${OUT}/bin/acp-upgrader" ./upgrader

cp scripts/*.sh "${OUT}/scripts/"
# Copy the whole web/ panel (vanilla HTML/CSS/JS) into the package.
cp -r web "${OUT}/"

chmod +x "${OUT}/bin/"* "${OUT}/scripts/"*.sh

echo "[build] outputs ready:"
ls -1 "${OUT}"

# --- 服务契约自动登记（幂等）---------------------------------------------------
# 契约的真源是代码里的 swag 注解：src/main.go 顶部的 General API Info + 每个
# handler 上的 @Summary/@Tags/@Router。这一步读注解 → swag init 生成
# docs/swagger.json → 上报注册中心（client/ 是注册中心脚本的原样拷贝，见
# client/README.md），幂等：规范没变化时 register.sh 会跳过 PUT，不刷 revision。
#
# 刻意**不参与打包成败**：注册中心默认只绑本机 127.0.0.1:4240，拿它的可达性挡发布
# 没有意义，所以失败只告警。要跳过：REGISTER_CONTRACT=0；换端口/部门/实例直接给
# INSTANCES / DEPARTMENT_ID / SERVICE_NAME 环境变量。
#
# REGISTRY_URL 必须显式给：client 脚本的默认值是 http://127.0.0.1:${SERVICE_PORT}，
# 而 SERVICE_PORT 常被"当前服务的端口"占用，不写死就可能把契约 PUT 到别的服务上。
if [ "${REGISTER_CONTRACT:-1}" = "1" ]; then
  echo "[build] 按注解登记服务契约（可 REGISTER_CONTRACT=0 跳过）"
  SERVICE_NAME="${SERVICE_NAME:-agent-control-plane-deployment}" \
  REGISTRY_URL="${REGISTRY_URL:-http://127.0.0.1:4240}" \
  SWAG_MAIN="${SWAG_MAIN:-src/main.go}" \
  SWAG_OUT="${SWAG_OUT:-docs}" \
  SWAG_ARGS="${SWAG_ARGS:---parseInternal --outputTypes json}" \
  DEPARTMENT_ID="${DEPARTMENT_ID:-D0004}" \
  INSTANCES="${INSTANCES:-127.0.0.1:${DEPLOYMENT_PORT:-4220}}" \
  OWNER="${OWNER:-kaulie}" \
  HEALTH_PATH="${HEALTH_PATH:-/health}" \
  VERSION="${VERSION}" \
    bash client/ci/register-go-service.sh \
    || echo "[build][warn] 契约登记失败（不影响打包）：注册中心不可达或缺 swag" >&2
fi
