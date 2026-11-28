package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"focusgate/internal/config"
	"focusgate/internal/store"
)

func TestSlackEventsAuthenticateFilterAndDeduplicate(t *testing.T) {
	cfg := config.Default()
	cfg.Sources.Slack = config.SlackSourceConfig{
		Enabled: true, SigningSecret: "test-only-signing-secret", WorkspaceID: "T123", MentionUserID: "U123",
	}
	cfg.Rules = []config.RuleConfig{{Name: "Slack digest", SourceType: "slack", Action: "queue_digest"}}
	srv, st := setupAuthServer(t, cfg)
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	send := func(body, secret string, stamp time.Time) *httptest.ResponseRecorder {
		t.Helper()
		req := newLocalRequest(http.MethodPost, "/api/slack/events", bytes.NewBufferString(body))
		timestamp := strconv.FormatInt(stamp.Unix(), 10)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte("v0:" + timestamp + ":" + body))
		req.Header.Set("X-Slack-Request-Timestamp", timestamp)
		req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
		res := httptest.NewRecorder()
		srv.Handler().ServeHTTP(res, req)
		return res
	}
	const challenge = `{"type":"url_verification","challenge":"known-challenge"}`
	if res := send(challenge, cfg.Sources.Slack.SigningSecret, now); res.Code != http.StatusOK ||
		!strings.Contains(res.Body.String(), "known-challenge") {
		t.Fatalf("signed Slack challenge returned %d: %s", res.Code, res.Body.String())
	}
	const mention = `{"type":"event_callback","team_id":"T123","event_id":"Ev123","event":{"type":"message","user":"U321","text":"Review <@U123>","channel":"C123","ts":"1700000000.000042"}}`
	if res := send(mention, "other-key", now); res.Code != http.StatusUnauthorized {
		t.Fatalf("invalid signature returned %d", res.Code)
	}
	if res := send(mention, cfg.Sources.Slack.SigningSecret, now.Add(-6*time.Minute)); res.Code != http.StatusUnauthorized {
		t.Fatalf("stale signature returned %d", res.Code)
	}
	for _, body := range []string{
		strings.Replace(mention, "U123", "U000", 1),
		strings.Replace(strings.Replace(mention, "T123", "T000", 1), "C123", "C000", 1),
		mention,
		mention,
		strings.Replace(strings.Replace(mention, "Ev123", "Ev999", 1), "\"message\"", "\"app_mention\"", 1),
	} {
		if res := send(body, cfg.Sources.Slack.SigningSecret, now); res.Code != http.StatusOK {
			t.Fatalf("Slack event returned %d: %s", res.Code, res.Body.String())
		}
	}
	items, err := st.ListItems(context.Background(), store.ItemFilter{Source: "slack"})
	if err != nil || len(items) != 1 {
		t.Fatalf("expected one saved mention, got %d, error %v", len(items), err)
	}
	if item := items[0]; item.UserID != 1 || item.Body != "Review <@U123>" ||
		item.URL != "https://app.slack.com/archives/C123/p1700000000000042" {
		t.Fatalf("wrong owner, preview or Slack link: %+v", item)
	}

	for i, subtype := range []string{"file_share", "thread_broadcast", "bot_message"} {
		body := strings.Replace(mention, `"type":"message",`, `"type":"message","subtype":"`+subtype+`",`, 1)
		body = strings.Replace(body, "C123", "C90"+strconv.Itoa(i), 1)
		if res := send(body, cfg.Sources.Slack.SigningSecret, now); res.Code != http.StatusOK {
			t.Fatalf("Slack subtype %s returned %d", subtype, res.Code)
		}
	}
	items, err = st.ListItems(context.Background(), store.ItemFilter{Source: "slack"})
	if err != nil || len(items) != 3 {
		t.Fatalf("expected original and both eligible subtypes, got %d, error %v", len(items), err)
	}
	for _, item := range items {
		if strings.Contains(item.URL, "/C902/") {
			t.Fatal("bot message subtype was accepted")
		}
	}

	ctx := context.Background()
	admin, err := st.CreateUser(ctx, "admin", "test-operator-password")
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateUser(ctx, "member", "member-password-123")
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []struct {
		id      int64
		token   string
		hasItem bool
	}{
		{admin.ID, "dashboard-operator-cookie", true},
		{member.ID, "dashboard-member-cookie", false},
	} {
		if err := st.CreateUserSession(ctx, account.token, account.id, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		req := newLocalRequest(http.MethodGet, "/api/items?status=all&limit=50", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: account.token})
		res := httptest.NewRecorder()
		srv.Handler().ServeHTTP(res, req)
		if res.Code != http.StatusOK || strings.Contains(res.Body.String(), "Slack mention") != account.hasItem {
			t.Fatalf("unexpected inbox for user %d: %d %s", account.id, res.Code, res.Body.String())
		}
	}
}
