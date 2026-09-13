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
