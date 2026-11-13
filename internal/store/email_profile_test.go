package store

import (
	"context"
	"testing"
)

func TestEmailCredentialsStore(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open memory store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	// Initial state: not enabled
	creds, err := st.GetEmailCredentials(ctx)
	if err != nil {
		t.Fatalf("GetEmailCredentials failed: %v", err)
	}
	if creds.Enabled {
		t.Errorf("expected disabled initially, got %+v", creds)
	}

	// Save credentials
	input := EmailCredentials{
		Enabled:     true,
		Provider:    "gmail",
		Host:        "imap.gmail.com",
		Port:        993,
		Username:    "test@gmail.com",
		Password:    "secretpass1234",
		Mailbox:     "INBOX",
		PollMinutes: 5,
	}
	if err := st.SetEmailCredentials(ctx, input); err != nil {
		t.Fatalf("SetEmailCredentials failed: %v", err)
	}

	// Verify retrieved
	creds, err = st.GetEmailCredentials(ctx)
	if err != nil {
		t.Fatalf("GetEmailCredentials failed: %v", err)
	}
	if !creds.Enabled || creds.Username != "test@gmail.com" || creds.Host != "imap.gmail.com" || creds.Port != 993 || creds.PollMinutes != 5 {
		t.Errorf("retrieved credentials mismatch: %+v", creds)
	}

	// Update poll minutes
	if err := st.SetEmailPollMinutes(ctx, 2); err != nil {
		t.Fatalf("SetEmailPollMinutes failed: %v", err)
	}
	creds, _ = st.GetEmailCredentials(ctx)
	if creds.PollMinutes != 2 {
		t.Errorf("expected poll_minutes 2, got %d", creds.PollMinutes)
	}

	// Clear credentials
	if err := st.ClearEmailCredentials(ctx); err != nil {
		t.Fatalf("ClearEmailCredentials failed: %v", err)
	}
	creds, err = st.GetEmailCredentials(ctx)
	if err != nil {
		t.Fatalf("GetEmailCredentials after clear failed: %v", err)
	}
	if creds.Enabled {
		t.Errorf("expected disabled after clear, got %+v", creds)
	}
	dis, err := st.IsEmailDisabled(ctx)
	if err != nil || !dis {
		t.Errorf("expected email_disabled=true, got %v (err=%v)", dis, err)
	}
}
