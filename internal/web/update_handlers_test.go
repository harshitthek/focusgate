package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"focusgate/internal/store"
	"focusgate/internal/version"
)

func TestHandleVersion(t *testing.T) {
	srv, st := setupTestServer(t)
	defer st.Close()

	req := newLocalRequest(http.MethodGet, "/api/version", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var info version.Info
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("failed to decode version info: %v", err)
	}

	if info.Version != version.Version {
		t.Errorf("expected version %q, got %q", version.Version, info.Version)
	}
	if info.OS == "" || info.Arch == "" {
		t.Errorf("expected non-empty OS and Arch in version info, got %s/%s", info.OS, info.Arch)
	}
}

func TestHandleUpdateCheck_Auth(t *testing.T) {
	srv, st := setupTestServer(t)
	defer st.Close()

	// 1. Setup vault auth
	setupReq := newLocalRequest(http.MethodPost, "/api/auth/setup", bytes.NewBufferString(`{"passphrase":"master-pass-1234"}`))
	setupRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(setupRec, setupReq)
	if setupRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on setup, got %d: %s", setupRec.Code, setupRec.Body.String())
	}

	var setupResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(setupRec.Body.Bytes(), &setupResp); err != nil {
		t.Fatalf("unmarshal setup response: %v", err)
	}

	// 2. Unauthenticated request should be rejected (401)
	req := newLocalRequest(http.MethodGet, "/api/update/check", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401 for unauthenticated request, got %d", rec.Code)
	}

	// Controlled metadata makes this an update contract check without external requests.
	originalTransport := http.DefaultTransport
	http.DefaultTransport = reviewRoundTripper(func(r *http.Request) (*http.Response, error) {
		ext := ".tar.gz"
		if runtime.GOOS == "windows" {
			ext = ".zip"
		}
		body := fmt.Sprintf(`{"id":7,"tag_name":"v99.0.0","assets":[{"id":9,"name":"focusgate-v99.0.0-%s-%s%s","browser_download_url":"https://example.test/archive","size":10}]}`, runtime.GOOS, runtime.GOARCH, ext)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	// 3. Authenticated administrator gets the reviewed release identity.
	reqAuth := newLocalRequest(http.MethodGet, "/api/update/check", nil)
	reqAuth.Header.Set("Authorization", "Bearer "+setupResp.Token)
	recAuth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recAuth, reqAuth)

	var checked struct {
		ReleaseIdentity string `json:"release_identity"`
	}
	if recAuth.Code != http.StatusOK || json.Unmarshal(recAuth.Body.Bytes(), &checked) != nil || checked.ReleaseIdentity != "7:9:v99.0.0:" {
		t.Fatalf("expected reviewed identity, got %d: %s", recAuth.Code, recAuth.Body)
	}

	// 4. Register a regular user (Bob)
	bobRegReq := newLocalRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(`{"username":"bob","password":"bob-password-123"}`))
	bobRegRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(bobRegRec, bobRegReq)
	if bobRegRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on bob registration, got %d: %s", bobRegRec.Code, bobRegRec.Body.String())
	}
	var bobRegResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(bobRegRec.Body.Bytes(), &bobRegResp); err != nil {
		t.Fatalf("unmarshal bob response: %v", err)
	}

	// 5. Ordinary user accessing /api/update/check must be forbidden (403)
	bobCheckReq := newLocalRequest(http.MethodGet, "/api/update/check", nil)
	bobCheckReq.Header.Set("Authorization", "Bearer "+bobRegResp.Token)
	bobCheckRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(bobCheckRec, bobCheckReq)
	if bobCheckRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for ordinary user update check, got %d: %s", bobCheckRec.Code, bobCheckRec.Body.String())
	}

	// 6. Ordinary user accessing /api/update/apply must be forbidden (403)
	bobApplyReq := newLocalRequest(http.MethodPost, "/api/update/apply", nil)
	bobApplyReq.Header.Set("Authorization", "Bearer "+bobRegResp.Token)
	bobApplyRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(bobApplyRec, bobApplyReq)
	if bobApplyRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for ordinary user update apply, got %d: %s", bobApplyRec.Code, bobApplyRec.Body.String())
	}
}

type reviewRoundTripper func(*http.Request) (*http.Response, error)

func (f reviewRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpdateApplyRejectsOverlappingCheck(t *testing.T) {
	srv, st := setupTestServer(t)
	defer st.Close()
	srv.updateMu.Lock()
	defer srv.updateMu.Unlock()
	req := newLocalRequest(http.MethodPost, "/api/update/apply", bytes.NewBufferString(`{"release_identity":"reviewed"}`))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, &store.SessionUser{UserID: 1, Role: "admin"}))
	rec := httptest.NewRecorder()
	srv.handleUpdateApply(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("overlap got %d: %s", rec.Code, rec.Body)
	}
}
