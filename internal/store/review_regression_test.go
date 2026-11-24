package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

func TestFeedScopedLegacyCollisionChecksOriginalIdentity(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	first := ingest.IncomingItem{SourceType: "rss", SourceName: "a|b", URL: "c", Title: "title", Body: "body"}
	second := ingest.IncomingItem{SourceType: "rss", SourceName: "a", URL: "b|c", Title: "title", Body: "body"}
	for _, item := range []*ingest.IncomingItem{&first, &second} {
		item.Normalize(1200)
		item.ExternalID = "uf:7:" + item.ExternalID
		item.LegacyExternalID = "uf:7:" + item.LegacyExternalID
		item.Normalize(1200)
	}
	if first.LegacyExternalID != second.LegacyExternalID || first.ExternalID == second.ExternalID {
		t.Fatal("fixture does not exercise a legacy collision")
	}
	legacy, _, err := st.InsertItem(ctx, ingest.IncomingItem{SourceType: first.SourceType, ExternalID: first.LegacyExternalID, SourceName: first.SourceName, Title: first.Title, Sender: first.Sender, URL: first.URL}, classify.Decision{Route: "queue_digest"})
	if err != nil {
		t.Fatal(err)
	}
	// The conflicting item arrives before the actual legacy replay.
	if exists, err := st.ReconcileIncomingItem(ctx, 1, second); err != nil || exists {
		t.Fatalf("feed prefix bypassed collision checks: %v %v", exists, err)
	}
	if exists, err := st.Exists(ctx, first.SourceType, first.LegacyExternalID); err != nil || !exists {
		t.Fatalf("mismatching item consumed legacy key: %v %v", exists, err)
	}
	if exists, err := st.ReconcileIncomingItem(ctx, 1, first); err != nil || !exists {
		t.Fatalf("matching feed replay missed legacy row: %v %v", exists, err)
	}
	rows, err := st.ListItems(ctx, ItemFilter{})
	if err != nil || len(rows) != 1 || rows[0].ID != legacy.ID || rows[0].ExternalID != first.ExternalID {
		t.Fatalf("matching replay did not preserve row: %+v %v", rows, err)
	}
}

func TestLegacyComparisonSurvivesStorageBoundsAndRepeatedNormalization(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	for _, source := range []string{"rss", "imap"} {
		original := ingest.LegacyIdentity{SourceName: strings.Repeat("source", 30), Title: strings.Repeat("x", 600) + "|tail", Sender: strings.Repeat("sender", 70), URL: "https://example.com/" + strings.Repeat("x", 2048)}
		item := ingest.IncomingItem{SourceType: source, SourceName: original.SourceName, Title: original.Title, Sender: original.Sender, URL: original.URL, Body: "body"}
		if source == "imap" {
			item.ExternalID, item.LegacyExternalID = "imap:new:5:21", "imap:old:5:21"
		}
		item.Normalize(1200)
		item.Normalize(1200)
		if item.LegacyIdentity == nil || *item.LegacyIdentity != original || utf8.RuneCountInString(item.Title) > 512 || utf8.RuneCountInString(item.SourceName) > 128 || item.URL != "" {
			t.Fatalf("%s lost original comparison or storage bounds: %+v", source, item)
		}
		row, _, err := st.InsertItem(ctx, ingest.IncomingItem{SourceType: source, ExternalID: item.LegacyExternalID}, classify.Decision{Route: "queue_digest"})
		if err != nil {
			t.Fatal(err)
		}
		// Install the original unbounded fields exactly as the pre-upgrade store kept them.
		if _, err := st.db.ExecContext(ctx, `UPDATE items SET source_name=?, title=?, sender=?, url=? WHERE id=?`, original.SourceName, original.Title, original.Sender, original.URL, row.ID); err != nil {
			t.Fatal(err)
		}
		if exists, err := st.ReconcileIncomingItem(ctx, 1, item); err != nil || !exists {
			t.Fatalf("%s long-field replay missed: %v %v", source, exists, err)
		}
		var id int64
		if err := st.db.QueryRowContext(ctx, `SELECT id FROM items WHERE source_type=? AND external_id=?`, source, item.ExternalID).Scan(&id); err != nil || id != row.ID {
			t.Fatalf("%s replay replaced original row: %d %v", source, id, err)
		}
	}
}

func TestIMAPLegacyBindingComesOnlyFromOriginalSchema(t *testing.T) {
	for _, originalSchema := range []bool{false, true} {
		t.Run(fmt.Sprint(originalSchema), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy-binding.db")
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if err := st.SaveFeedState(ctx, FeedSyncState{SourceKey: "imap:original-1993", ETag: "5", LastUID: 20}); err != nil {
				t.Fatal(err)
			}
			var ddl string
			if err := st.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name='items'`).Scan(&ddl); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.ExecContext(ctx, `ALTER TABLE feed_state DROP COLUMN legacy_imap_binding`); err != nil {
				t.Fatal(err)
			}
			if originalSchema {
				legacyDDL := strings.Replace(ddl, "user_id INTEGER NOT NULL DEFAULT 1,", "", 1)
				legacyDDL = strings.Replace(legacyDDL, "UNIQUE(user_id, source_type, external_id)", "UNIQUE(source_type, external_id)", 1)
				if legacyDDL == ddl {
					t.Fatal("fixture did not create original items schema")
				}
				for _, statement := range []string{`DROP TABLE items`, legacyDDL} {
					if _, err := raw.ExecContext(ctx, statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			old, err := st.GetFeedState(ctx, "imap:original-1993")
			if err != nil || old.LegacyIMAPBinding != originalSchema {
				t.Fatalf("old endpoint binding=%v, want %v (%v)", old.LegacyIMAPBinding, originalSchema, err)
			}
			// A new endpoint's first poll saves the same UIDVALIDITY and 20 messages.
			if err := st.SaveFeedState(ctx, FeedSyncState{SourceKey: "imap:new-2993", ETag: "5", LastUID: 20, LegacyIMAPBinding: true}); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveFeedState(ctx, FeedSyncState{SourceKey: old.SourceKey, ETag: "5", LastUID: 40}); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			for key, want := range map[string]bool{"imap:original-1993": originalSchema, "imap:new-2993": false} {
				state, err := st.GetFeedState(ctx, key)
				if err != nil || state.LegacyIMAPBinding != want {
					t.Fatalf("persisted binding for %s=%v, want %v (%v)", key, state.LegacyIMAPBinding, want, err)
				}
			}
		})
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

func TestDefaultIMAPEndpointRequiresLegacyBindingForReplay(t *testing.T) {
	for _, legacyEndpoint := range []bool{false, true} {
		t.Run(fmt.Sprintf("original_default_endpoint=%v", legacyEndpoint), func(t *testing.T) {
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
			// Both original 993 and original 1993 endpoints used this portless key.
			// Only a proven original 993 binding may supply the alias on its replay.
			item := ingest.IncomingItem{SourceType: "imap", SourceName: "Personal", ExternalID: "imap:mail.example:993|alice|INBOX:5:12"}
			if legacyEndpoint {
				item.LegacyExternalID = legacyID
			}
			item.Normalize(200)
			if exists, err := st.ExistsUser(ctx, 1, item.SourceType, item.ExternalID); err != nil || exists {
				t.Fatalf("new default endpoint collided directly with portless legacy key: %v %v", exists, err)
			}
			if exists, err := st.ReconcileIncomingItem(ctx, 1, item); err != nil || exists != legacyEndpoint {
				t.Fatalf("default endpoint reconciliation=%v, want %v: %v", exists, legacyEndpoint, err)
			}
			rows, err := st.ListItems(ctx, ItemFilter{})
			wantID := legacyID
			if legacyEndpoint {
				wantID = item.ExternalID
			}
			if err != nil || len(rows) != 1 || rows[0].ID != old.ID || rows[0].ExternalID != wantID {
				t.Fatalf("legacy row identity changed incorrectly: %+v %v", rows, err)
			}
		})
	}
}
