package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

func TestEngine_ProcessDeduplicateAndFlushDigest(t *testing.T) {
	var ntfyHits atomic.Int32
	ntfySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

func TestNextDigestSlot(t *testing.T) {
	loc := time.FixedZone("UTC+530", 5*3600+30*60)
	now := time.Date(2026, 11, 12, 14, 0, 0, 0, loc)
	got, label, ok := NextDigestSlot(now, []string{"19:30", "09:00", "13:30", "bad"})
	if !ok || label != "19:30" {
		t.Fatalf("expected next slot 19:30, got ok=%v label=%q", ok, label)
	}
	if got.Day() != 12 || got.Hour() != 19 || got.Minute() != 30 {
		t.Fatalf("unexpected same-day slot time: %v", got)
	}

	late := time.Date(2026, 11, 12, 22, 0, 0, 0, loc)
	got2, label2, ok2 := NextDigestSlot(late, []string{"19:30", "09:00"})
	if !ok2 || label2 != "09:00" || got2.Day() != 13 {
		t.Fatalf("expected next-day wraparound 09:00 on Nov 13, got ok=%v label=%q time=%v", ok2, label2, got2)
	}

	if _, _, ok3 := NextDigestSlot(now, nil); ok3 {
		t.Fatal("expected ok=false for empty digest schedule")
	}
}

func TestEngine_ModeSwitchDuringClassificationRoutesUnderNewMode(t *testing.T) {
	reqReceived := make(chan struct{})
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	releaseProceed := func() {
		proceedOnce.Do(func() {
			close(proceed)
		})
	}

	tsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(reqReceived)
		<-proceed
		w.Header().Set("Content-Type", "application/json")
		// Return valid TypeSafe response: direct message, high actionability, normal urgency.
		// Under 'normal' mode this routes to push_now.
		// Under 'dnd' mode this routes to queue_digest.
		w.Write([]byte(`{
			"model": "typesafe/system-one",
			"answers": {
				"is_actionable": {"type": "noul", "noul": 0.85},
				"is_time_sensitive": {"type": "noul", "noul": 0.75},
				"category": {
					"type": "choice",
					"choice": "direct_message",
					"confidence": 0.90,
					"probabilities": {"direct_message": 0.90}
				},
				"urgency": {"type": "score", "score": 1.5, "confidence": 0.80}
			}
		}`))
	}))
	defer tsSrv.Close()
	defer releaseProceed()

	cfg := config.Default()
	cfg.Storage.Path = ":memory:"
	cfg.Classifier.Mode = "hybrid"
	cfg.Classifier.TypeSafeBaseURL = tsSrv.URL
	cfg.Classifier.TypeSafeAPIKey = "ts-test-key"
	cfg.Classifier.MaxDailyAPICalls = 100
	cfg.Focus.DefaultMode = config.ModeNormal
	cfg.Notify.DesktopWindows.Enabled = false

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
	if eng.Mode() != config.ModeNormal {
		t.Fatalf("expected initial mode normal, got %v", eng.Mode())
	}

	item := ingest.IncomingItem{
		ExternalID: "msg-pending-mode-switch",
		SourceType: "webhook",
		SourceName: "chat",
		Title:      "Direct ping from lead",
		Body:       "Can we sync on the release timeline?",
	}

	type result struct {
		stored   store.StoredItem
		inserted bool
		err      error
	}
	resCh := make(chan result, 1)

	go func() {
		stored, inserted, err := eng.ProcessItem(ctx, item)
		resCh <- result{stored: stored, inserted: inserted, err: err}
	}()

	// Wait until TypeSafe client is actively evaluating the item
	select {
	case <-reqReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TypeSafe request")
	}

	// Switch mode to DND while classification is in flight
	if err := eng.SetMode(ctx, config.ModeDND); err != nil {
		t.Fatalf("SetMode DND failed: %v", err)
	}
	if eng.Mode() != config.ModeDND {
		t.Fatalf("expected mode DND, got %v", eng.Mode())
	}

	// Allow TypeSafe response to complete
	releaseProceed()

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("ProcessItem failed: %v", res.err)
		}
		if !res.inserted {
			t.Fatal("expected item to be inserted")
		}
		// Because mode changed to DND while request was pending,
		// routing must evaluate under DND -> queue_digest, NOT normal -> push_now.
		if res.stored.Route != "queue_digest" {
			t.Fatalf("expected route queue_digest under DND, got %q", res.stored.Route)
		}
		if res.stored.DecisionSource != "typesafe" {
			t.Fatalf("expected decision source typesafe, got %q", res.stored.DecisionSource)
		}
		if res.stored.Status != "queued" {
			t.Fatalf("expected status queued, got %q", res.stored.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ProcessItem to finish")
	}
}

func TestScheduledDigestRetriesAfterScheduledMinute(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cfg := config.Default()
	cfg.Classifier.Mode = "rules_only"
	cfg.Focus.DigestTimes = []string{"09:00"}
	cfg.Notify.DesktopWindows.Enabled = false
	cfg.Notify.Ntfy = config.NtfyConfig{Enabled: true, Server: server.URL, Topic: "test"}
	cfg.Rules = []config.RuleConfig{{Name: "digest", SourceType: "rss", Action: "queue_digest", Priority: 2}}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	eng, err := NewEngine(context.Background(), cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng.ProcessItem(context.Background(), ingest.IncomingItem{SourceType: "rss", ExternalID: "due", Title: "Digest"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	// The laptop was asleep through the scheduled minute. The first tick
	// after wake should attempt the missed slot and retain it on failure.
	eng.maybeRunDigest(context.Background(), now.Add(-time.Minute))
	now = now.Add(2 * time.Minute)
	eng.maybeRunDigest(context.Background(), now)
	if eng.pendingDigestSlot == "" || eng.lastDigestSlot != "" {
		t.Fatal("failed scheduled slot was lost")
	}
	eng.maybeRunDigest(context.Background(), now.Add(30*time.Second))
	if requests.Load() != 1 {
		t.Fatal("retry backoff was ignored")
	}
	eng.maybeRunDigest(context.Background(), now.Add(2*time.Minute))
	if requests.Load() != 2 || eng.pendingDigestSlot != "" || eng.lastDigestSlot == "" {
		t.Fatalf("due slot was not retried outside its minute: %d", requests.Load())
	}
}

func TestCanceledItemLeavesBusyIngestionGate(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	cfg.Notify.DesktopWindows.Enabled = false
	e, err := NewEngine(context.Background(), cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	e.processGate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := e.ProcessItem(ctx, ingest.IncomingItem{SourceType: "webhook", ExternalID: "canceled"})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("unexpected canceled result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request retained ingestion slot")
	}
	<-e.processGate
	if _, inserted, err := e.ProcessItem(context.Background(), ingest.IncomingItem{SourceType: "webhook", ExternalID: "next"}); err != nil || !inserted {
		t.Fatalf("next item blocked: %v %v", inserted, err)
	}
}
