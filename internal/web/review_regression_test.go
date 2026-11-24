package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

func TestDiscardSessionKeepsWinningSessionAndCookie(t *testing.T) {
	srv, st := setupTestServer(t)
	defer st.Close()
	u, err := st.CreateUser(t.Context(), "alice", "alice-password-123")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"obsolete", "winning"} {
		if err := st.CreateUserSession(t.Context(), token, u.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	req := newLocalRequest(http.MethodPost, "/api/auth/discard-session", nil)
	req.Header.Set("Authorization", "Bearer obsolete")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "winning"})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("discard mutated cookie: %d %s", rec.Code, rec.Body)
	}
	for token, want := range map[string]bool{"obsolete": false, "winning": true} {
		session, err := st.ValidateSessionUser(t.Context(), token)
		if err != nil || (session != nil) != want {
			t.Fatalf("session %q: %v %v", token, session, err)
		}
	}
}

func TestCommittedMutationsReportUnavailableStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.db")
	srv, st := setupAuthServerAt(t, config.Default(), path)
	defer st.Close()
	item, _, err := st.InsertItem(t.Context(), ingest.IncomingItem{SourceType: "rss", ExternalID: "one"}, classify.Decision{Route: "queue_digest"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Mutation columns remain usable while aggregate status queries fail.
	if _, err := db.Exec(`ALTER TABLE items RENAME COLUMN created_at TO unavailable_created_at`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, body string }{
		{"/api/mode", `{"mode":"dnd"}`},
		{fmt.Sprintf("/api/items/%d/status", item.ID), `{"status":"read"}`},
		{"/api/items/bulk", fmt.Sprintf(`{"ids":[%d],"status":"archived"}`, item.ID)},
	} {
		req := newLocalRequest(http.MethodPost, tc.path, bytes.NewBufferString(tc.body))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		var result map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || result["committed"] != true || result["stats"] != nil || result["status"] != nil {
			t.Fatalf("committed mutation returned misleading status: %s %d %s", tc.path, rec.Code, rec.Body)
		}
	}
	mode, err := st.GetFocusMode(t.Context(), config.ModeNormal)
	if err != nil || mode != config.ModeDND {
		t.Fatalf("mode was not committed: %s %v", mode, err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM items WHERE id=?`, item.ID).Scan(&status); err != nil || status != "archived" {
		t.Fatalf("item mutation was not committed: %s %v", status, err)
	}
}

func TestPasswordRotationAllowsMaximumUnicodeAndRejectsWrongPasswordWithoutLogout(t *testing.T) {
	srv, st := setupTestServer(t)
	defer st.Close()
	current, next := strings.Repeat("😀", 1024), strings.Repeat("😃", 1024)
	u, err := st.CreateUser(t.Context(), "alice", current)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUserSession(t.Context(), "session", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rotate := func(old string) *httptest.ResponseRecorder {
		raw, err := json.Marshal(map[string]string{"current_passphrase": old, "new_passphrase": next})
		if err != nil {
			t.Fatal(err)
		}
		req := newLocalRequest(http.MethodPost, "/api/auth/change-passphrase", bytes.NewBuffer(raw))
		req.Header.Set("Authorization", "Bearer session")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := rotate("incorrect-password"); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong password: %d %s", rec.Code, rec.Body)
	}
	if su, err := st.ValidateSessionUser(t.Context(), "session"); err != nil || su == nil {
		t.Fatalf("wrong password revoked session: %v %v", su, err)
	}
	if rec := rotate(current); rec.Code != http.StatusOK {
		t.Fatalf("max Unicode rotation: %d %s", rec.Code, rec.Body)
	}
}

func TestFeedPayloadConsumesEntireBoundedBody(t *testing.T) {
	srv, st := setupTestServer(t)
	defer st.Close()
	for _, suffix := range []string{strings.Repeat(" ", 4096), ` {"another":true}`} {
		req := newLocalRequest(http.MethodPost, "/api/feeds", bytes.NewBufferString(`{"name":"Example","url":"https://example.test/feed"}`+suffix))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		var payload struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusBadRequest || payload.Error != "invalid feed payload" {
			t.Fatalf("trailing data accepted: %d %s", rec.Code, rec.Body)
		}
	}
}

func TestEmptyEventTicketCannotFallBackToBearer(t *testing.T) {
	cfg := config.Default()
	cfg.Server.AuthToken = "operator-token"
	srv, st := setupAuthServer(t, cfg)
	defer st.Close()
	req := newLocalRequest(http.MethodGet, "/api/events?ticket=", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 100*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer operator-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty ticket accepted: %d", rec.Code)
	}
}

func TestWebhookURLsWithEllipsesSurvivePreviewSave(t *testing.T) {
	cfg := config.Default()
	cfg.Server.AuthToken = "operator-token"
	srv, st := setupAuthServer(t, cfg)
	defer st.Close()
	urls := store.NotifySettings{SlackWebhookURL: "https://bridge.example/hooks/a...b", MatrixWebhookURL: "https://bridge.example/hooks/c...d"}
	if err := st.SetNotifySettings(t.Context(), urls); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{"slack_webhook_url": maskWebhookURL(urls.SlackWebhookURL), "matrix_webhook_url": maskWebhookURL(urls.MatrixWebhookURL)})
	if err != nil {
		t.Fatal(err)
	}
	req := newLocalRequest(http.MethodPost, "/api/notify/settings", bytes.NewBuffer(raw))
	req.Header.Set("Authorization", "Bearer operator-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ellipsis preview rejected: %d %s", rec.Code, rec.Body)
	}
	saved, err := st.GetNotifySettings(t.Context())
	if err != nil || saved.SlackWebhookURL != urls.SlackWebhookURL || saved.MatrixWebhookURL != urls.MatrixWebhookURL {
		t.Fatalf("destinations changed: %+v %v", saved, err)
	}
}

func TestUppercaseWebhookPreviewMustMatchSavedDestination(t *testing.T) {
	cfg := config.Default()
	cfg.Server.AuthToken = "operator-token"
	srv, st := setupAuthServer(t, cfg)
	defer st.Close()
	saved := store.NotifySettings{SlackWebhookURL: "HTTPS://bridge.example/hooks/first-original-destination", MatrixWebhookURL: "HTTPS://bridge.example/hooks/first-original-destination"}
	if err := st.SetNotifySettings(t.Context(), saved); err != nil {
		t.Fatal(err)
	}
	preview := maskWebhookURL(saved.SlackWebhookURL)
	post := func(channel string) *httptest.ResponseRecorder {
		raw, err := json.Marshal(map[string]string{channel + "_webhook_url": preview})
		if err != nil {
			t.Fatal(err)
		}
		req := newLocalRequest(http.MethodPost, "/api/notify/settings", bytes.NewBuffer(raw))
		req.Header.Set("Authorization", "Bearer operator-token")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	for _, channel := range []string{"slack", "matrix"} {
		if rec := post(channel); rec.Code != http.StatusOK {
			t.Fatalf("current preview rejected: %d %s", rec.Code, rec.Body)
		}
	}
	saved.SlackWebhookURL, saved.MatrixWebhookURL = "https://other.example/changed", "https://other.example/changed"
	if err := st.SetNotifySettings(t.Context(), saved); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"slack", "matrix"} {
		if rec := post(channel); rec.Code != http.StatusConflict {
			t.Fatalf("stale uppercase preview accepted: %d %s", rec.Code, rec.Body)
		}
	}
	actual, err := st.GetNotifySettings(t.Context())
	if err != nil || actual.SlackWebhookURL != saved.SlackWebhookURL || actual.MatrixWebhookURL != saved.MatrixWebhookURL {
		t.Fatalf("stale preview changed destination: %+v %v", actual, err)
	}
}
