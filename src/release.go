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
	Artifact   *ArtifactMeta
}

// PackageEventFunc receives progress lines emitted by packageFromGit (build /
// upload) so the caller can record them on the pipeline timeline. Level uses
// the canonical eventlevel set. A nil sink simply discards the lines.
type PackageEventFunc func(level eventlevel.Level, message string)

// packageFromGit clones/fetches ref, runs build.sh, and stores the frozen
// outputs via the configured ArtifactStorage (local disk or GitHub Releases,
// etc.). Nothing is persisted under packagesDir unless the storage is local;
// the storage backend is the single source of truth for the bytes. events, when
// non-nil, receives granular build/upload progress lines (e.g. 上传开始/上传结束).
func packageFromGit(serviceID, gitRepoURL, ref string, maxSec int, storage ArtifactStorage, events PackageEventFunc) (PackageResult, error) {
	var out PackageResult
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
		cmd.Env = os.Environ()
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
	tag := "deployment-" + hash
	out.Tag = tag
	out.Hash = hash
	out.FullCommit = full

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
	buildCmd.Env = append(os.Environ(), "APP_VERSION="+hash)
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
