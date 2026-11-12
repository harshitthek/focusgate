package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"focusgate/internal/config"
	"focusgate/internal/store"
)

func TestDispatcher_NtfyPushAndDigest(t *testing.T) {
	var mu sync.Mutex
	type captured struct {
		path     string
		auth     string
		title    string
		priority string
		tags     string
		click    string
		body     string
	}
	var reqs []captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, captured{
			path:     r.URL.Path,
			auth:     r.Header.Get("Authorization"),
			title:    r.Header.Get("X-Title"),
			priority: r.Header.Get("X-Priority"),
			tags:     r.Header.Get("X-Tags"),
			click:    r.Header.Get("X-Click"),
			body:     string(raw),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Server.PublicURL = "http://focus.local:8787"
	cfg.Notify.DesktopWindows.Enabled = false
	cfg.Notify.Ntfy = config.NtfyConfig{
		Enabled: true,
		Server:  srv.URL,
		Topic:   "redmi-alerts",
		Token:   "tk_123",
	}

	d := NewDispatcher(cfg)
	ctx := context.Background()

	err := d.PushItem(ctx, store.StoredItem{
		ID:         1,
		SourceType: "github",
		SourceName: "org/repo",
		Title:      "PR Review Needed",
		Body:       "Please review #42",
		Sender:     "alice",
		URL:        "https://github.com/org/repo/pull/42",
		Priority:   5,
	})
	if err != nil {
		t.Fatalf("PushItem failed: %v", err)
	}

	delivered, err := d.SendDigest(ctx, []store.StoredItem{
		{ID: 2, SourceName: "Go Blog", Title: "Go 1.27 Notes", Uncertain: true},
		{ID: 3, SourceName: "Lobsters", Title: "SQLite WAL tips"},
	})
	if err != nil || !delivered {
		t.Fatalf("SendDigest failed: delivered=%v err=%v", delivered, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 ntfy requests, got %d", len(reqs))
	}
	if reqs[0].path != "/redmi-alerts" || reqs[0].auth != "Bearer tk_123" || reqs[0].priority != "5" || reqs[0].tags != "rotating_light" {
		t.Fatalf("unexpected immediate push request: %+v", reqs[0])
	}
	if reqs[1].priority != "2" || !strings.Contains(reqs[1].body, "[review]") || !strings.Contains(reqs[1].body, "SQLite WAL tips") {
		t.Fatalf("unexpected digest request: %+v", reqs[1])
	}
}

func TestSanitizeHeaderValue(t *testing.T) {
	input := "Line1\r\nLine2\x00with\x07bell\x7fDEL\tand Tab"
	got := sanitizeHeaderValue(input)
	expected := "Line1  Line2withbellDEL\tand Tab"
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
}

func TestNtfyPublisher_EmptyTopicErrorsAndTruncateUTF8(t *testing.T) {
	pub := NewNtfyPublisher(config.NtfyConfig{
		Enabled: true,
		Topic:   "   ",
	})
	if err := pub.Publish(context.Background(), Message{Title: "test"}); err == nil {
		t.Fatal("expected error when ntfy enabled with empty topic")
	}

	// Verify rune-safe truncation
	multiByte := strings.Repeat("世界", 100) // 100 * 6 bytes = 600 bytes
	truncated := truncateBytesUTF8(multiByte, 10)
	if len(truncated) > 10 {
		t.Fatalf("expected len <= 10, got %d", len(truncated))
	}
	// "世界" is 3 bytes each; 10 bytes should fit 3 runes = 9 bytes
	if len(truncated) != 9 {
		t.Fatalf("expected 9 bytes for 3 utf-8 characters, got %d", len(truncated))
	}
}

func TestNtfyPublisher_RedirectHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pub := NewNtfyPublisher(config.NtfyConfig{
		Enabled: true,
		Server:  srv.URL,
		Topic:   "redirect",
	})
	err := pub.Publish(context.Background(), Message{Title: "alert", Body: "hello"})
	if err == nil {
		t.Fatal("expected error on 302 redirect, got nil")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected rejection error, got: %v", err)
	}
}
