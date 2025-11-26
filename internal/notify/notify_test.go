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
