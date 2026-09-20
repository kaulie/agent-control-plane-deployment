# client/ —— 服务契约登记（从注册中心 vendor 过来的）

这两个脚本**不是本仓库的代码**，是 [kaulie/service-registry](https://github.com/kaulie/service-registry)
里 `client/register.sh` 与 `client/ci/register-go-service.sh` 的原样拷贝，放在这里是为了让 CI /
发版脚本离线可跑（注册中心的脚本自己会浅克隆一份，本仓库选择 vendor）。**不要手改**：要升级就
整份覆盖，并更新下面的 provenance。

| provenance | 值 |
|---|---|
| 上游 | `https://github.com/kaulie/service-registry` |
| 版本 | `main` @ `1e245a82705d09646f26228e36257bcba7fcc32c`（2026-09-20） |
| `register.sh` sha256 | `26459b790511327b76df77496dfb49638f4572b7d4f25827e079b9adbd47c3a2` |
| `register-go-service.sh` sha256 | `7bbf77efa63ae4543458d9b03f80055a3c7669aba3d72f4c5e135a10edc35a8b` |

刷新（在能访问 GitHub 的机器上）：

```bash
tmp="$(mktemp -d)"
git clone --depth 1 https://github.com/kaulie/service-registry "$tmp/service-registry"
cp "$tmp/service-registry/client/register.sh" client/register.sh
cp "$tmp/service-registry/client/ci/register-go-service.sh" client/ci/register-go-service.sh
shasum -a 256 client/register.sh client/ci/register-go-service.sh   # 更新上表
```

## 用法：一条命令（读注解 → 生成规范 → 上报）

契约的真源是**代码里的 swag 注解**（`src/main.go` 顶部的 General API Info + `src/server.go`
每个 handler 上的 `@Summary/@Tags/@Router/@Param/@Success`），`docs/swagger.json` 是生成物。
下面这条命令会自己 `swag init` 从注解生成规范，然后幂等地登记到注册中心（契约没变化时不会
刷 revision）：

```bash
SERVICE_NAME=agent-control-plane-deployment \
REGISTRY_URL=http://127.0.0.1:4240 \
SWAG_MAIN=src/main.go \
SWAG_OUT=docs \
SWAG_ARGS="--parseInternal --outputTypes json" \
DEPARTMENT_ID=D0004 \
INSTANCES=127.0.0.1:4220 \
OWNER=kaulie HEALTH_PATH=/health VERSION="$(git describe --tags --always)" \
  bash client/ci/register-go-service.sh
```

- `REGISTRY_URL` **必须显式给**：`register.sh` 的默认值是 `http://127.0.0.1:${SERVICE_PORT:-4240}`，
  而 `SERVICE_PORT` 常已被"当前服务的端口"占用（本机预设 `4211`），不写死就可能把契约 PUT 到
  **别的服务**上（症状是 404 + 一段不像注册中心的 JSON）。想让默认值可用，直接 `export SERVICE_PORT=4240`。
- `SWAG_MAIN=src/main.go`：脚本默认探测 `./cmd/<service>/main.go` → `./main.go`，本仓库入口在
  `src/`（服务名 `agent-control-plane-deployment` 也对不上），必须显式给。
- `SWAG_OUT=docs`：生成物固定为仓库里的 `docs/swagger.json`（默认也会写到 `docs/`，这里写明是为了自证）。
- `SWAG_ARGS="--parseInternal --outputTypes json"`：`--parseInternal` 让 swag 顺着本模块解析
  `eventlevel` 等内部包；`--outputTypes json` 只产出 `docs/swagger.json`，**不产出 `docs.go`**
  （否则会凭空多出一个需要 swag 运行期依赖的 Go 包）。注意 `-g` 是相对仓库根、`-d` 默认 `.`，
  所以别写 `-d src`（会被拼成 `src/src/main.go`）。
- `INSTANCES` / `DEPARTMENT_ID` / `OWNER` 都可用环境变量覆盖，部署换端口时不用改仓库。
- 注册中心默认只绑 `127.0.0.1:4240`（写接口默认开放，刻意不暴露到网络），所以这条命令要跑在
  **本机 / self-hosted runner** 上；GitHub-hosted runner 够不到。

自动化挂在 `build.sh` 末尾（发版打包完成后；失败只告警，不挡发布；`REGISTER_CONTRACT=0` 可跳过）。
