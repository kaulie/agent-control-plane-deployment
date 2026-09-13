package main

import (
	"context"
	"fmt"
)

// githubReleaseStorage stores build packages as package.tar.gz assets on each
// service's own GitHub repo release (tag deployment-<hash>). It wraps the
// low-level GitHub API helpers in release_storage.go.
type githubReleaseStorage struct {
	token string
}

func (s *githubReleaseStorage) Name() string { return "github_release" }

func (s *githubReleaseStorage) Exists(ctx context.Context, serviceID, gitRepoURL, tag string) (bool, error) {
	return releaseAssetExists(ctx, s.token, gitRepoURL, tag)
}

func (s *githubReleaseStorage) Upload(ctx context.Context, serviceID, gitRepoURL, tag, pkgDir string) (*ArtifactMeta, error) {
	meta, err := uploadPackageToRelease(ctx, s.token, gitRepoURL, tag, pkgDir)
	if err != nil {
		return nil, err
	}
	if meta != nil {
		meta.Storage = "github_release"
	}
	return meta, nil
}

func (s *githubReleaseStorage) Download(ctx context.Context, serviceID, gitRepoURL, tag, destDir, accessPath string) error {
	if s.token == "" {
		return fmt.Errorf("GITHUB_TOKEN not set; cannot download release asset")
	}
	return downloadPackageFromRelease(ctx, s.token, gitRepoURL, tag, destDir, accessPath)
}

func (s *githubReleaseStorage) List(ctx context.Context, gitRepoURL string) ([]ReleaseScanItem, error) {
	return listServiceReleases(ctx, s.token, gitRepoURL)
}
