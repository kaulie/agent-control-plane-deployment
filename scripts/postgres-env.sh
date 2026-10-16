#!/usr/bin/env bash
# Load DEPLOYMENT_POSTGRES_DSN for the deployment control plane.
# HOME_DIR must already be set (the runtime root). Existing env wins.
# Files are shell assignments, mode 600, and are not in git:
#   ${HOME_DIR}/data/postgres.env
#   ${HOME}/database/deployment/pg.env

load_deployment_postgres_env() {
  if [ -n "${DEPLOYMENT_POSTGRES_DSN:-}" ]; then
    return 0
  fi
  local f
  for f in "${HOME_DIR}/data/postgres.env" "${HOME}/database/deployment/pg.env"; do
    if [ -f "$f" ]; then
      set -a
      # shellcheck disable=SC1090
      . "$f"
      set +a
      if [ -n "${DEPLOYMENT_POSTGRES_DSN:-}" ]; then
        return 0
      fi
    fi
  done
  echo "[postgres][错误] 缺少 DEPLOYMENT_POSTGRES_DSN。写入 ${HOME_DIR}/data/postgres.env 或 ${HOME}/database/deployment/pg.env（库：49.234.45.173:5432/deployment）" >&2
  return 1
}
