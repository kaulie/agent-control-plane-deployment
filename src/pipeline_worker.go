package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

// PipelineWorker: package from git, then enqueue deploy (graceful notify/poll happens in DeployWorker).
type PipelineWorker struct {
	store *Store
	// machines / platforms：打包时要按**目标机器**的平台构建（mac/linux 两套产物）。
	machines  *MachineCatalog
	platforms *platformResolver
	cfg       Config
	storage   ArtifactStorage
	deploy    *DeployWorker
	drain     *GracefulDrain
	registry  *ServiceRegistry
	mu        sync.Mutex
	busy      bool
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

func NewPipelineWorker(store *Store, cfg Config, storage ArtifactStorage, deploy *DeployWorker, drain *GracefulDrain) *PipelineWorker {
	return &PipelineWorker{
		store:   store,
		cfg:     cfg,
		storage: storage,
		deploy:  deploy,
		drain:   drain,
		stopCh:  make(chan struct{}),
	}
}

func (w *PipelineWorker) Start() {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		w.tick()
		for {
			select {
			case <-w.stopCh:
				return
			case <-t.C:
				w.tick()
				w.syncDeploying()
			}
		}
	}()
}

func (w *PipelineWorker) Stop() {
	select {
	case <-w.stopCh:
	default:
		close(w.stopCh)
	}
	w.wg.Wait()
}

func (w *PipelineWorker) Kick() {
	go w.tick()
}

// packageOptions maps a claimed pipeline onto the packaging call: the per-request
// 「走本机代理」option travels with the job (the pack runs later, in this worker),
// together with where that proxy is configured.
func (w *PipelineWorker) packageOptions(job *PipelineJob, events PackageEventFunc) PackageOptions {
	return PackageOptions{
		MaxSec:        w.cfg.ReleaseMaxSec,
		Storage:       w.storage,
		UseProxy:      job.UseProxy,
		ProxyEnvFile:  w.cfg.ProxyEnvFile,
		BuildPlatform: w.platformFor(job),
		Events:        events,
	}
}

// platformFor 决定这次打包按哪个平台构建。平台是**机器的字段**：
//
//	① service_registry 上这台机器的实例登记的 metadata.platform（真源，控制面同步）；
//	② 注册中心没登记 → ssh 探测该机器（uname -s -m）兜底；
//	③ 本机机器 → 控制面自己的平台（tag 保持 deployment-<hash>，与历史一致）。
//
// 找不到机器/通道时退回本机平台 —— 部署前的平台校验会拦住发错的产物。
func (w *PipelineWorker) platformFor(job *PipelineJob) BuildPlatform {
	machine := strings.TrimSpace(job.TargetMachine)
	if machine == "" {
		machine = w.machinesDefault()
	}
	if w.machines != nil {
		if p, ok := w.machines.PlatformFor(context.Background(), machine); ok {
			_ = w.store.AddPipelineEvent(job.RequestID, eventlevel.Info,
				"构建平台="+p.String()+"（部署机器="+machine+"；平台来自 service_registry 登记的实例字段）")
			return p
		}
	}
	target, ok := w.machineTarget(job, machine)
	if !ok {
		return LocalBuildPlatform()
	}
	p, why := w.platforms.Resolve(target)
	if strings.TrimSpace(why) != "" {
		_ = w.store.AddPipelineEvent(job.RequestID, eventlevel.Info,
			"构建平台="+p.String()+"（部署机器="+machine+"；注册中心没登记平台，"+why+"）")
	}
	return p
}

func (w *PipelineWorker) machineTarget(job *PipelineJob, machine string) (MachineTarget, bool) {
	if w.machines == nil {
		return MachineTarget{}, false
	}
	return w.machines.TargetForService(context.Background(), job.ServiceID, machine)
}

func (w *PipelineWorker) machinesDefault() string {
	if w.machines == nil {
		return defaultDeployMachine
	}
	return w.machines.DefaultID(context.Background())
}

func (w *PipelineWorker) tick() {
	w.mu.Lock()
	if w.busy {
		w.mu.Unlock()
		return
	}
	if w.drain.IsDraining() {
		// graceful self-restart in progress: do not claim new pipelines
		w.mu.Unlock()
		return
	}
	w.busy = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.busy = false
		w.mu.Unlock()
	}()

	job, err := w.store.ClaimNextPipeline()
	if err != nil {
		fmt.Printf("[pipeline-worker] %v\n", err)
		return
	}
	if job == nil {
		return
	}
	w.execute(job)
}

func (w *PipelineWorker) execute(job *PipelineJob) {
	svc, err := w.store.GetService(job.ServiceID)
	if err != nil || svc == nil {
		failPipeline(w.store, job.RequestID, "unknown service: "+job.ServiceID)
		return
	}
	gitURL := resolveServiceGitRepo(context.Background(), w.registry, svc)
	if gitURL == "" {
		failPipeline(w.store, job.RequestID,
			"service has no gitRepoUrl: register one in service_registry（gitRepoUrl 只能由注册中心登记，"+
				"本机不能设置）")
		return
	}

	fmt.Printf("[pipeline] %s packaging service=%s ref=%s repo=%s\n", job.RequestID, job.ServiceID, job.Ref, gitURL)
	envNote := ""
	if job.UseProxy {
		envNote = "（走本机代理）"
	}
	_ = w.store.AddPipelineEvent(job.RequestID, eventlevel.Info,
		"开始打包：service="+job.ServiceID+" ref="+job.Ref+" repo="+gitURL+envNote)
	pkg, err := packageFromGit(job.ServiceID, gitURL, job.Ref, w.packageOptions(job, func(level eventlevel.Level, msg string) {
		_ = w.store.AddPipelineEvent(job.RequestID, level, msg)
	}))
	if err != nil {
		failPipeline(w.store, job.RequestID, "package failed: "+err.Error())
		return
	}
	msg := "package ready; enqueueing deploy"
	if pkg.Skipped {
		msg = "package already exists; enqueueing deploy"
		_ = w.store.AddPipelineEvent(job.RequestID, eventlevel.Info,
			"包已存在，跳过构建：tag="+pkg.Tag+" version="+pkg.Hash)
	} else {
		_ = w.store.AddPipelineEvent(job.RequestID, eventlevel.Success,
			"打包完成：tag="+pkg.Tag+" version="+pkg.Hash+" commit="+pkg.FullCommit)
	}
	// Record artifact metadata locally (the storage backend is pure storage;
	// the table holds the access path so deploys/panel can resolve without
	// re-querying the backend). Skipped builds already have a row.
	if pkg.Artifact != nil {
		if err := w.store.RecordArtifact(Artifact{
			ServiceID:          job.ServiceID,
			Tag:                pkg.Tag,
			Version:            pkg.Hash,
			Commit:             pkg.FullCommit,
			GitRepoURL:         gitURL,
			RepoSlug:           pkg.Artifact.RepoSlug,
			AssetName:          releaseAssetName,
			AssetID:            pkg.Artifact.AssetID,
			AssetURL:           pkg.Artifact.AssetURL,
			BrowserDownloadURL: pkg.Artifact.BrowserDownloadURL,
			ReleaseURL:         pkg.Artifact.ReleaseURL,
			Size:               pkg.Artifact.Size,
			Storage:            pkg.Artifact.Storage,
			CreatedAt:          nowISO(),
		}); err != nil {
			fmt.Printf("[pipeline] %s warn: record artifact: %v\n", job.RequestID, err)
		}
	}
	_ = w.store.UpdatePipeline(job.RequestID, PipelineJob{
		State:      PipelineDeploying,
		Deployment: pkg.Tag,
		Version:    pkg.Hash,
		Message:    msg,
	})

	deployID := job.RequestID
	if existing, _ := w.store.GetDeploy(deployID); existing != nil {
		deployID = "deploy-req-" + uuid.NewString()[:8]
	}
	_, err = w.store.CreateDeploy(deployID, job.ServiceID, pkg.Tag, job.Identity(),
		"queued after package (graceful notify+poll before restart)", job.TargetMachine)
	if err != nil {
		failPipeline(w.store, job.RequestID, "enqueue deploy failed: "+err.Error())
		return
	}
	deployNote := ""
	if job.TargetMachine != "" {
		deployNote = " 部署机器=" + job.TargetMachine
	}
	_ = w.store.AddPipelineEvent(job.RequestID, eventlevel.Info, "已入队部署任务：deployRequestId="+deployID+deployNote)
	_ = w.store.UpdatePipeline(job.RequestID, PipelineJob{
		State:           PipelineDeploying,
		Deployment:      pkg.Tag,
		DeployRequestID: deployID,
		Version:         pkg.Hash,
		Message:         "deploy queued; waiting for a free deploy slot, then graceful notify+poll before restart",
	})
	w.deploy.Kick()
	fmt.Printf("[pipeline] %s packaged %s → deploy %s (by=%s)\n",
		job.RequestID, pkg.Tag, deployID, job.Identity().String())
}

func (w *PipelineWorker) syncDeploying() {
	jobs, err := w.store.ListPipelinesByState(PipelineDeploying)
	if err != nil {
		return
	}
	for _, job := range jobs {
		if job.DeployRequestID == "" {
			continue
		}
		dep, err := w.store.GetDeploy(job.DeployRequestID)
		if err != nil || dep == nil {
			continue
		}
		switch dep.State {
		case StateSucceeded:
			_ = w.store.UpdatePipeline(job.RequestID, PipelineJob{
				State:           PipelineSucceeded,
				Deployment:      dep.Deployment,
				DeployRequestID: dep.RequestID,
				Version:         dep.Version,
				Message:         "pipeline succeeded",
			})
			_ = w.store.AddPipelineEvent(job.RequestID, eventlevel.Success,
				"流水线成功：version="+dep.Version)
		case StateFailed, StateCancelled:
			_ = w.store.UpdatePipeline(job.RequestID, PipelineJob{
				State:           PipelineFailed,
				Deployment:      dep.Deployment,
				DeployRequestID: dep.RequestID,
				Version:         dep.Version,
				Error:           dep.Error,
				Message:         "deploy failed",
			})
			_ = w.store.AddPipelineEvent(job.RequestID, eventlevel.Error,
				"部署失败："+dep.Error)
		}
	}
}
