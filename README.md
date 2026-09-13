# agent-control-plane-deployment

独立部署控制面：HTTP API + SQLite 服务契约，与 app（web-cursor）进程/目录完全解耦。

## 本机布局

| 路径 | 角色 |
|---|---|
| `~/runtime/agent-control-plane-deployment` | 本服务安装与数据根（`DEPLOYMENT_HOME`） |
| `$HOME/packages/<serviceId>/deployment-*` | 发版包（按服务名 serviceId 隔离） |
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

1. 制品由 `build.sh` 产出 `outputs/`，经当前制品存储后端（`ARTIFACT_STORAGE`，默认 `github_release`）保存：`github_release` 模式打成 `package.tar.gz` 上传到该服务仓库的 GitHub Release（tag=`deployment-<hash>`），**不落本地 `packages/`**；`local` 模式则存到 `<packagesDir>/<serviceId>/deployment-<hash>/`。本仓库自带 `build.sh`，因此 `POST /api/deploy-notify {serviceId:"agent-control-plane-deployment"}`（或 `DEPLOY_SERVICE_ID=agent-control-plane-deployment ./bin/release.sh main`）可直接打包+部署自身。`github_release` 模式需环境变量 `GITHUB_TOKEN`（或 `gh auth`），且对目标仓库有 `contents:write`。
2. `POST /api/deploys` 且 `serviceId=agent-control-plane-deployment`（`runtimeDir` 等于 `DEPLOYMENT_HOME`）：
   - **graceful**：ACP 自身也注册了 `restartNotifyUrl`/`restartPollUrl`（`POST /restart/notify`、`GET /restart/poll`，端口同 API）。部署前先通知自己进入 drain（worker 停止认领新任务），轮询直到无其它在途部署/流水线，再继续。
   - 从 release 下载 `package.tar.gz` 到临时目录 → rsync 到 runtime（保留 `data/`、`packages/`、`logs/`、pid、upgrade-requests）→ 删临时目录
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

### 服务方一键：通知 → 打包 → graceful 部署

业务服务调用 ACP 的部署通知接口后，由 ACP **统一打包**，再进入部署（部署前会调业务方 `restartNotifyUrl`，再轮询 `restartPollUrl`，就绪后 rsync+restart）：

```bash
# 服务契约需含 gitRepoUrl（以及建议配置 graceful URL）
curl -sS -X PUT http://127.0.0.1:4220/api/services/web-cursor \
  -H 'content-type: application/json' \
  -d '{"gitRepoUrl":"https://github.com/kaulie/agent-control-plane"}'

curl -sS -X POST http://127.0.0.1:4220/api/deploy-notify \
  -H 'content-type: application/json' \
  -d '{"serviceId":"web-cursor"}'
# 未传 ref → 默认拉该服务 defaultBranch（缺省 main）的最新 tip

curl -sS http://127.0.0.1:4220/api/pipelines/<requestId>
```

流水线状态：`queued` → `packaging` → `deploying` → `succeeded`/`failed`。

### 手工发版 + 部署

```bash
# 1) 构建包 → packages/<serviceId>/deployment-<hash>/
DEPLOY_SERVICE_ID=web-cursor ./bin/release.sh main

# 2) HTTP 触发部署
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
| GET/PUT/DELETE | `/api/services[/:id]` | 服务契约（含 `gitRepoUrl`、可选 graceful URL） |
| POST | `/api/deploy-notify` | 服务方通知：打包 → 再部署 |
| GET | `/api/pipelines[/:id]` | 打包+部署流水线状态 |
| GET | `/api/pipelines/:id/events` | 流水线事件日志 |
| POST | `/api/deploys` | 已有包直接入队部署 |
| GET | `/api/deploys[/:id]` | 查询部署任务 |
| GET | `/api/deploys/:id/events` | 部署执行事件日志 |
| GET | `/api/artifacts[?serviceId=]` | 制品元数据列表（本地表，存储后端为纯存储） |
| GET | `/api/artifacts/:tag?serviceId=` | 单个制品元数据（含访问路径 `assetUrl`/`browserDownloadUrl`） |
| POST | `/api/artifacts/scan?serviceId=` | 扫描当前存储后端的制品，回填本地 artifacts 表 |
| POST | `/restart/notify` | ACP 自身 graceful：通知进入 drain |
| GET | `/restart/poll` | ACP 自身 graceful：轮询是否可重启 |
| GET | `/api/meta` | 含 graceful / release 配置 |

旧的 `ops/` 文件队列守护已废弃，保留目录仅作历史参考；请用本 HTTP 服务。

## 制品存储（可插拔）+ 本地 artifacts 表

制品存储做成**可插拔**后端：本地磁盘（`local`）或 GitHub Releases（`github_release`），后续可扩展其它云存储（S3 等）。无论哪种后端，本地 `artifacts` 表始终是**存储无关的元数据索引**（含访问路径 `assetUrl`），部署/面板据此解析包，无需每次回查后端。

- 选择后端：环境变量 `ARTIFACT_STORAGE`，取值 `local` | `github_release`。留空时：配置了 `GITHUB_TOKEN` → `github_release`，否则 → `local`。`/api/meta` 的 `artifactStorage` 反映当前后端。
- `local`：包存 `<packagesDir>/<serviceId>/deployment-<hash>/`（原始本地布局，无需凭证）。上传=rsync 落盘，下载=rsync 到临时目录→rsync 到 runtime。
- `github_release`：包打成 `package.tar.gz` 上传到**每个服务自己仓库**的 GitHub Release（tag=`deployment-<hash>`，asset=`package.tar.gz`），不再落本地 `packages/`（release 作为唯一来源，节省本地存储）。需 `GITHUB_TOKEN`（回退 `GH_TOKEN`），对被部署服务仓库有 `contents:write`（上传）/`contents:read`（下载私有 repo）。`/api/meta` 的 `githubReleaseEnabled` 反映 token 是否配置。
- 打包：`packageFromGit`（`POST /api/deploy-notify` 或 `bin/release.sh`）clone+build 后，经当前后端 `Upload` 存储包，随后删本地临时构建目录。重复打包同 commit 会跳过构建（后端 `Exists` 命中）。上传成功后在本地 `artifacts` 表记录一行（`assetUrl`/`browserDownloadUrl`/`size`/`storage` 等）。
- 部署：`POST /api/deploys` 经后端 `Exists` 校验制品存在；`executeDeploy` 优先用 `artifacts` 表里的 `assetUrl` 直接下载（跳过解析），缺则回退到按 tag 解析；下载到临时目录 → rsync 到 runtime → 删临时目录。
- 一次性迁移旧本地包到 GitHub Releases：`./bin/upload-existing-packages.sh [--purge]`，遍历 `packages/<serviceId>/deployment-<hash>/` 上传到对应 release，`--purge` 上传成功后删本地包。
- 回填 artifacts 表：`POST /api/artifacts/scan?serviceId=<id>` 经当前后端 `List` 扫描，把制品元数据写进本地表。查询：`GET /api/artifacts[?serviceId=]`、`GET /api/artifacts/:tag?serviceId=`。

> 扩展新后端：实现 `ArtifactStorage` 接口（`Name/Exists/Upload/Download/List`，见 `src/artifact_storage.go`），在 `NewArtifactStorage` 工厂里注册新取值即可，调用方（`packageFromGit`/`executeDeploy`/`assertRelease`/`handleScanArtifacts`）无需改动。

## Web 控制面板（独立 panel）

本服务内置一个**独立前端项目**（`web/`，原生 HTML + CSS + JS，无构建步骤、无框架），
由 `deployment-server` 在**同一端口**（`:4220`）直接托管，无需额外进程或端口。

- 入口：`http://127.0.0.1:4220/` → 302 到 `/panel/`（`index.html`）
- 静态资源：`/panel/styles.css`、`/panel/app.js`（从 `DEPLOYMENT_HOME/web` 读取）
- 与 `/health`、`/api/*` 完全隔离，互不冲突

面板功能：

| 面板 | 能力 |
|---|---|
| 服务契约 | 列表 / 新建 / 编辑 / 删除（`PUT`/`DELETE /api/services`） |
| 部署流水线 | 列表 + 触发打包→部署（`POST /api/deploy-notify`），实时状态轮询 |
| 部署任务 | 列表 + 触发已有包部署（`POST /api/deploys`） |
| 元信息 | 展示 `/api/meta` |

状态徽标：`queued`/`packaging`/`deploying`/`running`/`succeeded`/`failed`/`cancelled`。
默认每 3s 自动刷新当前页签，可在右上角关闭。`install.sh` 已把 `web/` 一并 rsync 到 runtime。

