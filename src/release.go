package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

type PackageResult struct {
	Tag        string
	Hash       string
	FullCommit string
	Dir        string
	Skipped    bool
	Platform   string
	Artifact   *ArtifactMeta
}

// PackageEventFunc receives progress lines emitted by packageFromGit (build /
// upload) so the caller can record them on the pipeline timeline. Level uses
// the canonical eventlevel set. A nil sink simply discards the lines.
type PackageEventFunc func(level eventlevel.Level, message string)

// PackageOptions carries what packageFromGit needs beyond the service/ref:
// the time budget, the artifact storage backend, the per-request "use the
// machine proxy" switch (the local proxy is read from ProxyEnvFile) and the
// progress sink.
type PackageOptions struct {
	MaxSec  int
	Storage ArtifactStorage
	// UseProxy routes `git fetch` + `build.sh` through the machine's local
	// proxy (opt-in from the trigger page). ProxyEnvFile is where that proxy is
	// configured (default <home>/data/proxy.env); missing/unreadable falls back
	// to this process' own HTTP(S)_PROXY.
	UseProxy     bool
	ProxyEnvFile string
	// BuildPlatform 是这批产物要在哪个平台跑（跨机部署时 = 目标机器平台）。
	// 非本机平台时：给 build.sh 注入 GOOS/GOARCH，制品 tag 也带上平台维度。
	BuildPlatform BuildPlatform
	Events        PackageEventFunc
}

// useProxySettings resolves the machine proxy for this pack. Zero value (no
// proxy) unless the caller opted in and something is configured — applying a
// zero value is a no-op, so the default path is byte-for-byte unchanged. The
// env overlay only reaches the packaging subprocesses (git / build.sh), which
// is exactly why a per-request switch is possible at all.
func (o PackageOptions) useProxySettings() ProxySettings {
	if !o.UseProxy {
		return ProxySettings{}
	}
	return loadProxySettings(o.ProxyEnvFile)
}

// packageFromGit clones/fetches ref, runs build.sh, and stores the frozen
// outputs via the configured ArtifactStorage (local disk or GitHub Releases,
// etc.). Nothing is persisted under packagesDir unless the storage is local;
// the storage backend is the single source of truth for the bytes. events, when
// non-nil, receives granular build/upload progress lines (e.g. 上传开始/上传结束).
func packageFromGit(serviceID, gitRepoURL, ref string, opts PackageOptions) (PackageResult, error) {
	var out PackageResult
	storage := opts.Storage
	events := opts.Events
	maxSec := opts.MaxSec
	gitRepoURL = strings.TrimSpace(gitRepoURL)
	ref = strings.TrimSpace(ref)
	if gitRepoURL == "" {
		return out, fmt.Errorf("gitRepoUrl is required")
	}
	serviceID = strings.TrimSpace(serviceID)
	if serviceID == "" {
		return out, fmt.Errorf("serviceId is required for package isolation")
	}
	if ref == "" {
		ref = "main"
	}
	if maxSec < 60 {
		maxSec = 60
	}

	// 打包走本机代理（发起时勾选）：git fetch / build.sh 都能按请求换 env。
	// 勾了但本机没配代理时说清楚，别让人以为生效了（applyTo 对空设置是 no-op，
	// 所以「没勾选」的路径和以前完全一致）。
	proxy := opts.useProxySettings()
	cmdEnv := proxy.applyTo(os.Environ())
	if opts.UseProxy && events != nil {
		if proxy.Configured() {
			events(eventlevel.Info, "打包走本机代理："+proxy.Label())
		} else {
			events(eventlevel.Warn, "已勾选「走本机代理」，但本机没有代理配置（"+
				opts.ProxyEnvFile+" / 进程环境），本次按直连打包")
		}
	}

	workDir, err := os.MkdirTemp("", tempPrefixReleaseBuild+"*")
	if err != nil {
		return out, err
	}
	// 构建树里有 Go 的只读模块缓存，必须用 force 版（否则会残留几百 MB）。
	defer func() {
		if err := removeAllForce(workDir); err != nil {
			fmt.Printf("[release] warn: 清理构建树 %s 失败: %v\n", workDir, err)
		}
	}()

	run := func(dir string, name string, args ...string) (string, error) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = cmdEnv
		b, err := cmd.CombinedOutput()
		s := string(b)
		if len(s) > 50_000 {
			s = s[len(s)-50_000:]
		}
		if err != nil {
			return s, fmt.Errorf("%s %v: %w\n%s", name, args, err, s)
		}
		return s, nil
	}

	if _, err := run(workDir, "git", "init", "-q"); err != nil {
		return out, err
	}
	if _, err := run(workDir, "git", "remote", "add", "origin", gitRepoURL); err != nil {
		return out, err
	}
	if _, err := run(workDir, "git", "fetch", "--depth", "1", "origin", ref); err != nil {
		if _, err2 := run(workDir, "git", "fetch", "--depth", "1", "origin", "refs/heads/"+ref); err2 != nil {
			return out, fmt.Errorf("fetch ref %q failed: %v / %v", ref, err, err2)
		}
	}

	fullOut, err := run(workDir, "git", "rev-parse", "FETCH_HEAD")
	if err != nil {
		return out, err
	}
	full := strings.TrimSpace(fullOut)
	hashOut, err := run(workDir, "git", "rev-parse", "--short=8", full)
	if err != nil {
		return out, err
	}
	hash := strings.TrimSpace(hashOut)
	platform := opts.BuildPlatform
	if platform.IsZero() {
		platform = LocalBuildPlatform()
	}
	tag := deploymentTagFor(hash, platform)
	out.Tag = tag
	out.Hash = hash
	out.FullCommit = full
	out.Platform = platform.String()

	// Skip build if the artifact already exists in storage (idempotent re-deploys).
	if exists, err := storage.Exists(context.Background(), serviceID, gitRepoURL, tag); err == nil && exists {
		out.Skipped = true
		return out, nil
	}

	srcTree := filepath.Join(workDir, "src")
	if err := os.MkdirAll(srcTree, 0o755); err != nil {
		return out, err
	}
	archive := exec.Command("git", "archive", full)
	archive.Dir = workDir
	tar := exec.Command("tar", "-x", "-C", srcTree)
	pr, pw, err := os.Pipe()
	if err != nil {
		return out, err
	}
	archive.Stdout = pw
	tar.Stdin = pr
	if err := archive.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return out, err
	}
	if err := tar.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		_ = archive.Wait()
		return out, err
	}
	_ = pw.Close()
	if err := archive.Wait(); err != nil {
		_ = pr.Close()
		_ = tar.Wait()
		return out, fmt.Errorf("git archive: %w", err)
	}
	_ = pr.Close()
	if err := tar.Wait(); err != nil {
		return out, fmt.Errorf("tar extract: %w", err)
	}

	buildSh := filepath.Join(srcTree, "build.sh")
	if _, err := os.Stat(buildSh); err != nil {
		return out, fmt.Errorf("missing build.sh in repo")
	}
	_ = os.Chmod(buildSh, 0o755)

	buildCtx, cancel := context.WithTimeout(context.Background(), time.Duration(maxSec)*time.Second)
	defer cancel()
	buildCmd := exec.CommandContext(buildCtx, "/bin/bash", "./build.sh")
	buildCmd.Dir = srcTree
	buildEnv := []string{"APP_VERSION=" + hash}
	if !platform.IsLocal() {
		// 目标平台与本机不同：让服务的 build.sh 产出那一边的二进制（GOOS/GOARCH）。
		buildEnv = append(buildEnv, "GOOS="+platform.OS, "GOARCH="+platform.Arch)
		if events != nil {
			events(eventlevel.Info, "按目标平台构建：GOOS="+platform.OS+" GOARCH="+platform.Arch)
		}
	}
	buildCmd.Env = append(os.Environ(), buildEnv...)
	buildOut, err := buildCmd.CombinedOutput()
	if err != nil {
		s := string(buildOut)
		if len(s) > 8000 {
			s = s[len(s)-8000:]
		}
		if buildCtx.Err() == context.DeadlineExceeded {
			return out, fmt.Errorf("build.sh timed out after %ds\n%s", maxSec, s)
		}
		return out, fmt.Errorf("build.sh failed: %w\n%s", err, s)
	}

	outputs := filepath.Join(srcTree, "outputs")
	if st, err := os.Stat(outputs); err != nil || !st.IsDir() {
		return out, fmt.Errorf("build.sh did not produce outputs/")
	}
	// Stage the package in a temp dir, then hand it to the storage backend.
	// Nothing is written under packagesDir unless the backend is local.
	pkgDir, err := os.MkdirTemp("", tempPrefixReleasePkg+"*")
	if err != nil {
		return out, err
	}
	defer func() {
		if err := removeAllForce(pkgDir); err != nil {
			fmt.Printf("[release] warn: 清理打包暂存目录 %s 失败: %v\n", pkgDir, err)
		}
	}()
	rsync := exec.Command("rsync", "-a", outputs+"/", pkgDir+"/")
	if b, err := rsync.CombinedOutput(); err != nil {
		return out, fmt.Errorf("rsync outputs: %w\n%s", err, string(b))
	}
	_ = os.WriteFile(filepath.Join(pkgDir, "VERSION"), []byte(hash+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(pkgDir, "COMMIT"), []byte(full+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(pkgDir, "GIT_REPO_URL"), []byte(gitRepoURL+"\n"), 0o644)
	// 产物平台写进包里：部署前据此判断「这份能发到那台机器吗」。
	_ = os.WriteFile(filepath.Join(pkgDir, "PLATFORM"), []byte(platform.String()+"\n"), 0o644)
	// 打包时就核对产物平台：build.sh 可能把**构建机**的东西打进包里（例如按 uname 下载的
	// 平台相关工具）。在这里失败 = 没有上传、没有下载，比推到远端才发现便宜得多。
	if err := assertPackageMatchesPlatform(pkgDir, platform); err != nil {
		if events != nil {
			events(eventlevel.Error, "产物平台校验失败："+err.Error())
		}
		return out, err
	}

	if events != nil {
		events(eventlevel.Info, "上传开始：storage="+storage.Name()+" tag="+tag)
	}
	uploadStart := time.Now()
	meta, err := storage.Upload(context.Background(), serviceID, gitRepoURL, tag, pkgDir)
	if err != nil {
		if events != nil {
			events(eventlevel.Error, "上传失败：storage="+storage.Name()+" tag="+tag+" err="+err.Error())
		}
		return out, fmt.Errorf("upload artifact: %w", err)
	}
	if events != nil {
		events(eventlevel.Success, "上传结束：storage="+storage.Name()+" tag="+tag+
			" size="+humanBytes(meta.Size)+" 耗时="+humanDuration(time.Since(uploadStart)))
	}
	out.Artifact = meta
	return out, nil
}

// humanBytes renders a byte count like "12.3 MB" (used in event lines).
func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// humanDuration renders a duration like "3.4s" (used in event lines).
func humanDuration(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}
