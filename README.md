# agent-control-plane-deployment

独立部署控制面：HTTP API + SQLite 服务契约，与 app（web-cursor）进程/目录完全解耦。

## 本机布局

| 路径 | 角色 |
|---|---|
| `~/runtime/agent-control-plane-deployment` | 本服务安装与数据根（`DEPLOYMENT_HOME`） |
| `$HOME/packages/deployment-*` | 发版包 |
| `$HOME/data/deploy.sqlite` | 服务契约 + 部署任务 |
| `$HOME/upgrade-requests/` | ACP 自身升级请求（供 `acp-upgrader`） |
| HTTP `127.0.0.1:4220` | 部署 API |

被部署的应用（例如 web-cursor）仍使用自己的 runtime（如 `~/runtime/web-cursor`），由 SQLite 里的 **service contract** 描述启停方式。

## 安装 / 启停（本服务）

HTTP 服务实现为 **Go**（`src/` → `bin/deployment-server`）。需要本机安装 Go toolchain。
自身滚动升级另有独立进程 `bin/acp-upgrader`（**不**执行 `go build`）。

```bash
git clone https://github.com/kaulie/agent-control-plane-deployment
cd agent-control-plane-deployment
./install.sh
# → go build deployment-server + acp-upgrader
# → 启动 upgrader + deployment-server（监听 :4220，端口用 DEPLOYMENT_PORT，不继承应用 PORT）

~/runtime/agent-control-plane-deployment/scripts/stop.sh
~/runtime/agent-control-plane-deployment/scripts/start.sh
~/runtime/agent-control-plane-deployment/scripts/restart.sh
~/runtime/agent-control-plane-deployment/scripts/upgrader-start.sh
~/runtime/agent-control-plane-deployment/scripts/upgrader-stop.sh
```

### 自身升级（ACP）

deployment **不**在 worker 内对自己执行 `restartCmd`。流程：

1. 发版包 `packages/deployment-<hash>/` 内必须已有可执行 `bin/deployment-server`（制品由发版流水线准备，upgrader 不编译）。
2. `POST /api/deploys` 且 `serviceId=agent-control-plane-deployment`（`runtimeDir` 等于 `DEPLOYMENT_HOME`）：
   - rsync 制品到 runtime（保留 `data/`、`packages/`、`logs/`、pid、upgrade-requests）
   - 写入 `upgrade-requests/<requestId>.json`
   - 任务保持 `running`，等待独立 upgrader
3. `acp-upgrader`：`stop` → `start`（强制 `PORT=4220`）→ 探活；新进程 `reconcileOrphanDeploys` 收尾。

```bash
# 需先有 upgrader
~/runtime/agent-control-plane-deployment/scripts/upgrader-start.sh

curl -sS -X POST http://127.0.0.1:4220/api/deploys \
  -H 'content-type: application/json' \
  -d '{"serviceId":"agent-control-plane-deployment","deployment":"deployment-<hash>"}'
```

## 服务契约（SQLite，模式 B）

注册 / 更新：

```bash
curl -sS -X PUT http://127.0.0.1:4220/api/services/web-cursor \
  -H 'content-type: application/json' \
  -d '{
    "name": "Web Cursor Agent Gateway",
    "runtimeDir": "/Users/gaolei/runtime/web-cursor",
    "healthUrl": "http://127.0.0.1:4211/health",
    "startCmd": "bash scripts/start.sh",
    "stopCmd": "bash scripts/stop.sh",
    "restartCmd": "bash scripts/restart.sh"
  }'
```

首次启动若库中无 `web-cursor`，会自动 seed 一条默认契约（不含 graceful 端点 → 直接重启）。

### Graceful restart（可选）

在**本服务注册库**里为项目填写双方端点；**缺一或全空**视为不支持 graceful，部署时直接 rsync + restart。

| 字段 | 说明 |
|---|---|
| `restartNotifyUrl` | 部署前 `POST` 通知项目方即将重启 |
| `restartPollUrl` | 每 **15s** `GET` 轮询；JSON 含 `canRestart` / `canDeploy` / `ready` 任一为 `true` 则开始部署 |
| `gracefulRestartMaxWaitMs` | 可选；超时后**强制**继续部署。`0` 或省略则用环境变量 `GRACEFUL_RESTART_MAX_WAIT_MS`（默认 10 分钟） |

```bash
curl -sS -X PUT http://127.0.0.1:4220/api/services/web-cursor \
  -H 'content-type: application/json' \
  -d '{
    "restartNotifyUrl": "http://127.0.0.1:4211/api/ops/restart-notify",
    "restartPollUrl": "http://127.0.0.1:4211/api/ops/restart-status",
    "gracefulRestartMaxWaitMs": 600000
  }'
```

Notify 请求体示例：`{ serviceId, requestId, deployment, version, message }`。

部署步骤：若已配置 graceful → notify + 轮询（或超时强制）→ rsync → `restartCmd` → 探活 `healthUrl`。

本服务**不再**内置 watchdog（不探活、不自动 `startCmd`）。应用存活由外部 ops（如 `~/deployment/web-cursor/ops/watchdog.sh`）负责。

## 发版与部署

```bash
# 1) 构建包 → packages/deployment-<hash>/
./bin/release.sh main

# 2) HTTP 触发部署（不再写 deploy-requests 文件）
curl -sS -X POST http://127.0.0.1:4220/api/deploys \
  -H 'content-type: application/json' \
  -d '{"serviceId":"web-cursor","deployment":"deployment-<hash>"}'

curl -sS http://127.0.0.1:4220/api/deploys/<requestId>
```

部署期间防抖（重要）：

- **禁止**把本服务的 `PORT`/`HOST` 传给应用的 `restartCmd`（否则 `stop.sh` 会误杀 `:4220`）。
- 仅在 restart 窗口写入短 TTL 的 `watchdog-pause-until`（≤90s）；rsync 期间不暂停。
- 本服务启动时清理残留 pause / `.watchdog-paused`，并 reconcile 卡在 `running` 的任务。

## API 一览

| Method | Path | 说明 |
|---|---|---|
| GET | `/health` | 本服务探活 |
| GET/PUT/DELETE | `/api/services[/:id]` | 服务契约（含可选 graceful URL） |
| POST | `/api/deploys` | 入队部署 |
| GET | `/api/deploys[/:id]` | 查询任务 |
| GET | `/api/meta` | 含 `gracefulPollIntervalSec` / `gracefulMaxWaitMs` |

旧的 `ops/` 文件队列守护已废弃，保留目录仅作历史参考；请用本 HTTP 服务。
