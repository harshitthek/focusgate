package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
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
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	HasUpdate      bool   `json:"has_update"`
	ReleaseNotes   string `json:"release_notes"`
	PublishedAt    string `json:"published_at"`
	DownloadURL    string `json:"download_url,omitempty"`
	AssetSize      int64  `json:"asset_size,omitempty"`
	ChecksumURL    string `json:"checksum_url,omitempty"`
}

type githubRelease struct {
	TagName     string        `json:"tag_name"`
	PublishedAt string        `json:"published_at"`
	Body        string        `json:"body"`
	Assets      []githubAsset `json:"assets"`
}

type githubAsset struct {
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
		} else if asset.Name == "checksums.txt" {
			res.ChecksumURL = asset.BrowserDownloadURL
		}
	}

	return res, nil
}

// Apply downloads the matching release archive, verifies checksum if available, and atomically updates the running executable.
func (c *Client) Apply(ctx context.Context, res *CheckResult) error {
	if res.DownloadURL == "" {
		return errors.New("no compatible release binary found for current operating system and architecture")
	}

	parsedURL, err := url.Parse(res.DownloadURL)
	if err != nil || (parsedURL.Scheme != "https" && !isLoopbackHost(parsedURL.Host)) {
		return fmt.Errorf("invalid or non-HTTPS download url: %s", res.DownloadURL)
	}

	// 1. Download release archive
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, res.DownloadURL, nil)
	if err != nil {
		return fmt.Errorf("creating download request: %w", err)
	}
	req.Header.Set("User-Agent", "FocusGate-Updater")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading release archive: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	archiveBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetBytes+1))
	if err != nil {
		return fmt.Errorf("reading release archive: %w", err)
	}
	if int64(len(archiveBytes)) > maxAssetBytes {
		return fmt.Errorf("downloaded archive exceeds maximum size limit of %d bytes", maxAssetBytes)
	}

	// 2. Verify SHA256 checksum if checksums.txt is available
	fileName := filepath.Base(parsedURL.Path)
	if res.ChecksumURL != "" {
		if err := c.verifyChecksum(ctx, res.ChecksumURL, fileName, archiveBytes); err != nil {
			return fmt.Errorf("checksum validation failed: %w", err)
		}
	}

	// 3. Extract binary from archive
	downloadPath := strings.ToLower(parsedURL.Path)
	var binaryBytes []byte
	if strings.HasSuffix(downloadPath, ".zip") {
		binaryBytes, err = extractZipBinary(archiveBytes)
	} else if strings.HasSuffix(downloadPath, ".tar.gz") || strings.HasSuffix(downloadPath, ".tgz") {
		binaryBytes, err = extractTarGzBinary(ctx, archiveBytes)
	} else {
		// Raw binary
		binaryBytes = archiveBytes
	}
	if err != nil {
		return fmt.Errorf("extracting executable from archive: %w", err)
	}

	if len(binaryBytes) < 1024*1024 {
		return fmt.Errorf("downloaded binary size (%d bytes) is unexpectedly small", len(binaryBytes))
	}

	// 4. Locate current executable path
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating current executable: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("resolving executable symlink: %w", err)
	}

	// 5. Write to temporary file adjacent to current binary
	dir := filepath.Dir(execPath)
	tmpFile, err := os.CreateTemp(dir, ".focusgate-update-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temporary executable: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	if _, err := tmpFile.Write(binaryBytes); err != nil {
		tmpFile.Close()
		return fmt.Errorf("writing updated binary: %w", err)
	}
	if err := tmpFile.Chmod(0755); err != nil {
		tmpFile.Close()
		return fmt.Errorf("setting executable permissions: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing updated binary: %w", err)
	}

	// 6. In-place replacement
	if runtime.GOOS == "windows" {
		oldPath := execPath + ".old"
		_ = os.Remove(oldPath)
		if err := os.Rename(execPath, oldPath); err != nil {
			return fmt.Errorf("renaming active binary to .old: %w", err)
		}
		if err := os.Rename(tmpPath, execPath); err != nil {
			_ = os.Rename(oldPath, execPath)
			return fmt.Errorf("moving new binary into place: %w", err)
		}
		_ = os.Remove(oldPath)
	} else {
		if err := os.Rename(tmpPath, execPath); err != nil {
			return fmt.Errorf("replacing binary: %w", err)
		}
	}

	return nil
}

func (c *Client) verifyChecksum(ctx context.Context, checksumURL, expectedFileName string, data []byte) error {
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return err
	}

	computedHash := sha256.Sum256(data)
	computedHex := hex.EncodeToString(computedHash[:])

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

func targetArchiveSuffix(targetOS, targetArch string) string {
	if targetOS == "windows" {
		return fmt.Sprintf("%s-%s.zip", targetOS, targetArch)
	}
	return fmt.Sprintf("%s-%s.tar.gz", targetOS, targetArch)
}

func extractZipBinary(zipData []byte) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, err
	}

	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(file.Name)
		if base == "focusgate.exe" || base == "focusgate" {
			rc, err := file.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return readBounded(rc, maxAssetBytes)
		}
	}
	return nil, errors.New("focusgate executable not found in zip archive")
}

const maxTotalDecompressedBytes = 128 * 1024 * 1024 // 128 MB archive bomb protection

func extractTarGzBinary(ctx context.Context, tarGzData []byte) ([]byte, error) {
	gzReader, err := gzip.NewReader(bytes.NewReader(tarGzData))
	if err != nil {
		return nil, err
	}
	defer gzReader.Close()

	// Guard against decompressed explosion
	boundedStream := io.LimitReader(gzReader, maxTotalDecompressedBytes+1)
	tarReader := tar.NewReader(boundedStream)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		base := filepath.Base(header.Name)
		if base == "focusgate" || base == "focusgate.exe" || base == "focusgate-android" {
			return readBounded(tarReader, maxAssetBytes)
		}
	}
	return nil, errors.New("focusgate executable not found in tar.gz archive")
}

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
	rIds := strings.Split(rPre, ".")
	cIds := strings.Split(cPre, ".")
	maxLen := len(rIds)
	if len(cIds) > maxLen {
		maxLen = len(cIds)
	}

	for i := 0; i < maxLen; i++ {
		if i >= len(rIds) {
			return -1
		}
		if i >= len(cIds) {
			return 1
		}

		rId := rIds[i]
		cId := cIds[i]
		rNum, rIsNum := parseNumericId(rId)
		cNum, cIsNum := parseNumericId(cId)

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
			if rId != cId {
				if rId > cId {
					return 1
				}
				return -1
			}
		}
	}
	return 0
}

func parseNumericId(s string) (int, bool) {
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
