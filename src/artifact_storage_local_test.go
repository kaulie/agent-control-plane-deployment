package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalStorageRoundTrip(t *testing.T) {
	base := t.TempDir()
	s := &localStorage{base: base}

	// Stage a fake package dir.
	pkg := t.TempDir()
	if err := os.WriteFile(filepath.Join(pkg, "VERSION"), []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "bin", "app"), []byte("binary\n"), 0o644); err != nil {
		// create the bin subdir first
		_ = os.MkdirAll(filepath.Join(pkg, "bin"), 0o755)
		if err := os.WriteFile(filepath.Join(pkg, "bin", "app"), []byte("binary\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const serviceID, tag = "svc-a", "deployment-deadbeef"
	exists, err := s.Exists(context.Background(), serviceID, "", tag)
	if err != nil || exists {
		t.Fatalf("Exists before upload: got exists=%v err=%v", exists, err)
	}

	meta, err := s.Upload(context.Background(), serviceID, "", tag, pkg)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if meta == nil || meta.Storage != "local" || meta.LocalPath == "" || meta.AssetURL == "" {
		t.Fatalf("bad meta: %+v", meta)
	}
	wantDir := filepath.Join(base, serviceID, tag)
	if meta.LocalPath != wantDir {
		t.Fatalf("LocalPath=%q want %q", meta.LocalPath, wantDir)
	}
	if _, err := os.Stat(filepath.Join(wantDir, "VERSION")); err != nil {
		t.Fatalf("VERSION not stored: %v", err)
	}

	exists, err = s.Exists(context.Background(), serviceID, "", tag)
	if err != nil || !exists {
		t.Fatalf("Exists after upload: got exists=%v err=%v", exists, err)
	}

	// Download to a fresh dest and verify contents.
	dest := t.TempDir()
	if err := s.Download(context.Background(), serviceID, "", tag, dest, ""); err != nil {
		t.Fatalf("Download: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "VERSION"))
	if err != nil || string(b) != "deadbeef\n" {
		t.Fatalf("downloaded VERSION=%q err=%v", string(b), err)
	}
	if _, err := os.Stat(filepath.Join(dest, "bin", "app")); err != nil {
		t.Fatalf("downloaded bin/app missing: %v", err)
	}

	// Download via explicit accessPath (the stored AssetURL).
	dest2 := t.TempDir()
	if err := s.Download(context.Background(), serviceID, "", tag, dest2, meta.AssetURL); err != nil {
		t.Fatalf("Download via accessPath: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest2, "VERSION")); err != nil {
		t.Fatalf("accessPath download missing VERSION: %v", err)
	}

	// List enumerates the stored artifact.
	items, err := s.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].Tag != tag || items[0].LocalPath != wantDir {
		t.Fatalf("List items=%+v", items)
	}
}

func TestLocalStorageExistsMissingVersion(t *testing.T) {
	// A dir without VERSION must NOT be treated as present.
	base := t.TempDir()
	dir := filepath.Join(base, "svc-b", "deployment-aaa")
	_ = os.MkdirAll(dir, 0o755)
	s := &localStorage{base: base}
	exists, err := s.Exists(context.Background(), "svc-b", "", "deployment-aaa")
	if err != nil || exists {
		t.Fatalf("Exists without VERSION: got exists=%v err=%v", exists, err)
	}
}

func TestNewArtifactStorageFactory(t *testing.T) {
	// No token + no env → local.
	cfg := Config{PackagesDir: t.TempDir()}
	got, err := NewArtifactStorage(cfg)
	if err != nil || got.Name() != "local" {
		t.Fatalf("default-no-token: name=%q err=%v", nameOr(got), err)
	}

	// Explicit local even with token.
	cfg2 := Config{PackagesDir: t.TempDir(), GitHubToken: "x", ArtifactStorageType: "local"}
	got2, err := NewArtifactStorage(cfg2)
	if err != nil || got2.Name() != "local" {
		t.Fatalf("explicit-local: name=%q err=%v", nameOr(got2), err)
	}

	// Token + default → github_release.
	cfg3 := Config{GitHubToken: "x"}
	got3, err := NewArtifactStorage(cfg3)
	if err != nil || got3.Name() != "github_release" {
		t.Fatalf("default-with-token: name=%q err=%v", nameOr(got3), err)
	}

	// github_release without token → error.
	_, err = NewArtifactStorage(Config{ArtifactStorageType: "github_release"})
	if err == nil {
		t.Fatal("expected error for github_release without token")
	}

	// Unknown → error.
	_, err = NewArtifactStorage(Config{ArtifactStorageType: "s3"})
	if err == nil {
		t.Fatal("expected error for unknown storage")
	}
}

func nameOr(s ArtifactStorage) string {
	if s == nil {
		return "<nil>"
	}
	return s.Name()
}
