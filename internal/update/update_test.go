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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExtractArchiveToStreamsAndBounds(t *testing.T) {
	var compressed bytes.Buffer
	zw := zip.NewWriter(&compressed)
	entry, err := zw.Create("focusgate.exe")
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("x"), 2<<20)
	if _, err := entry.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "archive-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(compressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if n, err := extractArchiveTo(context.Background(), file, "release.zip", &output); err != nil || n != int64(len(content)) || !bytes.Equal(output.Bytes(), content) {
		t.Fatalf("stream extraction failed: %d (%v)", n, err)
	}
	output.Reset()
	if _, err := copyBounded(context.Background(), &output, bytes.NewReader(content), 16); err == nil || output.Len() > 16 {
		t.Fatal("stream copy exceeded its bound")
	}
}

func TestIsNewer(t *testing.T) {
	tests := []struct {
		remote  string
		current string
		want    bool
	}{
		{"v1.0.1", "1.0.0", true},
		{"1.1.0", "1.0.9", true},
		{"v2.0.0", "1.9.9", true},
		{"v1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"v1.0.0-rc1", "1.0.0", false},
		{"v1.0.0", "1.0.0-rc1", true},
		{"v1.0.0-rc2", "1.0.0-rc1", true},
		{"v1.0.0-rc1", "1.0.0-rc2", false},
		{"1.0.0-rc.10", "1.0.0-rc.2", true},
		{"1.0.0-rc.2", "1.0.0-rc.10", false},
		{"1.0.0+build1", "1.0.0+build2", false},
		{"1.0.1+build1", "1.0.0+build2", true},
		{"v1.0.0", "dev", true},
		{"v1.0.1", "v1.0.1", false},
		{"", "1.0.0", false},
		{"v1.0.0.1", "1.0.0", true},
	}

	for _, tt := range tests {
		got := IsNewer(tt.remote, tt.current)
		if got != tt.want {
			t.Errorf("IsNewer(%q, %q) = %v; want %v", tt.remote, tt.current, got, tt.want)
		}
	}
}

func TestClient_Check(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/testowner/testrepo/releases/latest" {
			http.NotFound(w, r)
			return
		}

		suffix := targetArchiveSuffix(runtime.GOOS, runtime.GOARCH)
		rel := githubRelease{
			ID:          7,
			TagName:     "v2.0.0",
			PublishedAt: "2026-11-13T12:00:00Z",
			Body:        "Major release with new features",
			Assets: []githubAsset{
				{
					ID:                 42,
					Name:               "focusgate-v2.0.0-" + suffix,
					BrowserDownloadURL: "https://example.com/download/" + suffix,
					Size:               15000000,
				},
				{
					Name:               "checksums.txt",
					BrowserDownloadURL: "https://example.com/checksums.txt",
					Size:               200,
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	}))
	defer server.Close()

	client := &Client{
		Repo:       "testowner/testrepo",
		HTTPClient: server.Client(),
	}

	// Override check URL by testing using custom roundtripper or path
	transport := server.Client().Transport
	client.HTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req.URL.Scheme = "http"
			req.URL.Host = server.Listener.Addr().String()
			return transport.RoundTrip(req)
		}),
	}

	res, err := client.Check(context.Background())
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}

	if !res.HasUpdate {
		t.Errorf("expected HasUpdate = true for v2.0.0 vs current, got false")
	}
	if res.LatestVersion != "v2.0.0" {
		t.Errorf("expected LatestVersion v2.0.0, got %s", res.LatestVersion)
	}
	if res.DownloadURL == "" {
		t.Errorf("expected non-empty DownloadURL for matching asset")
	}
	if res.ChecksumURL == "" {
		t.Errorf("expected non-empty ChecksumURL for checksums.txt")
	}
	if res.AssetID != 42 || !strings.HasPrefix(res.ReleaseIdentity, "7:42:") {
		t.Fatalf("checked asset identity lost: %+v", res)
	}
}

func TestExtractArchiveToZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// Add preceding directory entry
	if _, err := zw.Create("dist/"); err != nil {
		t.Fatalf("create zip dir entry: %v", err)
	}

	binData := bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 300000)
	f, err := zw.Create("dist/focusgate.exe")
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := f.Write(binData); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}

	extracted := extractFixture(t, buf.Bytes(), "release.zip")
	if !bytes.Equal(extracted, binData) {
		t.Errorf("extracted binary does not match original bytes")
	}
}

func TestExtractArchiveToTarGz(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	// Add preceding directory entry
	dirHdr := &tar.Header{
		Name:     "dist/",
		Mode:     0755,
		Typeflag: tar.TypeDir,
	}
	if err := tw.WriteHeader(dirHdr); err != nil {
		t.Fatalf("write tar dir header: %v", err)
	}

	binData := bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 300000)
	hdr := &tar.Header{
		Name:     "dist/focusgate",
		Mode:     0755,
		Typeflag: tar.TypeReg,
		Size:     int64(len(binData)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(binData); err != nil {
		t.Fatalf("write tar content: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	extracted := extractFixture(t, buf.Bytes(), "release.tar.gz")
	if !bytes.Equal(extracted, binData) {
		t.Errorf("extracted binary does not match original bytes")
	}
}

func extractFixture(t *testing.T, data []byte, name string) []byte {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "archive-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := extractArchiveTo(context.Background(), f, name, &output); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestApplyBindsDownloadToCheckedAsset(t *testing.T) {
	original := []byte("reviewed upload")
	replacement := []byte("same-name replacement")
	manifest := replacement
	var paths []string
	c := NewClient("owner/repo")
	c.HTTPClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		var body string
		switch req.URL.Path {
		case "/repos/owner/repo/releases/assets/42":
			if req.Header.Get("Accept") != "application/octet-stream" {
				t.Error("asset request did not select binary content")
			}
			body = string(original)
		case "/checksums.txt":
			body = fmt.Sprintf("%x  focusgate\n", sha256.Sum256(manifest))
		default:
			t.Errorf("used mutable browser URL: %s", req.URL)
			body = string(replacement)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	res := &CheckResult{AssetID: 42, DownloadURL: "https://example.com/focusgate", ChecksumURL: "https://example.com/checksums.txt"}
	if err := c.Apply(context.Background(), res); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("replacement manifest accepted: %v", err)
	}
	manifest = original
	if err := c.Apply(context.Background(), res); err == nil || !strings.Contains(err.Error(), "unexpectedly small") {
		t.Fatalf("checked bytes did not pass verification: %v", err)
	}
	if len(paths) != 4 {
		t.Fatalf("unexpected asset requests: %v", paths)
	}
	res.AssetID = 0
	if err := c.Apply(context.Background(), res); err == nil {
		t.Fatal("unbound asset accepted")
	}
}

type slowWriter struct{ delay time.Duration }

func (w slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return len(p), nil
}

func TestAssetReadIdleExcludesDiskWrites(t *testing.T) {
	c := NewClient("")
	c.HTTPClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("asset")), Request: req}, nil
	})
	if err := c.downloadArchiveWithIdle(context.Background(), slowWriter{100 * time.Millisecond}, "https://example.com/asset", 20*time.Millisecond); err != nil {
		t.Fatalf("disk write counted as network inactivity: %v", err)
	}
}

func TestAssetWrappedTransportRetainsHeaderTimeout(t *testing.T) {
	c := NewClient("")
	c.HTTPClient.Timeout = 30 * time.Millisecond
	c.HTTPClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	err := c.downloadArchive(ctx, io.Discard, "https://example.com/no-headers")
	if err == nil || time.Since(start) >= 500*time.Millisecond || ctx.Err() != nil {
		t.Fatalf("wrapped transport lost configured timeout: %v (%s)", err, time.Since(start))
	}
}

func TestVerifyChecksum(t *testing.T) {
	dummyData := []byte("hello world focusgate binary content")
	hash := sha256.Sum256(dummyData)
	hashHex := hex.EncodeToString(hash[:])

	checksumFileContent := fmt.Sprintf("%s  focusgate-v1.0.1-linux-amd64.tar.gz\n", hashHex)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(checksumFileContent))
	}))
	defer server.Close()

	client := &Client{
		HTTPClient: server.Client(),
	}

	err := client.verifyChecksum(
		context.Background(),
		server.URL+"/checksums.txt",
		"focusgate-v1.0.1-linux-amd64.tar.gz",
		bytes.NewReader(dummyData),
	)
	if err != nil {
		t.Errorf("expected checksum to verify cleanly, got: %v", err)
	}

	// Missing asset in checksums.txt should return error
	errMissing := client.verifyChecksum(
		context.Background(),
		server.URL+"/checksums.txt",
		"focusgate-v1.0.1-darwin-amd64.tar.gz",
		bytes.NewReader(dummyData),
	)
	if errMissing == nil {
		t.Errorf("expected error for missing asset in checksums.txt, got nil")
	}
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestWindowsReplacementReportsRetainedRecoveryCopy(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "focusgate.exe")
	pending := filepath.Join(dir, "pending.exe")
	if err := os.WriteFile(current, []byte("working executable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte("new executable"), 0600); err != nil {
		t.Fatal(err)
	}
	err := replaceWindowsExecutable(current, pending, func(from, to string) error {
		if from == pending {
			return errors.New("installation denied")
		}
		if from == current+".old" {
			return errors.New("restoration denied")
		}
		return os.Rename(from, to)
	})
	if err == nil || !strings.Contains(err.Error(), "installation denied") || !strings.Contains(err.Error(), "restoration denied") || !strings.Contains(err.Error(), current+".old") {
		t.Fatalf("lost recovery information: %v", err)
	}
	data, readErr := os.ReadFile(current + ".old")
	if readErr != nil || string(data) != "working executable" {
		t.Fatalf("recovery binary lost: %q %v", data, readErr)
	}
}

func TestAssetTransferHasIndependentInactivityBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stall" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Write([]byte("release-asset"))
	}))
	defer server.Close()
	client := NewClient("")
	client.HTTPClient.Timeout = time.Nanosecond
	var data bytes.Buffer
	if err := client.downloadArchive(context.Background(), &data, server.URL+"/asset"); err != nil {
		t.Fatal(err)
	}
	if data.String() != "release-asset" {
		t.Fatal("asset body lost")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.downloadArchiveWithIdle(ctx, &data, server.URL+"/stall", 100*time.Millisecond); !errors.Is(err, errTransferIdle) {
		t.Fatalf("stalled body not bounded by inactivity: %v", err)
	}
	if err := client.downloadArchiveWithIdle(ctx, &data, server.URL, 0); err == nil {
		t.Fatal("zero inactivity budget accepted")
	}
}
