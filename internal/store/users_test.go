package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"focusgate/internal/classify"
	"focusgate/internal/ingest"
)

func TestStore_MultiTenantUserLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory store: %v", err)
	}
	defer st.Close()

	// 1. Create first user -> should be admin
	u1, err := st.CreateUser(ctx, "alice", "alice-secret-123")
	if err != nil {
		t.Fatalf("failed to create alice: %v", err)
	}
	if u1.Role != "admin" {
		t.Fatalf("expected alice to be admin, got %s", u1.Role)
	}

	// 2. Create second user -> should be user
	u2, err := st.CreateUser(ctx, "bob", "bob-secret-456")
	if err != nil {
		t.Fatalf("failed to create bob: %v", err)
	}
	if u2.Role != "user" {
		t.Fatalf("expected bob to be user, got %s", u2.Role)
	}

	// 3. Duplicate username should fail
	_, err = st.CreateUser(ctx, "Alice", "another-password")
	if err == nil {
		t.Fatalf("expected duplicate username to fail")
	}

	// 4. Authenticate valid user
	authU, err := st.AuthenticateUser(ctx, "alice", "alice-secret-123")
	if err != nil || authU.ID != u1.ID {
		t.Fatalf("expected alice to authenticate: %v", err)
	}

	// 5. Authenticate invalid password
	_, err = st.AuthenticateUser(ctx, "alice", "wrong-password")
	if err == nil {
		t.Fatalf("expected wrong password to fail")
	}

	// 6. User Session Validation
	token := "session-token-alice-abc"
	exp := time.Now().Add(24 * time.Hour)
	if err := st.CreateUserSession(ctx, token, u1.ID, exp); err != nil {
		t.Fatalf("failed to create user session: %v", err)
	}

	su, err := st.ValidateSessionUser(ctx, token)
	if err != nil || su == nil {
		t.Fatalf("expected valid session user: %v", err)
	}
	if su.UserID != u1.ID || su.Username != "alice" || su.Role != "admin" {
		t.Fatalf("unexpected session user: %+v", su)
	}

	// 7. User Settings Isolation
	_ = st.SetUserSetting(ctx, u1.ID, "theme", "dark")
	_ = st.SetUserSetting(ctx, u2.ID, "theme", "light")

	val1, err := st.GetUserSetting(ctx, u1.ID, "theme")
	if err != nil || val1 != "dark" {
		t.Fatalf("expected alice theme dark, got %s", val1)
	}
	val2, err := st.GetUserSetting(ctx, u2.ID, "theme")
	if err != nil || val2 != "light" {
		t.Fatalf("expected bob theme light, got %s", val2)
	}

	// 8. User Feed Isolation
	_, err = st.AddUserFeed(ctx, u1.ID, "Alice Tech News", "https://93.184.216.34/alice/rss", 15)
	if err != nil {
		t.Fatalf("failed to add alice feed: %v", err)
	}
	_, err = st.AddUserFeed(ctx, u2.ID, "Bob Sports Feed", "https://93.184.216.34/bob/rss", 30)
	if err != nil {
		t.Fatalf("failed to add bob feed: %v", err)
	}

	aliceFeeds, err := st.ListUserFeeds(ctx, u1.ID)
	if err != nil || len(aliceFeeds) != 1 || aliceFeeds[0].Name != "Alice Tech News" {
		t.Fatalf("unexpected alice feeds: %+v", aliceFeeds)
	}
	bobFeeds, err := st.ListUserFeeds(ctx, u2.ID)
	if err != nil || len(bobFeeds) != 1 || bobFeeds[0].Name != "Bob Sports Feed" {
		t.Fatalf("unexpected bob feeds: %+v", bobFeeds)
	}

	// 9. Items Isolation
	itemA := ingest.IncomingItem{
		ExternalID:  "feed-alice-1",
		SourceType:  "rss",
		SourceName:  "Alice Tech",
		Title:       "Alice Private Post",
		PublishedAt: time.Now(),
	}
	dec := classify.Decision{Route: "queue_digest", Priority: 2, Source: "rule", Reason: "test"}
	_, _, err = st.InsertUserItem(ctx, u1.ID, itemA, dec)
	if err != nil {
		t.Fatalf("failed to insert alice item: %v", err)
	}

	itemB := ingest.IncomingItem{
		ExternalID:  "feed-bob-1",
		SourceType:  "rss",
		SourceName:  "Bob Sports",
		Title:       "Bob Private Post",
		PublishedAt: time.Now(),
	}
	_, _, err = st.InsertUserItem(ctx, u2.ID, itemB, dec)
	if err != nil {
		t.Fatalf("failed to insert bob item: %v", err)
	}

	// Verify Alice can only see Alice items
	aliceItems, err := st.ListItems(ctx, ItemFilter{UserID: u1.ID})
	if err != nil || len(aliceItems) != 1 || aliceItems[0].Title != "Alice Private Post" {
		t.Fatalf("alice saw unexpected items: %+v", aliceItems)
	}

	// Verify Bob can only see Bob items
	bobItems, err := st.ListItems(ctx, ItemFilter{UserID: u2.ID})
	if err != nil || len(bobItems) != 1 || bobItems[0].Title != "Bob Private Post" {
		t.Fatalf("bob saw unexpected items: %+v", bobItems)
	}

	// 10. Multi-tenant shared ExternalID uniqueness
	sharedItem := ingest.IncomingItem{
		ExternalID:  "shared-rss-guid",
		SourceType:  "rss",
		SourceName:  "Public RSS",
		Title:       "Shared Article",
		PublishedAt: time.Now(),
	}
	_, ins1, err := st.InsertUserItem(ctx, u1.ID, sharedItem, dec)
	if err != nil || !ins1 {
		t.Fatalf("expected alice to insert shared item, ins=%v err=%v", ins1, err)
	}
	_, ins2, err := st.InsertUserItem(ctx, u2.ID, sharedItem, dec)
	if err != nil || !ins2 {
		t.Fatalf("expected bob to insert shared item independently, ins=%v err=%v", ins2, err)
	}
	// Inserting duplicate for Alice must report inserted=false
	_, insDup, err := st.InsertUserItem(ctx, u1.ID, sharedItem, dec)
	if err != nil || insDup {
		t.Fatalf("expected duplicate insert for alice to be ignored, ins=%v err=%v", insDup, err)
	}
}

func TestAdminSessionInvalidationScope(t *testing.T) {
	ctx := context.Background()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open memory store: %v", err)
	}
	defer st.Close()

	// Provision admin user (id=1) and regular user bob (id=2)
	u1, err := st.CreateUser(ctx, "admin", "admin-password-123")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	u2, err := st.CreateUser(ctx, "bob", "bob-password-123")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}

	exp := time.Now().UTC().Add(24 * time.Hour)
	// Create admin session (user_id 1) and legacy session (user_id 0)
	if err := st.CreateUserSession(ctx, "tok-admin-1", u1.ID, exp); err != nil {
		t.Fatalf("create tok-admin-1: %v", err)
	}
	if err := st.CreateUserSession(ctx, "tok-admin-0", 0, exp); err != nil {
		t.Fatalf("create tok-admin-0: %v", err)
	}
	// Create regular user session (user_id 2)
	if err := st.CreateUserSession(ctx, "tok-user-2", u2.ID, exp); err != nil {
		t.Fatalf("create tok-user-2: %v", err)
	}

	// Invalidate admin sessions
	if err := st.InvalidateUserSessions(ctx, u1.ID); err != nil {
		t.Fatalf("invalidate admin sessions: %v", err)
	}

	su1, _ := st.ValidateSessionUser(ctx, "tok-admin-1")
	if su1 != nil {
		t.Fatalf("expected tok-admin-1 to be invalidated")
	}
	su0, _ := st.ValidateSessionUser(ctx, "tok-admin-0")
	if su0 != nil {
		t.Fatalf("expected tok-admin-0 to be invalidated")
	}
	su2, err := st.ValidateSessionUser(ctx, "tok-user-2")
	if err != nil || su2 == nil || su2.UserID != u2.ID {
		t.Fatalf("expected tok-user-2 to remain valid, got %+v (err %v)", su2, err)
	}
}

func TestUpgradeItemsUniqueConstraint(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "legacy.db")

	// 1. Manually construct an old schema with legacy UNIQUE(source_type, external_id)
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	legacySchema := []string{
		`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at DATETIME NOT NULL);`,
		`CREATE TABLE items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			external_id TEXT NOT NULL,
			source_type TEXT NOT NULL,
			source_name TEXT NOT NULL,
			title TEXT NOT NULL,
			body TEXT NOT NULL,
			sender TEXT NOT NULL,
			url TEXT NOT NULL,
			tag TEXT NOT NULL,
			reason TEXT NOT NULL,
			route TEXT NOT NULL,
			priority INTEGER NOT NULL,
			confidence REAL NOT NULL,
			uncertain INTEGER NOT NULL DEFAULT 0,
			decision_source TEXT NOT NULL,
			decision_reason TEXT NOT NULL,
			signals_json TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			published_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL,
			UNIQUE(source_type, external_id)
		);`,
	}
	for _, stmt := range legacySchema {
		if _, err := sqlDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("execute legacy stmt: %v", err)
		}
	}
	// Insert Alice's item in the old schema (without user_id column)
	now := time.Now().UTC()
	_, err = sqlDB.ExecContext(ctx, `
		INSERT INTO items (external_id, source_type, source_name, title, body, sender, url, tag, reason, route, priority, confidence, decision_source, decision_reason, status, published_at, created_at)
		VALUES ('guid-xyz', 'rss', 'Feed', 'Title', 'Body', '', '', '', '', 'queue_digest', 1, 0.9, 'rule', 'legacy test', 'queued', ?, ?)
	`, now, now)
	if err != nil {
		t.Fatalf("insert alice item in legacy db: %v", err)
	}
	sqlDB.Close()

	// 2. Open via store.Open to run migration on the existing legacy database
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open upgraded store: %v", err)
	}
	defer st.Close()

	// Verify Alice's legacy item was preserved and assigned to user_id = 1
	aliceItems, err := st.ListItems(ctx, ItemFilter{UserID: 1})
	if err != nil || len(aliceItems) != 1 {
		t.Fatalf("expected preserved legacy item for user 1, got %v (err %v)", aliceItems, err)
	}
	if aliceItems[0].ExternalID != "guid-xyz" || aliceItems[0].UserID != 1 {
		t.Fatalf("unexpected preserved alice item: %+v", aliceItems[0])
	}

	// 3. Insert the EXACT same item for Bob (user_id 2). In upgraded database, this MUST succeed!
	itemBob := ingest.IncomingItem{
		ExternalID:  "guid-xyz",
		SourceType:  "rss",
		SourceName:  "Feed",
		Title:       "Title",
		PublishedAt: now,
	}
	dec := classify.Decision{Route: "queue_digest", Priority: 1, Source: "rule", Reason: "upgrade test"}
	bobStored, inserted, err := st.InsertUserItem(ctx, 2, itemBob, dec)
	if err != nil || !inserted {
		t.Fatalf("expected bob insert to succeed after upgrade migration, ins=%v err=%v", inserted, err)
	}
	if bobStored.UserID != 2 {
		t.Fatalf("expected bob stored item to have user_id=2, got %d", bobStored.UserID)
	}

	// 4. Repeated insert for Bob must be ignored
	_, reinserted, err := st.InsertUserItem(ctx, 2, itemBob, dec)
	if err != nil || reinserted {
		t.Fatalf("expected duplicate insert for bob to report inserted=false, ins=%v err=%v", reinserted, err)
	}
}

func TestPartialMasterCredentialsDoNotReopenBootstrap(t *testing.T) {
	for _, pair := range []struct{ key, value string }{
		{"auth_passphrase_hash", "legacy-hash"}, {"auth_passphrase_salt", "legacy-salt"}, {"auth_passphrase_hash", ""},
	} {
		t.Run(pair.key+pair.value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if err := st.SetSetting(ctx, pair.key, pair.value); err != nil {
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
			if has, err := st.HasUsers(ctx); err != nil || has {
				t.Fatalf("migration created a user from incomplete credentials: has=%t err=%v", has, err)
			}
			if required, err := st.IsAuthSetupRequired(ctx); err != nil || required {
				t.Fatalf("incomplete credentials reopened setup: required=%v err=%v", required, err)
			}
			if err := st.InitializeMasterPassphrase(ctx, "other-hash", "other-salt"); !errors.Is(err, ErrAlreadyInitialized) {
				t.Fatalf("initialized over existing credential material: %v", err)
			}
			if _, err := st.CreateUser(ctx, "alice", "unapproved-password"); err == nil {
				t.Fatal("created administrator without verifying the stored credentials")
			}
			if _, err := st.CreateUserAuthorized(ctx, "operator", "authorized-password", true); err != nil {
				t.Fatalf("authorized administrator recovery failed: %v", err)
			}
		})
	}
}
