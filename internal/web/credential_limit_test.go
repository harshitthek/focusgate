package web

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"focusgate/internal/config"
)

func TestProxyCredentialQuotasTrustOnlyConfiguredPeers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		trusted   []string
		peer      string
		forwarded bool
		want      int
	}{
		{"direct ignores header", nil, "127.0.0.1:1234", true, http.StatusTooManyRequests},
		{"trusted separates clients", []string{"127.0.0.1"}, "127.0.0.1:1234", true, http.StatusNoContent},
		{"untrusted ignores header", []string{"127.0.0.1"}, "192.0.2.1:1234", true, http.StatusTooManyRequests},
		{"missing header uses peer", []string{"127.0.0.1"}, "127.0.0.1:1234", false, http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Server.TrustedProxyIPs = tc.trusted
			srv, st := setupAuthServer(t, cfg)
			defer st.Close()
			handler := srv.withBoundedCredentials(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			for i := 0; i < 61; i++ {
				req := newLocalRequest(http.MethodPost, "/api/auth/login", nil)
				req.RemoteAddr = tc.peer
				if tc.forwarded {
					req.Header.Set("X-Real-IP", fmt.Sprintf("198.51.100.%d", i+1))
				}
				rec := httptest.NewRecorder()
				handler(rec, req)
				if i == 60 && rec.Code != tc.want {
					t.Fatalf("quota = %d, want %d", rec.Code, tc.want)
				}
			}
		})
	}
	for _, value := range []string{"", "garbage", "198.51.100.1, 198.51.100.2"} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = "[::ffff:127.0.0.1]:1234"
		r.Header.Set("X-Real-IP", value)
		if got := credentialPeer(r, []string{"127.0.0.1"}); got != "127.0.0.1" {
			t.Fatalf("invalid forwarded identity %q changed quota: %q", value, got)
		}
	}
}

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
