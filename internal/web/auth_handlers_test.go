package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"focusgate/internal/app"
	"focusgate/internal/config"
	"focusgate/internal/store"
)

func setupTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.Path = ":memory:"
	cfg.Classifier.Mode = "rules_only"
	cfg.Notify.DesktopWindows.Enabled = false

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	eng, err := app.NewEngine(context.Background(), cfg, st)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	srv, err := NewServer(eng)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	return srv, st
}

func newLocalRequest(method, path string, body *bytes.Buffer) *http.Request {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, "http://127.0.0.1:8787"+path, nil)
	} else {
		r = httptest.NewRequest(method, "http://127.0.0.1:8787"+path, body)
	}
	r.Host = "127.0.0.1:8787"
	r.RemoteAddr = "127.0.0.1:1234"
	return r
}

func TestServer_AuthLifecycle(t *testing.T) {
	srv, st := setupTestServer(t)
	defer st.Close()

	// 1. Initial status on fresh DB must report setup_required = true
	req := newLocalRequest(http.MethodGet, "/api/auth/status", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /api/auth/status, got %d", rec.Code)
	}
	var statusResp struct {
		SetupRequired bool `json:"setup_required"`
		Authenticated bool `json:"authenticated"`
		HasGitHub     bool `json:"has_github"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("unmarshal auth status: %v", err)
	}
	if !statusResp.SetupRequired {
		t.Fatalf("expected setup_required to be true on fresh DB")
	}

	// 2. Setup with short passphrase must fail (400)
	req = newLocalRequest(http.MethodPost, "/api/auth/setup", bytes.NewBufferString(`{"passphrase":"12"}`))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for short passphrase, got %d", rec.Code)
	}

	// 3. Setup with valid master passphrase must succeed and return session cookie & token
	req = newLocalRequest(http.MethodPost, "/api/auth/setup", bytes.NewBufferString(`{"passphrase":"super-secret-master-pin-2026"}`))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on setup, got %d: %s", rec.Code, rec.Body.String())
	}
	var setupResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &setupResp); err != nil {
		t.Fatalf("unmarshal setup response: %v", err)
	}
	if setupResp.Token == "" {
		t.Fatalf("expected non-empty session token in setup response")
	}

	cookies := rec.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil || sessionCookie.Value != setupResp.Token {
		t.Fatalf("expected valid session cookie set on setup")
	}

	// 4. Repeated setup attempt must be rejected (400 or 409)
	req = newLocalRequest(http.MethodPost, "/api/auth/setup", bytes.NewBufferString(`{"passphrase":"another-passphrase"}`))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusConflict {
		t.Fatalf("expected 400 or 409 on repeated setup, got %d", rec.Code)
	}

	// 5. Accessing protected endpoint without session must fail (401)
	req = newLocalRequest(http.MethodGet, "/api/profile", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on /api/profile without session, got %d", rec.Code)
	}

	// 6. Accessing protected endpoint with session cookie must succeed (200)
	req = newLocalRequest(http.MethodGet, "/api/profile", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /api/profile with session cookie, got %d: %s", rec.Code, rec.Body.String())
	}
	var profResp struct {
		AuthConfigured bool `json:"auth_configured"`
		GitHub         struct {
			Connected         bool `json:"connected"`
			ParticipatingOnly bool `json:"participating_only"`
		} `json:"github"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &profResp); err != nil {
		t.Fatalf("unmarshal profile: %v", err)
	}
	if !profResp.AuthConfigured {
		t.Fatalf("expected auth_configured to be true")
	}
	if profResp.GitHub.Connected {
		t.Fatalf("expected github not connected yet")
	}

	// 7. Login with invalid passphrase must fail (401)
	req = newLocalRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(`{"passphrase":"wrong-password"}`))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on login with bad passphrase, got %d", rec.Code)
	}

	// 8. Login with correct passphrase must succeed (200)
	req = newLocalRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(`{"passphrase":"super-secret-master-pin-2026"}`))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on login, got %d: %s", rec.Code, rec.Body.String())
	}

	// 9. Change passphrase
	changePayload := `{"current_passphrase":"super-secret-master-pin-2026","new_passphrase":"new-master-pin-98765"}`
	req = newLocalRequest(http.MethodPost, "/api/auth/change-passphrase", bytes.NewBufferString(changePayload))
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on change-passphrase, got %d: %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			sessionCookie = c
			break
		}
	}

	// 10. Login with new passphrase
	req = newLocalRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(`{"passphrase":"new-master-pin-98765"}`))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on login with new passphrase, got %d: %s", rec.Code, rec.Body.String())
	}

	// 11. GitHub Settings preference update
	prefPayload := `{"participating_only":false,"client_id":"my-custom-client-id"}`
	req = newLocalRequest(http.MethodPost, "/api/github/settings", bytes.NewBufferString(prefPayload))
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on github settings update, got %d: %s", rec.Code, rec.Body.String())
	}

	// Check profile reflects updated preferences
	req = newLocalRequest(http.MethodGet, "/api/profile", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var profResp2 struct {
		GitHub struct {
			ParticipatingOnly bool   `json:"participating_only"`
			ClientID          string `json:"client_id"`
		} `json:"github"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &profResp2)
	if profResp2.GitHub.ParticipatingOnly != false || profResp2.GitHub.ClientID != "my-custom-client-id" {
		t.Fatalf("expected participating_only=false and custom client_id, got %+v", profResp2.GitHub)
	}

	// 12. GitHub Disconnect
	req = newLocalRequest(http.MethodPost, "/api/github/disconnect", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on github disconnect, got %d", rec.Code)
	}

	// 13. Logout
	req = newLocalRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on logout, got %d", rec.Code)
	}

	// Session token should now be invalidated
	req = newLocalRequest(http.MethodGet, "/api/profile", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", rec.Code)
	}
}
