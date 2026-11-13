package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

	// 3. Authenticated request with session token
	reqAuth := newLocalRequest(http.MethodGet, "/api/update/check", nil)
	reqAuth.Header.Set("Authorization", "Bearer "+setupResp.Token)
	recAuth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recAuth, reqAuth)

	if recAuth.Code == http.StatusUnauthorized {
		t.Fatalf("expected authenticated access, got 401")
	}
}
