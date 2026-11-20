package store

import (
	"context"
	"errors"
	"testing"
)

func TestDisconnectedAuthorizationCannotRestoreCredentials(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	github, err := st.BeginConnection(ctx, "github")
	if err != nil {
		t.Fatal(err)
	}
	email, err := st.BeginConnection(ctx, "email")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ClearGitHubCredentials(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearEmailCredentials(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.SetGitHubCredentialsIfCurrent(ctx, "stale-token", "{}", true, github); !errors.Is(err, ErrConnectionChanged) {
		t.Fatalf("stale GitHub result saved: %v", err)
	}
	creds := EmailCredentials{Host: "imap.example", Username: "alice", Password: " password with spaces "}
	if err := st.SetEmailCredentialsIfCurrent(ctx, creds, email); !errors.Is(err, ErrConnectionChanged) {
		t.Fatalf("stale email result saved: %v", err)
	}
	if disabled, err := st.IsGitHubDisabled(ctx); err != nil || !disabled {
		t.Fatalf("GitHub polling restored: %v %v", disabled, err)
	}
	if disabled, err := st.IsEmailDisabled(ctx); err != nil || !disabled {
		t.Fatalf("email polling restored: %v %v", disabled, err)
	}
	github, err = st.BeginConnection(ctx, "github")
	if err != nil {
		t.Fatal(err)
	}
	email, err = st.BeginConnection(ctx, "email")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetGitHubCredentialsIfCurrent(ctx, "new-token", "{}", true, github); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEmailCredentialsIfCurrent(ctx, creds, email); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetEmailCredentials(ctx)
	if err != nil || got.Password != creds.Password {
		t.Fatalf("current email save lost credential bytes: %q %v", got.Password, err)
	}
	if err := st.SetGitHubCredentialsIfCurrent(ctx, "unguarded", "{}", true, ""); !errors.Is(err, ErrConnectionChanged) {
		t.Fatalf("missing generation accepted: %v", err)
	}
}

func TestDeliveryExpiredLeaseCannotOverwriteNewReceipt(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	// Use an existing item fixture from the store, with no outbound network calls.
	_, err = st.db.ExecContext(ctx, `INSERT INTO items(user_id,external_id,source_type,source_name,title,body,sender,url,tag,reason,route,priority,confidence,decision_source,decision_reason,status,published_at,created_at)
	 VALUES(1,'expiry','webhook','','','','','','','','queue_digest',1,1,'rule','','queued',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := st.db.QueryRowContext(ctx, `SELECT id FROM items WHERE external_id='expiry'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	old, _, err := st.ClaimDelivery(ctx, id, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE notification_deliveries SET lease_until=0`); err != nil {
		t.Fatal(err)
	}
	current, _, err := st.ClaimDelivery(ctx, id, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDelivery(ctx, id, "test", old, true); err == nil {
		t.Fatal("expired sender overwrote a new lease")
	}
	if err := st.FinishDelivery(ctx, id, "test", current, true); err != nil {
		t.Fatal(err)
	}
}

func TestSuccessfulDeliveryReceiptCannotBeReverted(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.db.ExecContext(ctx, `INSERT INTO items(user_id,external_id,source_type,source_name,title,body,sender,url,tag,reason,route,priority,confidence,decision_source,decision_reason,status,published_at,created_at) VALUES(1,'final-receipt','webhook','','','','','','','','queue_digest',1,1,'rule','','queued',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	var id int64
	st.db.QueryRowContext(ctx, `SELECT id FROM items WHERE external_id='final-receipt'`).Scan(&id)
	lease, _, err := st.ClaimDelivery(ctx, id, "channel")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDelivery(ctx, id, "channel", lease, true); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDelivery(ctx, id, "channel", lease, false); err == nil {
		t.Fatal("late failure reverted success")
	}
	if _, done, err := st.ClaimDelivery(ctx, id, "channel"); err != nil || !done {
		t.Fatalf("success receipt was lost: %v %v", done, err)
	}
}
