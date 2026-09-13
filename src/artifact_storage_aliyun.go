package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Defaults for the Aliyun "packages" generic repo holding deployment
// artifacts. The product ID and repo name are not secret (they appear in the
// API path); credentials are supplied via env and never hardcoded.
const (
	defaultAliyunBaseURL   = "https://packages.aliyun.com"
	defaultAliyunProductID = "6a1940346e68a85a0d176340"
	defaultAliyunRepo      = "deployment-artifact"
)

// aliyunPackagesStorage stores build packages in an Aliyun "packages" (制品仓库)
// generic repository over its REST protocol:
//
//	POST {base}/api/protocol/{productId}/generic/{repo}/files/{filePath}
//	     ?version=&fileName=&downloadFileName=&versionDescription=
//	GET  {base}/api/protocol/{productId}/generic/{repo}/files/{filePath}?version=
//
// Auth is HTTP basic. Each package is stored at
// <serviceId>/<tag>/package.tar.gz with version=<tag>, so the access path
// (Artifact.AssetURL recorded in the local artifacts table) is the
// authenticated GET URL and a deploy re-downloads it using the configured
// credentials. Credentials come from config (env) and are never persisted.
type aliyunPackagesStorage struct {
	baseURL   string
	productID string
	repo      string
	username  string
	password  string
	http      *http.Client
}

// aliyunHTTPClient is shared across requests; the upload is a single multipart
// POST and packages can be large, hence the generous timeout.
var aliyunHTTPClient = &http.Client{Timeout: 10 * time.Minute}

func (s *aliyunPackagesStorage) Name() string { return "aliyun" }

func (s *aliyunPackagesStorage) client() *http.Client {
	if s.http != nil {
		return s.http
	}
	return aliyunHTTPClient
}

// sanitizeAliyunSegment replaces characters that are unsafe in a URL path
// segment (the API explicitly rejects '&', '?' and spaces) with '_'.
func sanitizeAliyunSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// objectPath is the generic-repo file path (including the file name) for the
// package of <serviceId>/<tag>. Segments are escaped individually so '/' stays
// a separator while special characters are percent-encoded.
func (s *aliyunPackagesStorage) objectPath(serviceID, tag string) string {
	return "api/protocol/" + url.PathEscape(s.productID) +
		"/generic/" + url.PathEscape(s.repo) +
		"/files/" + url.PathEscape(sanitizeAliyunSegment(serviceID)) +
		"/" + url.PathEscape(sanitizeAliyunSegment(tag)) +
		"/" + url.PathEscape(releaseAssetName)
}

// uploadURL is the POST endpoint; filePath excludes the file name, which is
// supplied by the downloadFileName query parameter.
func (s *aliyunPackagesStorage) uploadURL(serviceID, tag string) string {
	return strings.TrimRight(s.baseURL, "/") + "/api/protocol/" + url.PathEscape(s.productID) +
		"/generic/" + url.PathEscape(s.repo) +
		"/files/" + url.PathEscape(sanitizeAliyunSegment(serviceID)) +
		"/" + url.PathEscape(sanitizeAliyunSegment(tag))
}

// downloadURL is the authenticated GET endpoint (the stored access path).
func (s *aliyunPackagesStorage) downloadURL(serviceID, tag string) string {
	return strings.TrimRight(s.baseURL, "/") + "/" + s.objectPath(serviceID, tag) +
		"?version=" + url.QueryEscape(tag)
}

type aliyunUploadResponse struct {
	Object struct {
		FileMD5    string `json:"fileMd5"`
		FileSHA1   string `json:"fileSha1"`
		FileSHA256 string `json:"fileSha256"`
		FileSize   int64  `json:"fileSize"`
		URL        string `json:"url"`
	} `json:"object"`
	Successful bool `json:"successful"`
}

// Exists probes the download endpoint: 200 means the version is present, 404
// absent. The body is closed without being read so a large package is not
// transferred just to test existence.
func (s *aliyunPackagesStorage) Exists(ctx context.Context, serviceID, gitRepoURL, tag string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.downloadURL(serviceID, tag), nil)
	if err != nil {
		return false, err
	}
	req.SetBasicAuth(s.username, s.password)
	resp, err := s.client().Do(req)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("aliyun exists check: %s", resp.Status)
	}
}

func (s *aliyunPackagesStorage) Upload(ctx context.Context, serviceID, gitRepoURL, tag, pkgDir string) (*ArtifactMeta, error) {
	var tarBuf bytes.Buffer
	if err := tarDir(pkgDir, &tarBuf); err != nil {
		return nil, fmt.Errorf("tar package: %w", err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", releaseAssetName)
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(tarBuf.Bytes()); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("version", tag)
	// The Aliyun API stores the object at <filePath>/<fileName> (empirically
	// verified), NOT <filePath>/<downloadFileName>. downloadFileName only sets
	// the browser download name. We therefore set fileName to the same
	// package.tar.gz name the Download path expects, otherwise the uploaded
	// object is stored as "package" and every later GET 404s.
	q.Set("fileName", releaseAssetName)
	q.Set("downloadFileName", releaseAssetName)
	q.Set("versionDescription", "deployment package "+strings.TrimPrefix(tag, "deployment-"))
	uploadURL := s.uploadURL(serviceID, tag) + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetBasicAuth(s.username, s.password)
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("aliyun upload: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("aliyun upload: %s: %s", resp.Status, string(raw))
	}
	var out aliyunUploadResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("aliyun upload: decode response: %w (%s)", err, string(raw))
	}
	if !out.Successful {
		return nil, fmt.Errorf("aliyun upload: unsuccessful: %s", string(raw))
	}
	size := out.Object.FileSize
	if size <= 0 {
		size = int64(tarBuf.Len())
	}
	return &ArtifactMeta{
		Storage:  "aliyun",
		RepoSlug: s.repo,
		// Durable, authenticated GET path (the temp URL returned by the API
		// expires, so it is not stored). BrowserDownloadURL is intentionally
		// left empty: downloads need the basic-auth credentials.
		AssetURL: s.downloadURL(serviceID, tag),
		Size:     size,
	}, nil
}

func (s *aliyunPackagesStorage) Download(ctx context.Context, serviceID, gitRepoURL, tag, destDir, accessPath string) error {
	u := strings.TrimSpace(accessPath)
	if u == "" {
		u = s.downloadURL(serviceID, tag)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(s.username, s.password)
	resp, err := s.client().Do(req)
	if err != nil {
		return fmt.Errorf("aliyun download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("aliyun artifact not found: %s", tag)
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("aliyun download: %s: %s", resp.Status, string(b))
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	return untarGz(resp.Body, destDir)
}

// List is not supported: the Aliyun generic-repo protocol used here exposes no
// version listing endpoint. Artifacts are indexed locally on upload, so the
// scan/backfill path is only meaningful for backends that can enumerate
// (GitHub Releases, local disk).
func (s *aliyunPackagesStorage) List(ctx context.Context, gitRepoURL string) ([]ReleaseScanItem, error) {
	return nil, fmt.Errorf("aliyun storage does not support listing; artifacts are indexed on upload")
}
