---
name: deployment-platform
description: >-
  操作部署控制面（agent-control-plane-deployment，:4220）：查服务、制品、流水线和部署任务，改部署配置，或通过平台打包上线。用户提到部署、发版、上线、部署状态、部署机器、制品 tag 时使用。通过 deployment MCP 工具调用，不要在本机 rsync、restart 或改 runtime。
---

# 部署控制面

部署只走控制面 HTTP API。本仓库的 MCP 服务 `deployment-mcp`（`./mcp`）把这些 API 收成工具。

## 何时用

- 查某个服务现在部署在哪、版本是什么：`deployment_inventory`、`deployment_meta`、`deployment_get_service`
- 查能不能发、历史和在途任务：`deployment_list_services`、`deployment_service_history`、`deployment_list_pipelines`、`deployment_list_deploys`
- 部署一个已经打好的包：`deployment_deploy`（`deployment` tag 或 `hash`）
- 从 git ref 打包再部署：`deployment_release`（`ref` 可省略，走服务的 defaultBranch）
- 看进度：`deployment_get_pipeline`、`deployment_pipeline_events`、`deployment_get_deploy`、`deployment_deploy_events`

`deployment_release` 和 `deployment_deploy` 返回 HTTP 202，任务在控制面里异步跑。拿到 `requestId` 之后轮询对应的查询工具。

## 不要做的事

- 不要在 workspace 里执行 `install.sh`、`scripts/restart.sh`、rsync，或改 `/Users/gaolei/runtime/**`
- 不要调用控制面的 `/restart/notify` 和 `/restart/poll`。MCP 没有这两个工具，它们只给控制面自己排空
- `deployment_delete_service_config` 和 `deployment_move_service_history` 会改或搬走历史。只有用户明确要求时才调用
- `deployment_configure_service` 不能改 `gitRepoUrl`。省略的字段会沿用已有配置；不要为了改端口把其它字段传空字符串

## 身份

触发部署的请求带 `identity_role` / `identity_id`。默认来自 MCP 进程的 `IDENTITY_ROLE`（`agent`）和 `IDENTITY_ID`（`deployment-mcp`）。需要记到具体的人或 agent 上时，给 `deployment_deploy` / `deployment_release` 传 `identityRole` 和 `identityId`。

先用 `deployment_client` 确认连的是哪台控制面、默认身份是谁。
