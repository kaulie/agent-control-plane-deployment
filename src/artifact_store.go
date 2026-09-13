package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// Artifact is the local metadata record for a build package. The bytes live
// on GitHub Releases (pure storage); this table is the local index of where
// to find them (access paths) plus identifying metadata.
type Artifact struct {
	ID                 int64  `json:"id"`
	ServiceID          string `json:"serviceId"`
	Tag                string `json:"tag"` // deployment-<hash>
	Version            string `json:"version"`
	Commit             string `json:"commit,omitempty"` // full sha
	GitRepoURL         string `json:"gitRepoUrl"`
	RepoSlug           string `json:"repoSlug"` // owner/repo
	AssetName          string `json:"assetName"`
	AssetID            int64  `json:"assetId,omitempty"`
	AssetURL           string `json:"assetUrl"`            // GitHub API download URL
	BrowserDownloadURL string `json:"browserDownloadUrl,omitempty"` // direct https download
	ReleaseURL         string `json:"releaseUrl,omitempty"`         // release html_url
	Size               int64  `json:"size,omitempty"`
	Storage            string `json:"storage"` // github_release
	CreatedAt          string `json:"createdAt"`
}

// migrateArtifacts creates the artifacts metadata table. GitHub Releases is
// the single source of truth for the bytes; this table only mirrors metadata
// (access paths, version, repo) so deploys and the panel can resolve a
// package without re-querying the GitHub API each time.
func (s *Store) migrateArtifacts() error {
	_, err := s.db.Exec(`
      CREATE TABLE IF NOT EXISTS artifacts (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        service_id TEXT NOT NULL,
        tag TEXT NOT NULL,
        version TEXT NOT NULL,
        commit_sha TEXT NOT NULL DEFAULT '',
        git_repo_url TEXT NOT NULL,
        repo_slug TEXT NOT NULL,
        asset_name TEXT NOT NULL DEFAULT 'package.tar.gz',
        asset_id INTEGER NOT NULL DEFAULT 0,
        asset_url TEXT NOT NULL DEFAULT '',
        browser_download_url TEXT NOT NULL DEFAULT '',
        release_url TEXT NOT NULL DEFAULT '',
        size INTEGER NOT NULL DEFAULT 0,
        storage TEXT NOT NULL DEFAULT 'github_release',
        created_at TEXT NOT NULL,
        UNIQUE(service_id, tag)
      );
      CREATE INDEX IF NOT EXISTS idx_artifacts_service ON artifacts(service_id);
      CREATE INDEX IF NOT EXISTS idx_artifacts_tag ON artifacts(tag);
    `)
	return err
}

// RecordArtifact upserts an artifact row keyed by (service_id, tag). The id
// and created_at are preserved on update; only metadata is refreshed (e.g.
// asset id/URL can change if an asset is deleted and re-uploaded).
func (s *Store) RecordArtifact(a Artifact) error {
	a.ServiceID = strings.TrimSpace(a.ServiceID)
	a.Tag = strings.TrimSpace(a.Tag)
	if a.ServiceID == "" || a.Tag == "" {
		return fmt.Errorf("serviceId and tag are required")
	}
	if a.AssetURL == "" {
		return fmt.Errorf("assetUrl is required (the access path to the storage)")
	}
	if a.Storage == "" {
		a.Storage = "github_release"
	}
	if a.CreatedAt == "" {
		a.CreatedAt = nowISO()
	}
	_, err := s.db.Exec(`
      INSERT INTO artifacts (
        service_id, tag, version, commit_sha, git_repo_url, repo_slug,
        asset_name, asset_id, asset_url, browser_download_url, release_url,
        size, storage, created_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
      ON CONFLICT(service_id, tag) DO UPDATE SET
        version = excluded.version,
        commit_sha = excluded.commit_sha,
        git_repo_url = excluded.git_repo_url,
        repo_slug = excluded.repo_slug,
        asset_name = excluded.asset_name,
        asset_id = excluded.asset_id,
        asset_url = excluded.asset_url,
        browser_download_url = excluded.browser_download_url,
        release_url = excluded.release_url,
        size = excluded.size,
        storage = excluded.storage`,
		a.ServiceID, a.Tag, a.Version, a.Commit, a.GitRepoURL, a.RepoSlug,
		a.AssetName, a.AssetID, a.AssetURL, a.BrowserDownloadURL, a.ReleaseURL,
		a.Size, a.Storage, a.CreatedAt,
	)
	return err
}

// GetArtifact returns the artifact for a service+tag, or nil if absent.
func (s *Store) GetArtifact(serviceID, tag string) (*Artifact, error) {
	row := s.db.QueryRow(`
      SELECT id, service_id, tag, version, commit_sha, git_repo_url, repo_slug,
        asset_name, asset_id, asset_url, browser_download_url, release_url,
        size, storage, created_at
      FROM artifacts WHERE service_id = ? AND tag = ?`,
		serviceID, tag)
	a, err := scanArtifact(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

// ListArtifacts returns artifacts, optionally filtered by serviceId. When
// serviceID is empty all artifacts are returned, newest first.
func (s *Store) ListArtifacts(serviceID string) ([]Artifact, error) {
	q := `SELECT id, service_id, tag, version, commit_sha, git_repo_url, repo_slug,
        asset_name, asset_id, asset_url, browser_download_url, release_url,
        size, storage, created_at
      FROM artifacts`
	var (
		rows *sql.Rows
		err  error
	)
	if serviceID = strings.TrimSpace(serviceID); serviceID != "" {
		rows, err = s.db.Query(q+" WHERE service_id = ? ORDER BY id DESC", serviceID)
	} else {
		rows, err = s.db.Query(q + " ORDER BY id DESC")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

type artifactScanner interface {
	Scan(dest ...any) error
}

func scanArtifact(sc artifactScanner) (*Artifact, error) {
	var a Artifact
	err := sc.Scan(
		&a.ID, &a.ServiceID, &a.Tag, &a.Version, &a.Commit, &a.GitRepoURL, &a.RepoSlug,
		&a.AssetName, &a.AssetID, &a.AssetURL, &a.BrowserDownloadURL, &a.ReleaseURL,
		&a.Size, &a.Storage, &a.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &a, nil
}
