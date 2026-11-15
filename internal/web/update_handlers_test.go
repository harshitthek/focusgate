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

	// 3. Authenticated request with admin session token must NOT receive 401 or 403
	reqAuth := newLocalRequest(http.MethodGet, "/api/update/check", nil)
	reqAuth.Header.Set("Authorization", "Bearer "+setupResp.Token)
	recAuth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recAuth, reqAuth)

	if recAuth.Code == http.StatusUnauthorized || recAuth.Code == http.StatusForbidden {
		t.Fatalf("expected admin session access, got status %d: %s", recAuth.Code, recAuth.Body.String())
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
