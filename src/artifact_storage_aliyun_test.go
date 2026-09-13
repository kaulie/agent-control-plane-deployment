package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAliyun simulates the Aliyun packages generic-repo protocol (basic-auth
// upload/download) on one httptest server.
type fakeAliyun struct {
	t        *testing.T
	username string
	password string
	objects  map[string][]byte // key: "<path>?version=<v>"
	url      string
}

func newFakeAliyun(t *testing.T, user, pass string) *fakeAliyun {
	fa := &fakeAliyun{t: t, username: user, password: pass, objects: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/protocol/", fa.handle)
	srv := httptest.NewServer(mux)
	fa.url = srv.URL
	t.Cleanup(srv.Close)
	return fa
}

func (f *fakeAliyun) handle(w http.ResponseWriter, r *http.Request) {
	u, p, ok := r.BasicAuth()
	if !ok || u != f.username || p != f.password {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	version := r.URL.Query().Get("version")
	switch r.Method {
	case http.MethodPost:
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
			return
		}
		fh, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "no file: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer fh.Close()
		b, _ := io.ReadAll(fh)
		storedPath := r.URL.Path + "/" + r.URL.Query().Get("downloadFileName")
		f.objects[storedPath+"?version="+version] = b
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object":     map[string]any{"fileSize": len(b), "url": f.url + storedPath},
			"successful": true,
		})
	case http.MethodGet:
		b, ok := f.objects[r.URL.Path+"?version="+version]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(b)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func TestAliyunStorageRoundTrip(t *testing.T) {
	fa := newFakeAliyun(t, "user1", "pass1")
	s := &aliyunPackagesStorage{
		baseURL: fa.url, productID: "PID", repo: "deployment-artifact",
		username: "user1", password: "pass1",
	}
	ctx := context.Background()
	const serviceID, tag = "svc-a", "deployment-deadbeef"

	exists, err := s.Exists(ctx, serviceID, "", tag)
	if err != nil || exists {
		t.Fatalf("Exists before upload: exists=%v err=%v", exists, err)
	}

	pkg := t.TempDir()
	if err := os.WriteFile(filepath.Join(pkg, "VERSION"), []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := s.Upload(ctx, serviceID, "", tag, pkg)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if meta == nil || meta.Storage != "aliyun" || meta.AssetURL == "" {
		t.Fatalf("bad meta: %+v", meta)
	}
	if !strings.Contains(meta.AssetURL, "/files/svc-a/deployment-deadbeef/package.tar.gz") {
		t.Fatalf("AssetURL=%q", meta.AssetURL)
	}

	exists, err = s.Exists(ctx, serviceID, "", tag)
	if err != nil || !exists {
		t.Fatalf("Exists after upload: exists=%v err=%v", exists, err)
	}

	// Download resolving the URL from service+tag.
	dest := t.TempDir()
	if err := s.Download(ctx, serviceID, "", tag, dest, ""); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "VERSION")); strings.TrimSpace(string(b)) != "deadbeef" {
		t.Fatalf("downloaded VERSION=%q", b)
	}

	// Download via the stored access path.
	dest2 := t.TempDir()
	if err := s.Download(ctx, serviceID, "", tag, dest2, meta.AssetURL); err != nil {
		t.Fatalf("Download via accessPath: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest2, "VERSION")); err != nil {
		t.Fatalf("accessPath download missing VERSION: %v", err)
	}

	// Download of an unknown tag fails.
	if err := s.Download(ctx, serviceID, "", "deployment-nope", t.TempDir(), ""); err == nil {
		t.Fatal("expected error downloading unknown tag")
	}
}

func TestAliyunStorageBadCredentials(t *testing.T) {
	fa := newFakeAliyun(t, "user1", "pass1")
	s := &aliyunPackagesStorage{
		baseURL: fa.url, productID: "PID", repo: "deployment-artifact",
		username: "user1", password: "WRONG",
	}
	if _, err := s.Upload(context.Background(), "svc", "", "deployment-x", t.TempDir()); err == nil {
		t.Fatal("expected upload error with wrong credentials")
	}
}

func TestNewArtifactStorageAliyun(t *testing.T) {
	// aliyun without credentials -> error.
	if _, err := NewArtifactStorage(Config{ArtifactStorageType: "aliyun"}); err == nil {
		t.Fatal("expected error for aliyun without credentials")
	}

	// aliyun with credentials -> backend with defaulted product/repo/base.
	cfg := Config{
		ArtifactStorageType: "aliyun",
		AliyunUsername:      "u", AliyunPassword: "p",
	}
	got, err := NewArtifactStorage(cfg)
	if err != nil || got.Name() != "aliyun" {
		t.Fatalf("aliyun factory: name=%q err=%v", nameOr(got), err)
	}
	as, ok := got.(*aliyunPackagesStorage)
	if !ok {
		t.Fatalf("expected *aliyunPackagesStorage, got %T", got)
	}
	if as.productID != defaultAliyunProductID || as.repo != defaultAliyunRepo || as.baseURL != defaultAliyunBaseURL {
		t.Fatalf("defaults not applied: %+v", as)
	}
}
