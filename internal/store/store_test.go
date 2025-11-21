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
