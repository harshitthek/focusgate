package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

func TestEngine_ProcessDeduplicateAndFlushDigest(t *testing.T) {
	var ntfyHits atomic.Int32
	ntfySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfySrv.Close()

	cfg := config.Default()
	cfg.Storage.Path = ":memory:"
	cfg.Classifier.Mode = "rules_only"
	cfg.Notify.DesktopWindows.Enabled = false
	cfg.Notify.Ntfy = config.NtfyConfig{
		Enabled: true,
		Server:  ntfySrv.URL,
		Topic:   "test-topic",
	}
	cfg.Rules = []config.RuleConfig{
		{Name: "RSS Digest", SourceType: "rss", Action: "queue_digest", Priority: 2},
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	eng, err := NewEngine(ctx, cfg, st)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	item := ingest.IncomingItem{
		ExternalID: "rss-42",
		SourceType: "rss",
		SourceName: "TechFeed",
		Title:      "Low-RAM Go Services",
		Body:       "Using SetMemoryLimit effectively",
	}

	stored, inserted, err := eng.ProcessItem(ctx, item)
	if err != nil || !inserted || stored.Status != "queued" {
		t.Fatalf("expected first ProcessItem queued, got inserted=%v stored=%+v err=%v", inserted, stored, err)
	}
	// Duplicate must be skipped before dispatch.
	_, insertedDup, err := eng.ProcessItem(ctx, item)
	if err != nil || insertedDup {
		t.Fatalf("expected duplicate skipped, got inserted=%v err=%v", insertedDup, err)
	}
	if ntfyHits.Load() != 0 {
		t.Fatalf("expected 0 immediate pushes for queued item, got %d", ntfyHits.Load())
	}

	flushed, err := eng.FlushDigest(ctx)
	if err != nil || flushed != 1 {
		t.Fatalf("expected 1 digested item, got flushed=%d err=%v", flushed, err)
	}
	if ntfyHits.Load() != 1 {
		t.Fatalf("expected 1 ntfy digest push, got %d", ntfyHits.Load())
	}

	// Simulate an out-of-process CLI mode update written directly to SQLite.
	if err := st.SetFocusMode(ctx, config.ModeDND); err != nil {
		t.Fatalf("SetFocusMode on store: %v", err)
	}
	if got := eng.Mode(); got != config.ModeDND {
		t.Fatalf("expected Engine.Mode() to refresh to dnd, got %v", got)
	}

	// Verify FlushDigest leaves items queued when all enabled channels fail.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()

	failCfg := cfg
	failCfg.Notify.Ntfy.Server = failSrv.URL
	failEng, err := NewEngine(ctx, failCfg, st)
	if err != nil {
		t.Fatalf("NewEngine failCfg: %v", err)
	}
	item2 := ingest.IncomingItem{
		ExternalID: "rss-43",
		SourceType: "rss",
		SourceName: "TechFeed",
		Title:      "Retry Safety",
		Body:       "Should stay queued on failed push",
	}
	if _, ins, err := failEng.ProcessItem(ctx, item2); err != nil || !ins {
		t.Fatalf("ProcessItem item2: ins=%v err=%v", ins, err)
	}
	if flushed2, err := failEng.FlushDigest(ctx); err == nil || flushed2 != 0 {
		t.Fatalf("expected FlushDigest error and 0 flushed, got flushed=%d err=%v", flushed2, err)
	}
	stillQueued, err := st.ListQueuedForDigest(ctx, 10)
	if err != nil || len(stillQueued) != 1 {
		t.Fatalf("expected item2 to stay queued after delivery failure, got %d (%v)", len(stillQueued), err)
	}
}
