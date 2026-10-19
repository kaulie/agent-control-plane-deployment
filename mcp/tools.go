package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// skill is the deployment control plane exposed as MCP tools.
type skill struct {
	client *Client
}

func (s *skill) register(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_client",
		Description: "查看本 MCP 连的是哪一个部署控制面，以及触发部署时默认使用的身份（identity_role / identity_id）。不访问网络。",
	}, s.clientInfo)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_health",
		Description: "部署控制面健康检查（GET /health）。确认 :4220 进程在跑。",
	}, s.health)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_meta",
		Description: "控制面元信息（GET /api/meta）：路径、端口、制品后端、注册中心状态、可部署机器。serviceId 可选，用来把部署机器列表收窄到该服务。",
	}, s.meta)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_inventory",
		Description: "部署清单（GET /api/deployment-inventory）：每个服务在各台机器上当前部署的版本。",
	}, s.inventory)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_list_services",
		Description: "服务目录（GET /api/services）：service-registry 契约合并本机部署配置。看 registered / configured 判断能不能发版。",
	}, s.listServices)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_get_service",
		Description: "单个服务目录项（GET /api/services/{serviceId}）。未登记且本机也没配置时 404。",
	}, s.getService)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_service_history",
		Description: "服务历史概览（GET /api/services/{serviceId}/history）：流水线 / 部署 / 制品条数、在途任务、可迁移的目标服务。",
	}, s.serviceHistory)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_configure_service",
		Description: "配置或更新服务的本机部署参数（PUT /api/services/{serviceId}）。只改已在 service-registry 登记的服务。省略的字段沿用已有配置；首次配置必须给出 port、runtimeDir、healthUrl、startCmd、stopCmd、restartCmd。gitRepoUrl 只读，不能通过本工具修改。runtimeEnv 一旦传入会整份替换；要清空用 clearRuntimeEnv。",
	}, s.configureService)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_delete_service_config",
		Description: "清除该服务的本机部署配置（DELETE /api/services/{serviceId}）。不删注册中心契约，不删历史记录。有在途任务时 409。这是破坏性操作，只在用户明确要求清除配置时调用。",
	}, s.deleteService)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_move_service_history",
		Description: "把一个服务名下的流水线、部署记录和制品索引迁到另一个已配置的服务（POST /api/services/{serviceId}/history/move）。deleteSourceContract 为 true 时顺带删来源的本机配置。有在途任务时整单拒绝。这是破坏性操作，只在用户明确要求迁移历史时调用。",
	}, s.moveHistory)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_list_artifacts",
		Description: "制品索引（GET /api/artifacts）。serviceId 可选。tag 形如 deployment-<hash>。",
	}, s.listArtifacts)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_get_artifact",
		Description: "单个制品（GET /api/artifacts/{tag}?serviceId=）。tag 在服务内唯一，所以 serviceId 必填。",
	}, s.getArtifact)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_scan_artifacts",
		Description: "扫描该服务的制品存储并回填本地索引（POST /api/artifacts/scan?serviceId=）。需要服务已登记 gitRepoUrl。aliyun 后端可能不支持列举。",
	}, s.scanArtifacts)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_list_deploys",
		Description: "部署任务历史（GET /api/deploys），筛选加分页。state 取值 queued、running、succeeded、failed、cancelled。",
	}, s.listDeploys)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_get_deploy",
		Description: "单个部署任务（GET /api/deploys/{requestId}）。发版返回 202 之后用它看状态，不要在本地等进程退出。",
	}, s.getDeploy)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_deploy_events",
		Description: "部署任务事件时间线（GET /api/deploys/{requestId}/events）。",
	}, s.deployEvents)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_deploy",
		Description: "部署一个已经打好的制品（POST /api/deploys）。传 deployment tag，或传 hash（会当成 tag）。可选 targetMachine，必须是 deployment_meta 里的已知机器，空则用默认机器。返回 HTTP 202，任务异步执行；接着用 deployment_get_deploy 和 deployment_deploy_events 看结果。不要在本机执行 restart、rsync 或 runtime 目录里的部署脚本。",
	}, s.deploy)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_list_pipelines",
		Description: "部署流水线历史（GET /api/pipelines），筛选加分页。state 取值 queued、packaging、deploying、succeeded、failed。",
	}, s.listPipelines)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_get_pipeline",
		Description: "单个部署流水线（GET /api/pipelines/{requestId}）。deployment_release 返回 202 之后用它看打包和部署进度。",
	}, s.getPipeline)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_pipeline_events",
		Description: "流水线事件时间线（GET /api/pipelines/{requestId}/events）。",
	}, s.pipelineEvents)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deployment_release",
		Description: "按 git ref 打包并部署（POST /api/deploy-notify）。ref 省略时用该服务的 defaultBranch。useProxy 为 true 时打包走控制面本机代理。targetMachine 可选，必须是已知部署机器。返回 HTTP 202，流水线异步执行；接着用 deployment_get_pipeline 看进度。不要在 workspace 里跑 build.sh 或安装脚本去上线。",
	}, s.release)
}

type emptyArgs struct{}

type serviceIDArgs struct {
	ServiceID string `json:"serviceId" jsonschema:"服务 id（service-registry 里的 name）"`
}

type metaArgs struct {
	ServiceID string `json:"serviceId,omitempty" jsonschema:"可选。按这个服务收窄部署机器列表"`
}

type artifactArgs struct {
	ServiceID string `json:"serviceId" jsonschema:"服务 id"`
	Tag       string `json:"tag" jsonschema:"制品 tag，形如 deployment-<hash>"`
}

type requestIDArgs struct {
	RequestID string `json:"requestId" jsonschema:"任务 id"`
}

type deployListArgs struct {
	ServiceID       string `json:"serviceId,omitempty" jsonschema:"按服务 id 过滤"`
	State           string `json:"state,omitempty" jsonschema:"queued、running、succeeded、failed 或 cancelled"`
	TriggeredByRole string `json:"triggeredByRole,omitempty" jsonschema:"触发者角色：user 或 agent"`
	TriggeredByID   string `json:"triggeredById,omitempty" jsonschema:"触发者 id"`
	Deployment      string `json:"deployment,omitempty" jsonschema:"deployment tag"`
	Version         string `json:"version,omitempty" jsonschema:"版本"`
	Q               string `json:"q,omitempty" jsonschema:"关键字"`
	From            string `json:"from,omitempty" jsonschema:"requestedAt 下界，ISO-8601"`
	To              string `json:"to,omitempty" jsonschema:"requestedAt 上界，ISO-8601"`
	Page            *int   `json:"page,omitempty" jsonschema:"页码，默认 1"`
	PageSize        *int   `json:"pageSize,omitempty" jsonschema:"每页条数，默认 20，最大 200"`
}

type pipelineListArgs struct {
	ServiceID       string `json:"serviceId,omitempty" jsonschema:"按服务 id 过滤"`
	State           string `json:"state,omitempty" jsonschema:"queued、packaging、deploying、succeeded 或 failed"`
	TriggeredByRole string `json:"triggeredByRole,omitempty" jsonschema:"触发者角色：user 或 agent"`
	TriggeredByID   string `json:"triggeredById,omitempty" jsonschema:"触发者 id"`
	Ref             string `json:"ref,omitempty" jsonschema:"git ref"`
	Version         string `json:"version,omitempty" jsonschema:"版本"`
	Q               string `json:"q,omitempty" jsonschema:"关键字"`
	From            string `json:"from,omitempty" jsonschema:"requestedAt 下界，ISO-8601"`
	To              string `json:"to,omitempty" jsonschema:"requestedAt 上界，ISO-8601"`
	Page            *int   `json:"page,omitempty" jsonschema:"页码，默认 1"`
	PageSize        *int   `json:"pageSize,omitempty" jsonschema:"每页条数，默认 20，最大 200"`
}

type configureServiceArgs struct {
	ServiceID                string            `json:"serviceId" jsonschema:"服务 id（service-registry 里的 name）"`
	Name                     *string           `json:"name,omitempty" jsonschema:"显示名。省略则沿用已有配置"`
	RuntimeDir               *string           `json:"runtimeDir,omitempty" jsonschema:"运行目录。首次配置必填"`
	RuntimeDirs              map[string]string `json:"runtimeDirs,omitempty" jsonschema:"按平台覆盖运行目录，键为 darwin 或 linux。传入则整份替换"`
	HealthURL                *string           `json:"healthUrl,omitempty" jsonschema:"健康检查 URL。首次配置必填"`
	Port                     *int              `json:"port,omitempty" jsonschema:"服务端口 1..65535。首次配置必填；省略则沿用已有端口"`
	StartCmd                 *string           `json:"startCmd,omitempty" jsonschema:"启动命令。首次配置必填"`
	StopCmd                  *string           `json:"stopCmd,omitempty" jsonschema:"停止命令。首次配置必填"`
	RestartCmd               *string           `json:"restartCmd,omitempty" jsonschema:"重启命令。首次配置必填"`
	DefaultBranch            *string           `json:"defaultBranch,omitempty" jsonschema:"发版未指定 ref 时使用的分支"`
	RestartNotifyURL         *string           `json:"restartNotifyUrl,omitempty" jsonschema:"graceful 排空通知 URL。必须和 restartPollUrl 同时设置或同时为空"`
	RestartPollURL           *string           `json:"restartPollUrl,omitempty" jsonschema:"graceful 排空轮询 URL"`
	GracefulRestartMaxWaitMs *int              `json:"gracefulRestartMaxWaitMs,omitempty" jsonschema:"graceful 最长等待毫秒"`
	Supervise                *bool             `json:"supervise,omitempty" jsonschema:"是否由 watchdog 守护"`
	IntervalSec              *int              `json:"intervalSec,omitempty" jsonschema:"watchdog 探活周期（秒）"`
	RuntimeEnv               map[string]string `json:"runtimeEnv,omitempty" jsonschema:"启动时额外环境变量。传入则整份替换。不要写 SERVICE_PORT、PORT、APP_VERSION、DEPLOY_MACHINE、RUNTIME_DIR"`
	ClearRuntimeEnv          bool              `json:"clearRuntimeEnv,omitempty" jsonschema:"为 true 时清空 runtimeEnv。不要和 runtimeEnv 同时传"`
}

type moveHistoryArgs struct {
	ServiceID            string `json:"serviceId" jsonschema:"来源服务 id"`
	To                   string `json:"to" jsonschema:"目标服务 id，必须已有本机部署配置"`
	DeleteSourceContract bool   `json:"deleteSourceContract,omitempty" jsonschema:"迁移完成后删除来源服务的本机部署配置"`
}

type deployArgs struct {
	ServiceID     string `json:"serviceId" jsonschema:"要部署的服务 id"`
	Deployment    string `json:"deployment,omitempty" jsonschema:"制品 tag，形如 deployment-<hash>。与 hash 二选一"`
	Hash          string `json:"hash,omitempty" jsonschema:"制品 hash。deployment 为空时使用"`
	RequestID     string `json:"requestId,omitempty" jsonschema:"可选的任务 id。重复会 409"`
	TargetMachine string `json:"targetMachine,omitempty" jsonschema:"部署到哪台机器。空则默认机器。必须出现在 deployment_meta 的部署机器列表里"`
	IdentityRole  string `json:"identityRole,omitempty" jsonschema:"可选。覆盖 identity_role，user 或 agent"`
	IdentityID    string `json:"identityId,omitempty" jsonschema:"可选。覆盖 identity_id，写入部署记录的触发者"`
}

type releaseArgs struct {
	ServiceID     string `json:"serviceId" jsonschema:"要打包并部署的服务 id"`
	Ref           string `json:"ref,omitempty" jsonschema:"git ref。省略则用该服务的 defaultBranch"`
	RequestID     string `json:"requestId,omitempty" jsonschema:"可选的流水线 id。重复会 409"`
	UseProxy      *bool  `json:"useProxy,omitempty" jsonschema:"打包（git fetch 和 build）是否走控制面本机代理"`
	TargetMachine string `json:"targetMachine,omitempty" jsonschema:"部署到哪台机器。空则默认机器"`
	IdentityRole  string `json:"identityRole,omitempty" jsonschema:"可选。覆盖 identity_role，user 或 agent"`
	IdentityID    string `json:"identityId,omitempty" jsonschema:"可选。覆盖 identity_id，写入流水线的触发者"`
}

func (s *skill) clientInfo(context.Context, *mcp.CallToolRequest, emptyArgs) (*mcp.CallToolResult, any, error) {
	id := s.client.DefaultIdentity()
	return textResult(http.StatusOK, map[string]string{
		"baseUrl":      s.client.BaseURL(),
		"identityRole": id.Role,
		"identityId":   id.ID,
	}), nil, nil
}

func (s *skill) health(ctx context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
	return s.call(ctx, Request{Method: http.MethodGet, Path: "/health"})
}

func (s *skill) meta(ctx context.Context, _ *mcp.CallToolRequest, args metaArgs) (*mcp.CallToolResult, any, error) {
	q := url.Values{}
	setQuery(q, "serviceId", args.ServiceID)
	return s.call(ctx, Request{Method: http.MethodGet, Path: "/api/meta", Query: q})
}

func (s *skill) inventory(ctx context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
	return s.call(ctx, Request{Method: http.MethodGet, Path: "/api/deployment-inventory"})
}

func (s *skill) listServices(ctx context.Context, _ *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
	return s.call(ctx, Request{Method: http.MethodGet, Path: "/api/services"})
}

func (s *skill) getService(ctx context.Context, _ *mcp.CallToolRequest, args serviceIDArgs) (*mcp.CallToolResult, any, error) {
	path, err := servicePath(args.ServiceID, "")
	if err != nil {
		return nil, nil, err
	}
	return s.call(ctx, Request{Method: http.MethodGet, Path: path})
}

func (s *skill) serviceHistory(ctx context.Context, _ *mcp.CallToolRequest, args serviceIDArgs) (*mcp.CallToolResult, any, error) {
	path, err := servicePath(args.ServiceID, "/history")
	if err != nil {
		return nil, nil, err
	}
	return s.call(ctx, Request{Method: http.MethodGet, Path: path})
}

func (s *skill) configureService(ctx context.Context, _ *mcp.CallToolRequest, args configureServiceArgs) (*mcp.CallToolResult, any, error) {
	path, err := servicePath(args.ServiceID, "")
	if err != nil {
		return nil, nil, err
	}
	body, err := args.body()
	if err != nil {
		return nil, nil, err
	}
	return s.call(ctx, Request{Method: http.MethodPut, Path: path, Body: body})
}

func (a configureServiceArgs) body() (map[string]any, error) {
	if a.ClearRuntimeEnv && a.RuntimeEnv != nil {
		return nil, errors.New("clearRuntimeEnv and runtimeEnv cannot both be set")
	}
	body := map[string]any{}
	putString(body, "name", a.Name)
	putString(body, "runtimeDir", a.RuntimeDir)
	putString(body, "healthUrl", a.HealthURL)
	putString(body, "startCmd", a.StartCmd)
	putString(body, "stopCmd", a.StopCmd)
	putString(body, "restartCmd", a.RestartCmd)
	putString(body, "defaultBranch", a.DefaultBranch)
	putString(body, "restartNotifyUrl", a.RestartNotifyURL)
	putString(body, "restartPollUrl", a.RestartPollURL)
	if a.RuntimeDirs != nil {
		body["runtimeDirs"] = a.RuntimeDirs
	}
	if a.Port != nil {
		body["port"] = *a.Port
	}
	if a.GracefulRestartMaxWaitMs != nil {
		body["gracefulRestartMaxWaitMs"] = *a.GracefulRestartMaxWaitMs
	}
	if a.Supervise != nil {
		body["supervise"] = *a.Supervise
	}
	if a.IntervalSec != nil {
		body["intervalSec"] = *a.IntervalSec
	}
	if a.ClearRuntimeEnv {
		body["runtimeEnv"] = map[string]string{}
	} else if a.RuntimeEnv != nil {
		body["runtimeEnv"] = a.RuntimeEnv
	}
	return body, nil
}

func putString(body map[string]any, key string, value *string) {
	if value != nil {
		body[key] = *value
	}
}

func (s *skill) deleteService(ctx context.Context, _ *mcp.CallToolRequest, args serviceIDArgs) (*mcp.CallToolResult, any, error) {
	path, err := servicePath(args.ServiceID, "")
	if err != nil {
		return nil, nil, err
	}
	return s.call(ctx, Request{Method: http.MethodDelete, Path: path})
}

func (s *skill) moveHistory(ctx context.Context, _ *mcp.CallToolRequest, args moveHistoryArgs) (*mcp.CallToolResult, any, error) {
	path, err := servicePath(args.ServiceID, "/history/move")
	if err != nil {
		return nil, nil, err
	}
	to := strings.TrimSpace(args.To)
	if _, err := segment("to", to); err != nil {
		return nil, nil, err
	}
	return s.call(ctx, Request{
		Method: http.MethodPost,
		Path:   path,
		Body: map[string]any{
			"to":                   to,
			"deleteSourceContract": args.DeleteSourceContract,
		},
	})
}

func (s *skill) listArtifacts(ctx context.Context, _ *mcp.CallToolRequest, args metaArgs) (*mcp.CallToolResult, any, error) {
	q := url.Values{}
	setQuery(q, "serviceId", args.ServiceID)
	return s.call(ctx, Request{Method: http.MethodGet, Path: "/api/artifacts", Query: q})
}

func (s *skill) getArtifact(ctx context.Context, _ *mcp.CallToolRequest, args artifactArgs) (*mcp.CallToolResult, any, error) {
	tag, err := segment("tag", args.Tag)
	if err != nil {
		return nil, nil, err
	}
	if _, err := segment("serviceId", args.ServiceID); err != nil {
		return nil, nil, err
	}
	q := url.Values{}
	q.Set("serviceId", args.ServiceID)
	return s.call(ctx, Request{Method: http.MethodGet, Path: "/api/artifacts/" + tag, Query: q})
}

func (s *skill) scanArtifacts(ctx context.Context, _ *mcp.CallToolRequest, args serviceIDArgs) (*mcp.CallToolResult, any, error) {
	if _, err := segment("serviceId", args.ServiceID); err != nil {
		return nil, nil, err
	}
	q := url.Values{}
	q.Set("serviceId", args.ServiceID)
	return s.call(ctx, Request{Method: http.MethodPost, Path: "/api/artifacts/scan", Query: q})
}

func (s *skill) listDeploys(ctx context.Context, _ *mcp.CallToolRequest, args deployListArgs) (*mcp.CallToolResult, any, error) {
	return s.call(ctx, Request{Method: http.MethodGet, Path: "/api/deploys", Query: args.query()})
}

func (a deployListArgs) query() url.Values {
	q := url.Values{}
	setQuery(q, "serviceId", a.ServiceID)
	setQuery(q, "state", a.State)
	setQuery(q, "triggeredByRole", a.TriggeredByRole)
	setQuery(q, "triggeredById", a.TriggeredByID)
	setQuery(q, "deployment", a.Deployment)
	setQuery(q, "version", a.Version)
	setQuery(q, "q", a.Q)
	setQuery(q, "from", a.From)
	setQuery(q, "to", a.To)
	setQueryInt(q, "page", a.Page)
	setQueryInt(q, "pageSize", a.PageSize)
	return q
}

func (s *skill) getDeploy(ctx context.Context, _ *mcp.CallToolRequest, args requestIDArgs) (*mcp.CallToolResult, any, error) {
	id, err := segment("requestId", args.RequestID)
	if err != nil {
		return nil, nil, err
	}
	return s.call(ctx, Request{Method: http.MethodGet, Path: "/api/deploys/" + id})
}

func (s *skill) deployEvents(ctx context.Context, _ *mcp.CallToolRequest, args requestIDArgs) (*mcp.CallToolResult, any, error) {
	id, err := segment("requestId", args.RequestID)
	if err != nil {
		return nil, nil, err
	}
	return s.call(ctx, Request{Method: http.MethodGet, Path: "/api/deploys/" + id + "/events"})
}

func (s *skill) deploy(ctx context.Context, _ *mcp.CallToolRequest, args deployArgs) (*mcp.CallToolResult, any, error) {
	if _, err := segment("serviceId", args.ServiceID); err != nil {
		return nil, nil, err
	}
	body := map[string]any{"serviceId": args.ServiceID}
	if args.Deployment != "" {
		body["deployment"] = args.Deployment
	}
	if args.Hash != "" {
		body["hash"] = args.Hash
	}
	if args.RequestID != "" {
		body["requestId"] = args.RequestID
	}
	if args.TargetMachine != "" {
		body["targetMachine"] = args.TargetMachine
	}
	return s.call(ctx, Request{
		Method:   http.MethodPost,
		Path:     "/api/deploys",
		Body:     body,
		Identity: identityOverride(args.IdentityRole, args.IdentityID),
	})
}

func (s *skill) listPipelines(ctx context.Context, _ *mcp.CallToolRequest, args pipelineListArgs) (*mcp.CallToolResult, any, error) {
	return s.call(ctx, Request{Method: http.MethodGet, Path: "/api/pipelines", Query: args.query()})
}

func (a pipelineListArgs) query() url.Values {
	q := url.Values{}
	setQuery(q, "serviceId", a.ServiceID)
	setQuery(q, "state", a.State)
	setQuery(q, "triggeredByRole", a.TriggeredByRole)
	setQuery(q, "triggeredById", a.TriggeredByID)
	setQuery(q, "ref", a.Ref)
	setQuery(q, "version", a.Version)
	setQuery(q, "q", a.Q)
	setQuery(q, "from", a.From)
	setQuery(q, "to", a.To)
	setQueryInt(q, "page", a.Page)
	setQueryInt(q, "pageSize", a.PageSize)
	return q
}

func (s *skill) getPipeline(ctx context.Context, _ *mcp.CallToolRequest, args requestIDArgs) (*mcp.CallToolResult, any, error) {
	id, err := segment("requestId", args.RequestID)
	if err != nil {
		return nil, nil, err
	}
	return s.call(ctx, Request{Method: http.MethodGet, Path: "/api/pipelines/" + id})
}

func (s *skill) pipelineEvents(ctx context.Context, _ *mcp.CallToolRequest, args requestIDArgs) (*mcp.CallToolResult, any, error) {
	id, err := segment("requestId", args.RequestID)
	if err != nil {
		return nil, nil, err
	}
	return s.call(ctx, Request{Method: http.MethodGet, Path: "/api/pipelines/" + id + "/events"})
}

func (s *skill) release(ctx context.Context, _ *mcp.CallToolRequest, args releaseArgs) (*mcp.CallToolResult, any, error) {
	if _, err := segment("serviceId", args.ServiceID); err != nil {
		return nil, nil, err
	}
	body := map[string]any{"serviceId": args.ServiceID}
	if args.Ref != "" {
		body["ref"] = args.Ref
	}
	if args.RequestID != "" {
		body["requestId"] = args.RequestID
	}
	if args.UseProxy != nil {
		body["useProxy"] = *args.UseProxy
	}
	if args.TargetMachine != "" {
		body["targetMachine"] = args.TargetMachine
	}
	return s.call(ctx, Request{
		Method:   http.MethodPost,
		Path:     "/api/deploy-notify",
		Body:     body,
		Identity: identityOverride(args.IdentityRole, args.IdentityID),
	})
}

func (s *skill) call(ctx context.Context, req Request) (*mcp.CallToolResult, any, error) {
	res, err := s.client.Do(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	out := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: formatBody(res.Status, res.Body)}},
	}
	if res.Status < 200 || res.Status >= 300 {
		out.IsError = true
	}
	return out, nil, nil
}

func textResult(status int, v any) *mcp.CallToolResult {
	raw, err := json.Marshal(v)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			IsError: true,
		}
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: formatBody(status, raw)}},
	}
}

func servicePath(serviceID, suffix string) (string, error) {
	seg, err := segment("serviceId", serviceID)
	if err != nil {
		return "", err
	}
	return "/api/services/" + seg + suffix, nil
}

// identityOverride carries any per-call identity fields. Empty fields fall
// back to the client default inside Client.Do.
func identityOverride(role, id string) Identity {
	return Identity{Role: role, ID: id}
}
