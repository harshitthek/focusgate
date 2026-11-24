package mobile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"

	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

func TestReauthenticationPreservesLocalItems(t *testing.T) {
	eng, err := NewEngineWithDir(test.NewApp(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	ctx := context.Background()
	if err := eng.applyVerifiedAccount(ctx, "https://example.test", "alice", "old-token", 9, true, 30); err != nil {
		t.Fatal(err)
	}
	_, _, err = eng.Store().InsertItem(ctx, ingest.IncomingItem{
		ExternalID: "local-1", SourceType: "rss", Title: "Offline history",
	}, classify.Decision{Route: "queue_digest"})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Store().SetFocusMode(ctx, config.ModeDND); err != nil {
		t.Fatal(err)
	}
	if err := eng.applyVerifiedAccount(ctx, "https://example.test", "alice", "fresh-token", 9, true, 30); err != nil {
		t.Fatal(err)
	}
	items, err := eng.Store().ListItems(ctx, store.ItemFilter{})
	if err != nil || len(items) != 1 {
		t.Fatalf("reauthentication deleted history: %+v (%v)", items, err)
	}
	if mode, _ := eng.Store().GetFocusMode(ctx, config.ModeNormal); mode != config.ModeDND {
		t.Fatalf("reauthentication reset focus: %s", mode)
	}
	originalPath := eng.cfgPath
	eng.cfgPath = filepath.Join(t.TempDir(), "missing", "config.json")
	if err := eng.applyVerifiedAccount(ctx, "https://example.test", "bob", "bob-token", 10, true, 30); err == nil {
		t.Fatal("expected config write failure")
	}
	eng.cfgPath = originalPath
	if eng.Config().Username != "alice" {
		t.Fatal("failed save switched account")
	}
	items, err = eng.Store().ListItems(ctx, store.ItemFilter{})
	if err != nil || len(items) != 1 {
		t.Fatal("failed save deleted history")
	}
	if err := eng.applyVerifiedAccount(ctx, "https://example.test", "bob", "bob-token", 10, true, 30); err != nil {
		t.Fatal(err)
	}
	items, err = eng.Store().ListItems(ctx, store.ItemFilter{})
	if err != nil || len(items) != 0 {
		t.Fatal("account transition retained foreign history")
	}
}

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

	var calls atomic.Int32
	var archived atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/items/42/status" {
			if r.Header.Get("Authorization") != "Bearer mock-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			archived.Store(true)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/api/items" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if calls.Add(1) > 1 {
				_, _ = w.Write([]byte(`{"items":[{"id":42,"source_type":"rss","external_id":"test-feed-1","route":"push_now","priority":5,"status":"pushed"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{
				"mode": "deep_work",
    "items": [
     {
						"id": 42,
						"source_type": "rss",
						"source_name": "Go Blog",
						"external_id": "test-feed-1",
						"title": "Go 1.28 Release Notes",
						"body": "New features in Go 1.28 runtime.",
						"route": "queue_digest",
						"status": "queued",
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
	mode, err := eng.Store().GetFocusMode(ctx, config.ModeNormal)
	if err != nil || mode != config.ModeDeepWork {
		t.Fatalf("server focus mode did not sync: %q %v", mode, err)
	}
	localID := items[0].ID
	if localID == 42 {
		t.Fatal("expected local ID to differ from server ID")
	}
	if err := eng.SyncWithServer(ctx); err != nil {
		t.Fatal(err)
	}
	items, err = eng.Store().ListItems(ctx, store.ItemFilter{})
	if err != nil || len(items) != 1 || items[0].ID != localID || items[0].Status != "pushed" || items[0].Priority != 5 {
		t.Fatalf("server changes did not update the existing item: %+v (%v)", items, err)
	}
	if err := eng.MarkRead(ctx, localID); err != nil || !archived.Load() {
		t.Fatalf("expected archive request for remote item 42: %v", err)
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
			_, _ = w.Write([]byte(`{"status":"authenticated","token":"mock-session-tok-123","user":{"id":7,"username":"alice"}}`))
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
	if eng.Config().UserID != 7 {
		t.Fatalf("expected persisted server user ID 7, got %d", eng.Config().UserID)
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

func TestMobileCanonicalOriginsAndSecureTransport(t *testing.T) {
	for _, raw := range []string{"http://192.168.1.2", "http://example.test", "http://100.64.0.1"} {
		if _, err := normalizeServerURL(raw); err == nil {
			t.Errorf("cleartext remote URL accepted: %s", raw)
		}
	}
	if !sameServerURL("https://EXAMPLE.test:443/base/", "https://example.test/base") {
		t.Fatal("default HTTPS ports were not equivalent")
	}
	if !sameServerURL("http://127.0.0.1:80/base", "http://127.0.0.1/base") {
		t.Fatal("default loopback HTTP ports were not equivalent")
	}
	if !sameServerURL("https://example.test:00443/base", "https://example.test/base") {
		t.Fatal("numeric default port was not canonicalized")
	}
	if _, err := normalizeServerURL("https://example.test:65536"); err == nil {
		t.Fatal("out-of-range server port accepted")
	}
	if sameServerURL("https://example.test/base", "https://example.test/other") {
		t.Fatal("distinct base paths merged")
	}
}

func TestObserverCanArchiveAfterSync(t *testing.T) {
	eng, err := NewEngineWithDir(test.NewApp(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[]}`))
	}))
	defer server.Close()
	if err := eng.UpdateConfig(server.URL, "", false, 30); err != nil {
		t.Fatal(err)
	}
	item, _, err := eng.Store().InsertItem(context.Background(), ingest.IncomingItem{SourceType: "rss", ExternalID: "observer-item"}, classify.Decision{Route: "queue_digest"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var once sync.Once
	result := make(chan error, 1)
	eng.SetOnStateChange(func() { once.Do(func() { result <- eng.MarkRead(ctx, item.ID) }) })
	synced := make(chan error, 1)
	go func() { synced <- eng.SyncWithServer(ctx) }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("observer deadlocked on the request mutex")
	}
	select {
	case err := <-synced:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("sync did not complete")
	}
}

func TestCanonicalCacheUpgradePreservesLegacyOwnership(t *testing.T) {
	dir := t.TempDir()
	eng, err := NewEngineWithDir(test.NewApp(), dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := MobileConfig{ServerURL: "https://EXAMPLE.test:443/base/", Username: "alice", UserID: 7, AuthToken: "legacy-token", PollMinutes: 30}
	ctx := context.Background()
	if err := eng.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := eng.Store().ActivateMobileAccount(ctx, legacyMobileCacheOwner(cfg)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng.Store().InsertItem(ctx, ingest.IncomingItem{SourceType: "rss", ExternalID: "old-cache-history"}, classify.Decision{Route: "queue_digest"}); err != nil {
		t.Fatal(err)
	}
	eng.Close()
	eng, err = NewEngineWithDir(test.NewApp(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if exists, err := eng.Store().Exists(ctx, "rss", "old-cache-history"); err != nil || !exists {
		t.Fatalf("canonicalization erased legacy cache: %v", err)
	}
	if err := eng.applyVerifiedAccount(ctx, "https://example.test/base", "alice", "new-token", 7, false, 30); err != nil {
		t.Fatal(err)
	}
	if exists, err := eng.Store().Exists(ctx, "rss", "old-cache-history"); err != nil || !exists {
		t.Fatalf("equivalent origin rotation erased cache: %v", err)
	}
}

func TestLegacyCacheMigrationChecksPreviousCredential(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		status               int
		owner                int
		wantCache, wantError bool
	}{
		{"same account", 200, 7, true, false},
		{"other account with same local username", 200, 8, false, false},
		{"expired credential", 401, 0, false, false},
		{"identity service unavailable", 503, 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/auth/me" || r.Header.Get("Authorization") != "Bearer old-token" {
					t.Errorf("unexpected identity request")
				}
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"user":{"id":%d}}`, tc.owner)
			}))
			defer server.Close()
			eng, err := NewEngineWithDir(test.NewApp(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			ctx := context.Background()
			if err := eng.UpdateUserConfig(server.URL, "alice", "old-token", false, 30); err != nil {
				t.Fatal(err)
			}
			if _, _, err := eng.Store().InsertItem(ctx, ingest.IncomingItem{SourceType: "rss", ExternalID: "history"}, classify.Decision{Route: "queue_digest"}); err != nil {
				t.Fatal(err)
			}
			err = eng.applyVerifiedAccount(ctx, server.URL, "alice", "renewed-token", 7, false, 30)
			if (err != nil) != tc.wantError {
				t.Fatalf("transition error: %v", err)
			}
			exists, err := eng.Store().Exists(ctx, "rss", "history")
			if err != nil || exists != tc.wantCache {
				t.Fatalf("cached=%v, err=%v", exists, err)
			}
			if tc.wantError && eng.Config().AuthToken != "old-token" {
				t.Fatal("failed lookup changed active credentials")
			}
		})
	}
}

func TestUnreadableAccountConfigPreservesCache(t *testing.T) {
	dir := t.TempDir()
	eng, err := NewEngineWithDir(test.NewApp(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.UpdateUserConfig("https://example.test", "alice", "existing", false, 30); err != nil {
		t.Fatal(err)
	}
	path := eng.cfgPath
	ctx := context.Background()
	if _, _, err := eng.Store().InsertItem(ctx, ingest.IncomingItem{SourceType: "rss", ExternalID: "keep"}, classify.Decision{Route: "queue_digest"}); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	for _, contents := range []string{"{", "null", `{"server_url":"http://192.168.1.2","auth_token":"existing","username":"alice"}`, ""} {
		if contents == "" {
			err = os.Remove(path)
		} else {
			err = os.WriteFile(path, []byte(contents), 0600)
		}
		if err != nil {
			t.Fatal(err)
		}
		if reopened, err := NewEngineWithDir(test.NewApp(), dir); err == nil {
			reopened.Close()
			t.Fatal("invalid configuration activated a different cache owner")
		}
	}
	if err := os.WriteFile(path, good, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewEngineWithDir(test.NewApp(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if exists, err := reopened.Store().Exists(ctx, "rss", "keep"); err != nil || !exists {
		t.Fatalf("history missing after recovery: %v", err)
	}
}

func TestModeRejectionPreservesConfirmedState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/mode" {
			t.Error("unexpected mode request")
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	eng, err := NewEngineWithDir(test.NewApp(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.UpdateConfig(server.URL, "token", true, 30); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = eng.setFocusMode(ctx, eng.account(), config.ModeDND)
	if !errors.Is(err, serverStatus(http.StatusForbidden)) {
		t.Fatalf("wanted rejection, got %v", err)
	}
	if mode, err := eng.Store().GetFocusMode(ctx, config.ModeNormal); err != nil || mode != config.ModeNormal {
		t.Fatalf("rejected mode persisted: %s (%v)", mode, err)
	}
	server.Close()
	if err := eng.setFocusMode(ctx, eng.account(), config.ModeDND); err == nil {
		t.Fatal("offline change should report server unavailable")
	}
	if mode, err := eng.Store().GetFocusMode(ctx, config.ModeNormal); err != nil || mode != config.ModeDND {
		t.Fatalf("offline mode=%s (%v)", mode, err)
	}
}

func TestLoginRejectsSupersededAccount(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			close(started)
			<-release
			fmt.Fprint(w, `{"token":"alice-session","user":{"id":7,"username":"alice"}}`)
			return
		}
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer server.Close()
	eng, err := NewEngineWithDir(test.NewApp(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- eng.LoginWithServer(ctx, server.URL, "alice", "password") }()
	select {
	case <-started:
	case <-ctx.Done():
		close(release)
		t.Fatal("login did not start")
	}
	if err := eng.UpdateUserConfig(server.URL, "bob", "bob-session", false, 1); err != nil {
		close(release)
		t.Fatal(err)
	}
	if _, _, err := eng.Store().InsertItem(ctx, ingest.IncomingItem{SourceType: "rss", ExternalID: "bob-history"}, classify.Decision{Route: "queue_digest"}); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, errAccountChanged) {
		t.Fatalf("stale login error = %v", err)
	}
	if eng.Config().Username != "bob" {
		t.Fatal("superseded login replaced Bob")
	}
	if exists, err := eng.Store().Exists(ctx, "rss", "bob-history"); err != nil || !exists {
		t.Fatalf("superseded login cleared Bob's cache: %v", err)
	}
}

func TestLoginCommitsSelectedPreferences(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			fmt.Fprint(w, `{"token":"alice-session","user":{"id":7,"username":"alice"}}`)
			return
		}
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer server.Close()
	eng, err := NewEngineWithDir(test.NewApp(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	committed, err := eng.loginWithServer(context.Background(), eng.account(), server.URL, "alice", "password", false, 1)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := loadMobileConfig(eng.cfgPath)
	if err != nil || persisted.UserID != 7 || persisted.AuthToken != committed.AuthToken || persisted.AutoOffline || persisted.PollMinutes != 1 {
		t.Fatalf("login and preferences were not persisted together: %+v (%v)", persisted, err)
	}
	select {
	case <-eng.pollChanged:
	default:
		t.Fatal("poll interval change did not wake the timer")
	}
}

func TestInterruptedVerifiedOwnerUpgradePreservesCache(t *testing.T) {
	dir := t.TempDir()
	eng, err := NewEngineWithDir(test.NewApp(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := eng.UpdateUserConfig("https://example.test", "alice", "old-token", false, 30); err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng.Store().InsertItem(ctx, ingest.IncomingItem{SourceType: "rss", ExternalID: "offline-history"}, classify.Decision{Route: "queue_digest"}); err != nil {
		t.Fatal(err)
	}
	previous, staged := eng.Config(), eng.Config()
	staged.UserID, staged.AuthToken = 7, "renewed-token"
	if err := eng.Store().PrepareMobileOwnerTransition(ctx, mobileCacheOwner(staged), mobileCacheOwner(previous)); err != nil {
		t.Fatal(err)
	}
	if err := eng.saveConfig(staged); err != nil {
		t.Fatal(err)
	}
	// Stop between publishing the verified config and activating its cache.
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	eng, err = NewEngineWithDir(test.NewApp(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if exists, err := eng.Store().Exists(ctx, "rss", "offline-history"); err != nil || !exists {
		t.Fatalf("interrupted verified upgrade erased history: %v", err)
	}
	if _, err := eng.Store().GetSetting(ctx, "mobile_cache_owner_transition"); err == nil {
		t.Fatal("completed upgrade retained its recovery journal")
	}
}

func TestInboxRowsRetainAccountAndClearOnFailedRefresh(t *testing.T) {
	eng, err := NewEngineWithDir(test.NewApp(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	ctx := context.Background()
	if err := eng.UpdateUserConfig("https://example.test", "alice", "alice-token", false, 30); err != nil {
		t.Fatal(err)
	}
	item, _, err := eng.Store().InsertItem(ctx, ingest.IncomingItem{SourceType: "rss", ExternalID: "alice-item", URL: "https://example.test/alice"}, classify.Decision{Route: "queue_digest"})
	if err != nil {
		t.Fatal(err)
	}
	ui := &UI{engine: eng, modeLabel: widget.NewLabel("")}
	ui.refreshItems()
	owner := ui.itemsAccount
	if len(ui.items) != 1 || owner.Username != "alice" {
		t.Fatal("rows were not bound to Alice")
	}
	if err := eng.UpdateUserConfig("https://example.test", "bob", "bob-token", false, 30); err != nil {
		t.Fatal(err)
	}
	if err := eng.markRead(ctx, owner, item.ID); !errors.Is(err, errAccountChanged) {
		t.Fatalf("stale row action was accepted: %v", err)
	}
	if err := eng.Store().Close(); err != nil {
		t.Fatal(err)
	}
	ui.refreshItems()
	if len(ui.items) != 0 || ui.modeLabel.Text != "MODE: —" {
		t.Fatal("failed refresh retained the previous account's rows or mode")
	}
}

func TestMissingServerItemCanArchiveLocally(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			eng, err := NewEngineWithDir(test.NewApp(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			if err := eng.UpdateConfig(server.URL, "token", false, 30); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := eng.Store().ImportMobileItems(ctx, []store.StoredItem{{ID: 42, SourceType: "rss", ExternalID: "remote-item", Route: "queue_digest", Status: "queued"}}); err != nil {
				t.Fatal(err)
			}
			items, err := eng.Store().ListItems(ctx, store.ItemFilter{})
			if err != nil || len(items) != 1 {
				t.Fatal(err)
			}
			err = eng.MarkRead(ctx, items[0].ID)
			mapping, lookupErr := eng.Store().MobileServerID(ctx, items[0].ID)
			if lookupErr != nil {
				t.Fatal(lookupErr)
			}
			updated, lookupErr := eng.Store().ListItems(ctx, store.ItemFilter{})
			if lookupErr != nil || len(updated) != 1 {
				t.Fatal(lookupErr)
			}
			if status == http.StatusNotFound {
				if err != nil || mapping != 0 || updated[0].Status != "archived" {
					t.Fatalf("missing item reconciliation: mapping=%d status=%s err=%v", mapping, updated[0].Status, err)
				}
			} else if err == nil || mapping != 42 || updated[0].Status != "queued" {
				t.Fatalf("failure was mistaken for deletion: mapping=%d status=%s err=%v", mapping, updated[0].Status, err)
			}
		})
	}
}
