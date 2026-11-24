package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"focusgate/internal/classify"
	"focusgate/internal/ingest"
)

func TestRebuildPreservesReceiptsLinksAndPushHold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	item, _, err := st.InsertItem(ctx, ingest.IncomingItem{SourceType: "rss", ExternalID: "legacy"}, classify.Decision{Route: "queue_digest"})
	if err != nil {
		t.Fatal(err)
	}
	lease, _, err := st.ClaimDelivery(ctx, item.ID, "channel")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDelivery(ctx, item.ID, "channel", lease, true); err != nil {
		t.Fatal(err)
	}
	if err := st.InitMobileCache(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO mobile_item_links(item_id,server_id) VALUES(?,42)`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE items SET initial_push_until=9999999999`); err != nil {
		t.Fatal(err)
	}
	var ddl string
	if err := st.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name='items'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	st.Close()
	legacy := strings.Replace(ddl, "UNIQUE(user_id, source_type, external_id)", "UNIQUE(source_type, external_id)", 1)
	if legacy == ddl {
		t.Fatal("fixture did not create a legacy unique key")
	}
	// This fixture connection intentionally has foreign_keys disabled so child
	// rows survive installing the old parent schema before testing the upgrade.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	for _, statement := range []string{`PRAGMA foreign_keys=OFF`, `CREATE TEMP TABLE saved_items AS SELECT * FROM items`, `DROP TABLE items`, legacy, `INSERT INTO items SELECT * FROM saved_items`, `DROP TABLE saved_items`} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			raw.Close()
			t.Fatal(err)
		}
	}
	raw.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, done, err := st.ClaimDelivery(ctx, item.ID, "channel"); err != nil || !done {
		t.Fatalf("completed receipt lost during rebuild: %v %v", done, err)
	}
	if id, err := st.MobileServerID(ctx, item.ID); err != nil || id != 42 {
		t.Fatalf("mobile link lost: %d %v", id, err)
	}
	if queued, err := st.ListQueuedForDigest(ctx, 50); err != nil || len(queued) != 0 {
		t.Fatalf("push hold lost: %v %v", queued, err)
	}
	// A structurally current schema is recognized through index metadata.
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	need, err := itemsNeedRebuild(ctx, tx)
	tx.Rollback()
	if err != nil || need {
		t.Fatalf("rebuilt schema was not recognized: %v %v", need, err)
	}
	st.Close()
	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	for _, statement := range []string{`PRAGMA writable_schema=ON`, `UPDATE sqlite_master SET sql=replace(sql,'UNIQUE','unique') WHERE name='items'`, `PRAGMA writable_schema=OFF`} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			raw.Close()
			t.Fatal(err)
		}
	}
	tx, err = raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	need, err = itemsNeedRebuild(ctx, tx)
	tx.Rollback()
	if err != nil || need {
		t.Fatalf("lowercase DDL requires rebuild: %v %v", need, err)
	}
	raw.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, done, err := reopened.ClaimDelivery(ctx, item.ID, "channel"); err != nil || !done {
		t.Fatalf("case-only schema variation lost receipt: %v %v", done, err)
	}

}
