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
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

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
			TagName:     "v2.0.0",
			PublishedAt: "2026-11-13T12:00:00Z",
			Body:        "Major release with new features",
			Assets: []githubAsset{
				{
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
}

func TestExtractZipBinary(t *testing.T) {
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

	extracted, err := extractZipBinary(buf.Bytes())
	if err != nil {
		t.Fatalf("extractZipBinary error: %v", err)
	}
	if !bytes.Equal(extracted, binData) {
		t.Errorf("extracted binary does not match original bytes")
	}
}

func TestExtractTarGzBinary(t *testing.T) {
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

	extracted, err := extractTarGzBinary(context.Background(), buf.Bytes())
	if err != nil {
		t.Fatalf("extractTarGzBinary error: %v", err)
	}
	if !bytes.Equal(extracted, binData) {
		t.Errorf("extracted binary does not match original bytes")
	}
}

func TestVerifyChecksum(t *testing.T) {
	dummyData := []byte("hello world focusgate binary content")
	hash := sha256.Sum256(dummyData)
	hashHex := hex.EncodeToString(hash[:])

	checksumFileContent := fmt.Sprintf("%s  focusgate-v1.0.1-linux-amd64.tar.gz\n", hashHex)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		dummyData,
	)
	if err != nil {
		t.Errorf("expected checksum to verify cleanly, got: %v", err)
	}

	// Missing asset in checksums.txt should return error
	errMissing := client.verifyChecksum(
		context.Background(),
		server.URL+"/checksums.txt",
		"focusgate-v1.0.1-darwin-amd64.tar.gz",
		dummyData,
	)
	if errMissing == nil {
		t.Errorf("expected error for missing asset in checksums.txt, got nil")
	}
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
