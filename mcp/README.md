# deployment-mcp

把部署控制面（`http://127.0.0.1:4220`）的 HTTP API 收成一个 stdio MCP 服务，给 Cursor / Agent 当技能用。

本进程只转发请求。打包和部署仍由控制面异步执行（接口返回 202）。它不调用 `/restart/notify` 或 `/restart/poll`：那两个接口是控制面自己排空用的，不是给 agent 的工具。

## 环境变量

| 变量 | 默认 | 作用 |
|---|---|---|
| `DEPLOYMENT_URL` | `http://127.0.0.1:4220` | 控制面地址，仅 `http` / `https` |
| `IDENTITY_ROLE` | `agent` | 触发部署时的 `identity_role`（`user` 或 `agent`） |
| `IDENTITY_ID` | `deployment-mcp` | 触发部署时的 `identity_id`，写进流水线 / 部署记录的触发者 |
| `DEPLOYMENT_HTTP_TIMEOUT` | `60s` | 单次 HTTP 超时（Go duration） |

`POST /api/deploys` 和 `POST /api/deploy-notify` 在控制面开启身份校验时必须带这两个头。其它工具也会带上同一组默认身份。单次发版可以用工具参数 `identityRole` / `identityId` 覆盖。

## 构建

```bash
go build -o bin/deployment-mcp ./mcp
```

`build.sh` 和 `install.sh` 会把这个二进制打进 `bin/deployment-mcp`。控制面自己的下一次安装或发版之后，路径是：

```text
~/runtime/agent-control-plane-deployment/bin/deployment-mcp
```

开发时也可以不安装，直接：

```bash
go run ./mcp
```

## 接到 Cursor

```json
{
  "mcpServers": {
    "deployment": {
      "command": "/Users/gaolei/runtime/agent-control-plane-deployment/bin/deployment-mcp",
      "env": {
        "DEPLOYMENT_URL": "http://127.0.0.1:4220",
        "IDENTITY_ROLE": "agent",
        "IDENTITY_ID": "cursor-agent"
      }
    }
  }
}
```

仓库还带了一份给 agent 读的技能说明：`.cursor/skills/deployment-platform/SKILL.md`。

## 工具

只读：

| 工具 | 控制面 |
|---|---|
| `deployment_client` | 本进程的地址和默认身份（不访问网络） |
| `deployment_health` | `GET /health` |
| `deployment_meta` | `GET /api/meta` |
| `deployment_inventory` | `GET /api/deployment-inventory` |
| `deployment_list_services` / `deployment_get_service` | `GET /api/services` |
| `deployment_service_history` | `GET /api/services/{serviceId}/history` |
| `deployment_list_artifacts` / `deployment_get_artifact` | `GET /api/artifacts` |
| `deployment_list_deploys` / `deployment_get_deploy` / `deployment_deploy_events` | `GET /api/deploys` |
| `deployment_list_pipelines` / `deployment_get_pipeline` / `deployment_pipeline_events` | `GET /api/pipelines` |

会改控制面状态：

| 工具 | 控制面 | 说明 |
|---|---|---|
| `deployment_configure_service` | `PUT /api/services/{serviceId}` | 省略的字段沿用已有配置。`gitRepoUrl` 不能改 |
| `deployment_delete_service_config` | `DELETE /api/services/{serviceId}` | 只删本机部署配置 |
| `deployment_move_service_history` | `POST .../history/move` | 迁移历史，可选删来源配置 |
| `deployment_scan_artifacts` | `POST /api/artifacts/scan` | 回填制品索引 |
| `deployment_deploy` | `POST /api/deploys` | 部署已有制品，返回 202 |
| `deployment_release` | `POST /api/deploy-notify` | 按 git ref 打包再部署，返回 202 |

发版之后用 `deployment_get_pipeline` 或 `deployment_get_deploy` 看状态，不要在 workspace 里跑重启或 rsync。
