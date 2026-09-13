package main

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// ArtifactStorage is a pluggable backend for build package storage. The
// bytes live somewhere (local disk, GitHub Releases, a future cloud bucket);
// the local artifacts table is the storage-agnostic index of where to find
// them (access paths). Implementations: localStorage, githubReleaseStorage.
type ArtifactStorage interface {
	// Name is the storage backend identifier persisted in artifacts.storage
	// (e.g. "local", "github_release").
	Name() string

	// Exists reports whether an artifact for <tag> is present in this storage.
	Exists(ctx context.Context, serviceID, gitRepoURL, tag string) (bool, error)

	// Upload stores the package built in pkgDir under <tag> and returns the
	// metadata + access path to be recorded in the local artifacts table.
	Upload(ctx context.Context, serviceID, gitRepoURL, tag, pkgDir string) (*ArtifactMeta, error)

	// Download fetches the artifact for <tag> into destDir. accessPath is the
	// stored access path from the artifacts table (AssetURL); when non-empty
	// the implementation may use it to skip resolution (e.g. GitHub asset URL
	// or a local dir path).
	Download(ctx context.Context, serviceID, gitRepoURL, tag, destDir, accessPath string) error

	// List enumerates artifacts in this storage (used to backfill the local
	// artifacts table from existing storage).
	List(ctx context.Context, gitRepoURL string) ([]ReleaseScanItem, error)
}

// ArtifactMeta is the metadata returned after uploading a package, later
// persisted into the local artifacts table as the access path to the storage.
// Fields are populated by the implementation that produced them; storage
// identifies which backend filled them.
type ArtifactMeta struct {
	Storage            string // "github_release" | "local"
	RepoSlug           string // owner/repo (github)
	ReleaseID          int64  // github release id
	ReleaseURL         string // release html_url (github)
	AssetID            int64  // github asset id
	AssetURL           string // github: API download URL; local: package dir path
	BrowserDownloadURL string // github: direct https download URL
	LocalPath          string // local: the on-disk package dir path
	Size               int64
}

// ReleaseScanItem describes one artifact found while scanning a storage
// backend (used to backfill the local artifacts table).
type ReleaseScanItem struct {
	Tag                string
	ReleaseID          int64
	ReleaseURL         string
	AssetID            int64
	AssetURL           string
	BrowserDownloadURL string
	LocalPath          string
	Size               int64
}

// NewArtifactStorage picks the storage backend from cfg.ArtifactStorageType
// ("local" | "github_release"). An empty type defaults to github_release when
// a GitHub token is configured, else local.
func NewArtifactStorage(cfg Config) (ArtifactStorage, error) {
	t := strings.TrimSpace(cfg.ArtifactStorageType)
	if t == "" {
		if cfg.GitHubToken != "" {
			t = "github_release"
		} else {
			t = "local"
		}
	}
	switch t {
	case "local":
		return &localStorage{base: cfg.PackagesDir}, nil
	case "github_release":
		if cfg.GitHubToken == "" {
			return nil, fmt.Errorf("artifact storage %q requires GITHUB_TOKEN", t)
		}
		return &githubReleaseStorage{token: cfg.GitHubToken}, nil
	default:
		return nil, fmt.Errorf("unknown artifact storage %q (want local|github_release)", t)
	}
}

// dirSize returns the total byte size of a directory tree (best-effort).
func dirSize(path string) int64 {
	var size int64
	_ = filepathWalkSize(path, &size)
	return size
}

func filepathWalkSize(root string, size *int64) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		full := root + "/" + e.Name()
		if e.IsDir() {
			if err := filepathWalkSize(full, size); err != nil {
				return err
			}
			continue
		}
		if info, err := e.Info(); err == nil {
			*size += info.Size()
		}
	}
	return nil
}
