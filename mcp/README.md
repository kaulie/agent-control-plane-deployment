# deployment-mcp

把部署控制面（`http://127.0.0.1:4220`）的 HTTP API 收成一个 stdio MCP 服务，给 Cursor / Agent 当技能用。

本进程只转发请求。打包和部署仍由控制面异步执行（接口返回 202）。它不调用 `/restart/notify` 或 `/restart/poll`：那两个接口是控制面自己排空用的，不是给 agent 的工具。

在终端里执行 `deployment-mcp --help` 会打印用法。不带参数、且标准输入是终端时也打印用法并退出。Cursor 用管道启动它时才进入 stdio MCP。

## HTTP

控制面进程同时在 `/mcp` 提供 Streamable HTTP。别的机器把 Cursor 的 MCP 配成这个地址即可，不用在每台机器上放二进制。监听地址跟控制面一样，默认 `0.0.0.0:4220`。只要本机访问时把 `DEPLOYMENT_HOST` 设成 `127.0.0.1`。身份仍然是请求头 `identity_role` / `identity_id`，会记成这次发版的触发者。这不是新的凭证，不要裸暴露到公网。

```json
{
  "mcpServers": {
    "deployment": {
      "url": "http://127.0.0.1:4220/mcp",
      "headers": {
        "identity_role": "agent",
        "identity_id": "cursor-agent"
      }
    }
  }
}
```

控制面还没带上这段代码时，可以先单独起一个 HTTP MCP，由它去调已经在跑的 `:4220`：

```bash
go build -o bin/deployment-mcp ./cmd/deployment-mcp
./bin/deployment-mcp --http 127.0.0.1:4221
```

独立进程的 MCP 地址是 `http://127.0.0.1:4221`（根路径）。控制面自己提供的地址是 `http://<控制面主机>:<端口>/mcp`。

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
go build -o bin/deployment-mcp ./cmd/deployment-mcp
```

`build.sh` 和 `install.sh` 会把这个二进制打进 `bin/deployment-mcp`。控制面自己的下一次安装或发版之后，路径是：

```text
~/runtime/agent-control-plane-deployment/bin/deployment-mcp
```

开发时也可以不安装，直接：

```bash
go run ./cmd/deployment-mcp
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
