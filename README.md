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

## 服务目录（唯一真源：service_registry）

**本服务不再自建服务契约**。服务列表统一从 **service-registry**（`:4240`）拉取：

```
service-registry :4240  ──pull(GET /v1/services)──▶  本控制面 :4220
（契约 / API / 实例，唯一真源）                        （只存"部署参数"）
```

`GET /api/services` 返回**合并视图**：注册中心的契约（`registry`，含 namespace / version / owner / description / tags / gitRepoUrl / API 端点）+ 本机的部署配置：

| 状态 | 含义 | 面板 |
|---|---|---|
| `registered:true, configured:true` | 注册中心有 + 本机配了部署参数 | 「编辑配置」 |
| `registered:true, configured:false` | 注册中心有，本机还没配 | 「配置」（否则不会出现在触发下拉里） |
| `registered:false, configured:true` | 本机有配置，但注册中心没返回（未登记，或注册中心拉取失败） | 「未登记」徽标 + 仍可编辑 |

响应里还带 `registry: {url, enabled, ok, services, error}`，面板顶部据此显示「在线 · N 个服务 / 拉取失败」，**拉取失败不会伪装成"没有服务"**。

本机只存注册中心没有的部署参数：`runtimeDir` / `healthUrl` / `startCmd` / `stopCmd` / `restartCmd` / 可选 graceful 端点 / `defaultBranch`。`name`、`version`、`owner`、`tags`、**`gitRepoUrl`** 以注册中心为准（`gitRepoUrl` 本机留空即用注册中心登记的值；打包、部署已有包、扫描制品都走这个兜底）。

配置 / 编辑：

```bash
# 只能配置"已在注册中心登记"的服务（未登记 → 400）
curl -sS -X PUT http://127.0.0.1:4220/api/services/web-cursor \
  -H 'content-type: application/json' \
  -d '{
    "runtimeDir": "/Users/gaolei/runtime/web-cursor",
    "healthUrl": "http://127.0.0.1:4211/health",
    "startCmd": "bash scripts/start.sh",
    "stopCmd": "bash scripts/stop.sh",
    "restartCmd": "bash scripts/restart.sh"
  }'
# 首次配置且没给 gitRepoUrl → 自动取注册中心登记的仓库地址
```

- 已存在本地配置的服务照旧可改（注册中心不可用也不会把运维锁死）；**未登记**的服务一律拒绝新建：`400 未在 service_registry 中登记`；注册中心不可用/未配置时无法确认 → `503`。
- `DELETE /api/services/{id}` = **清除本机部署配置**（服务仍在注册中心，可重新配置）。
- 首次启动仍会 seed `web-cursor` / `agent-control-plane-deployment` 的本地部署配置（注册中心里登记它们之前会显示「未登记」）。
- 关闭注册中心拉取：`SERVICE_REGISTRY_URL=off`（此时面板只显示本机已配置的服务，且不能新建）。

### 配置项

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `SERVICE_REGISTRY_URL` | `http://127.0.0.1:4240` | 服务目录来源；`off` / `disabled` / `none` = 关闭拉取 |
| `SERVICE_REGISTRY_TOKEN` | 空 | 注册中心读令牌（`REGISTRY_READ_AUTH=token` 时用） |
| `SERVICE_REGISTRY_TIMEOUT_SEC` | `5` | 单次拉取超时（秒） |


### Graceful restart（可选）

在本机的**部署配置**里为项目填写双方端点；**缺一或全空**视为不支持 graceful，部署时直接 rsync + restart。

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
# 服务需在 service_registry 里登记（gitRepoUrl 以注册中心为准），
# 并在本机配置过部署参数 + （建议）graceful URL。
curl -sS -X POST http://127.0.0.1:4220/api/deploy-notify \
  -H 'content-type: application/json' \
  -H 'identity_role: agent' -H 'identity_id: agent_002' \
  -d '{"serviceId":"web-cursor"}'
# 未传 ref → 默认拉该服务 defaultBranch（缺省 main）的最新 tip
# identity_role / identity_id 为必填（第一阶段身份校验，见下节）

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
  -H 'identity_role: user' -H 'identity_id: user_001' \
  -d '{"serviceId":"web-cursor","deployment":"deployment-<hash>"}'

curl -sS http://127.0.0.1:4220/api/deploys/<requestId>
```

部署期间防抖（重要）：

- **禁止**把本服务的 `PORT`/`HOST` 传给应用的 `restartCmd`（否则 `stop.sh` 会误杀 `:4220`）。
- 仅在 restart 窗口写入短 TTL 的 `watchdog-pause-until`（≤90s）；rsync 期间不暂停。
- 本服务启动时清理残留 pause / `.watchdog-paused`，并 reconcile 卡在 `running` 的任务。

### 临时文件（构建包 / 制品下载）的生命周期

打包与部署全程用系统临时目录（`os.TempDir()`）当工作区，名字带前缀，**用完必须删干净**：

| 前缀 | 用途 | 生命周期 |
|---|---|---|
| `release-acp-*` | `packageFromGit` 的构建树：`git fetch` + `build.sh`（**含 `build.sh` 建在树里的 `.gocache`/`.gomodcache`**） | 打包函数返回时删除 |
| `release-pkg-*` | `outputs/` 的打包暂存目录 → 交给制品存储后端上传 | 上传结束（或失败）时删除 |
| `deploy-pkg-*` | 部署时把制品下载到本地、再 rsync 到 `runtimeDir` | 部署函数返回时删除（自升级被 kill 的情况见下） |

两个坑（都已在代码里处理 + 有回归测试）：

1. **只读的 Go 缓存让删除失败**：`build.sh` 把 `GOCACHE`/`GOMODCACHE`/`GOPATH` 建在构建树里，Go 会把模块缓存设成 `0555`/`0444`。从只读目录里 unlink 子项需要目录可写，于是 `os.RemoveAll`（以及 `rm -rf`）会 **`Permission denied`、只删掉一部分**——实测每次构建在 `/var/folders` 里残留约 **460 MB**（恰好剩 `src/.gomodcache`）。现在统一走 `removeAllForce()`：先 `chmod u+w` 整棵树再删，且**不再吞掉错误**（`bin/release.sh` 的 `trap cleanup` 同样先 `chmod -R u+w`）。
2. **被 kill 的进程来不及删**：自升级时 `acp-upgrader` 会 `stop` 掉本进程，`deploy-pkg-*` 以及正在构建的 `release-acp-*` 会留在临时目录里（defer 不会执行）。因此：
   - **启动时**（`worker` 起来之前，此刻不可能有本进程的构建在跑）扫一遍临时目录，把这些前缀**全部**清掉，并把释放的字节数写进日志；
   - 运行期每 30 分钟兜底扫一次，只清"超过 1 小时没动过"的（避免误删正在跑的构建）；
   - `upgrade-requests/` 里 acp-upgrader 处理完的记录（`*.json.done` / `*.json.failed` / `*.json.bad`）保留 7 天后清理；**待处理的 `*.json` 永远不动**（upgrader 还要读）。

## API 一览

| Method | Path | 说明 |
|---|---|---|
| GET | `/health` | 本服务探活 |
| GET | `/api/services` | **服务目录**：service_registry 契约 + 本机部署配置的合并视图（含 `registered` / `configured` / `registry` 状态） |
| GET | `/api/services/:id` | 单个服务的合并视图 |
| PUT | `/api/services/:id` | **只配置**已登记服务的部署参数（未登记 → 400；注册中心不可用 → 503） |
| DELETE | `/api/services/:id` | 清除本机部署配置（服务仍在注册中心） |
| POST | `/api/deploy-notify` | 服务方通知：打包 → 再部署（**需身份头**） |
| GET | `/api/pipelines` | 流水线列表（**多属性筛选 + 分页**，见下） |
| GET | `/api/pipelines/:id` | 单条流水线状态 |
| GET | `/api/pipelines/:id/events` | 流水线事件日志 |
| POST | `/api/deploys` | 已有包直接入队部署（**需身份头**） |
| GET | `/api/deploys` | 部署任务列表（**多属性筛选 + 分页**，见下） |
| GET | `/api/deploys/:id` | 查询单条部署任务 |
| GET | `/api/deploys/:id/events` | 部署执行事件日志 |
| GET | `/api/artifacts[?serviceId=]` | 制品元数据列表（本地表，存储后端为纯存储） |
| GET | `/api/artifacts/:tag?serviceId=` | 单个制品元数据（含访问路径 `assetUrl`/`browserDownloadUrl`） |
| POST | `/api/artifacts/scan?serviceId=` | 扫描当前存储后端的制品，回填本地 artifacts 表 |
| POST | `/restart/notify` | ACP 自身 graceful：通知进入 drain |
| GET | `/restart/poll` | ACP 自身 graceful：轮询是否可重启 |
| GET | `/api/meta` | 含 graceful / release / 身份校验 配置 |

### 事件级别（部署流水线 / 部署任务）

流水线事件（`GET /api/pipelines/:id/events`）与部署事件（`GET /api/deploys/:id/events`）统一使用同一套 canonical 级别名：

| level | 含义 |
|---|---|
| `info` | 信息性进展事件 |
| `success` | 步骤成功完成 |
| `warn` | 非致命问题（流程继续） |
| `error` | 当前步骤致命失败 |

写入事件时会把空级别规范为 `info`，把历史遗留的 `ok`（以及大小写变体）规范为 `success`，忽略首尾空白，其它未知名称也统一规范为 `info`；服务启动迁移会把事件表中已存在的 `ok` 改写为 `success`、其它非 canonical 名称改写为 `info`。面板与 API 按同一套名称渲染。

### 列表查询（`GET /api/deploys`、`GET /api/pipelines`）

两个列表接口支持同一组筛选 + 分页参数（都不传 = 全部 / 第 1 页），响应统一含 `total` / `page` / `pageSize`。

| 参数 | 说明 |
|---|---|
| `serviceId` | 精确匹配服务 |
| `state` | 精确匹配状态（deploy：`queued`/`running`/`succeeded`/`failed`/`cancelled`；pipeline：`queued`/`packaging`/`deploying`/`succeeded`/`failed`） |
| `triggeredByRole` / `triggeredById` | 触发者身份（精确匹配） |
| `deployment` / `version` | 包含匹配 |
| `ref` | 仅 pipeline：包含匹配 |
| `q` | 关键字：对 `request_id` / `service_id` / `deployment` / `version` / `message` / `error` / `triggered_by_id` 做包含匹配 |
| `from` / `to` | `requested_at` 闭区间，ISO 字符串（面板按 UTC 整天传入） |
| `page` / `pageSize` | 分页（默认 `1` / `20`，`pageSize` 上限 200；`limit` 仍是 `pageSize` 的别名） |

```bash
# 失败的部署，第 2 页，每页 10 条
curl -sS 'http://127.0.0.1:4220/api/deploys?state=failed&page=2&pageSize=10'
# web-cursor 的流水线，ref 含 "feature"
curl -sS 'http://127.0.0.1:4220/api/pipelines?serviceId=web-cursor&ref=feature'
```

旧的 `ops/` 文件队列守护已废弃，保留目录仅作历史参考；请用本 HTTP 服务。

## 身份校验（第一阶段）

触发部署的两个写接口必须带**身份头**，用于审计与面板展示（"谁触发的这次部署"）。本阶段只做最简单的校验，**没有密钥/token**：

| Header | 取值 | 说明 |
|---|---|---|
| `identity_role` | `user` \| `agent` | 调用方类型（大小写不敏感，做 trim） |
| `identity_id` | `user_001` / `agent_002` / … | 调用方标识，非空、无空白、≤64 字符 |

- 需要身份头的接口：`POST /api/deploys`、`POST /api/deploy-notify`。其它接口（只读、服务契约、制品扫描、`/restart/*`）**不校验**。
- 缺失或非法 → **401**，响应 `{"error":"missing identity: set headers identity_role (user|agent) and identity_id"}`。
- 通过的请求会把身份存进任务记录（`deploys` / `pipelines` 表的 `triggered_by_role` / `triggered_by_id`），API 返回 `triggeredByRole` / `triggeredById` / `triggeredBy`（`role:id`），并在事件里带上 `触发者=…`；面板"部署流水线 / 部署任务"列表与详情页显示"触发者"列。
- 逃生开关：`IDENTITY_ENFORCE=0`（或 `false`/`no`/`off`）→ 不拦截，缺头请求照常执行、身份记为未知（仅打日志）。默认开启；`GET /api/meta` 的 `identityEnforce` 反映当前值。
- 面板顶栏有身份选择器（`user`/`agent` + id，默认 `user/user_001`，记在浏览器 localStorage），所有写请求自动带上这两个头。

```bash
curl -sS -X POST http://127.0.0.1:4220/api/deploys \
  -H 'content-type: application/json' \
  -H 'identity_role: user' -H 'identity_id: user_001' \
  -d '{"serviceId":"web-cursor","deployment":"deployment-<hash>"}'
```

仓库内的调用方已同步带上身份：面板（`web/`，默认 `user:user_001`）、`bin/deploy.sh`（默认 `agent:deploy-agent`，可用 `IDENTITY_ROLE`/`IDENTITY_ID` 覆盖）、`ops/deploy-agent.sh`（透传）、`bin/release.sh` 打印的示例。⚠️ **仓库外的调用方**（例如 web-cursor 后端的 deploy-queue）需要自行补上这两个头，否则会收到 401。

## 制品存储（可插拔）+ 本地 artifacts 表

制品存储做成**可插拔**后端：本地磁盘（`local`）、GitHub Releases（`github_release`）或阿里云制品仓库（`aliyun`），后续可扩展其它云存储（S3 等）。无论哪种后端，本地 `artifacts` 表始终是**存储无关的元数据索引**（含访问路径 `assetUrl`），部署/面板据此解析包，无需每次回查后端。

- 选择后端：环境变量 `ARTIFACT_STORAGE`，取值 `local` | `github_release` | `aliyun`。留空时：配置了 `GITHUB_TOKEN` → `github_release`，否则 → `local`。`/api/meta` 的 `artifactStorage` 反映当前后端。
  - 本控制面自身的 `scripts/start.sh` 已默认 `export ARTIFACT_STORAGE=aliyun`（可用环境变量覆盖）。选择 `aliyun` 时若缺少凭证，`start.sh` 会**快速失败并给出明确提示**（否则服务会起不来）。
- `local`：包存 `<packagesDir>/<serviceId>/deployment-<hash>/`（原始本地布局，无需凭证）。上传=rsync 落盘，下载=rsync 到临时目录→rsync 到 runtime。
- `github_release`：包打成 `package.tar.gz` 上传到**每个服务自己仓库**的 GitHub Release（tag=`deployment-<hash>`，asset=`package.tar.gz`），不再落本地 `packages/`（release 作为唯一来源，节省本地存储）。需 `GITHUB_TOKEN`（回退 `GH_TOKEN`），对被部署服务仓库有 `contents:write`（上传）/`contents:read`（下载私有 repo）。`/api/meta` 的 `githubReleaseEnabled` 反映 token 是否配置。
- `aliyun`：包打成 `package.tar.gz` 上传到**阿里云制品仓库的 generic 仓库**（`packages.aliyun.com`），路径 `<serviceId>/<tag>/package.tar.gz`，version=`<tag>`。使用 HTTP basic 鉴权：
  - `ALIYUN_PACKAGES_USER` / `ALIYUN_PACKAGES_PASSWORD`（**必填，不入库也不进 git**；可由 `scripts/start.sh` 从 `data/aliyun-credentials` 加载，格式：第 1 行用户名、第 2 行密码，随 `data/` 在自升级时保留）。
  - `ALIYUN_PACKAGES_PRODUCT_ID`（默认 `6a1940346e68a85a0d176340`）、`ALIYUN_PACKAGES_REPO`（默认 `deployment-artifact`）、`ALIYUN_PACKAGES_BASE_URL`（默认 `https://packages.aliyun.com`）均非机密。
  - 上传调用 `POST {base}/api/protocol/{productId}/generic/{repo}/files/{filePath}?version=&fileName=&downloadFileName=`；下载调用 `GET .../files/{filePath}?version=`。**对象实际存储名 = `fileName`**（已实测；`downloadFileName` 只决定浏览器下载名），故本实现把两者都设为 `package.tar.gz`，与下载路径一致。`artifacts.assetUrl` 记录的是**带鉴权的持久下载地址**（上传接口返回的临时免密地址会过期，不落表）；`browserDownloadUrl` 留空（浏览器下载需 basic 凭证）。
  - 该协议未提供版本列举接口，故 `aliyun` 后端的 `List`（`POST /api/artifacts/scan` 回填）返回「不支持」；制品在**上传时**即写入本地 `artifacts` 表，正常打包/部署路径不受影响。
- 打包：`packageFromGit`（`POST /api/deploy-notify` 或 `bin/release.sh`）clone+build 后，经当前后端 `Upload` 存储包，随后删本地临时构建目录。重复打包同 commit 会跳过构建（后端 `Exists` 命中）。上传成功后在本地 `artifacts` 表记录一行（`assetUrl`/`browserDownloadUrl`/`size`/`storage` 等）。打包/上传过程会写流水线事件：`开始打包` → `上传开始` → `上传结束`（含 storage/tag/size/耗时；失败为 `上传失败`）。
- 部署：`POST /api/deploys` 经后端 `Exists` 校验制品存在；`executeDeploy` 优先用 `artifacts` 表里的 `assetUrl` 直接下载（跳过解析），缺则回退到按 tag 解析；下载到临时目录 → rsync 到 runtime → 删临时目录。下载过程会写部署事件：`下载开始` → `下载结束`（含 storage/tag/size/耗时；失败为 `下载失败`）。流水线详情页会把流水线事件与关联部署事件合并成一条时间线展示。
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
| 服务契约 | **只配置、不新建**：列表/来源来自 `service_registry`（顶部显示在线状态与服务数），为已登记服务配置部署参数（`PUT /api/services/:id`），可「清除本地配置」（`DELETE`） |
| 部署流水线 | 两个子页：**发起**（触发打包→部署，`POST /api/deploy-notify`） / **历史列表**（多属性筛选 + 分页，实时轮询） |
| 部署任务 | 两个子页：**发起**（触发已有包部署，`POST /api/deploys`） / **历史列表**（多属性筛选 + 分页，实时轮询） |
| 元信息 | 展示 `/api/meta` |

状态徽标：`queued`/`packaging`/`deploying`/`running`/`succeeded`/`failed`/`cancelled`。
默认每 3s 自动刷新当前页签，可在右上角关闭。`install.sh` 已把 `web/` 一并 rsync 到 runtime。

- **发起 / 历史列表 分离**：流水线、部署各自拆成「发起」与「历史列表」两个子页；历史列表支持按 服务 / 状态 / 触发者 / ref / deployment / version / 关键字 / 时间范围 筛选，并在**服务端分页**（每页 10/20/50/100）。筛选作为"已应用"快照生效，避免 3s 自动刷新把正在输入的内容当成筛选条件：部署流水线历史列表在点「查询」（或输入框回车 / 改每页）时应用，**部署任务历史列表只在点「查询」时应用**。
- **发起后自动进入详情页**：在「发起」子页提交后，自动切到「历史列表」并打开刚创建的那条记录的详情（流水线详情含事件时间线 + 关联部署任务；部署详情含该次部署的事件时间线）。历史列表里点任意一行也可打开详情。
- **面板行为测试**：`npm test`（首次先 `npm install`）用 jsdom 加载真实 `web/index.html` + `web/app.js` 并拦截 `fetch`，验证：①「部署任务历史列表」修改筛选项不会触发刷新、只有点「查询」才刷新；②「服务契约只配置不新建」——列表来自 `service_registry`（顶部显示在线状态 + 服务数）、未配置的服务不进触发下拉、面板里没有「新建」入口。

