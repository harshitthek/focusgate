package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"modernc.org/sqlite"
)

func TestDeliveryClaimIsExclusiveAndRetryable(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	item, _, err := st.InsertItem(ctx, ingest.IncomingItem{ExternalID: "lease", SourceType: "webhook"},
		classify.Decision{Route: "queue_digest"})
	if err != nil {
		t.Fatal(err)
	}
	lease, done, err := st.ClaimDelivery(ctx, item.ID, "channel")
	if err != nil || done || lease == "" {
		t.Fatalf("claim failed: %q %v (%v)", lease, done, err)
	}
	if _, _, err := st.ClaimDelivery(ctx, item.ID, "channel"); err == nil {
		t.Fatal("concurrent sender claimed an active delivery")
	}
	if err := st.FinishDelivery(ctx, item.ID, "channel", lease, false); err != nil {
		t.Fatal(err)
	}
	retry, done, err := st.ClaimDelivery(ctx, item.ID, "channel")
	if err != nil || done || retry == lease {
		t.Fatalf("failed delivery was not retryable: %v (%v)", done, err)
	}
	if err := st.FinishDelivery(ctx, item.ID, "channel", retry, true); err != nil {
		t.Fatal(err)
	}
	if _, done, err := st.ClaimDelivery(ctx, item.ID, "channel"); err != nil || !done {
		t.Fatalf("completed receipt was not retained: %v (%v)", done, err)
	}
}

func TestReserveDailyUsageRejectsOlderDay(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if ok, err := st.ReserveDailyUsage(ctx, "20261116", 2, 10, 5); err != nil || !ok {
		t.Fatalf("reserve current day: %v (%v)", ok, err)
	}
	if ok, err := st.ReserveDailyUsage(ctx, "20261115", 2, 10, 5); err != nil || ok {
		t.Fatalf("reserve older day: %v (%v)", ok, err)
	}
	for _, id := range []int64{1, 2} {
		if used, err := st.GetUserDailyUsage(ctx, "20261116", id); err != nil || used != 1 {
			t.Fatalf("account %d current-day usage: %d (%v)", id, used, err)
		}
	}
}

func TestStore_DeduplicationModeAndDigest(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open memory store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if err := st.SetFocusMode(ctx, config.ModeDeepWork); err != nil {
		t.Fatalf("SetFocusMode: %v", err)
	}
	mode, err := st.GetFocusMode(ctx, config.ModeNormal)
	if err != nil || mode != config.ModeDeepWork {
		t.Fatalf("expected deep_work, got %v (%v)", mode, err)
	}

	item := ingest.IncomingItem{
		ExternalID: "item-1",
		SourceType: "rss",
		SourceName: "Go Blog",
		Title:      "Memory Tuning",
		Body:       "Details on GOMEMLIMIT",
	}
	item.Normalize(200)
	dec := classify.Decision{
		Route:      "queue_digest",
		Priority:   2,
		Confidence: 0.9,
		Source:     "rule",
		Reason:     "Matched RSS rule",
	}

	stored, inserted, err := st.InsertItem(ctx, item, dec)
	if err != nil || !inserted || stored.Status != "queued" {
		t.Fatalf("expected first insert queued, got inserted=%v stored=%+v err=%v", inserted, stored, err)
	}

	// Duplicate external_id must be ignored cleanly.
	_, insertedAgain, err := st.InsertItem(ctx, item, dec)
	if err != nil || insertedAgain {
		t.Fatalf("expected duplicate ignored, got insertedAgain=%v err=%v", insertedAgain, err)
	}

	queued, err := st.ListQueuedForDigest(ctx, 10)
	if err != nil || len(queued) != 1 {
		t.Fatalf("expected 1 queued item, got %d (%v)", len(queued), err)
	}

	if err := st.MarkDigested(ctx, []int64{stored.ID}); err != nil {
		t.Fatalf("MarkDigested: %v", err)
	}
	queuedAfter, err := st.ListQueuedForDigest(ctx, 10)
	if err != nil || len(queuedAfter) != 0 {
		t.Fatalf("expected 0 queued items after digest, got %d", len(queuedAfter))
	}
}

func TestStore_PruneOldOverflowAndListFilter(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open memory store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	// Insert item with old created_at
	item1 := ingest.IncomingItem{
		ExternalID: "old-queued-1",
		SourceType: "github",
		SourceName: "org/repo",
		Title:      "Outage incident",
		Body:       "Core API returning 500",
	}
	item1.Normalize(200)
	stored1, _, err := st.InsertItem(ctx, item1, classify.Decision{Route: "queue_digest", Priority: 3, Reason: "Digest rule"})
	if err != nil {
		t.Fatalf("insert item1: %v", err)
	}

	// Manually backdate created_at to 40 days ago
	_, err = st.db.ExecContext(ctx, `UPDATE items SET created_at = datetime('now', '-40 days') WHERE id = ?`, stored1.ID)
	if err != nil {
		t.Fatalf("backdate item1: %v", err)
	}

	// Prune with 30 days retention: undelivered queued item must become 'archived' with backlog note
	if _, err := st.PruneOld(ctx, 30); err != nil {
		t.Fatalf("PruneOld failed: %v", err)
	}

	// Verify item1 is now archived with explanatory reason
	var status, reason string
	if err := st.db.QueryRowContext(ctx, `SELECT status, decision_reason FROM items WHERE id = ?`, stored1.ID).Scan(&status, &reason); err != nil {
		t.Fatalf("query item1 after prune: %v", err)
	}
	if status != "archived" {
		t.Fatalf("expected overflow transition to archived, got %q", status)
	}

	// Test ListItems with search query and source filter
	results, err := st.ListItems(ctx, ItemFilter{
		Status: "archived",
		Source: "github",
		Query:  "outage",
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("ListItems filter: %v", err)
	}
	if len(results) != 1 || results[0].ID != stored1.ID {
		t.Fatalf("expected 1 result matching filter, got %d", len(results))
	}

	// Test DailyUsage persistence
	if count, err := st.GetDailyUsage(ctx, "20261113"); err != nil || count != 0 {
		t.Fatalf("expected initial daily usage 0, got %d (err: %v)", count, err)
	}
	if err := st.SetDailyUsage(ctx, "20261113", 42); err != nil {
		t.Fatalf("SetDailyUsage failed: %v", err)
	}
	if count, err := st.GetDailyUsage(ctx, "20261113"); err != nil || count != 42 {
		t.Fatalf("expected daily usage 42, got %d (err: %v)", count, err)
	}
	// Different day key returns 0
	if count, err := st.GetDailyUsage(ctx, "20261114"); err != nil || count != 0 {
		t.Fatalf("expected daily usage 0 for different day, got %d", count)
	}

	// Test BulkUpdateStatus wildcard safeguards
	if _, err := st.BulkUpdateStatus(ctx, nil, "all", "queued"); err == nil {
		t.Fatal("expected error on wildcard bulk update to non-terminal status")
	}
	if n, err := st.BulkUpdateStatus(ctx, nil, "all", "read"); err != nil {
		t.Fatalf("wildcard bulk update to read failed: %v", err)
	} else if n < 1 {
		t.Fatalf("expected at least 1 row updated to read, got %d", n)
	}
}

func TestMobileOwnerMigrationAndMissingItem(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	item, _, err := st.InsertItem(ctx, ingest.IncomingItem{ExternalID: "legacy-offline", SourceType: "rss"}, classify.Decision{Route: "queue_digest"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InitMobileCache(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.ActivateMobileAccount(ctx, "current-owner"); err != nil {
		t.Fatal(err)
	}
	if exists, err := st.Exists(ctx, "rss", "legacy-offline"); err != nil || !exists {
		t.Fatalf("owner migration deleted existing cache: %v", err)
	}
	if err := st.UpdateUserItemStatus(ctx, 2, item.ID, "archived"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("another owner's item was accepted: %v", err)
	}
	if err := st.UpdateUserItemStatus(ctx, 1, item.ID+1, "archived"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("missing item was accepted: %v", err)
	}
	if err := st.UpdateUserItemStatus(ctx, 1, item.ID, "archived"); err != nil {
		t.Fatal(err)
	}
	if err := st.ActivateMobileAccount(ctx, "different-owner"); err != nil {
		t.Fatal(err)
	}
	if exists, err := st.Exists(ctx, "rss", "legacy-offline"); err != nil || exists {
		t.Fatalf("account change retained prior history: %v", err)
	}
}

func TestPendingPushIsHeldFromDigestAndRecoversAfterCrash(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	item, inserted, err := st.InsertPendingItem(ctx, 1, ingest.IncomingItem{ExternalID: "held-push", SourceType: "webhook"}, config.ModeNormal, func(config.FocusMode) classify.Decision {
		return classify.Decision{Route: "push_now"}
	})
	if err != nil || !inserted {
		t.Fatalf("insert: %v %v", inserted, err)
	}
	if queued, err := st.ListQueuedForDigest(ctx, 50); err != nil || len(queued) != 0 {
		t.Fatalf("digest took in-flight push: %v %v", queued, err)
	}
	if inbox, err := st.ListItems(ctx, ItemFilter{UserID: 1, Status: "queued"}); err != nil || len(inbox) != 1 {
		t.Fatalf("pending push disappeared from inbox: %v %v", inbox, err)
	}
	if _, err := st.FinishInitialPush(ctx, item.ID, false); err != nil {
		t.Fatal(err)
	}
	if queued, err := st.ListQueuedForDigest(ctx, 50); err != nil || len(queued) != 1 {
		t.Fatalf("failed immediate send not retryable: %v %v", queued, err)
	}
	// A crashed sender never calls FinishInitialPush.
	crashed, inserted, err := st.InsertPendingItem(ctx, 1, ingest.IncomingItem{ExternalID: "crashed-push", SourceType: "webhook"}, config.ModeNormal, func(config.FocusMode) classify.Decision {
		return classify.Decision{Route: "push_now"}
	})
	if err != nil || !inserted {
		t.Fatalf("crash fixture: %v %v", inserted, err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE items SET initial_push_until=1 WHERE id=?`, crashed.ID); err != nil {
		t.Fatal(err)
	}
	queued, err := st.ListQueuedForDigest(ctx, 50)
	found := false
	for _, row := range queued {
		found = found || row.ID == crashed.ID
	}
	if err != nil || !found {
		t.Fatalf("expired crash hold was not recoverable: %v %v", queued, err)
	}
	if _, err := st.FinishInitialPush(ctx, crashed.ID, true); err != nil {
		t.Fatal(err)
	}

	if _, err := st.FinishInitialPush(ctx, item.ID, true); err != nil {
		t.Fatal(err)
	}
	if queued, err := st.ListQueuedForDigest(ctx, 50); err != nil || len(queued) != 0 {
		t.Fatalf("completed push remained in digest: %v %v", queued, err)
	}
}

func TestRoutingTransactionSerializesIndependentModeWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "focusgate.sqlite")
	daemon, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	cli, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	// Make the independent writer report lock contention immediately.
	if _, err := cli.db.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		t.Fatal(err)
	}
	item := ingest.IncomingItem{ExternalID: "during-mode-change", SourceType: "webhook"}
	_, inserted, err := daemon.InsertPendingItem(ctx, 1, item, config.ModeNormal, func(mode config.FocusMode) classify.Decision {
		if mode != config.ModeNormal {
			t.Fatalf("initial mode = %q", mode)
		}
		writeErr := cli.SetFocusMode(ctx, config.ModeDND)
		var busy *sqlite.Error
		if !errors.As(writeErr, &busy) || busy.Code() != 5 { // SQLITE_BUSY
			t.Fatalf("independent mode write during final routing = %v, want SQLITE_BUSY", writeErr)
		}
		return classify.Decision{Route: "push_now"}
	})
	if err != nil || !inserted {
		t.Fatalf("inserted = %v, error = %v", inserted, err)
	}
	if err := cli.SetFocusMode(ctx, config.ModeDND); err != nil {
		t.Fatalf("mode update after commit: %v", err)
	}
	item.ExternalID = "after-mode-change"
	_, inserted, err = daemon.InsertPendingItem(ctx, 1, item, config.ModeNormal, func(mode config.FocusMode) classify.Decision {
		if mode != config.ModeDND {
			t.Fatalf("mode after independent write = %q", mode)
		}
		return classify.Decision{Route: "queue_digest"}
	})
	if err != nil || !inserted {
		t.Fatalf("second insert = %v, error = %v", inserted, err)
	}
}
