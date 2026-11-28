package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"focusgate/internal/app"
	"focusgate/internal/config"
	"focusgate/internal/store"
)

func setupTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	cfg := config.Default()
	return setupAuthServer(t, cfg)
}

func setupAuthServer(t *testing.T, cfg config.Config) (*Server, *store.Store) {
	t.Helper()
	return setupAuthServerAt(t, cfg, ":memory:")
}

func setupAuthServerAt(t *testing.T, cfg config.Config, path string) (*Server, *store.Store) {
	t.Helper()
	cfg.Storage.Path = path
	cfg.Classifier.Mode = "rules_only"
	cfg.Notify.DesktopWindows.Enabled = false

	st, err := store.Open(path)
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

func TestRemoteBootstrapRequiresServerToken(t *testing.T) {
	cfg := config.Default()
	cfg.Server.AuthToken = "operator-token"
	cfg.Server.PublicURL = "https://focusgate.example"
	srv, st := setupAuthServer(t, cfg)
	t.Cleanup(func() { _ = st.Close() })

	const payload = `{"passphrase":"initial-passphrase"}`
	request := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, cfg.Server.PublicURL+"/api/auth/setup", bytes.NewBufferString(payload))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	for _, credential := range []string{"", "incorrect-token"} {
		if res := request(credential); res.Code != http.StatusForbidden {
			t.Fatalf("expected forbidden during unapproved setup, got %d", res.Code)
		}
	}
	login := httptest.NewRequest(http.MethodPost, cfg.Server.PublicURL+"/api/auth/login",
		bytes.NewBufferString(`{"passphrase":"operator-token"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, login)
	if rec.Code != http.StatusPreconditionRequired || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("expected token login to require setup without issuing a session, got %d", rec.Code)
	}
	if res := request(cfg.Server.AuthToken); res.Code != http.StatusOK {
		t.Fatalf("expected authorized setup to succeed, got %d: %s", res.Code, res.Body)
	}
}

func TestTokenlessSetupRequiresDirectLoopback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		peer    string
		proxies []string
		header  string
		want    bool
	}{
		{name: "direct IPv4", peer: "127.0.0.1:1234", want: true},
		{name: "direct IPv6", peer: "[::1]:1234", want: true},
		{name: "missing peer"},
		{name: "configured proxy", peer: "127.0.0.1:1234", proxies: []string{"127.0.0.1"}},
		{name: "standard forwarding header", peer: "127.0.0.1:1234", header: "Forwarded"},
		{name: "conventional forwarding header", peer: "127.0.0.1:1234", header: "X-Forwarded-For"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Server.TrustedProxyIPs = tc.proxies
			srv, st := setupAuthServer(t, cfg)
			t.Cleanup(func() { _ = st.Close() })
			req := newLocalRequest(http.MethodGet, "/api/auth/status", nil)
			req.RemoteAddr = tc.peer
			if tc.header != "" {
				req.Header.Set(tc.header, "present")
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			var status struct {
				SetupRequired       bool `json:"setup_required"`
				Authenticated       bool `json:"authenticated"`
				RegistrationAllowed bool `json:"registration_allowed"`
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status returned %d", rec.Code)
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.SetupRequired != tc.want || status.Authenticated != tc.want || status.RegistrationAllowed != tc.want {
				t.Fatalf("initial local privileges = %+v, want all %t", status, tc.want)
			}
		})
	}
}

func TestGitHubAuthorizationReturnsConflictWhenSupersededDuringProfileFetch(t *testing.T) {
	for _, path := range []string{"/api/github/device/poll", "/api/github/token"} {
		t.Run(path, func(t *testing.T) {
			cfg := config.Default()
			cfg.Server.AuthToken = "test-operator-token"
			srv, st := setupAuthServer(t, cfg)
			t.Cleanup(func() { _ = st.Close() })
			generation, err := st.BeginConnection(t.Context(), "github")
			if err != nil {
				t.Fatal(err)
			}
			srv.engine.HTTPClient().Transport = reviewRoundTripper(func(r *http.Request) (*http.Response, error) {
				body := `{"access_token":"test-device-token"}`
				if r.URL.Path == "/user" {
					if _, err := st.BeginConnection(r.Context(), "github"); err != nil {
						return nil, err
					}
					body = `{"login":"test-user","id":1}`
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			body := `{"token":"test-personal-token"}`
			if path == "/api/github/device/poll" {
				body = `{"device_code":"test-device-code","generation":"` + generation + `"}`
			}
			req := newLocalRequest(http.MethodPost, path, bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer "+cfg.Server.AuthToken)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), store.ErrConnectionChanged.Error()) {
				t.Fatalf("expected restart guidance with HTTP 409, got %d: %s", rec.Code, rec.Body.String())
			}
			token, _, _, err := st.GetGitHubCredentials(t.Context())
			if err != nil || token != "" {
				t.Fatalf("superseded authorization changed stored credentials, error = %v", err)
			}
		})
	}
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

	expired := map[string]bool{}
	for _, c := range rec.Result().Cookies() {
		expired[c.Name] = c.MaxAge < 0
	}
	if !expired[sessionCookieName] || !expired["focusgate_token"] {
		t.Fatal("logout did not expire both supported browser credentials")
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

func TestStaleWebhookPreviewCannotOverwriteCurrentDestination(t *testing.T) {
	cfg := config.Default()
	cfg.Server.AuthToken = "operator-token"
	srv, st := setupAuthServer(t, cfg)
	defer st.Close()
	original := store.NotifySettings{SlackWebhookURL: "https://hooks.slack.com/services/new-destination"}
	if err := st.SetNotifySettings(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	req := newLocalRequest(http.MethodPost, "/api/notify/settings", bytes.NewBufferString(`{"slack_webhook_url":"https://...old123"}`))
	req.Header.Set("Authorization", "Bearer operator-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale preview accepted: %d %s", rec.Code, rec.Body)
	}
	got, err := st.GetNotifySettings(context.Background())
	if err != nil || got.SlackWebhookURL != original.SlackWebhookURL {
		t.Fatalf("webhook was overwritten: %q %v", got.SlackWebhookURL, err)
	}
}

func TestAdministratorJSONInputIsBounded(t *testing.T) {
	cfg := config.Default()
	cfg.Server.AuthToken = "operator-token"
	srv, st := setupAuthServer(t, cfg)
	defer st.Close()
	// Outlook is rejected locally, so removing the size guard never contacts a mail server.
	body := `{"provider":"outlook","username":"alice@example.test","password":"` + strings.Repeat("x", 1<<20) + `"}`
	req := newLocalRequest(http.MethodPost, "/api/email/connect", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer operator-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid json payload") {
		t.Fatalf("oversized body reached provider handling: %d %s", rec.Code, rec.Body)
	}
}

func TestLogoutLookupFailurePreservesSessionsForRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logout.db")
	srv, st := setupAuthServerAt(t, config.Default(), path)
	defer st.Close()
	for _, token := range []string{"this-session", "another-session"} {
		if err := st.CreateSession(t.Context(), token, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("ALTER TABLE users RENAME TO saved_users"); err != nil {
		t.Fatal(err)
	}
	req := newLocalRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "this-session"})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("logout lookup failure returned %d with %d cookies", rec.Code, len(rec.Result().Cookies()))
	}
	var sessions int
	if err := db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessions); err != nil || sessions != 2 {
		t.Fatalf("session count after failure %d, err=%v", sessions, err)
	}
}
