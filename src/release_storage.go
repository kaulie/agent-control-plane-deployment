package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// releaseAssetName is the single tar.gz asset attached to each deployment
// release. The release is created on the service's own git repo, tagged
// deployment-<hash>; the asset contains the build outputs + VERSION/COMMIT
// metadata so a deploy can restore the exact package on any host.
const releaseAssetName = "package.tar.gz"

var ghHTTPClient = &http.Client{Timeout: 10 * time.Minute}

// ghAPIBase / ghUploadBase are package vars (overridable in tests) pointing
// at the GitHub REST and upload endpoints.
var (
	ghAPIBase   = "https://api.github.com"
	ghUploadBase = "https://uploads.github.com"
)

// parseRepoOwnerName extracts "owner/repo" from a GitHub git URL, accepting
// https://github.com/O/R[.git] and git@github.com:O/R[.git] forms.
func parseRepoOwnerName(gitRepoURL string) (owner, repo string, err error) {
	s := strings.TrimSpace(gitRepoURL)
	if s == "" {
		return "", "", fmt.Errorf("empty git repo url")
	}
	if strings.HasPrefix(s, "git@") {
		rest := strings.TrimPrefix(s, "git@")
		colon := strings.IndexByte(rest, ':')
		if colon < 0 {
			return "", "", fmt.Errorf("invalid ssh url: %s", gitRepoURL)
		}
		return splitOwnerRepo(rest[colon+1:])
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", "", fmt.Errorf("invalid url %q: %w", gitRepoURL, err)
	}
	return splitOwnerRepo(u.Path)
}

func splitOwnerRepo(path string) (string, string, error) {
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("cannot parse owner/repo from %q", path)
	}
	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	if repo == "" {
		return "", "", fmt.Errorf("empty repo in %q", path)
	}
	return owner, repo, nil
}

type ghRelease struct {
	ID     int64     `json:"id"`
	TagName string    `json:"tag_name"`
	Assets []ghAsset `json:"assets"`
}

type ghAsset struct {
	ID                  int64  `json:"id"`
	Name                string `json:"name"`
	URL                 string `json:"url"`
	BrowserDownloadURL  string `json:"browser_download_url"`
}

func ghDo(ctx context.Context, token, method, url string, body io.Reader, contentType, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return ghHTTPClient.Do(req)
}

// releaseAssetExists reports whether release <tag> exists on the service repo
// AND has an asset named package.tar.gz.
func releaseAssetExists(ctx context.Context, token, gitRepoURL, tag string) (bool, error) {
	owner, repo, err := parseRepoOwnerName(gitRepoURL)
	if err != nil {
		return false, err
	}
	u := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", ghAPIBase, owner, repo, tag)
	resp, err := ghDo(ctx, token, http.MethodGet, u, nil, "", "application/vnd.github+json")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("github get release %s: %s", tag, resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return false, err
	}
	for _, a := range rel.Assets {
		if a.Name == releaseAssetName {
			return true, nil
		}
	}
	return false, nil
}

// uploadPackageToRelease tars pkgDir and uploads it as the package.tar.gz
// asset of release <tag> on the service repo, creating the release if needed.
// If an asset with the same name already exists it is deleted first (GitHub
// does not allow overwriting assets).
func uploadPackageToRelease(ctx context.Context, token, gitRepoURL, tag, pkgDir string) error {
	owner, repo, err := parseRepoOwnerName(gitRepoURL)
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("GITHUB_TOKEN not set; cannot upload release asset")
	}

	// Prepare tar.gz in memory (packages are modestly sized).
	var buf bytes.Buffer
	if err := tarDir(pkgDir, &buf); err != nil {
		return fmt.Errorf("tar package: %w", err)
	}

	rel, err := ghGetOrCreateRelease(ctx, token, owner, repo, tag)
	if err != nil {
		return err
	}
	// Remove existing asset of the same name (no overwrite).
	for _, a := range rel.Assets {
		if a.Name == releaseAssetName {
			delURL := fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d", ghAPIBase, owner, repo, a.ID)
			dresp, derr := ghDo(ctx, token, http.MethodDelete, delURL, nil, "", "application/vnd.github+json")
			if derr != nil {
				return fmt.Errorf("delete old asset: %w", derr)
			}
			_ = dresp.Body.Close()
			if dresp.StatusCode >= 300 {
				return fmt.Errorf("delete old asset: %s", dresp.Status)
			}
			break
		}
	}

	// Upload asset. GitHub requires a Content-Type for the asset body; the
	// Accept header stays the standard API media type.
	uploadURL := fmt.Sprintf("%s/repos/%s/%s/releases/%d/assets?name=%s",
		ghUploadBase, owner, repo, rel.ID, releaseAssetName)
	resp, err := ghDo(ctx, token, http.MethodPost, uploadURL, &buf, "application/octet-stream", "application/vnd.github+json")
	if err != nil {
		return fmt.Errorf("upload asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("upload asset: %s: %s", resp.Status, string(b))
	}
	return nil
}

func ghGetOrCreateRelease(ctx context.Context, token, owner, repo, tag string) (*ghRelease, error) {
	getURL := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", ghAPIBase, owner, repo, tag)
	resp, err := ghDo(ctx, token, http.MethodGet, getURL, nil, "", "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		var rel ghRelease
		err = json.NewDecoder(resp.Body).Decode(&rel)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		return &rel, nil
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		return nil, fmt.Errorf("get release %s: %s", tag, resp.Status)
	}
	// Create release.
	body := fmt.Sprintf(`{"tag_name":%q,"name":%q,"body":"deployment package","prerelease":false}`, tag, tag)
	cresp, err := ghDo(ctx, token, http.MethodPost,
		fmt.Sprintf("%s/repos/%s/%s/releases", ghAPIBase, owner, repo),
		strings.NewReader(body), "application/json", "application/vnd.github+json")
	if err != nil {
		return nil, fmt.Errorf("create release: %w", err)
	}
	defer cresp.Body.Close()
	if cresp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(cresp.Body, 4096))
		return nil, fmt.Errorf("create release: %s: %s", cresp.Status, string(b))
	}
	var rel ghRelease
	if err := json.NewDecoder(cresp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// downloadPackageFromRelease downloads the package.tar.gz asset of release
// <tag> from the service repo and extracts it into destDir.
func downloadPackageFromRelease(ctx context.Context, token, gitRepoURL, tag, destDir string) error {
	owner, repo, err := parseRepoOwnerName(gitRepoURL)
	if err != nil {
		return err
	}
	getURL := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", ghAPIBase, owner, repo, tag)
	resp, err := ghDo(ctx, token, http.MethodGet, getURL, nil, "", "application/vnd.github+json")
	if err != nil {
		return fmt.Errorf("get release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("release not found for tag %s on %s/%s", tag, owner, repo)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get release %s: %s", tag, resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return err
	}
	var assetURL string
	for _, a := range rel.Assets {
		if a.Name == releaseAssetName {
			assetURL = a.URL
			break
		}
	}
	if assetURL == "" {
		return fmt.Errorf("release %s has no asset %s", tag, releaseAssetName)
	}

	aresp, err := ghDo(ctx, token, http.MethodGet, assetURL, nil, "", "application/octet-stream")
	if err != nil {
		return fmt.Errorf("download asset: %w", err)
	}
	defer aresp.Body.Close()
	if aresp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(aresp.Body, 4096))
		return fmt.Errorf("download asset: %s: %s", aresp.Status, string(b))
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	return untarGz(aresp.Body, destDir)
}

// tarDir writes a gzip+tar of dir's contents (paths relative to dir) to w.
func tarDir(dir string, w io.Writer) error {
	gw := gzip.NewWriter(w)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Symlinks: tar.FileInfoHeader sets TypeSymlink with Size=0 but an empty
		// Linkname. If we then os.Open (which follows the link) and copy the
		// target's bytes, the writer overflows the 0-size header with
		// "archive/tar: write too long". Fill Linkname from os.Readlink and write
		// no content, mirroring how `tar`/`untarGz` handle symlinks.
		if info.Mode()&os.ModeSymlink != 0 {
			link, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			hdr, herr := tar.FileInfoHeader(info, link)
			if herr != nil {
				return herr
			}
			hdr.Name = rel
			hdr.Linkname = link
			return tw.WriteHeader(hdr)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		// Close immediately rather than deferring: Walk visits many files and
		// deferred closes would accumulate until tarDir returns, leaking FDs.
		_, copyErr := io.Copy(tw, f)
		_ = f.Close()
		return copyErr
	})
}

// untarGz extracts a gzip+tar stream into destDir.
func untarGz(r io.Reader, destDir string) error {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Prevent path traversal.
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("unsafe tar entry: %s", hdr.Name)
		}
		target := filepath.Join(destDir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			_ = f.Close()
		case tar.TypeSymlink:
			_ = os.Symlink(hdr.Linkname, target) // best-effort
		}
	}
}
