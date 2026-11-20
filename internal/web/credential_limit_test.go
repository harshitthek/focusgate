package web

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCredentialLimitsBoundAttemptsAndMemory(t *testing.T) {
	var limiter credentialLimiter
	now := time.Now()
	for i := 0; i < 10; i++ {
		if !limiter.allow("account:alice", 10, now) {
			t.Fatal("initial attempt rejected")
		}
	}
	if limiter.allow("account:alice", 10, now.Add(59*time.Second)) {
		t.Fatal("allowed attempt before reset")
	}
	if !limiter.allow("account:bob", 10, now) {
		t.Fatal("another account was blocked")
	}
	if !limiter.allow("account:alice", 10, now.Add(time.Minute)) {
		t.Fatal("minute boundary did not reset limit")
	}
	for i := 0; i < 1000; i++ {
		limiter.allow(fmt.Sprint(i), 10, now)
	}
	if len(limiter.windows) > maxCredentialWindows {
		t.Fatal("unbounded credential bookkeeping")
	}
}

func TestLoginLimitsAccountAcrossPeerAddresses(t *testing.T) {
	srv, st := setupTestServer(t)
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateUser(t.Context(), "alice", "alice-correct-password"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 11; i++ {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/auth/login", bytes.NewBufferString(`{"username":"alice","password":"incorrect"}`))
		req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", i+1)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		expected := http.StatusUnauthorized
		if i == 10 {
			expected = http.StatusTooManyRequests
		}
		if rec.Code != expected {
			t.Fatalf("attempt %d returned %d, expected %d", i, rec.Code, expected)
		}
	}
}

func TestLiveCredentialQuotaCannotBeEvicted(t *testing.T) {
	var limiter credentialLimiter
	now := time.Now()
	for i := 0; i < 10; i++ {
		if !limiter.allow("protected", 10, now) {
			t.Fatal("initial attempt failed")
		}
	}
	for i := 0; i < maxCredentialWindows-1; i++ {
		if !limiter.allow(fmt.Sprint(i), 10, now) {
			t.Fatal("map filled prematurely")
		}
	}
	if limiter.allow("new-key", 10, now) {
		t.Fatal("live capacity was evicted")
	}
	if limiter.allow("protected", 10, now) {
		t.Fatal("exhausted quota disappeared")
	}
	if !limiter.allow("new-key", 10, now.Add(time.Minute)) {
		t.Fatal("expired windows were not reclaimed")
	}
}

func TestLegacyLoginRateCannotBeSplitWithDummyUsernames(t *testing.T) {
	srv, st := setupTestServer(t)
	defer st.Close()
	for i := 0; i < 11; i++ {
		body := fmt.Sprintf(`{"username":"dummy%d","passphrase":"wrong"}`, i)
		req := newLocalRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		expected := http.StatusUnauthorized
		if i == 10 {
			expected = http.StatusTooManyRequests
		}
		if rec.Code != expected {
			t.Fatalf("attempt %d got %d, want %d", i, rec.Code, expected)
		}
	}
}

func TestUnknownAccountChurnLeavesKnownLoginAvailable(t *testing.T) {
	srv, st := setupTestServer(t)
	defer st.Close()
	if _, err := st.CreateUser(t.Context(), "alice", "alice-correct-password"); err != nil {
		t.Fatal(err)
	}
	attempt := func(peer, username, password string) int {
		payload := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
		req := newLocalRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(payload))
		req.RemoteAddr = peer
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 520; i++ {
		peer := fmt.Sprintf("192.0.2.%d:1234", 1+i%16)
		attempt(peer, fmt.Sprintf("missing_%d", i), "incorrect-password")
	}
	if code := attempt("192.0.2.1:1234", "alice", "alice-correct-password"); code != http.StatusOK {
		t.Fatalf("known account login returned HTTP %d after unknown login churn", code)
	}
}
