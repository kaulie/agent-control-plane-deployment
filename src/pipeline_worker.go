package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// PipelineWorker: package from git, then enqueue deploy (graceful notify/poll happens in DeployWorker).
type PipelineWorker struct {
	store   *Store
	cfg     Config
	storage ArtifactStorage
	deploy  *DeployWorker
	drain   *GracefulDrain
	mu      sync.Mutex
	busy    bool
	stopCh  chan struct{}
	wg      sync.WaitGroup
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
	gitURL := strings.TrimSpace(svc.GitRepoURL)
	if gitURL == "" {
		failPipeline(w.store, job.RequestID, "service missing gitRepoUrl; register it via PUT /api/services/"+job.ServiceID)
		return
	}

	fmt.Printf("[pipeline] %s packaging service=%s ref=%s repo=%s\n", job.RequestID, job.ServiceID, job.Ref, gitURL)
	_ = w.store.AddPipelineEvent(job.RequestID, "info",
		"开始打包：service="+job.ServiceID+" ref="+job.Ref+" repo="+gitURL)
	pkg, err := packageFromGit(job.ServiceID, gitURL, job.Ref, w.cfg.ReleaseMaxSec, w.storage,
		func(level, msg string) {
			_ = w.store.AddPipelineEvent(job.RequestID, level, msg)
		})
	if err != nil {
		failPipeline(w.store, job.RequestID, "package failed: "+err.Error())
		return
	}
	msg := "package ready; enqueueing deploy"
	if pkg.Skipped {
		msg = "package already exists; enqueueing deploy"
		_ = w.store.AddPipelineEvent(job.RequestID, "info",
			"包已存在，跳过构建：tag="+pkg.Tag+" version="+pkg.Hash)
	} else {
		_ = w.store.AddPipelineEvent(job.RequestID, "success",
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
			CreatedAt:           nowISO(),
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
	_, err = w.store.CreateDeploy(deployID, job.ServiceID, pkg.Tag,
		"queued after package (graceful notify+poll before restart)")
	if err != nil {
		failPipeline(w.store, job.RequestID, "enqueue deploy failed: "+err.Error())
		return
	}
	_ = w.store.AddPipelineEvent(job.RequestID, "info", "已入队部署任务：deployRequestId="+deployID)
	_ = w.store.UpdatePipeline(job.RequestID, PipelineJob{
		State:           PipelineDeploying,
		Deployment:      pkg.Tag,
		DeployRequestID: deployID,
		Version:         pkg.Hash,
		Message:         "deploy queued; waiting for graceful restart window then apply",
	})
	w.deploy.Kick()
	fmt.Printf("[pipeline] %s packaged %s → deploy %s\n", job.RequestID, pkg.Tag, deployID)
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
			_ = w.store.AddPipelineEvent(job.RequestID, "success",
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
			_ = w.store.AddPipelineEvent(job.RequestID, "error",
				"部署失败："+dep.Error)
		}
	}
}
