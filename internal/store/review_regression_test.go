package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
)

func TestLegacyIdentityUpgradeAndLosslessInboxKeys(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	first := ingest.IncomingItem{SourceType: "rss", SourceName: "a|b", URL: "c", Title: "title", Body: "body"}
	first.Normalize(1200)
	legacy, _, err := st.InsertItem(ctx, ingest.IncomingItem{SourceType: "rss", ExternalID: first.LegacyExternalID, SourceName: first.SourceName, Title: first.Title, Sender: first.Sender, URL: first.URL}, classify.Decision{Route: "queue_digest"})
	if err != nil {
		t.Fatal(err)
	}
	if exists, err := st.ReconcileIncomingItem(ctx, 1, first); err != nil || !exists {
		t.Fatalf("legacy replay missed: %v %v", exists, err)
	}
	rows, err := st.ListItems(ctx, ItemFilter{UserID: 1})
	if err != nil || len(rows) != 1 || rows[0].ID != legacy.ID || rows[0].ExternalID != first.ExternalID {
		t.Fatalf("legacy row not retained: %v %v", rows, err)
	}
	second := ingest.IncomingItem{SourceType: "rss", SourceName: "a", URL: "b|c", Title: "title", Body: "body"}
	second.Normalize(1200)
	if exists, err := st.ReconcileIncomingItem(ctx, 1, second); err != nil || exists {
		t.Fatalf("legacy alias merged distinct v2 item: %v %v", exists, err)
	}
	for i, suffix := range []string{"A", "B"} {
		raw := strings.Repeat("x", 4096) + suffix
		row, _, err := st.InsertItem(ctx, ingest.IncomingItem{SourceType: "rss", ExternalID: suffix}, classify.Decision{Route: "queue_digest"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `UPDATE items SET external_id=? WHERE id=?`, raw, row.ID); err != nil {
			t.Fatal(err)
		}
		listed, err := st.ListItems(ctx, ItemFilter{UserID: 1})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range listed {
			if r.ID == row.ID {
				found = r.ExternalID == raw
			}
		}
		if !found {
			t.Fatalf("legacy key %d truncated", i)
		}
		incoming := ingest.IncomingItem{SourceType: "rss", ExternalID: raw}
		incoming.Normalize(1200)
		if exists, err := st.ReconcileIncomingItem(ctx, 1, incoming); err != nil || !exists {
			t.Fatalf("oversized provider replay missed: %v %v", exists, err)
		}
	}
}

func TestPendingItemRequiresCurrentSubscriptionAndBoundedFeedBatch(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.ListDueUserFeeds(ctx, time.Now(), -1); err == nil {
		t.Fatal("negative batch accepted")
	}
	var routed bool
	_, inserted, err := st.InsertPendingItem(ctx, 1, ingest.IncomingItem{SourceType: "rss", ExternalID: "deleted-feed", UserFeedID: 42}, config.ModeNormal, func(config.FocusMode) classify.Decision {
		routed = true
		return classify.Decision{Route: "queue_digest"}
	})
	if err != nil || inserted || routed {
		t.Fatalf("deleted subscription committed: inserted=%v routed=%v err=%v", inserted, routed, err)
	}
}

func TestGitHubCredentialSnapshotContainsDisconnect(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.SetGitHubCredentials(ctx, "token", "{}", true); err != nil {
		t.Fatal(err)
	}
	state, err := st.GetGitHubCredentialState(ctx)
	if err != nil || state.Disabled || state.Token != "token" {
		t.Fatalf("connected snapshot: %+v %v", state, err)
	}
	if err := st.ClearGitHubCredentials(ctx); err != nil {
		t.Fatal(err)
	}
	state, err = st.GetGitHubCredentialState(ctx)
	if err != nil || !state.Disabled || state.Token != "" {
		t.Fatalf("disconnected snapshot: %+v %v", state, err)
	}
}

func TestLegacyIMAPReplayRetainsRowAndPortIsolation(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	legacyID := "imap:mail.example|alice|INBOX:5:12"
	old, inserted, err := st.InsertItem(ctx, ingest.IncomingItem{SourceType: "imap", SourceName: "Personal", ExternalID: legacyID}, classify.Decision{Route: "queue_digest"})
	if err != nil || !inserted {
		t.Fatalf("legacy fixture: %v %v", inserted, err)
	}
	item := ingest.IncomingItem{SourceType: "imap", SourceName: "Other account", ExternalID: "imap:mail.example:1993|alice|INBOX:5:12", LegacyExternalID: legacyID}
	if exists, err := st.ReconcileIncomingItem(ctx, 1, item); err != nil || exists {
		t.Fatalf("different mailbox binding reused old row: %v %v", exists, err)
	}
	item.SourceName = "Personal"
	if exists, err := st.ReconcileIncomingItem(ctx, 1, item); err != nil || !exists {
		t.Fatalf("replayed custom-port message was not reconciled: %v %v", exists, err)
	}
	rows, err := st.ListItems(ctx, ItemFilter{})
	if err != nil || len(rows) != 1 || rows[0].ID != old.ID {
		t.Fatalf("legacy row lost: %+v %v", rows, err)
	}
	item.ExternalID = "imap:mail.example:2993|alice|INBOX:5:12"
	if exists, err := st.ReconcileIncomingItem(ctx, 1, item); err != nil || exists {
		t.Fatalf("new port merged with upgraded item: %v %v", exists, err)
	}
}
