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

echo "[build] version=${VERSION}"

go build -o "${OUT}/bin/deployment-server" ./src
go build -o "${OUT}/bin/acp-upgrader" ./upgrader

cp scripts/*.sh "${OUT}/scripts/"
# Copy the whole web/ panel (vanilla HTML/CSS/JS) into the package.
cp -r web "${OUT}/"

chmod +x "${OUT}/bin/"* "${OUT}/scripts/"*.sh

echo "[build] outputs ready:"
ls -1 "${OUT}"
