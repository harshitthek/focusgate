package mobile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"focusgate/internal/classify"
	"focusgate/internal/ingest"
	"focusgate/internal/store"

	"fyne.io/fyne/v2/test"
)

func TestMobileEngine_InitAndConfig(t *testing.T) {
	testApp := test.NewApp()
	eng, err := NewEngineWithDir(testApp, t.TempDir())
	if err != nil {
		t.Fatalf("failed to initialize engine: %v", err)
	}
	defer eng.Close()

	cfg := eng.Config()
	if cfg.ServerURL == "" {
		t.Fatalf("expected non-empty default server URL")
	}
	if cfg.PollMinutes <= 0 {
		t.Fatalf("expected positive default poll interval")
	}

	err = eng.UpdateConfig("https://example.com/gate", "custom-token-xyz", false, 15)
	if err != nil {
		t.Fatalf("failed to update config: %v", err)
	}

	updated := eng.Config()
	if updated.ServerURL != "https://example.com/gate" {
		t.Fatalf("expected updated server url, got %s", updated.ServerURL)
	}
	if updated.AuthToken != "custom-token-xyz" {
		t.Fatalf("expected updated auth token, got %s", updated.AuthToken)
	}
	if updated.PollMinutes != 15 {
		t.Fatalf("expected 15 min poll interval, got %d", updated.PollMinutes)
	}
}

func TestMobileEngine_SyncWithServer(t *testing.T) {
	testApp := test.NewApp()
	eng, err := NewEngineWithDir(testApp, t.TempDir())
	if err != nil {
		t.Fatalf("failed to initialize engine: %v", err)
	}
	defer eng.Close()

	// Mock server returning items
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/items" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"items": [
					{
						"id": 1,
						"source_type": "rss",
						"source_name": "Go Blog",
						"external_id": "test-feed-1",
						"title": "Go 1.28 Release Notes",
						"body": "New features in Go 1.28 runtime.",
						"route": "queue",
						"priority": 1,
						"created_at": "2026-11-15T10:00:00Z"
					}
				]
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	_ = eng.UpdateConfig(server.URL, "mock-token", true, 30)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = eng.SyncWithServer(ctx)
	if err != nil {
		t.Fatalf("SyncWithServer failed: %v", err)
	}

	if eng.Status() != StatusOnline {
		t.Fatalf("expected StatusOnline, got %s", eng.Status())
	}

	// Verify item was inserted into local store
	items, err := eng.Store().ListItems(ctx, store.ItemFilter{})
	if err != nil {
		t.Fatalf("failed to list items from store: %v", err)
	}
	if len(items) == 0 {
		t.Fatalf("expected at least 1 item synced to local store")
	}
}

func TestMobileEngine_MarkRead(t *testing.T) {
	testApp := test.NewApp()
	eng, err := NewEngineWithDir(testApp, t.TempDir())
	if err != nil {
		t.Fatalf("failed to initialize engine: %v", err)
	}
	defer eng.Close()

	ctx := context.Background()
	inc := ingest.IncomingItem{
		SourceType:  "rss",
		SourceName:  "Security Feed",
		ExternalID:  "item-markread-1",
		Title:       "Security Notice",
		Body:        "Critical CVE update",
		PublishedAt: time.Now().UTC(),
	}
	dec := classify.Decision{
		Route:    "push_now",
		Priority: 5,
		Reason:   "cve detected",
		Source:   "rule",
	}

	stored, inserted, err := eng.Store().InsertItem(ctx, inc, dec)
	if err != nil || !inserted {
		t.Fatalf("failed to insert test item: %v", err)
	}

	err = eng.MarkRead(ctx, stored.ID)
	if err != nil {
		t.Fatalf("failed to mark item read: %v", err)
	}

	// Verify archived
	items, err := eng.Store().ListItems(ctx, store.ItemFilter{Status: "archived"})
	if err != nil {
		t.Fatalf("failed to fetch items: %v", err)
	}
	found := false
	for _, it := range items {
		if it.ID == stored.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected item %d to be marked archived", stored.ID)
	}
}

func TestMobileEngine_LoginAndClassifyOnline(t *testing.T) {
	testApp := test.NewApp()
	eng, err := NewEngineWithDir(testApp, t.TempDir())
	if err != nil {
		t.Fatalf("failed to initialize engine: %v", err)
	}
	defer eng.Close()

	// Mock server handling /api/auth/login and /api/classify
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"authenticated","token":"mock-session-tok-123","user":{"username":"alice"}}`))
			return
		}
		if r.URL.Path == "/api/classify" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"route":"push_now","priority":5,"source":"typesafe","reason":"Jev classified critical outage"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 1. Test LoginWithServer
	err = eng.LoginWithServer(ctx, server.URL, "alice", "alice-password")
	if err != nil {
		t.Fatalf("LoginWithServer failed: %v", err)
	}
	if eng.Config().AuthToken != "mock-session-tok-123" {
		t.Fatalf("expected auth token mock-session-tok-123, got %s", eng.Config().AuthToken)
	}
	if eng.Config().Username != "alice" {
		t.Fatalf("expected username alice, got %s", eng.Config().Username)
	}

	// 2. Test ClassifyOnline using VPS online intelligence
	item := ingest.IncomingItem{
		ExternalID: "feed-test-999",
		SourceType: "rss",
		Title:      "Major Outage Reported",
		Body:       "Production outage across database cluster.",
	}
	dec := eng.ClassifyOnline(ctx, item)
	if dec.Route != "push_now" || dec.Source != "typesafe" {
		t.Fatalf("unexpected classify online decision: %+v", dec)
	}
}
