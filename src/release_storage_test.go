package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseRepoOwnerName(t *testing.T) {
	cases := []struct {
		in                  string
		owner, repo, errSub string
	}{
		{"https://github.com/kaulie/agent-control-plane-deployment", "kaulie", "agent-control-plane-deployment", ""},
		{"https://github.com/kaulie/agent-control-plane-deployment.git", "kaulie", "agent-control-plane-deployment", ""},
		{"git@github.com:kaulie/agent-control-plane-deployment.git", "kaulie", "agent-control-plane-deployment", ""},
		{"git@github.com:kaulie/agent-control-plane-deployment", "kaulie", "agent-control-plane-deployment", ""},
		{"", "", "", "empty"},
		{"https://github.com/", "", "", "cannot parse"},
	}
	for _, c := range cases {
		o, r, err := parseRepoOwnerName(c.in)
		gotErr := ""
		if err != nil {
			gotErr = err.Error()
		}
		if o != c.owner || r != c.repo || (c.errSub != "" && !strings.Contains(gotErr, c.errSub)) {
			t.Errorf("parse(%q) = (%q,%q,%q) want (%q,%q,contains %q)",
				c.in, o, r, gotErr, c.owner, c.repo, c.errSub)
		}
	}
}

// fakeGitHub simulates the GitHub releases + upload + asset-download API on a
// single httptest server. Set ghAPIBase and ghUploadBase to its URL.
type fakeGitHub struct {
	t        *testing.T
	token    string
	releases map[string]*ghRelease
	assets   map[int64][]byte
	nextID   int64
	srvURL   string
}

func newFakeGitHub(t *testing.T, token string) *fakeGitHub {
	fg := &fakeGitHub{
		t: t, token: token,
		releases: map[string]*ghRelease{}, assets: map[int64][]byte{}, nextID: 1,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/tags/", fg.handleGetByTag)
	mux.HandleFunc("/repos/o/r/releases", fg.handleReleasesRoot)
	mux.HandleFunc("/repos/o/r/releases/assets/", fg.handleDeleteAsset)
	mux.HandleFunc("/repos/o/r/releases/", fg.handleUploadAsset) // POST .../releases/<id>/assets
	mux.HandleFunc("/asset/", fg.handleDownloadAsset)
	srv := httptest.NewServer(mux)
	fg.srvURL = srv.URL
	t.Cleanup(srv.Close)
	return fg
}

func (f *fakeGitHub) checkAuth(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+f.token
}

func (f *fakeGitHub) handleGetByTag(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(r) {
		http.Error(w, "auth", http.StatusUnauthorized)
		return
	}
	tag := strings.TrimPrefix(r.URL.Path, "/repos/o/r/releases/tags/")
	rel, ok := f.releases[tag]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rel)
}

func (f *fakeGitHub) handleReleasesRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if _, ok := f.releases[body.TagName]; ok {
		http.Error(w, "exists", http.StatusUnprocessableEntity)
		return
	}
	rel := &ghRelease{ID: f.nextID, TagName: body.TagName}
	f.nextID++
	f.releases[body.TagName] = rel
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rel)
}

func (f *fakeGitHub) handleDeleteAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/repos/o/r/releases/assets/")
	id, _ := strconv.ParseInt(idStr, 10, 64)
	delete(f.assets, id)
	for _, rel := range f.releases {
		for i, a := range rel.Assets {
			if a.ID == id {
				rel.Assets = append(rel.Assets[:i], rel.Assets[i+1:]...)
				break
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUploadAsset matches POST /repos/o/r/releases/<id>/assets?name=...
func (f *fakeGitHub) handleUploadAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/repos/o/r/releases/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "assets" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	rid, _ := strconv.ParseInt(parts[0], 10, 64)
	name := r.URL.Query().Get("name")
	b, _ := io.ReadAll(r.Body)
	aid := f.nextID
	f.nextID++
	f.assets[aid] = b
	assetURL := f.srvURL + "/asset/" + strconv.FormatInt(aid, 10)
	for _, rel := range f.releases {
		if rel.ID == rid {
			rel.Assets = append(rel.Assets, ghAsset{ID: aid, Name: name, URL: assetURL})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": aid})
}

func (f *fakeGitHub) handleDownloadAsset(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/asset/")
	id, _ := strconv.ParseInt(idStr, 10, 64)
	b, ok := f.assets[id]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(b)
}

func makePkgDir(t *testing.T) string {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "bin", "app"), []byte("#!/bin/sh\n"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "VERSION"), []byte("hash12345\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "COMMIT"), []byte("fullcommit\n"), 0o644)
	return dir
}

func TestTarUntarRoundTrip(t *testing.T) {
	pkg := makePkgDir(t)
	var buf bytes.Buffer
	if err := tarDir(pkg, &buf); err != nil {
		t.Fatalf("tarDir: %v", err)
	}
	// Verify gzip+tar contains VERSION with expected content.
	gr, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gr)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if h.Name == "VERSION" {
			b, _ := io.ReadAll(tr)
			if strings.TrimSpace(string(b)) == "hash12345" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("tar did not contain VERSION with expected content")
	}
	// untarGz into a fresh dir reproduces the package.
	dest := t.TempDir()
	if err := untarGz(bytes.NewReader(buf.Bytes()), dest); err != nil {
		t.Fatalf("untarGz: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "bin", "app"))
	if err != nil {
		t.Fatalf("read restored app: %v", err)
	}
	if !strings.HasPrefix(string(b), "#!/bin/sh") {
		t.Fatalf("restored app content wrong: %q", b)
	}
}

func TestUploadDownloadReleaseFlow(t *testing.T) {
	fg := newFakeGitHub(t, "tok")
	oldAPI, oldUpload := ghAPIBase, ghUploadBase
	ghAPIBase = fg.srvURL
	ghUploadBase = fg.srvURL
	t.Cleanup(func() { ghAPIBase = oldAPI; ghUploadBase = oldUpload })

	ctx := context.Background()
	repoURL := "https://github.com/o/r"
	tag := "deployment-hash12345"

	// Initially absent.
	exists, err := releaseAssetExists(ctx, "tok", repoURL, tag)
	if err != nil || exists {
		t.Fatalf("expected absent, got exists=%v err=%v", exists, err)
	}

	// Upload package.
	pkg := makePkgDir(t)
	if err := uploadPackageToRelease(ctx, "tok", repoURL, tag, pkg); err != nil {
		t.Fatalf("upload: %v", err)
	}
	exists, _ = releaseAssetExists(ctx, "tok", repoURL, tag)
	if !exists {
		t.Fatal("expected asset to exist after upload")
	}

	// Download into a fresh dir and verify content.
	dest := t.TempDir()
	if err := downloadPackageFromRelease(ctx, "tok", repoURL, tag, dest); err != nil {
		t.Fatalf("download: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "VERSION"))
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	if strings.TrimSpace(string(b)) != "hash12345" {
		t.Fatalf("VERSION = %q", b)
	}

	// Re-upload replaces the asset (delete + re-add), still exactly one asset.
	if err := uploadPackageToRelease(ctx, "tok", repoURL, tag, pkg); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	rel := fg.releases[tag]
	if rel == nil || len(rel.Assets) != 1 {
		t.Fatalf("expected exactly 1 asset after re-upload, got %d", len(rel.Assets))
	}

	// Missing token -> upload error.
	if err := uploadPackageToRelease(ctx, "", repoURL, tag, pkg); err == nil {
		t.Fatal("expected error uploading without token")
	}

	// Missing tag -> download error.
	if err := downloadPackageFromRelease(ctx, "tok", repoURL, "deployment-nope", dest); err == nil {
		t.Fatal("expected error downloading missing release")
	}
}

func TestReleaseAssetExistsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := releaseAssetExists(ctx, "tok", "https://github.com/kaulie/some-repo", "deployment-x"); err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}
