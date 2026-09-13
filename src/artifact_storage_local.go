package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// localStorage keeps build packages on local disk under
// <base>/<serviceID>/<tag>/. It mirrors the original pre-release layout and
// needs no external credentials. "Download" is a direct rsync from the
// stored dir (no tar/untar round-trip).
type localStorage struct {
	base string // packagesDir
}

func (s *localStorage) Name() string { return "local" }

func (s *localStorage) pkgDir(serviceID, tag string) string {
	return filepath.Join(s.base, serviceID, tag)
}

func (s *localStorage) Exists(ctx context.Context, serviceID, gitRepoURL, tag string) (bool, error) {
	dir := s.pkgDir(serviceID, tag)
	st, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !st.IsDir() {
		return false, nil
	}
	// Require a VERSION file so a partial/empty dir is not treated as present.
	_, err = os.Stat(filepath.Join(dir, "VERSION"))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (s *localStorage) Upload(ctx context.Context, serviceID, gitRepoURL, tag, src string) (*ArtifactMeta, error) {
	dst := s.pkgDir(serviceID, tag)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	// rsync the staged package dir into storage.
	cmd := exec.Command("rsync", "-a", "--delete", src+"/", dst+"/")
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("rsync to local storage: %w\n%s", err, string(b))
	}
	return &ArtifactMeta{
		Storage:   "local",
		AssetURL:  dst, // access path = the on-disk package dir
		LocalPath: dst,
		Size:      dirSize(dst),
	}, nil
}

func (s *localStorage) Download(ctx context.Context, serviceID, gitRepoURL, tag, destDir, accessPath string) error {
	src := accessPath
	if src == "" {
		src = s.pkgDir(serviceID, tag)
	}
	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("local artifact not found: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("local artifact is not a directory: %s", src)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	cmd := exec.Command("rsync", "-a", "--delete", strings.TrimRight(src, "/")+"/", destDir+"/")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rsync from local storage: %w\n%s", err, string(b))
	}
	return nil
}

func (s *localStorage) List(ctx context.Context, gitRepoURL string) ([]ReleaseScanItem, error) {
	// Enumerate <base>/<serviceID>/<tag>/ directories that carry a VERSION file.
	entries, err := os.ReadDir(s.base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var items []ReleaseScanItem
	for _, svc := range entries {
		if !svc.IsDir() {
			continue
		}
		serviceID := svc.Name()
		pkgs, err := os.ReadDir(filepath.Join(s.base, serviceID))
		if err != nil {
			continue
		}
		for _, pkg := range pkgs {
			if !pkg.IsDir() {
				continue
			}
			tag := pkg.Name()
			if !strings.HasPrefix(tag, "deployment-") {
				continue
			}
			dir := filepath.Join(s.base, serviceID, tag)
			if _, err := os.Stat(filepath.Join(dir, "VERSION")); err != nil {
				continue
			}
			items = append(items, ReleaseScanItem{
				Tag:       tag,
				LocalPath: dir,
				AssetURL:  dir,
				Size:      dirSize(dir),
			})
		}
	}
	return items, nil
}
