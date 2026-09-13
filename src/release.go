package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type PackageResult struct {
	Tag        string
	Hash       string
	FullCommit string
	Dir        string
	Skipped    bool
	Artifact   *ArtifactMeta
}

// packageFromGit clones/fetches ref, runs build.sh, and uploads the frozen
// outputs as a package.tar.gz asset of a GitHub release (tag deployment-<hash>)
// on the service's own git repo. Nothing is persisted under packagesDir;
// the release is the single source of truth (saves local disk space).
func packageFromGit(packagesDir, serviceID, gitRepoURL, ref string, maxSec int, token string) (PackageResult, error) {
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

	workDir, err := os.MkdirTemp("", "release-acp-*")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(workDir)

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

	// Skip build if the release asset already exists (idempotent re-deploys).
	if exists, err := releaseAssetExists(context.Background(), token, gitRepoURL, tag); err == nil && exists {
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
	// Stage the package in a temp dir, then upload to the service repo's
	// GitHub release. Nothing is written under packagesDir.
	pkgDir, err := os.MkdirTemp("", "release-pkg-*")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(pkgDir)
	rsync := exec.Command("rsync", "-a", outputs+"/", pkgDir+"/")
	if b, err := rsync.CombinedOutput(); err != nil {
		return out, fmt.Errorf("rsync outputs: %w\n%s", err, string(b))
	}
	_ = os.WriteFile(filepath.Join(pkgDir, "VERSION"), []byte(hash+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(pkgDir, "COMMIT"), []byte(full+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(pkgDir, "GIT_REPO_URL"), []byte(gitRepoURL+"\n"), 0o644)

	meta, err := uploadPackageToRelease(context.Background(), token, gitRepoURL, tag, pkgDir)
	if err != nil {
		return out, fmt.Errorf("upload release: %w", err)
	}
	out.Artifact = meta
	return out, nil
}
