package store

import (
	"context"
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
		source := []string{"rss", "imap"}[i]
		raw := strings.Repeat("x", 4096) + suffix
		row, _, err := st.InsertItem(ctx, ingest.IncomingItem{SourceType: source, ExternalID: suffix}, classify.Decision{Route: "queue_digest"})
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
		incoming := ingest.IncomingItem{SourceType: source, ExternalID: raw}
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
	for _, source := range []string{"rss", "github"} {
		original := ingest.LegacyIdentity{SourceName: strings.Repeat("source", 30), Title: strings.Repeat("x", 600) + "|tail", Sender: strings.Repeat("sender", 70), URL: "https://example.com/" + strings.Repeat("x", 2048)}
		item := ingest.IncomingItem{SourceType: source, SourceName: original.SourceName, Title: original.Title, Sender: original.Sender, URL: original.URL, Body: "body"}
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

func TestPortlessIMAPRowCannotBeAttributedToLoneCursor(t *testing.T) {
	for _, sameBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("identical_body=%v", sameBody), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing-original-cursor.db")
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			// The old endpoint inserted its item, then crashed before saving its cursor.
			// Only the different endpoint's cursor survived before the upgrade.
			if err := st.SaveFeedState(ctx, FeedSyncState{SourceKey: "imap:lone-2993", ETag: "5", LastUID: 10}); err != nil {
				t.Fatal(err)
			}
			var ddl string
			if err := st.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name='items'`).Scan(&ddl); err != nil {
				t.Fatal(err)
			}
			legacyDDL := strings.Replace(ddl, "user_id INTEGER NOT NULL DEFAULT 1,", "", 1)
			legacyDDL = strings.Replace(legacyDDL, "UNIQUE(user_id, source_type, external_id)", "UNIQUE(source_type, external_id)", 1)
			if legacyDDL == ddl {
				t.Fatal("fixture did not create original items schema")
			}
			for _, statement := range []string{`DROP TABLE items`, legacyDDL} {
				if _, err := st.db.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			// Simulate the original schema, before the abandoned cursor-binding marker.
			if hasLegacyIMAPFlagColumn(t, st) {
				if _, err := st.db.ExecContext(ctx, `ALTER TABLE feed_state DROP COLUMN legacy_imap_binding`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.db.ExecContext(ctx, `INSERT INTO items
				(external_id, source_type, source_name, title, body, sender, url, tag, reason, route,
				priority, confidence, uncertain, decision_source, decision_reason, signals_json, status, published_at, created_at)
				VALUES ('imap:mail.example|alice|INBOX:5:12', 'imap', 'Personal', 'same subject',
				'original message from port 1993', 'Alice', '', '', '', 'queue_digest', 0, 0, 0, 'local', '', '', 'queued', ?, ?)`, time.Now().UTC(), time.Now().UTC()); err != nil {
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
			assertNoLegacyIMAPFlags(t, st)
			state, err := st.GetFeedState(ctx, "imap:lone-2993")
			if err != nil || state.ETag != "5" || state.LastUID != 10 {
				t.Fatalf("cursor was not retained: %+v %v", state, err)
			}
			rows, err := st.ListItems(ctx, ItemFilter{})
			if err != nil || len(rows) != 1 {
				t.Fatalf("legacy row was not retained: %+v %v", rows, err)
			}
			legacy := rows[0]
			body := "different message with identical compared metadata"
			if sameBody {
				body = legacy.Body
			}
			for _, port := range []int{2993, 993} {
				assertPortQualifiedIMAPItemIsDistinct(t, st, port, body, legacy)
			}
		})
	}
}

func TestReopenRevokesEveryLegacyIMAPFlag(t *testing.T) {
	for _, cursors := range []int{1, 2} {
		t.Run(fmt.Sprintf("previously_eligible=%d", cursors), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "obsolete-bindings.db")
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			for _, port := range []int{1993, 2993}[:cursors] {
				if err := st.SaveFeedState(ctx, FeedSyncState{SourceKey: fmt.Sprintf("imap:original-%d", port), ETag: "5", LastUID: 10}); err != nil {
					t.Fatal(err)
				}
			}
			if !hasLegacyIMAPFlagColumn(t, st) {
				if _, err := st.db.ExecContext(ctx, `ALTER TABLE feed_state ADD COLUMN legacy_imap_binding INTEGER NOT NULL DEFAULT 0`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.db.ExecContext(ctx, `UPDATE feed_state SET legacy_imap_binding = 1`); err != nil {
				t.Fatal(err)
			}
			legacy, inserted, err := st.InsertItem(ctx, ingest.IncomingItem{
				SourceType: "imap", ExternalID: "imap:mail.example|alice|INBOX:5:12",
				SourceName: "Personal", Title: "same subject", Sender: "Alice", Body: "original message from port 1993",
			}, classify.Decision{Route: "queue_digest"})
			if err != nil || !inserted {
				t.Fatalf("legacy fixture: %v %v", inserted, err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			for reopen := range 2 {
				st, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				assertNoLegacyIMAPFlags(t, st)
				if reopen == 0 {
					for _, port := range []int{1993, 2993, 993} {
						assertPortQualifiedIMAPItemIsDistinct(t, st, port, legacy.Body, legacy)
					}
				}
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func hasLegacyIMAPFlagColumn(t *testing.T, st *Store) bool {
	t.Helper()
	var found bool
	if err := st.db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pragma_table_info('feed_state') WHERE name='legacy_imap_binding')`).Scan(&found); err != nil {
		t.Fatal(err)
	}
	return found
}

func assertNoLegacyIMAPFlags(t *testing.T, st *Store) {
	t.Helper()
	if hasLegacyIMAPFlagColumn(t, st) {
		var count int
		if err := st.db.QueryRowContext(t.Context(), `SELECT count(*) FROM feed_state WHERE legacy_imap_binding != 0`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("cursor history authorized %d unsafe legacy aliases: %v", count, err)
		}
	}
}

func assertPortQualifiedIMAPItemIsDistinct(t *testing.T, st *Store, port int, body string, legacy StoredItem) {
	t.Helper()
	ctx := t.Context()
	item := ingest.IncomingItem{
		SourceType: "imap", ExternalID: fmt.Sprintf("imap:mail.example:%d|alice|INBOX:5:12", port),
		SourceName: legacy.SourceName, Title: legacy.Title, Sender: legacy.Sender, URL: legacy.URL, Body: body,
	}
	item.Normalize(200)
	if item.LegacyExternalID != "" {
		t.Fatalf("port %d generated an unsafe legacy alias: %q", port, item.LegacyExternalID)
	}
	if exists, err := st.ReconcileIncomingItem(ctx, 1, item); err != nil || exists {
		t.Fatalf("port %d consumed an unrelated portless row: %v %v", port, exists, err)
	}
	row, inserted, err := st.InsertItem(ctx, item, classify.Decision{Route: "queue_digest"})
	if err != nil || !inserted || row.ID == legacy.ID {
		t.Fatalf("port %d was not ingested separately: %+v %v %v", port, row, inserted, err)
	}
	if _, inserted, err := st.InsertItem(ctx, item, classify.Decision{Route: "queue_digest"}); err != nil || inserted {
		t.Fatalf("port %d replay did not deduplicate: %v %v", port, inserted, err)
	}
	if exists, err := st.ExistsUser(ctx, 1, item.SourceType, item.ExternalID); err != nil || !exists {
		t.Fatalf("port %d canonical identity was not retained: %v %v", port, exists, err)
	}
	rows, err := st.ListItems(ctx, ItemFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, original := range rows {
		if original.ID == legacy.ID {
			if original.ExternalID != legacy.ExternalID || original.SourceName != legacy.SourceName || original.Title != legacy.Title || original.Sender != legacy.Sender || original.URL != legacy.URL || original.Body != legacy.Body {
				t.Fatalf("port %d changed the original row: %+v", port, original)
			}
			return
		}
	}
	t.Fatalf("port %d removed the original row", port)
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
