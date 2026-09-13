package main

import (
	"context"
	"path/filepath"
	"testing"
)

func TestArtifactRecordGetList(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	a := Artifact{
		ServiceID:          "web-cursor",
		Tag:                "deployment-94ad00da",
		Version:             "94ad00da",
		Commit:              "94ad00da2f4c8b97a279324d745a050a5a6bf574",
		GitRepoURL:          "https://github.com/kaulie/agent-control-plane",
		RepoSlug:            "kaulie/agent-control-plane",
		AssetName:           "package.tar.gz",
		AssetID:             42,
		AssetURL:            "https://api.github.com/repos/kaulie/agent-control-plane/releases/assets/42",
		BrowserDownloadURL:  "https://github.com/kaulie/agent-control-plane/releases/download/deployment-94ad00da/package.tar.gz",
		ReleaseURL:          "https://github.com/kaulie/agent-control-plane/releases/tag/deployment-94ad00da",
		Size:                95149824,
		Storage:             "github_release",
		CreatedAt:           "2026-09-13T11:00:00.000Z",
	}
	if err := s.RecordArtifact(a); err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}

	got, err := s.GetArtifact(a.ServiceID, a.Tag)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if got == nil {
		t.Fatal("expected artifact, got nil")
	}
	if got.AssetURL != a.AssetURL || got.BrowserDownloadURL != a.BrowserDownloadURL || got.Size != a.Size {
		t.Fatalf("metadata mismatch: %+v", got)
	}
	if got.Version != a.Version || got.Commit != a.Commit {
		t.Fatalf("version/commit mismatch: %+v", got)
	}

	// Upsert: same (service_id, tag) updates metadata, preserves id.
	origID := got.ID
	updated := a
	updated.Size = 12345
	updated.AssetID = 99
	if err := s.RecordArtifact(updated); err != nil {
		t.Fatalf("RecordArtifact upsert: %v", err)
	}
	got2, err := s.GetArtifact(a.ServiceID, a.Tag)
	if err != nil {
		t.Fatalf("GetArtifact after upsert: %v", err)
	}
	if got2.ID != origID {
		t.Fatalf("upsert changed id: %d -> %d", origID, got2.ID)
	}
	if got2.Size != 12345 || got2.AssetID != 99 {
		t.Fatalf("upsert did not update metadata: %+v", got2)
	}

	// Second artifact + list.
	if err := s.RecordArtifact(Artifact{
		ServiceID: "web-cursor", Tag: "deployment-aaaaaaaa", Version: "aaaaaaaa",
		GitRepoURL: "u", RepoSlug: "o/r", AssetURL: "https://api/x", Storage: "github_release",
		CreatedAt: "2026-09-13T12:00:00.000Z",
	}); err != nil {
		t.Fatalf("RecordArtifact second: %v", err)
	}
	all, err := s.ListArtifacts("web-cursor")
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 artifacts, got %d", len(all))
	}
	other, err := s.ListArtifacts("other-service")
	if err != nil {
		t.Fatalf("ListArtifacts other: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("expected 0 artifacts for other service, got %d", len(other))
	}

	// Missing artifact -> nil, no error.
	miss, err := s.GetArtifact("web-cursor", "deployment-nope")
	if err != nil {
		t.Fatalf("GetArtifact missing: %v", err)
	}
	if miss != nil {
		t.Fatalf("expected nil for missing, got %+v", miss)
	}

	// RecordArtifact requires the access path (assetUrl).
	if err := s.RecordArtifact(Artifact{ServiceID: "x", Tag: "t", Version: "v", GitRepoURL: "u", RepoSlug: "o/r"}); err == nil {
		t.Fatal("expected error when assetUrl is empty")
	}
}

func TestListServiceReleases(t *testing.T) {
	fg := newFakeGitHub(t, "tok")
	oldAPI, oldUpload := ghAPIBase, ghUploadBase
	ghAPIBase = fg.srvURL
	ghUploadBase = fg.srvURL
	t.Cleanup(func() { ghAPIBase = oldAPI; ghUploadBase = oldUpload })

	repoURL := "https://github.com/o/r"
	ctx := context.Background()

	// Upload two packages (creates two releases with the package asset).
	for _, tag := range []string{"deployment-11111111", "deployment-22222222"} {
		pkg := makePkgDir(t)
		if _, err := uploadPackageToRelease(ctx, "tok", repoURL, tag, pkg); err != nil {
			t.Fatalf("upload %s: %v", tag, err)
		}
	}

	items, err := listServiceReleases(ctx, "tok", repoURL)
	if err != nil {
		t.Fatalf("listServiceReleases: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 release items, got %d", len(items))
	}
	for _, it := range items {
		if it.AssetURL == "" || it.BrowserDownloadURL == "" || it.Size == 0 {
			t.Fatalf("scan item missing access paths: %+v", it)
		}
	}
}
