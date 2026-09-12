# agent-control-plane-deployment

agent 控制面的**独立部署 / 运维**仓库。与应用仓库 [`agent-control-plane`](https://github.com/kaulie/agent-control-plane) 分离管理。

## 本机布局

安装后（默认）：

| 路径 | 角色 |
|---|---|
| `/Users/gaolei/deployment/web-cursor/` | 本仓库的安装目录（`DEPLOY_HOME`） |
| `DEPLOY_HOME/bin/release.sh` | 从 app `main`（或指定 ref）构建 `deployment-<hash>/` |
| `DEPLOY_HOME/bin/deploy.sh` | rsync 发版包到 runtime 并重启 |
| `DEPLOY_HOME/ops/` | watchdog + deploy-agent（不跑在 runtime 内） |
| `/Users/gaolei/runtime/web-cursor/` | 线上运行目录（只收 deploy rsync） |

`repo.url` 指向应用仓库（默认 `https://github.com/kaulie/agent-control-plane`），供 `release.sh` 拉取源码。

## 安装 / 更新到本机 DEPLOY_HOME

```bash
git clone https://github.com/kaulie/agent-control-plane-deployment
cd agent-control-plane-deployment
DEPLOY_HOME=/Users/gaolei/deployment/web-cursor ./install.sh
```

`install.sh` 会同步 `bin/`、`ops/`、`repo.url`，并启动 ops 守护进程。

## 发版与上线

```bash
# 1) 构建发版包（从 app 仓库）
/Users/gaolei/deployment/web-cursor/bin/release.sh main
# → deployment-<hash>/

# 2) 异步上线（推荐；gateway 默认 graceful）
curl -sS -X POST http://127.0.0.1:4211/api/ops/deploy \
  -H 'content-type: application/json' \
  -d '{"deployment":"deployment-<hash>"}'

# 若有 agent 在跑：state=waiting_for_idle，轮询
curl -sS http://127.0.0.1:4211/api/ops/restart-status

# 仅排障时同步上线（会杀 gateway）
/Users/gaolei/deployment/web-cursor/bin/deploy.sh deployment-<hash>
```

应用侧 graceful 配置见 app 仓库 `backend/.env`：`GRACEFUL_RESTART`、`DEPLOY_GRACEFUL_WAIT_MS`。

## 目录说明

```
bin/           release.sh / deploy.sh（本产品专用，自包含）
ops/           watchdog / deploy-agent / start-ops / stop-ops / install
repo.url       应用仓库 URL
install.sh     安装到 DEPLOY_HOME 并拉起 ops
```

**不要**把 `deployment-*` 快照、`deploy-requests/`、日志、pid 提交进本仓库。
