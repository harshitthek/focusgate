// Package update provides self-update inspection and binary replacement.
package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"focusgate/internal/version"
)

const (
	defaultRepo   = "harshitthek/focusgate"
	maxAssetBytes = 64 * 1024 * 1024 // 64 MB max download safeguard
)

// Client handles checking and applying software updates.
type Client struct {
	Repo       string
	HTTPClient *http.Client
}

// NewClient returns an update client targeting the specified GitHub repo (or default).
func NewClient(repo string) *Client {
	if repo == "" {
		repo = defaultRepo
	}
	return &Client{
		Repo: repo,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				if req.URL.Scheme != "https" && !isLoopbackHost(req.URL.Host) {
					return fmt.Errorf("redirect to non-HTTPS scheme %s rejected", req.URL.Scheme)
				}
				return nil
			},
		},
	}
}

func isLoopbackHost(host string) bool {
	h := host
	if strings.Contains(host, ":") {
		if hostOnly, _, err := net.SplitHostPort(host); err == nil {
			h = hostOnly
		}
	}
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

// CheckResult contains update status and asset download details.
type CheckResult struct {
	ReleaseIdentity string `json:"release_identity,omitempty"`
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version"`
	HasUpdate       bool   `json:"has_update"`
	ReleaseNotes    string `json:"release_notes"`
	PublishedAt     string `json:"published_at"`
	DownloadURL     string `json:"download_url,omitempty"`
	AssetSize       int64  `json:"asset_size,omitempty"`
	AssetID         int64  `json:"asset_id,omitempty"`
	ChecksumURL     string `json:"checksum_url,omitempty"`
}

type githubRelease struct {
	ID          int64         `json:"id"`
	TagName     string        `json:"tag_name"`
	PublishedAt string        `json:"published_at"`
	Body        string        `json:"body"`
	Assets      []githubAsset `json:"assets"`
}

type githubAsset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Check queries GitHub Releases for the latest release and matches the current platform.
func (c *Client) Check(ctx context.Context) (*CheckResult, error) {
	current := version.Get()
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", c.Repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating update request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "FocusGate-Updater")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("checking for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return &CheckResult{
			CurrentVersion: current.Version,
			LatestVersion:  current.Version,
			HasUpdate:      false,
			ReleaseNotes:   "No releases published yet.",
		}, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api returned status %d", resp.StatusCode)
	}

	var rel githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parsing release json: %w", err)
	}

	res := &CheckResult{
		CurrentVersion: current.Version,
		LatestVersion:  rel.TagName,
		ReleaseNotes:   rel.Body,
		PublishedAt:    rel.PublishedAt,
		HasUpdate:      IsNewer(rel.TagName, current.Version),
	}

	targetAssetSuffix := targetArchiveSuffix(runtime.GOOS, runtime.GOARCH)
	for _, asset := range rel.Assets {
		if strings.HasSuffix(asset.Name, targetAssetSuffix) {
			res.DownloadURL = asset.BrowserDownloadURL
			res.AssetSize = asset.Size
			res.AssetID = asset.ID
			res.ReleaseIdentity = fmt.Sprintf("%d:%d:%s:%s", rel.ID, asset.ID, rel.TagName, rel.PublishedAt)
		} else if asset.Name == "checksums.txt" {
			res.ChecksumURL = asset.BrowserDownloadURL
		}
	}

	return res, nil
}

// Apply downloads a checked archive and replaces the executable, retaining Windows recovery backups.
func (c *Client) Apply(ctx context.Context, res *CheckResult) error {
	if res.DownloadURL == "" {
		return errors.New("no compatible release binary found for current operating system and architecture")
	}

	parsedURL, err := url.Parse(res.DownloadURL)
	if err != nil || (parsedURL.Scheme != "https" && !isLoopbackHost(parsedURL.Host)) {
		return fmt.Errorf("invalid or non-HTTPS download url: %s", res.DownloadURL)
	}

	if res.ChecksumURL == "" {
		return errors.New("release checksum manifest is required")
	}
	if res.AssetID <= 0 {
		return errors.New("checked release asset ID is required")
	}
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating current executable: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("resolving executable symlink: %w", err)
	}
	// Check replacement access before spending network or decompression work.
	tmpFile, err := os.CreateTemp(filepath.Dir(execPath), ".focusgate-update-*.tmp")
	if err != nil {
		return fmt.Errorf("executable directory is read-only; install the update through the service operator: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = tmpFile.Close(); _ = os.Remove(tmpPath) }()

	archive, err := os.CreateTemp("", ".focusgate-archive-*")
	if err != nil {
		return err
	}
	defer func() { _ = archive.Close(); _ = os.Remove(archive.Name()) }()
	// Asset IDs identify an upload; a same-name replacement cannot change this download.
	assetURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/assets/%d", c.Repo, res.AssetID)
	if err := c.downloadArchive(ctx, archive, assetURL); err != nil {
		return err
	}

	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := c.verifyChecksum(ctx, res.ChecksumURL, filepath.Base(parsedURL.Path), archive); err != nil {
		return fmt.Errorf("checksum validation failed: %w", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	size, err := extractArchiveTo(ctx, archive, strings.ToLower(parsedURL.Path), tmpFile)
	if err != nil {
		return fmt.Errorf("extracting executable: %w", err)
	}
	if size < 1024*1024 {
		return fmt.Errorf("downloaded binary size (%d bytes) is unexpectedly small", size)
	}
	if err := tmpFile.Chmod(0755); err != nil {
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	// 6. In-place replacement
	if runtime.GOOS == "windows" {
		if err := replaceWindowsExecutable(execPath, tmpPath, os.Rename); err != nil {
			return err
		}
	} else {
		if err := os.Rename(tmpPath, execPath); err != nil {
			return fmt.Errorf("replacing binary: %w", err)
		}
	}

	return nil
}

func (c *Client) verifyChecksum(ctx context.Context, checksumURL, expectedFileName string, data io.Reader) error {
	parsedCheckURL, err := url.Parse(checksumURL)
	if err != nil || (parsedCheckURL.Scheme != "https" && !isLoopbackHost(parsedCheckURL.Host)) {
		return fmt.Errorf("invalid or non-HTTPS checksum url: %s", checksumURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching checksums.txt returned %d", resp.StatusCode)
	}

	body, err := readBounded(resp.Body, 1024*1024)
	if err != nil {
		return err
	}

	digest := sha256.New()
	if _, err := copyBounded(ctx, digest, data, maxAssetBytes); err != nil {
		return err
	}
	computedHex := hex.EncodeToString(digest.Sum(nil))

	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			expectedHash := strings.ToLower(fields[0])
			assetName := filepath.Base(fields[1])
			if assetName == expectedFileName {
				if expectedHash != computedHex {
					return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, computedHex)
				}
				return nil
			}
		}
	}

	return fmt.Errorf("asset %s not found in checksums.txt", expectedFileName)
}

// copyBounded streams with a fixed buffer and checks cancellation and total bytes.
func copyBounded(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	r := io.LimitReader(src, limit+1)
	buf := make([]byte, 32<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := r.Read(buf)
		if total+int64(n) > limit {
			return total, errors.New("input exceeds size limit")
		}
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

func extractArchiveTo(ctx context.Context, archive *os.File, name string, dst io.Writer) (int64, error) {
	if strings.HasSuffix(name, ".zip") {
		info, err := archive.Stat()
		if err != nil {
			return 0, err
		}
		z, err := zip.NewReader(archive, info.Size())
		if err != nil {
			return 0, err
		}
		for _, entry := range z.File {
			base := filepath.Base(entry.Name)
			if entry.Mode().IsRegular() && (base == "focusgate.exe" || base == "focusgate") {
				r, err := entry.Open()
				if err != nil {
					return 0, err
				}
				defer r.Close()
				return copyBounded(ctx, dst, r, maxAssetBytes)
			}
		}
		return 0, errors.New("focusgate executable not found in zip")
	}
	if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") {
		gz, err := gzip.NewReader(archive)
		if err != nil {
			return 0, err
		}
		defer gz.Close()
		tr := tar.NewReader(io.LimitReader(gz, maxTotalDecompressedBytes+1))
		for {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			entry, err := tr.Next()
			if err != nil {
				if err == io.EOF {
					return 0, errors.New("focusgate executable not found in tar.gz")
				}
				return 0, err
			}
			base := filepath.Base(entry.Name)
			if entry.Typeflag == tar.TypeReg && (base == "focusgate" || base == "focusgate.exe" || base == "focusgate-android") {
				return copyBounded(ctx, dst, tr, maxAssetBytes)
			}
		}
	}
	return copyBounded(ctx, dst, archive, maxAssetBytes)
}

func targetArchiveSuffix(targetOS, targetArch string) string {
	if targetOS == "windows" {
		return fmt.Sprintf("%s-%s.zip", targetOS, targetArch)
	}
	return fmt.Sprintf("%s-%s.tar.gz", targetOS, targetArch)
}

const maxTotalDecompressedBytes = 128 * 1024 * 1024 // 128 MB archive bomb protection

func readBounded(r io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("extracted binary exceeds maximum size limit of %d bytes", maxBytes)
	}
	return data, nil
}

// IsNewer compares semantic version tags (e.g. "v1.0.1" vs "1.0.0", "v1.0.0" vs "1.0.0-rc1").
func IsNewer(remote, current string) bool {
	rClean := strings.TrimPrefix(strings.TrimSpace(remote), "v")
	cClean := strings.TrimPrefix(strings.TrimSpace(current), "v")

	if rClean == "" {
		return false
	}
	if cClean == "dev" || cClean == "none" || cClean == "unknown" {
		return true
	}
	if rClean == cClean {
		return false
	}

	// Remove build metadata (e.g. +build123) before SemVer comparison
	rClean, _, _ = strings.Cut(rClean, "+")
	cClean, _, _ = strings.Cut(cClean, "+")
	if rClean == cClean {
		return false
	}

	rBase, rPre, rHasPre := strings.Cut(rClean, "-")
	cBase, cPre, cHasPre := strings.Cut(cClean, "-")

	rParts := strings.Split(rBase, ".")
	cParts := strings.Split(cBase, ".")
	maxLen := len(rParts)
	if len(cParts) > maxLen {
		maxLen = len(cParts)
	}

	for i := 0; i < maxLen; i++ {
		var rNum, cNum int
		if i < len(rParts) {
			rNum, _ = strconv.Atoi(cleanVersionNumber(rParts[i]))
		}
		if i < len(cParts) {
			cNum, _ = strconv.Atoi(cleanVersionNumber(cParts[i]))
		}
		if rNum > cNum {
			return true
		}
		if rNum < cNum {
			return false
		}
	}

	// Numeric base parts match: compare prerelease semantics
	if cHasPre && !rHasPre {
		// A stable release (1.0.0) is newer than any prerelease (1.0.0-rc1)
		return true
	}
	if !cHasPre && rHasPre {
		// A prerelease (1.0.0-rc1) is older than a stable release (1.0.0)
		return false
	}
	if cHasPre && rHasPre {
		return comparePrereleases(rPre, cPre) > 0
	}

	return false
}

func comparePrereleases(rPre, cPre string) int {
	if rPre == cPre {
		return 0
	}
	rIDs := strings.Split(rPre, ".")
	cIDs := strings.Split(cPre, ".")
	maxLen := len(rIDs)
	if len(cIDs) > maxLen {
		maxLen = len(cIDs)
	}

	for i := 0; i < maxLen; i++ {
		if i >= len(rIDs) {
			return -1
		}
		if i >= len(cIDs) {
			return 1
		}

		rID := rIDs[i]
		cID := cIDs[i]
		rNum, rIsNum := parseNumericID(rID)
		cNum, cIsNum := parseNumericID(cID)

		if rIsNum && cIsNum {
			if rNum != cNum {
				if rNum > cNum {
					return 1
				}
				return -1
			}
		} else if rIsNum && !cIsNum {
			return -1
		} else if !rIsNum && cIsNum {
			return 1
		} else {
			if rID != cID {
				if rID > cID {
					return 1
				}
				return -1
			}
		}
	}
	return 0
}

func parseNumericID(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	val, err := strconv.Atoi(s)
	return val, err == nil
}

func cleanVersionNumber(part string) string {
	var b strings.Builder
	for _, r := range part {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			break
		}
	}
	if b.Len() == 0 {
		return "0"
	}
	return b.String()
}

// Windows executable replacement has a recoverable backup between its two renames.
func replaceWindowsExecutable(execPath, tmpPath string, rename func(string, string) error) error {
	oldPath := execPath + ".old"
	_ = os.Remove(oldPath)
	if err := rename(execPath, oldPath); err != nil {
		return fmt.Errorf("renaming active binary to .old: %w", err)
	}
	if err := rename(tmpPath, execPath); err != nil {
		if restoreErr := rename(oldPath, execPath); restoreErr != nil {
			return fmt.Errorf("install failed: %w; restoring executable failed: %v; recovery binary retained at %s", err, restoreErr, oldPath)
		}
		return fmt.Errorf("moving new binary into place (original restored): %w", err)
	}
	// Keep .old until the next explicit update. An interruption between renames
	// leaves this recovery executable available for an operator to restore.
	return nil
}
