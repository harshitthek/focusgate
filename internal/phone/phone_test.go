package phone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

func openTest(t *testing.T, dir, profile string) *Engine {
	t.Helper()
	e, err := Open(dir, profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func doRefresh(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.BeginNetwork("test"); err != nil {
		t.Fatal(err)
	}
	defer e.EndNetwork("test")
	if err := e.Refresh("test", "", ""); err != nil {
		t.Fatal(err)
	}
}

func incoming(id, title string) ingest.IncomingItem {
	return ingest.IncomingItem{ExternalID: id, Title: title, Body: "summary", SourceType: "rss", PublishedAt: time.Now()}
}

func TestFeedBacklogDedupAndOfflineArchive(t *testing.T) {
	dir := t.TempDir()
	e := openTest(t, dir, "")
	if err := e.SaveFeed(Feed{URL: "https://EXAMPLE.com:443/feed#fragment", Name: "First"}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetSettings(Settings{Alerts: true, Mode: config.ModeDND}); err != nil {
		t.Fatal(err)
	}
	first := incoming("one", "Security advisory: first entry")
	second := incoming("two", "Security advisory: new entry")
	attempt := 0
	e.fetch = func(_ context.Context, f Feed) ([]ingest.IncomingItem, ingest.FeedCache, bool, error) {
		attempt++
		if attempt == 1 {
			return []ingest.IncomingItem{first}, ingest.FeedCache{ETag: "first"}, true, nil
		}
		if f.etag != "first" || f.Name != "Renamed" {
			t.Fatalf("feed cache or rename lost: %+v", f)
		}
		return []ingest.IncomingItem{first, second}, ingest.FeedCache{ETag: "second"}, true, nil
	}
	doRefresh(t, e)
	list, err := e.State(false, 0)
	if err != nil || len(list.Items) != 1 || list.Items[0].Priority != 5 || list.Feeds[0].URL != "https://example.com/feed" {
		t.Fatalf("first page: %+v, %v", list, err)
	}
	notices, err := e.Notices("UTC", 0)
	if err != nil || len(notices) != 0 {
		t.Fatalf("backlog was noisy: %+v, %v", notices, err)
	}
	f := list.Feeds[0]
	f.Name = "Renamed"
	if err := e.SaveFeed(f); err != nil {
		t.Fatal(err)
	}
	doRefresh(t, e)
	notices, err = e.Notices("UTC", 0)
	if err != nil || len(notices) != 1 || notices[0].Title != second.Title {
		t.Fatalf("fresh severity rule was not applied: %+v %v", notices, err)
	}
	if err := e.Acknowledge(notices[0]); err != nil {
		t.Fatal(err)
	}
	if err := e.SetSettings(Settings{Offline: true, Alerts: true, Mode: config.ModeDND}); err != nil {
		t.Fatal(err)
	}
	if err := e.Mark(list.Items[0].ID, 2); err != nil {
		t.Fatal(err)
	}
	restarted := openTest(t, dir, "")
	if err := restarted.BeginNetwork("offline"); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline profile allowed network: %v", err)
	}
	archive, err := restarted.State(true, 0)
	if err != nil || len(archive.Items) != 1 || archive.Items[0].ID != list.Items[0].ID {
		t.Fatalf("archive state lost: %+v %v", archive, err)
	}
	notices, err = restarted.Notices("UTC", 0)
	if err != nil || len(notices) != 0 {
		t.Fatalf("replayed a delivered notice: %+v %v", notices, err)
	}
}

func TestOfflineCancelsNetworkAndFencesOldOperation(t *testing.T) {
	e := openTest(t, t.TempDir(), "")
	if err := e.SaveFeed(Feed{URL: "https://example.com/feed"}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	e.fetch = func(ctx context.Context, _ Feed) ([]ingest.IncomingItem, ingest.FeedCache, bool, error) {
		close(started)
		<-ctx.Done()
		return nil, ingest.FeedCache{}, false, ctx.Err()
	}
	if err := e.BeginNetwork("old"); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- e.Refresh("old", "", "") }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("fetch did not begin")
	}
	if err := e.SetSettings(Settings{Offline: true, Mode: config.ModeNormal}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fetch not cancelled")
	}
	if err := e.SetSettings(Settings{Mode: config.ModeNormal}); err != nil {
		t.Fatal(err)
	}
	if err := e.BeginNetwork("new"); err != nil {
		t.Fatal(err)
	}
	defer e.EndNetwork("new")
	e.EndNetwork("old")
	if _, err := e.networkContext("new"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.networkContext("old"); !errors.Is(err, context.Canceled) {
		t.Fatalf("old operation reused online mode: %v", err)
	}
}

func TestCloudConflictAndProfileIsolation(t *testing.T) {
	dir := t.TempDir()
	e := openTest(t, dir, "first")
	local := Feed{URL: "https://example.com/feed", Name: "My change", Assess: true}
	if err := e.SaveFeed(local); err != nil {
		t.Fatal(err)
	}
	_, ops, err := e.pending(context.Background())
	if err != nil || len(ops) != 1 {
		t.Fatalf("outbox: %v %v", ops, err)
	}
	other := Feed{ID: ops[0].Entity, URL: local.URL, Name: "Server version", Revision: 3}
	if err := e.applySync(context.Background(), ops, syncResponse{
		Cursor: "3", Acks: []acknowledgement{{ID: ops[0].ID, Conflict: true, Feed: &other}},
	}); err != nil {
		t.Fatal(err)
	}
	state, err := e.State(false, 0)
	if err != nil || len(state.Feeds) != 1 || len(state.Conflicts) != 1 || state.Feeds[0].Name != "Server version" ||
		state.Conflicts[0].Name != "My change" || !state.Feeds[0].Assess {
		t.Fatalf("conflict incorrectly applied: %+v %v", state, err)
	}
	e2 := openTest(t, dir, "second")
	state, err = e2.State(false, 0)
	if err != nil || len(state.Feeds) != 0 || len(state.Conflicts) != 0 {
		t.Fatalf("cross-account data: %+v %v", state, err)
	}
}

func TestAssessmentNeverReopensReadItem(t *testing.T) {
	e := openTest(t, t.TempDir(), "account")
	if err := e.SetSettings(Settings{Alerts: true, Mode: config.ModeNormal}); err != nil {
		t.Fatal(err)
	}
	if err := e.SaveFeed(Feed{URL: "https://example.com/feed", Assess: true}); err != nil {
		t.Fatal(err)
	}
	id := hash("https://example.com/feed")
	e.fetch = func(_ context.Context, _ Feed) ([]ingest.IncomingItem, ingest.FeedCache, bool, error) {
		return []ingest.IncomingItem{incoming("entry", "Dev update")}, ingest.FeedCache{}, true, nil
	}
	doRefresh(t, e)
	itemID := hash(id + "\x00entry")
	if err := e.Mark(itemID, 1); err != nil {
		t.Fatal(err)
	}
	sig := classify.SmartSignals{Engine: "local-model", Model: "v1", Category: "security_or_outage", CategoryProb: 1, CategoryConf: 1, UrgencyScore: 3, UrgencyConf: 1}
	if err := e.applyAssessment(context.Background(), itemID, id, 0, sig); err != nil {
		t.Fatal(err)
	}
	state, err := e.State(false, 0)
	if err != nil || len(state.Items) != 1 || state.Items[0].Status != 1 || state.Items[0].Priority == 5 {
		t.Fatalf("late assessment changed a read item: %+v %v", state, err)
	}
	data, _ := json.Marshal(Item{ID: hash("restored"), FeedID: id, Title: "Cloud history", Status: 0, Priority: 5, Published: time.Now().Unix()})
	if err := e.applySync(context.Background(), nil, syncResponse{Cursor: "2", Changes: []change{{Kind: "item", Data: data}}}); err != nil {
		t.Fatal(err)
	}
	notices, err := e.Notices("UTC", 0)
	if err != nil || len(notices) != 0 {
		t.Fatalf("restored history alerted: %+v %v", notices, err)
	}
}

func TestDigestUsesDeviceTimeAndLatestSlot(t *testing.T) {
	zone := time.FixedZone("UTC+05:30", 19800)
	now := time.Date(2026, 11, 25, 7, 20, 0, 0, zone)
	want := time.Date(2026, 11, 24, 19, 30, 0, 0, zone)
	if got := latestDigest(now); !got.Equal(want) {
		t.Fatalf("slot was %s, want %s", got, want)
	}
	now = time.Date(2026, 11, 25, 15, 0, 0, 0, zone)
	if got := latestDigest(now); got.Hour() != 13 || got.Minute() != 30 {
		t.Fatalf("slot was %s", got)
	}
}

func TestDigestViewWorksWithAlertsOff(t *testing.T) {
	e := openTest(t, t.TempDir(), "")
	if err := e.SaveFeed(Feed{URL: "https://example.com/feed"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	e.fetch = func(_ context.Context, _ Feed) ([]ingest.IncomingItem, ingest.FeedCache, bool, error) {
		calls++
		if calls == 1 {
			return nil, ingest.FeedCache{}, true, nil
		}
		return []ingest.IncomingItem{incoming("update", "New article")}, ingest.FeedCache{}, true, nil
	}
	doRefresh(t, e)
	doRefresh(t, e)
	digest, err := e.Digest(0)
	if err != nil || len(digest) != 1 || digest[0].Title != "New article" {
		t.Fatalf("digest: %+v, %v", digest, err)
	}
	notices, err := e.Notices("UTC", 0)
	if err != nil || len(notices) != 0 {
		t.Fatalf("alerts were delivered: %+v %v", notices, err)
	}
}

func TestLegacyImportPreservesArchiveWithoutLinkingAccounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	item, _, err := st.InsertItem(context.Background(), incoming("old", "Legacy article"), classify.Decision{Route: "queue_digest", Priority: 2})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.UpdateItemStatus(context.Background(), item.ID, "archived"); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	local := openTest(t, dir, "")
	if err := local.ImportLegacy(path); err != nil {
		t.Fatal(err)
	}
	if err := local.ImportLegacy(path); err != nil {
		t.Fatal(err)
	}
	archived, err := local.State(true, 0)
	if err != nil || len(archived.Items) != 1 || archived.Items[0].Title != "Legacy article" {
		t.Fatalf("legacy archive lost: %+v %v", archived, err)
	}
	account := openTest(t, dir, "new account")
	if err := account.ImportLocal(dir); err != nil {
		t.Fatal(err)
	}
	newArchive, err := account.State(true, 0)
	if err != nil || len(newArchive.Items) != 0 {
		t.Fatalf("legacy archive attached to new account: %+v %v", newArchive, err)
	}
}

func TestCanonicalFeedURLChecksSerializedLength(t *testing.T) {
	prefix := "https://example.com?"
	for _, size := range []int{2047, 2048} {
		raw := prefix + strings.Repeat("a", size-len(prefix))
		normalized, err := canonical(raw)
		if size == 2047 && (err != nil || len(normalized) != 2048) {
			t.Fatalf("expected URL at exactly the limit: length %d, error %v", len(normalized), err)
		}
		if size == 2048 && err == nil {
			t.Fatal("accepted URL that exceeds the limit after normalization")
		}
	}
}

func TestSnapshotFeedRollbackPreservesPendingEdits(t *testing.T) {
	ctx := context.Background()
	e := openTest(t, t.TempDir(), "account")
	const raw = "https://example.com/feed"
	id := hash(raw)
	apply := func(rev int64, name, cursor string, reset, more bool) {
		t.Helper()
		data, err := json.Marshal(Feed{ID: id, URL: raw, Name: name, Revision: rev})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.applySync(ctx, nil, syncResponse{Cursor: cursor, Reset: reset, More: more,
			Changes: []change{{Kind: "feed", Data: data}}}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(name string, revision int64) {
		t.Helper()
		state, err := e.State(false, 0)
		if err != nil || len(state.Feeds) != 1 || state.Feeds[0].Name != name || state.Feeds[0].Revision != revision {
			t.Fatalf("feed: %+v %v, want %q at revision %d", state.Feeds, err, name, revision)
		}
	}
	apply(120, "later", "120", false, false)
	apply(80, "older delta", "121", false, false)
	check("later", 120)
	if err := e.applySync(ctx, nil, syncResponse{Cursor: "s:79", Reset: true, More: true}); err != nil {
		t.Fatal(err)
	}
	apply(80, "restored", "80", false, false)
	check("restored", 80)
	if err := e.SaveFeed(Feed{URL: raw, Name: "unsent edit"}); err != nil {
		t.Fatal(err)
	}
	apply(50, "restored again", "50", true, false)
	check("unsent edit", 80)
}

func TestRefreshReadsValidatorsAfterFeedReplacement(t *testing.T) {
	e := openTest(t, t.TempDir(), "")
	first := Feed{URL: "https://example.com/a", Name: "A"}
	second := Feed{URL: "https://example.com/b", Name: "B"}
	for _, feed := range []Feed{first, second} {
		if err := e.SaveFeed(feed); err != nil {
			t.Fatal(err)
		}
	}
	e.fetch = func(context.Context, Feed) ([]ingest.IncomingItem, ingest.FeedCache, bool, error) {
		return nil, ingest.FeedCache{ETag: "old", LastModified: "old-date"}, true, nil
	}
	doRefresh(t, e)
	calls := 0
	e.fetch = func(_ context.Context, f Feed) ([]ingest.IncomingItem, ingest.FeedCache, bool, error) {
		calls++
		if f.URL == first.URL {
			if err := e.SaveFeed(Feed{URL: second.URL, Removed: true}); err != nil {
				t.Fatal(err)
			}
			if err := e.SaveFeed(second); err != nil {
				t.Fatal(err)
			}
		} else if f.etag != "" || f.modified != "" {
			t.Fatalf("fetched replacement using stale validators: %+v", f)
		}
		return nil, ingest.FeedCache{}, true, nil
	}
	doRefresh(t, e)
	if calls != 2 {
		t.Fatalf("received %d fetches, want two", calls)
	}
}

func TestDigestBoundaryItemCountsInNextSlot(t *testing.T) {
	e := openTest(t, t.TempDir(), "")
	if err := e.SaveFeed(Feed{URL: "https://example.com/feed"}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetSettings(Settings{Alerts: true, Mode: config.ModeNormal}); err != nil {
		t.Fatal(err)
	}
	e.fetch = func(context.Context, Feed) ([]ingest.IncomingItem, ingest.FeedCache, bool, error) {
		return []ingest.IncomingItem{incoming("earlier", "Earlier"), incoming("boundary", "Boundary")}, ingest.FeedCache{}, true, nil
	}
	doRefresh(t, e)
	slot := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	if _, err := e.db.Exec("UPDATE preferences SET last_digest=?", slot.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec("UPDATE inbox SET digest=1, received=CASE title WHEN 'Earlier' THEN ? ELSE ? END", slot.Unix()-1, slot.Unix()); err != nil {
		t.Fatal(err)
	}
	notices, err := e.noticesAt(slot)
	if err != nil || len(notices) != 1 || notices[0].Body != "1 unread feed items" {
		t.Fatalf("earlier item digest: %+v, %v", notices, err)
	}
	if err := e.Acknowledge(notices[0]); err != nil {
		t.Fatal(err)
	}
	notices, err = e.noticesAt(slot.Add(5 * time.Hour))
	if err != nil || len(notices) != 1 || notices[0].Body != "1 unread feed items" {
		t.Fatalf("boundary item digest: %+v, %v", notices, err)
	}
}

func TestSyncReadsPageLargerThan512KiB(t *testing.T) {
	e := openTest(t, t.TempDir(), "account")
	page := syncResponse{Cursor: "50"}
	for i := range 50 {
		item := Item{
			ID: hash(fmt.Sprint(i)), FeedID: hash("feed"),
			Title: strings.Repeat("\x01", 512), Snippet: strings.Repeat("\x01", 900),
			URL:      "https://example.com/" + strings.Repeat("a", 2020),
			Priority: 1, Published: time.Now().Unix(),
		}
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		page.Changes = append(page.Changes, change{Kind: "item", Data: data})
	}
	wire, err := json.Marshal(page)
	if err != nil || len(wire) <= 512<<10 {
		t.Fatalf("test page size %d, error %v", len(wire), err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(wire)
	}))
	t.Cleanup(server.Close)
	previous := remoteClient
	remoteClient = server.Client()
	t.Cleanup(func() { remoteClient = previous })
	if err := e.BeginNetwork("test"); err != nil {
		t.Fatal(err)
	}
	defer e.EndNetwork("test")
	if err := e.Sync("test", server.URL, "test"); err != nil {
		t.Fatal(err)
	}
	cursor, _, err := e.pending(context.Background())
	if err != nil || cursor != page.Cursor {
		t.Fatalf("cursor %q, error %v", cursor, err)
	}
	state, err := e.State(false, 0)
	if err != nil || len(state.Items) != 50 {
		t.Fatalf("sync stored %d items, error %v", len(state.Items), err)
	}
	for _, item := range state.Items {
		if item.ID == hash("0") && item.URL == "https://example.com/"+strings.Repeat("a", 2020) {
			return
		}
	}
	t.Fatal("expected item and valid long URL were not stored")

}

func TestEvictionKeepsQueuedArchiveAndItsCreation(t *testing.T) {
	e := openTest(t, t.TempDir(), "account")
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, raw := range []string{"already-synced", "unsynced", "untouched"} {
		item := Item{ID: hash(raw), FeedID: hash("feed"), Title: raw, Published: now.AddDate(0, 0, -31).Unix(), Priority: 1}
		if err := putItem(ctx, tx, item, 0, 0, "", 0); err != nil {
			t.Fatal(err)
		}
		if raw != "already-synced" {
			if err := enqueue(tx, "item", item.ID, 0, item); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"already-synced", "unsynced"} {
		if err := e.Mark(hash(raw), 2); err != nil {
			t.Fatal(err)
		}
	}
	tx, err = e.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := prune(ctx, tx, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var items int
	if err := e.db.QueryRowContext(ctx, "SELECT count(*) FROM inbox").Scan(&items); err != nil || items != 0 {
		t.Fatalf("retention left %d inbox rows, error %v", items, err)
	}
	_, ops, err := e.pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ kind, entity string }{
		{"item", hash("unsynced")},
		{"status", hash("already-synced")},
		{"status", hash("unsynced")},
	}
	if len(ops) != len(want) {
		t.Fatalf("retention left %d operations, want %d", len(ops), len(want))
	}
	acks := make([]acknowledgement, len(ops))
	for i, op := range ops {
		if op.Kind != want[i].kind || op.Entity != want[i].entity {
			t.Fatalf("operation %d was %s for %s", i, op.Kind, op.Entity)
		}
		acks[i] = acknowledgement{ID: op.ID}
	}
	if err := e.applySync(ctx, ops, syncResponse{Cursor: "1", Acks: acks}); err != nil {
		t.Fatal(err)
	}
	if _, ops, err := e.pending(ctx); err != nil || len(ops) != 0 {
		t.Fatalf("acknowledged operations were retained: %d, error %v", len(ops), err)
	}
}

func TestRevokingAssessmentCancelsOnlyTheAffectedFeed(t *testing.T) {
	e := openTest(t, t.TempDir(), "account")
	first := Feed{URL: "https://example.com/a", Assess: true}
	second := Feed{URL: "https://example.com/b", Assess: true}
	for _, f := range []Feed{first, second} {
		if err := e.SaveFeed(f); err != nil {
			t.Fatal(err)
		}
	}
	e.fetch = func(context.Context, Feed) ([]ingest.IncomingItem, ingest.FeedCache, bool, error) {
		return []ingest.IncomingItem{incoming("entry", "Article")}, ingest.FeedCache{}, true, nil
	}
	doRefresh(t, e)
	if err := e.BeginNetwork("next"); err != nil {
		t.Fatal(err)
	}
	defer e.EndNetwork("next")
	parent, err := e.networkContext("next")
	if err != nil {
		t.Fatal(err)
	}
	feed := hash(first.URL)
	item := hash(feed + "\x00entry")
	ctx, finish, err := e.beginAssessment(parent, item, feed, 0)
	if err != nil || ctx == nil {
		t.Fatalf("could not start assessment: %v", err)
	}
	defer finish()
	second.Assess = false
	if err := e.SaveFeed(second); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("editing another feed cancelled this assessment")
	}
	first.Assess = false
	if err := e.SaveFeed(first); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("assessment was not cancelled: %v", ctx.Err())
	}
	if parent.Err() != nil {
		t.Fatal("revocation cancelled unrelated network operations")
	}
	next, nextFinish, err := e.beginAssessment(parent, item, feed, 0)
	if next != nil {
		nextFinish()
	}
	if err != nil || next != nil {
		t.Fatalf("revoked consent allowed the next assessment: %v", err)
	}
}

func TestSyncURLPortValidation(t *testing.T) {
	for _, port := range []string{"65535", "65536", "999999999999999999999999"} {
		raw := "https://example.com:" + port + "/feed"
		_, err := canonical(raw)
		if (err == nil) != (port == "65535") {
			t.Fatalf("canonical port %s: %v", port, err)
		}
		want := ""
		if port == "65535" {
			want = raw
		}
		if got := articleURL("https://example.org/feed", raw); got != want {
			t.Fatalf("article port %s: got %q, want %q", port, got, want)
		}
	}
}

func TestOpenRepairsPreviouslyQueuedInvalidPorts(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir, "profile")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	bad := Feed{URL: "https://example.com:65536/feed", Name: "Old invalid feed"}
	bad.ID = hash(bad.URL)
	good := Feed{URL: "https://example.com:65535/feed", Name: "Valid feed"}
	good.ID = hash(good.URL)
	if err := e.SaveFeed(good); err != nil {
		t.Fatal(err)
	}
	tx, err := e.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO feeds(id,url,name) VALUES(?,?,?)", bad.ID, bad.URL, bad.Name); err != nil {
		t.Fatal(err)
	}
	if err := enqueueFeed(tx, bad); err != nil {
		t.Fatal(err)
	}
	item := Item{ID: hash("article"), FeedID: good.ID, URL: "https://example.com:65536/article",
		Title: "Preserved", Published: time.Now().Unix(), Priority: 1}
	if err := putItem(context.Background(), tx, item, 0, 0, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := enqueue(tx, "item", item.ID, 0, item); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE preferences SET cursor='s:23'"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("PRAGMA user_version=0"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	migrated := openTest(t, dir, "profile")
	cursor, ops, err := migrated.pending(context.Background())
	if err != nil || cursor != "0" || len(ops) != 2 || ops[0].Entity != good.ID || ops[1].Entity != item.ID {
		t.Fatalf("migrated outbox: cursor %q, %v, %v", cursor, ops, err)
	}
	var queued Item
	if err := json.Unmarshal(ops[1].Data, &queued); err != nil || queued.URL != "" || queued.Title != item.Title {
		t.Fatalf("migrated queued article: %+v, %v", queued, err)
	}
	state, err := migrated.State(false, 0)
	if err != nil || len(state.Feeds) != 1 || state.Feeds[0].ID != good.ID ||
		len(state.Items) != 1 || state.Items[0].URL != "" {
		t.Fatalf("migrated cache: %+v, %v", state, err)
	}
}

func TestSnapshotRemovesAbsentFeedsAfterLastPage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := openTest(t, dir, "account")
	feedChange := func(name string, revision int64) change {
		t.Helper()
		raw := "https://example.org/" + name
		data, err := json.Marshal(Feed{ID: hash(raw), URL: raw, Name: name, Revision: revision})
		if err != nil {
			t.Fatal(err)
		}
		return change{Kind: "feed", Data: data}
	}
	if err := first.applySync(ctx, nil, syncResponse{Cursor: "120",
		Changes: []change{feedChange("keep", 90), feedChange("absent", 120)}}); err != nil {
		t.Fatal(err)
	}
	if err := first.SaveFeed(Feed{URL: "https://example.org/pending", Name: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := first.applySync(ctx, nil, syncResponse{Cursor: "s:80", Reset: true, More: true,
		Changes: []change{feedChange("keep", 80)}}); err != nil {
		t.Fatal(err)
	}
	if state, err := first.State(false, 0); err != nil || len(state.Feeds) != 3 {
		t.Fatalf("intermediate feeds %+v, %v", state.Feeds, err)
	}

	reopened := openTest(t, dir, "account")
	if err := reopened.applySync(ctx, nil, syncResponse{Cursor: "80"}); err != nil {
		t.Fatal(err)
	}
	state, err := reopened.State(false, 0)
	if err != nil || len(state.Feeds) != 2 || state.Feeds[0].Name != "keep" || state.Feeds[1].Name != "pending" {
		t.Fatalf("final feeds %+v, %v", state.Feeds, err)
	}
	if err := reopened.SaveFeed(Feed{URL: "https://example.org/absent"}); err != nil {
		t.Fatal(err)
	}
	if _, ops, err := reopened.pending(ctx); err != nil || len(ops) != 2 || ops[1].Base != 0 {
		t.Fatalf("recreated subscription must start at server revision zero: %+v, %v", ops, err)
	}
}

func TestImportLocalReportsCapacityWithoutPartialImport(t *testing.T) {
	dir := t.TempDir()
	local := openTest(t, dir, "")
	signedIn := openTest(t, dir, "account")
	for i := range maxFeeds {
		if err := signedIn.SaveFeed(Feed{URL: fmt.Sprintf("https://example.com/%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.SaveFeed(Feed{URL: "https://example.com/0"}); err != nil {
		t.Fatal(err)
	}
	if err := signedIn.ImportLocal(dir); err != nil {
		t.Fatalf("existing feed counted as new: %v", err)
	}
	for _, name := range []string{"first", "next"} {
		if err := local.SaveFeed(Feed{URL: "https://example.org/" + name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := signedIn.SaveFeed(Feed{URL: "https://example.com/1", Removed: true}); err != nil {
		t.Fatal(err)
	}
	if err := signedIn.ImportLocal(dir); err == nil {
		t.Fatal("partial import unexpectedly succeeded")
	}
	state, err := signedIn.State(false, 0)
	if err != nil || len(state.Feeds) != maxFeeds-1 {
		t.Fatalf("partial import changed feeds: %+v, %v", state.Feeds, err)
	}
	if err := signedIn.SaveFeed(Feed{URL: "https://example.com/2", Removed: true}); err != nil {
		t.Fatal(err)
	}
	if err := signedIn.ImportLocal(dir); err != nil {
		t.Fatalf("import after freeing slots: %v", err)
	}
	state, err = signedIn.State(false, 0)
	if err != nil || len(state.Feeds) != maxFeeds {
		t.Fatalf("imported feeds: %+v, %v", state.Feeds, err)
	}
}

func TestMarkDistinguishesMissingItemsFromIdempotentRetries(t *testing.T) {
	e := openTest(t, t.TempDir(), "account")
	if err := e.Mark(hash("missing"), 2); err == nil {
		t.Fatal("archiving a missing item reported success")
	}
	item := Item{ID: hash("archived"), FeedID: hash("feed"), Published: time.Now().Unix(), Priority: 1, Status: 2}
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := e.applySync(ctx, nil, syncResponse{Cursor: "1", Changes: []change{{Kind: "item", Data: data}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Mark(item.ID, 2); err != nil {
		t.Fatalf("idempotent archive: %v", err)
	}
	if _, ops, err := e.pending(ctx); err != nil || len(ops) != 0 {
		t.Fatalf("idempotent archive enqueued operations: %v, %v", ops, err)
	}
}
