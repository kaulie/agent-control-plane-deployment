package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kaulie/agent-control-plane-deployment/pgdb"
)

func seedDefaultService(store *Store) {
	svc, err := store.GetService("web-cursor")
	if err != nil || svc != nil {
		return
	}
	home, _ := os.UserHomeDir()
	runtimeDir := filepath.Join(home, "runtime", "web-cursor")
	_, err = store.UpsertService(ServiceContract{
		ServiceID:  "web-cursor",
		Name:       "Web Cursor Agent Gateway",
		RuntimeDir: runtimeDir,
		// URL 只存路径（部署时按目标机器拼 host:port）。
		HealthURL:  "/health",
		Port:       4211,
		StartCmd:   fmt.Sprintf("bash %q", filepath.Join(runtimeDir, "scripts", "start.sh")),
		StopCmd:    fmt.Sprintf("bash %q", filepath.Join(runtimeDir, "scripts", "stop.sh")),
		RestartCmd: fmt.Sprintf("bash %q", filepath.Join(runtimeDir, "scripts", "restart.sh")),
		GitRepoURL: "https://github.com/kaulie/agent-control-plane",
	})
	if err != nil {
		log.Printf("[seed] failed: %v", err)
		return
	}
	log.Printf("[seed] registered service web-cursor → %s", runtimeDir)
}

func seedACPService(store *Store, cfg Config) {
	svc, err := store.GetService("agent-control-plane-deployment")
	if err != nil || svc != nil {
		return
	}
	_, err = store.UpsertService(ServiceContract{
		ServiceID:        "agent-control-plane-deployment",
		Name:             "Agent Control Plane Deployment",
		RuntimeDir:       cfg.Home,
		HealthURL:        "/health",
		Port:             cfg.Port,
		StartCmd:         fmt.Sprintf("bash %q", filepath.Join(cfg.Home, "scripts", "start.sh")),
		StopCmd:          fmt.Sprintf("bash %q", filepath.Join(cfg.Home, "scripts", "stop.sh")),
		RestartCmd:       fmt.Sprintf("bash %q", filepath.Join(cfg.Home, "scripts", "restart.sh")),
		RestartNotifyURL: "/restart/notify",
		RestartPollURL:   "/restart/poll",
		Supervise:        true,
	})
	if err != nil {
		log.Printf("[seed] acp service failed: %v", err)
		return
	}
	log.Printf("[seed] registered service agent-control-plane-deployment → %s", cfg.Home)
}

// General API Info for swag — the **single source of truth** of this service's
// HTTP contract. `swag init -d src -g main.go -o docs --parseInternal
// --outputTypes json` reads this block plus the @Summary/@Tags/@Router
// annotations on every handler (see server.go) and writes docs/swagger.json.
// `client/ci/register-go-service.sh` then reports that spec to service-registry
// (:4240). There is no hand-maintained spec file to keep in sync: change a
// route, change its annotation.
//
// @title        agent-control-plane-deployment
// @version      1.0.0
// @description  独立部署控制面（:4220）：打包 / 部署 / 制品索引 / 服务契约部署参数 + Web 面板。
// @description  服务目录（GET /api/services）的唯一真源是 service-registry（:4240），本机只存部署参数。
// @description  鉴权（第一阶段）：触发部署的接口（POST /api/deploys、POST /api/deploy-notify）要求身份头 identity_role / identity_id；只读接口与 /restart/* 不校验。
// @description  HTTP MCP：POST /mcp（Streamable HTTP）。工具调用转发到本服务的 HTTP API。身份头会记成触发者。监听地址与本服务相同，默认 0.0.0.0（DEPLOYMENT_HOST=127.0.0.1 可改回仅本机）。
// @BasePath     /
// @schemes      http
// @host         127.0.0.1:4220
//
// @securityDefinitions.apikey  IdentityRole
// @in                          header
// @name                        identity_role
// @description                 调用者角色（user | agent）；触发部署的接口必填
//
// @securityDefinitions.apikey  IdentityID
// @in                          header
// @name                        identity_id
// @description                 调用者标识（如 user_001 / agent_002）；触发部署的接口必填
//
// @tag.name         meta
// @tag.description  元信息与健康检查
// @tag.name         services
// @tag.description  服务契约：目录来自 service-registry，本机只配置部署参数
// @tag.name         pipelines
// @tag.description  部署流水线（打包 → 部署）
// @tag.name         deploys
// @tag.description  部署任务（部署已打包好的制品）
// @tag.name         artifacts
// @tag.description  制品索引（GitHub Release / 阿里云制品仓库）
// @tag.name         ops
// @tag.description  graceful 重启（drain 通知与轮询）
func main() {
	cfg := loadConfig()
	if cfg.PostgresDSN == "" {
		log.Fatal("db: DEPLOYMENT_POSTGRES_DSN is empty (set it, or write data/postgres.env or ~/database/deployment/pg.env)")
	}
	log.Printf("[store] postgres %s", pgdb.RedactedTarget(cfg.PostgresDSN))
	store, err := NewStore(cfg.PostgresDSN)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer store.Close()
	legacySQLite := filepath.Join(cfg.DataDir, "deploy.sqlite")
	imported, err := store.ImportLegacySQLite(legacySQLite)
	if err != nil {
		log.Fatalf("import legacy sqlite: %v", err)
	}
	if imported {
		log.Printf("[store] imported legacy sqlite %s", legacySQLite)
	}

	storage, err := NewArtifactStorage(cfg)
	if err != nil {
		log.Fatalf("artifact storage: %v", err)
	}
	log.Printf("[storage] artifact storage backend: %s", storage.Name())

	seedDefaultService(store)
	seedACPService(store, cfg)
	// 老库里的完整 URL（http://127.0.0.1:4211/health）归一成路径（/health）+ 端口：
	// contract 与机器无关，部署时再按目标机器拼。幂等。
	if n, err := store.NormalizeServiceURLs(); err != nil {
		log.Printf("[store] warn: normalize service urls: %v", err)
	} else if n > 0 {
		log.Printf("[store] normalized %d service contract(s) to path-only URLs", n)
	}

	registry := NewServiceRegistry(cfg)
	if registry.Enabled() {
		log.Printf("[registry] service catalog pulled from %s", registry.BaseURL())
	} else {
		log.Printf("[registry] SERVICE_REGISTRY_URL=off: only locally configured services are shown")
	}

	// 部署机器目录：真源是 service-registry 登记的实例主机（+ 本机 + DEPLOY_MACHINES），
	// 面板下拉、触发校验、部署执行都从这里取。平台探测（打包用）挂在它上面。
	ssh := sshRemoteRunner{home: cfg.Home}
	machinesPlatformResolver := newPlatformResolver(ssh)
	machines := NewMachineCatalog(cfg, registry)

	drain := &GracefulDrain{}
	worker := NewDeployWorker(store, cfg, storage, drain, machines, ssh)
	pipeline := NewPipelineWorker(store, cfg, storage, worker, drain)
	pipeline.registry = registry
	// 打包要按目标机器的平台构建（mac/linux 两套产物）：接上机器目录与平台探测。
	pipeline.machines = machines
	pipeline.platforms = machinesPlatformResolver
	api := &apiServer{store: store, cfg: cfg, storage: storage, worker: worker, pipeline: pipeline,
		drain: drain, registry: registry, machines: machines}

	pidFile := filepath.Join(cfg.Home, "deployment.pid")
	_ = os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)

	httpServer := &http.Server{
		Addr:    net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port)),
		Handler: api.routes(),
	}

	// Listen first, announce this address, and only then accept requests.
	// Registration failure closes the socket and aborts startup so the process
	// is not left running with no catalog entry. See service-registry
	// docs/register.md. Serving still starts before reconcile: a self-upgrade
	// must be able to see /health, and reconcile used to run too early.
	ln, err := net.Listen("tcp", httpServer.Addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	httpServer.Addr = ln.Addr().String()
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || tcpAddr.Port < 1 {
		_ = ln.Close()
		log.Fatalf("listen: 无法解析监听端口 %s", ln.Addr())
	}
	announceCtx, announceCancel := context.WithTimeout(context.Background(), announceTimeout(cfg))
	instanceID, err := announceDeployment(announceCtx, registry, cfg, tcpAddr.Port)
	announceCancel()
	if err != nil {
		_ = ln.Close()
		log.Fatalf("[registry] 实例登记失败，启动中止：%v", err)
	}
	go func() {
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()
	// 0.0.0.0 / :: 是监听地址，不能当作拨号地址（macOS 上连 0.0.0.0 会失败）。
	healthURL := fmt.Sprintf("http://%s/health", dialableAddr(httpServer.Addr, cfg.Host, tcpAddr.Port))
	readyDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(readyDeadline) {
		if healthOK(healthURL) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	cleared := clearStaleDeployPauses(store)
	if cleared > 0 {
		log.Printf("cleared stale deploy pause flags for %d service(s)", cleared)
	}
	reconciled := reconcileOrphanDeploys(store)
	if reconciled > 0 {
		log.Printf("reconciled %d orphan deploy(s) left running", reconciled)
	}
	// 上一次运行要是被 kill / 自升级重启掉的，构建树（几百 MB 起）与制品下载目录
	// 会留在系统临时目录里。这里清一遍：此刻 worker 还没启动，不可能有本进程的
	// 构建在跑（放这个位置而不是更早，是为了不拖慢 /health 起来的时间）。
	if n, freed, failed := cleanupStaleTempWork(0); n > 0 || failed > 0 {
		log.Printf("[cleanup] 清理残留临时工作目录 %d 个，释放 %s（失败 %d 个）",
			n, humanBytes(freed), failed)
	}
	if n, failed := pruneUpgradeRequests(cfg.Home, upgradeRequestRetention); n > 0 || failed > 0 {
		log.Printf("[cleanup] 清理已完成的升级请求记录 %d 个（失败 %d 个）", n, failed)
	}

	worker.Start()
	pipeline.Start()

	stopCh := make(chan struct{})
	// 运行期兜底清理（启动时那次是主力；这里防止长期不重启的进程一直留着垃圾）。
	startTempWorkJanitor(stopCh, tempWorkSweepInterval, tempWorkStaleAfter)
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		// Drop the address before waiting on in-flight deploys. stop.sh sends
		// SIGKILL about two seconds after SIGTERM, and worker.Stop waits for
		// those deploys. A failed delete is logged only — shutdown must proceed.
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := deregisterDeployment(dropCtx, registry, cfg, instanceID); err != nil {
			log.Printf("[registry] 退出时摘掉实例失败（地址会留在目录里）：%v", err)
		}
		dropCancel()
		pipeline.Stop()
		worker.Stop()
		_ = os.Remove(pidFile)
		_ = httpServer.Close()
		close(stopCh)
	}()

	log.Printf("deployment service on http://%s home=%s", httpServer.Addr, cfg.Home)
	<-stopCh
}

// dialableAddr is the address the process uses to probe itself. A wildcard
// listen address is not a dial target (connecting to 0.0.0.0 fails on macOS).
func dialableAddr(listenAddr, host string, port int) string {
	switch strings.Trim(strings.TrimSpace(host), "[]") {
	case "", "0.0.0.0", "::":
		return fmt.Sprintf("127.0.0.1:%d", port)
	default:
		return listenAddr
	}
}
