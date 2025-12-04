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

func TestServer_WebhookModeAndAuth(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Path = ":memory:"
	cfg.Server.AuthToken = "ui-secret"
	cfg.Sources.Webhook.Enabled = true
	cfg.Sources.Webhook.Secret = "hook-secret"
	cfg.Notify.DesktopWindows.Enabled = false
	cfg.Classifier.Mode = "rules_only"

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	eng, err := app.NewEngine(context.Background(), cfg, st)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	srv, err := NewServer(eng)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	// Unauthorized status request must return 401.
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}

	// Authorized mode switch to deep_work.
	req = httptest.NewRequest(http.MethodPost, "/api/mode", bytes.NewBufferString(`{"mode":"deep_work"}`))
	req.Header.Set("Authorization", "Bearer ui-secret")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /api/mode, got %d: %s", rec.Code, rec.Body.String())
	}

	// Webhook ingestion with valid hook secret.
	req = httptest.NewRequest(http.MethodPost, "/api/webhook/uptime", bytes.NewBufferString(`{"title":"Weekly stats","body":"All green"}`))
	req.Header.Set("X-Webhook-Secret", "hook-secret")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 on webhook, got %d: %s", rec.Code, rec.Body.String())
	}

	var hookResp struct {
		Inserted bool `json:"inserted"`
		Item     struct {
			Status string `json:"status"`
		} `json:"item"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hookResp); err != nil {
		t.Fatalf("decode webhook response: %v", err)
	}
	if !hookResp.Inserted || hookResp.Item.Status != "queued" {
		t.Fatalf("expected inserted queued item during deep_work, got %+v", hookResp)
	}

	// Cross-origin browser request must be rejected by CrossOriginProtection.
	req = httptest.NewRequest(http.MethodPost, "/api/mode", bytes.NewBufferString(`{"mode":"normal"}`))
	req.Header.Set("Authorization", "Bearer ui-secret")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-site browser POST, got %d", rec.Code)
	}

	// Unauthenticated server must reject non-loopback Host headers (DNS rebinding guard).
	openCfg := config.Default()
	openCfg.Storage.Path = ":memory:"
	openCfg.Classifier.Mode = "rules_only"
	openCfg.Notify.DesktopWindows.Enabled = false
	openEng, err := app.NewEngine(context.Background(), openCfg, st)
	if err != nil {
		t.Fatalf("new open engine: %v", err)
	}
	openSrv, err := NewServer(openEng)
	if err != nil {
		t.Fatalf("new open server: %v", err)
	}
	rebindingReq := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rebindingReq.Host = "evil.rebind.example:8787"
	rebindingRec := httptest.NewRecorder()
	openSrv.Handler().ServeHTTP(rebindingRec, rebindingReq)
	if rebindingRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback Host without auth, got %d", rebindingRec.Code)
	}
	localReq := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	localReq.Host = "127.0.0.1:8787"
	localRec := httptest.NewRecorder()
	openSrv.Handler().ServeHTTP(localRec, localReq)
	if localRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for loopback Host, got %d", localRec.Code)
	}
}
