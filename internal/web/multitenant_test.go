package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"time"

	"focusgate/internal/app"
	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

func TestWeb_MultiTenantUserAccountsAndClassify(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	cfg := config.Default()
	engine, err := app.NewEngine(context.Background(), cfg, st)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	srv, err := NewServer(engine)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// 1. Register User 1: Alice
	regAlicePayload := `{"username":"alice","password":"alice-password-123"}`
	req := newLocalRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(regAlicePayload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on register alice, got %d: %s", rec.Code, rec.Body.String())
	}

	var aliceRegResp struct {
		Status string `json:"status"`
		Token  string `json:"token"`
		User   struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &aliceRegResp); err != nil {
		t.Fatalf("failed to decode alice register response: %v", err)
	}
	if aliceRegResp.User.Username != "alice" || aliceRegResp.User.Role != "admin" {
		t.Fatalf("unexpected alice user info: %+v", aliceRegResp.User)
	}

	// 2. Query /api/auth/me with Alice's token
	req = newLocalRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+aliceRegResp.Token)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /api/auth/me, got %d: %s", rec.Code, rec.Body.String())
	}
	var meResp struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &meResp)
	if meResp.User.Username != "alice" {
		t.Fatalf("expected me=alice, got %s", meResp.User.Username)
	}

	// 3. Register User 2: Bob
	regBobPayload := `{"username":"bob","password":"bob-password-456"}`
	req = newLocalRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(regBobPayload))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on register bob, got %d: %s", rec.Code, rec.Body.String())
	}
	var bobRegResp struct {
		Status string `json:"status"`
		Token  string `json:"token"`
		User   struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &bobRegResp)
	if bobRegResp.User.Username != "bob" || bobRegResp.User.Role != "user" {
		t.Fatalf("expected bob to be role user, got %+v", bobRegResp.User)
	}

	// 4. Test Online Classify Proxy via /api/classify with Bob's token
	classifyItemPayload := `{
		"external_id": "test-123",
		"source_type": "rss",
		"source_name": "Go Blog",
		"title": "Production Outage Postmortem",
		"body": "Detailed postmortem on server outage incident."
	}`
	req = newLocalRequest(http.MethodPost, "/api/classify", bytes.NewBufferString(classifyItemPayload))
	req.Header.Set("Authorization", "Bearer "+bobRegResp.Token)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /api/classify, got %d: %s", rec.Code, rec.Body.String())
	}
	var decResp struct {
		Route    string `json:"route"`
		Priority int    `json:"priority"`
		Source   string `json:"source"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decResp); err != nil {
		t.Fatalf("failed to decode classify response: %v", err)
	}
	if decResp.Route == "" {
		t.Fatalf("expected non-empty routing decision")
	}

	// 4b. Test Online Classify Proxy with enveloped payload {"item": {...}}
	classifyEnvelopedPayload := `{
		"item": {
			"external_id": "test-enveloped-456",
			"source_type": "rss",
			"source_name": "Alerts",
			"title": "Database Connection Pool Exhausted",
			"body": "Outage due to deadlocked connection pool."
		}
	}`
	req = newLocalRequest(http.MethodPost, "/api/classify", bytes.NewBufferString(classifyEnvelopedPayload))
	req.Header.Set("Authorization", "Bearer "+bobRegResp.Token)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on enveloped /api/classify, got %d: %s", rec.Code, rec.Body.String())
	}
	var envDecResp struct {
		Route    string `json:"route"`
		Priority int    `json:"priority"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envDecResp); err != nil || envDecResp.Route == "" {
		t.Fatalf("failed to decode enveloped classify response: %v", err)
	}

	// 5. Test Logout
	req = newLocalRequest(http.MethodPost, "/api/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+bobRegResp.Token)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on logout, got %d", rec.Code)
	}

	// 6. Accessing /api/auth/me after logout must fail (401)
	req = newLocalRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+bobRegResp.Token)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", rec.Code)
	}
}

func TestWeb_MultiTenantItemRoutingAndFocusMode(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	cfg := config.Default()
	engine, err := app.NewEngine(context.Background(), cfg, st)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}

	srv, err := NewServer(engine)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ctx := context.Background()

	// Register Admin (User 1)
	u1, err := st.CreateUser(ctx, "admin", "admin-pass-123")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}

	// Register User 2 (Bob)
	u2, err := st.CreateUser(ctx, "bob", "bob-password-456")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}

	// 1. Verify push_now status retainment for User 2 (must be queued, not terminal pushed)
	urgentItem := ingest.IncomingItem{
		ExternalID:  "urgent-bob-1",
		SourceType:  "rss",
		SourceName:  "Alerts",
		Title:       "Server CPU Spike",
		PublishedAt: time.Now().UTC(),
	}
	urgentDec := classify.Decision{Route: "push_now", Priority: 3, Source: "rule", Reason: "urgent alert"}

	// Direct InsertUserItem
	storedDirect, inserted, err := st.InsertUserItem(ctx, u2.ID, urgentItem, urgentDec)
	if err != nil || !inserted {
		t.Fatalf("failed to insert urgent item for bob: inserted=%v err=%v", inserted, err)
	}
	if storedDirect.Status != "queued" {
		t.Fatalf("expected bob urgent item to be retained as queued, got %q", storedDirect.Status)
	}

	// Admin item with push_now stays pushed
	adminItem := ingest.IncomingItem{
		ExternalID:  "urgent-admin-1",
		SourceType:  "rss",
		SourceName:  "Admin Alerts",
		Title:       "Cluster Health Warning",
		PublishedAt: time.Now().UTC(),
	}
	storedAdmin, insAdmin, err := st.InsertUserItem(ctx, u1.ID, adminItem, urgentDec)
	if err != nil || !insAdmin {
		t.Fatalf("failed to insert urgent item for admin: ins=%v err=%v", insAdmin, err)
	}
	if storedAdmin.Status != "pushed" {
		t.Fatalf("expected admin urgent item to be pushed, got %q", storedAdmin.Status)
	}

	// 2. ProcessItem for user 2
	urgentItem2 := ingest.IncomingItem{
		UserID:      u2.ID,
		ExternalID:  "urgent-bob-2",
		SourceType:  "rss",
		SourceName:  "Alerts",
		Title:       "Out of Memory Warning",
		PublishedAt: time.Now().UTC(),
	}
	storedProc, insProc, err := engine.ProcessItem(ctx, urgentItem2)
	if err != nil || !insProc {
		t.Fatalf("failed to process item for bob: ins=%v err=%v", insProc, err)
	}
	if storedProc.UserID != u2.ID {
		t.Fatalf("expected stored item user_id=%d, got %d", u2.ID, storedProc.UserID)
	}

	// 3. User 2 mode switching isolation
	bobToken := "bob-session-token-test"
	if err := st.CreateUserSession(ctx, bobToken, u2.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create bob session: %v", err)
	}

	req := newLocalRequest(http.MethodPost, "/api/mode", bytes.NewBufferString(`{"mode":"deep_work"}`))
	req.Header.Set("Authorization", "Bearer "+bobToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /api/mode for bob, got %d: %s", rec.Code, rec.Body.String())
	}

	// Global engine mode must still be normal
	if engine.Mode() != config.ModeNormal {
		t.Fatalf("expected global engine mode to remain normal, got %q", engine.Mode())
	}

	// Query status as Bob -> must reflect deep_work
	stBob, err := engine.StatusUser(ctx, u2.ID)
	if err != nil {
		t.Fatalf("status for bob: %v", err)
	}
	if stBob.Stats.Mode != config.ModeDeepWork {
		t.Fatalf("expected bob mode to be deep_work, got %q", stBob.Stats.Mode)
	}

	// 4. Test SSE ticket consumption: explicit invalid ticket must return 401 even if valid session token/cookie is present
	req = newLocalRequest(http.MethodGet, "/api/events?ticket=invalid-ticket-123", nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on /api/events with invalid ticket, got %d", rec.Code)
	}

	// 5. Test valid ticket generation & consumption
	req = newLocalRequest(http.MethodPost, "/api/auth/ticket", nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /api/auth/ticket, got %d", rec.Code)
	}
	var ticketResp struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ticketResp)
	if ticketResp.Ticket == "" {
		t.Fatalf("expected non-empty ticket")
	}

	// First use of valid ticket succeeds
	su := srv.consumeTicket(ticketResp.Ticket)
	if su == nil || su.UserID != u2.ID {
		t.Fatalf("expected ticket to belong to bob (user %d), got %+v", u2.ID, su)
	}

	// Second use of the same ticket must fail (returns nil)
	su2 := srv.consumeTicket(ticketResp.Ticket)
	if su2 != nil {
		t.Fatalf("expected consumed ticket to be deleted, got %+v", su2)
	}

	// Request with consumed ticket must fail with 401
	req = newLocalRequest(http.MethodGet, "/api/events?ticket="+ticketResp.Ticket, nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on reused consumed ticket, got %d", rec.Code)
	}
}
