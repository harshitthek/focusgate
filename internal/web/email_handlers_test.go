package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEmailHandlers_AuthRequired(t *testing.T) {
	srv, st := setupTestServer(t)

	// Initialize master passphrase so auth is enforced
	if err := st.InitializeMasterPassphrase(t.Context(), "some-hash", "some-salt"); err != nil {
		t.Fatalf("InitializeMasterPassphrase: %v", err)
	}

	// Without session, should be 401 Unauthorized
	req := newLocalRequest(http.MethodPost, "/api/email/connect", bytes.NewBufferString(`{"username":"test@gmail.com","password":"pwd"}`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 on /api/email/connect without session, got %d", w.Code)
	}
}

func TestEmailHandlers_DisconnectAndSettings(t *testing.T) {
	srv, st := setupTestServer(t)

	// Create test session
	tok := "test-session-tok-12345"
	expiresAt := time.Now().Add(24 * time.Hour)
	if err := st.CreateSession(t.Context(), tok, expiresAt); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	cookie := &http.Cookie{Name: sessionCookieName, Value: tok}

	// Test settings update
	settingsBody, _ := json.Marshal(map[string]any{"poll_minutes": 10})
	req := newLocalRequest(http.MethodPost, "/api/email/settings", bytes.NewBuffer(settingsBody))
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on /api/email/settings, got %d (body: %s)", w.Code, w.Body.String())
	}

	creds, err := st.GetEmailCredentials(t.Context())
	if err != nil {
		t.Fatalf("GetEmailCredentials: %v", err)
	}
	if creds.PollMinutes != 10 {
		t.Errorf("expected poll_minutes 10, got %d", creds.PollMinutes)
	}

	// Test disconnect
	reqDisc := newLocalRequest(http.MethodPost, "/api/email/disconnect", nil)
	reqDisc.AddCookie(cookie)
	wDisc := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wDisc, reqDisc)

	if wDisc.Code != http.StatusOK {
		t.Fatalf("expected 200 on /api/email/disconnect, got %d (body: %s)", wDisc.Code, wDisc.Body.String())
	}

	dis, _ := st.IsEmailDisabled(t.Context())
	if !dis {
		t.Errorf("expected email to be disabled after disconnect")
	}
}
