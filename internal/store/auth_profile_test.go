package store

import (
	"context"
	"testing"
	"time"
)

func TestStore_AuthAndSessions(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open memory store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	// Initial check on fresh database
	req, err := st.IsAuthSetupRequired(ctx)
	if err != nil {
		t.Fatalf("IsAuthSetupRequired error: %v", err)
	}
	if !req {
		t.Fatalf("expected setup_required to be true on fresh DB")
	}

	// Set master passphrase
	hash := "fake-base64-hash"
	salt := "fake-base64-salt"
	if err := st.SetMasterPassphrase(ctx, hash, salt); err != nil {
		t.Fatalf("SetMasterPassphrase error: %v", err)
	}

	// Should now not require setup
	req, err = st.IsAuthSetupRequired(ctx)
	if err != nil {
		t.Fatalf("IsAuthSetupRequired error: %v", err)
	}
	if req {
		t.Fatalf("expected setup_required to be false after setting passphrase")
	}

	// Retrieve passphrase
	gotHash, gotSalt, err := st.GetMasterPassphrase(ctx)
	if err != nil {
		t.Fatalf("GetMasterPassphrase error: %v", err)
	}
	if gotHash != hash || gotSalt != salt {
		t.Fatalf("hash/salt mismatch: got (%s, %s), want (%s, %s)", gotHash, gotSalt, hash, salt)
	}

	// Session management
	sessionToken := "test-session-token-12345"
	exp := time.Now().UTC().Add(1 * time.Hour)
	if err := st.CreateSession(ctx, sessionToken, exp); err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	valid, err := st.ValidateSession(ctx, sessionToken)
	if err != nil {
		t.Fatalf("ValidateSession error: %v", err)
	}
	if !valid {
		t.Fatalf("expected session to be valid")
	}

	// Invalid session
	valid, err = st.ValidateSession(ctx, "non-existent-token")
	if err != nil {
		t.Fatalf("ValidateSession error: %v", err)
	}
	if valid {
		t.Fatalf("expected non-existent token to be invalid")
	}

	// Expired session pruning
	expiredToken := "expired-token-999"
	past := time.Now().UTC().Add(-1 * time.Hour)
	if err := st.CreateSession(ctx, expiredToken, past); err != nil {
		t.Fatalf("CreateSession expired: %v", err)
	}
	if err := st.PruneSessions(ctx); err != nil {
		t.Fatalf("PruneSessions error: %v", err)
	}
	valid, err = st.ValidateSession(ctx, expiredToken)
	if err != nil {
		t.Fatalf("ValidateSession expired: %v", err)
	}
	if valid {
		t.Fatalf("expected expired session to be pruned/invalid")
	}

	// Explicit session deletion (logout)
	if err := st.DeleteSession(ctx, sessionToken); err != nil {
		t.Fatalf("DeleteSession error: %v", err)
	}
	valid, err = st.ValidateSession(ctx, sessionToken)
	if err != nil {
		t.Fatalf("ValidateSession after delete: %v", err)
	}
	if valid {
		t.Fatalf("expected deleted session to be invalid")
	}
}

func TestStore_GitHubCredentials(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open memory store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	// Initial check
	tok, user, part, err := st.GetGitHubCredentials(ctx)
	if err != nil {
		t.Fatalf("GetGitHubCredentials: %v", err)
	}
	if tok != "" || user != "" || !part {
		t.Fatalf("expected empty credentials with default participating_only=true, got (%s, %s, %v)", tok, user, part)
	}

	// Save credentials
	tokenVal := "ghp_mocktesttoken123456789"
	userVal := `{"login":"focuscoder","avatar_url":"https://example.com/avatar.png","name":"Focus Coder"}`
	if err := st.SetGitHubCredentials(ctx, tokenVal, userVal, false); err != nil {
		t.Fatalf("SetGitHubCredentials: %v", err)
	}

	tok, user, part, err = st.GetGitHubCredentials(ctx)
	if err != nil {
		t.Fatalf("GetGitHubCredentials after save: %v", err)
	}
	if tok != tokenVal || user != userVal || part != false {
		t.Fatalf("credentials mismatch: got (%s, %s, %v)", tok, user, part)
	}

	// Update participating only
	if err := st.SetGitHubCredentials(ctx, tokenVal, userVal, true); err != nil {
		t.Fatalf("SetGitHubCredentials update: %v", err)
	}
	_, _, part, err = st.GetGitHubCredentials(ctx)
	if err != nil || !part {
		t.Fatalf("expected participating_only to be true, got %v (%v)", part, err)
	}

	// Clear credentials
	if err := st.ClearGitHubCredentials(ctx); err != nil {
		t.Fatalf("ClearGitHubCredentials: %v", err)
	}
	tok, user, _, err = st.GetGitHubCredentials(ctx)
	if err != nil {
		t.Fatalf("GetGitHubCredentials after clear: %v", err)
	}
	if tok != "" || user != "" {
		t.Fatalf("expected cleared credentials, got tok=%s, user=%s", tok, user)
	}
}
