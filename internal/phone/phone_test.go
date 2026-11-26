package phone

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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
