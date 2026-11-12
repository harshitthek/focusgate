package store

import (
	"context"
	"testing"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
)

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
