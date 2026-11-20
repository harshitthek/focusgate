package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

func TestDigestRetriesOnlyFailedDestinations(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	item, _, err := st.InsertItem(ctx, ingest.IncomingItem{
		SourceType: "rss", ExternalID: "receipt-1", Title: "Retry one destination",
	}, classify.Decision{Route: "queue_digest"})
	if err != nil {
		t.Fatal(err)
	}
	var ntfyHits atomic.Int32
	var slackHits atomic.Int32
	var slackOK atomic.Bool
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ntfyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		slackHits.Add(1)
		if !slackOK.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer slackServer.Close()
	cfg := config.Default()
	cfg.Notify.DesktopWindows.Enabled = false
	cfg.Notify.Ntfy = config.NtfyConfig{Enabled: true, Server: ntfyServer.URL, Topic: "receipt"}
	cfg.Notify.Slack = config.SlackConfig{Enabled: true, WebhookURL: slackServer.URL}
	first := NewDispatcher(cfg)
	first.SetDeliveryStore(st)
	if done, err := first.SendDigest(ctx, []store.StoredItem{item}); done || err == nil {
		t.Fatalf("partial send must remain pending: %v (%v)", done, err)
	}
	slackOK.Store(true)
	second := NewDispatcher(cfg)
	second.SetDeliveryStore(st)
	if done, err := second.SendDigest(ctx, []store.StoredItem{item}); !done || err != nil {
		t.Fatalf("retry did not complete: %v (%v)", done, err)
	}
	if ntfyHits.Load() != 1 || slackHits.Load() != 2 {
		t.Fatalf("duplicate successful destination: ntfy=%d slack=%d", ntfyHits.Load(), slackHits.Load())
	}
}

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
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/redirect" {
				http.Redirect(w, r, "http://external-host.example.com/target", code)
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
			t.Fatalf("expected error on %d redirect, got nil", code)
		}
		if !strings.Contains(err.Error(), "rejected") {
			t.Fatalf("expected rejection error for status %d, got: %v", code, err)
		}
	}
}

func TestSlackPublisher_Publish(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		receivedBody = string(raw)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	pub := NewSlackPublisher(config.SlackConfig{
		Enabled:       true,
		WebhookURL:    srv.URL,
		MentionHandle: "<@U999>",
	})

	err := pub.Publish(context.Background(), store.StoredItem{
		Title:      "PR Review Needed",
		Body:       "Please check this pull request",
		Priority:   4,
		SourceType: "github",
		URL:        "https://github.com/owner/repo/pull/1",
	}, "http://127.0.0.1:8787")

	if err != nil {
		t.Fatalf("Publish failed: %v", err)
	}
	if !strings.Contains(receivedBody, "U999") {
		t.Errorf("expected mention in payload, got %s", receivedBody)
	}
	if !strings.Contains(receivedBody, "PR Review Needed") {
		t.Errorf("expected title in payload, got %s", receivedBody)
	}

	// Test title with special characters -> summaryText and header must escape them
	err = pub.Publish(context.Background(), store.StoredItem{
		Title:      "Security alert: <script> & attack",
		Priority:   5,
		SourceType: "github",
	}, "http://127.0.0.1:8787")
	if err != nil {
		t.Fatalf("Publish with special characters failed: %v", err)
	}
	if strings.Contains(receivedBody, "<script>") {
		t.Errorf("expected <script> to be escaped in payload")
	}
	var sentPayload map[string]any
	if err := json.Unmarshal([]byte(receivedBody), &sentPayload); err != nil {
		t.Fatalf("failed to unmarshal sent payload: %v", err)
	}
	textVal, _ := sentPayload["text"].(string)
	if !strings.Contains(textVal, "&lt;script&gt; &amp; attack") {
		t.Errorf("expected escaped entities in unmarshaled text, got %s", textVal)
	}

	// Test huge URL exceeding header budget -> link should be omitted without breaking markdown
	hugeURL := "https://example.com/" + strings.Repeat("a", 3500)
	err = pub.Publish(context.Background(), store.StoredItem{
		Title:      "Excessive URL Alert",
		Priority:   3,
		SourceType: "webhook",
		URL:        hugeURL,
	}, "http://127.0.0.1:8787")
	if err != nil {
		t.Fatalf("Publish with huge URL failed: %v", err)
	}
	if strings.Contains(receivedBody, hugeURL) {
		t.Errorf("expected huge URL to be omitted from header")
	}
	if !strings.Contains(receivedBody, "Excessive URL Alert") {
		t.Errorf("expected title to be preserved in header")
	}

	// Test URL with markup delimiters attempting link escape and injected mention
	maliciousURL := "https://example.com/item> <!channel> | injected"
	err = pub.Publish(context.Background(), store.StoredItem{
		Title:      "Malicious URL Alert",
		Priority:   3,
		SourceType: "webhook",
		URL:        maliciousURL,
	}, "http://127.0.0.1:8787")
	if err != nil {
		t.Fatalf("Publish with malicious URL failed: %v", err)
	}
	var malPayload map[string]any
	if err := json.Unmarshal([]byte(receivedBody), &malPayload); err != nil {
		t.Fatalf("failed to unmarshal payload: %v", err)
	}
	blocks, _ := malPayload["blocks"].([]any)
	if len(blocks) > 0 {
		section, _ := blocks[0].(map[string]any)
		textMap, _ := section["text"].(map[string]any)
		headerText, _ := textMap["text"].(string)
		if strings.Contains(headerText, "<!channel>") {
			t.Errorf("vulnerability: raw <!channel> escaped link boundary in header: %s", headerText)
		}
		if !strings.Contains(headerText, "%3E") {
			t.Errorf("expected encoded delimiter %%3E in header text, got %s", headerText)
		}
	}
}

func TestSlackPublisher_PublishDigest_OversizedURL(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		receivedBody = string(raw)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	pub := NewSlackPublisher(config.SlackConfig{
		Enabled:    true,
		WebhookURL: srv.URL,
	})

	hugeURL := "https://example.com/" + strings.Repeat("a", 3500)
	err := pub.PublishDigest(context.Background(), "FocusGate Digest (5 items)", "• Item 1\n• Item 2", hugeURL)
	if err != nil {
		t.Fatalf("PublishDigest failed: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(receivedBody), &payload); err != nil {
		t.Fatalf("unmarshal payload failed: %v", err)
	}
	blocks, _ := payload["blocks"].([]any)
	if len(blocks) > 0 {
		section, _ := blocks[0].(map[string]any)
		textMap, _ := section["text"].(map[string]any)
		headerText, _ := textMap["text"].(string)
		if strings.Contains(headerText, hugeURL) {
			t.Errorf("expected oversized URL to be omitted from digest header")
		}
		if !strings.Contains(headerText, "FocusGate Digest (5 items)") {
			t.Errorf("expected digest title preserved in header, got: %s", headerText)
		}
	}
}

func TestTruncateEscapedSlackText(t *testing.T) {
	// 1. Fits within maxLen -> untouched
	s := "Short text &amp; more"
	if res := truncateEscapedSlackText(s, 50); res != s {
		t.Errorf("expected %q, got %q", s, res)
	}

	// 2. Truncation does not cut in middle of &amp;
	// s has "Hello " (6) + "&amp;" (5) = 11 chars. Truncating to 10 with "\n..." (4 chars)
	// target is 6. Truncated is "Hello ". Result should be "Hello \n..."
	s2 := "Hello &amp; world"
	res2 := truncateEscapedSlackText(s2, 10)
	if strings.Contains(res2, "&am") && !strings.Contains(res2, "&amp;") {
		t.Errorf("found broken entity in: %q", res2)
	}
	if len(res2) > 10 {
		t.Errorf("expected len <= 10, got %d", len(res2))
	}
}

func TestMatrixPublisher_Publish(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		receivedBody = string(raw)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"event_id":"$abc"}`))
	}))
	defer srv.Close()

	pub := NewMatrixPublisher(config.MatrixConfig{
		Enabled:       true,
		WebhookURL:    srv.URL,
		MentionHandle: "@alice:matrix.org",
	})

	err := pub.Publish(context.Background(), store.StoredItem{
		Title:      "Outage Warning",
		Body:       "High error rate detected",
		Priority:   5,
		SourceType: "webhook",
	}, "http://127.0.0.1:8787")

	if err != nil {
		t.Fatalf("Publish failed: %v", err)
	}
	if !strings.Contains(receivedBody, "@alice:matrix.org") {
		t.Errorf("expected matrix mention in payload, got %s", receivedBody)
	}
	if !strings.Contains(receivedBody, "Outage Warning") {
		t.Errorf("expected title in payload, got %s", receivedBody)
	}
}

func TestFilter_IsMention(t *testing.T) {
	handles := []string{"@harshit", "devops"}

	// GitHub mention reason
	ghItem := store.StoredItem{SourceType: "github", Reason: "mention"}
	if !IsMention(ghItem, handles) {
		t.Errorf("expected GitHub reason mention to match")
	}

	// GitHub review_requested
	ghReview := store.StoredItem{SourceType: "github", Reason: "review_requested"}
	if !IsMention(ghReview, handles) {
		t.Errorf("expected GitHub review_requested to match")
	}

	// GitHub team_mention
	ghTeam := store.StoredItem{SourceType: "github", Reason: "team_mention"}
	if !IsMention(ghTeam, handles) {
		t.Errorf("expected GitHub team_mention to match")
	}

	// Body mention
	bodyItem := store.StoredItem{SourceType: "email", Title: "Build failed", Body: "cc @harshit to investigate"}
	if !IsMention(bodyItem, handles) {
		t.Errorf("expected body mention to match")
	}

	// Unrelated item
	unrelated := store.StoredItem{SourceType: "rss", Title: "Tech news", Body: "Just another article"}
	if IsMention(unrelated, handles) {
		t.Errorf("expected unrelated item not to match")
	}
}

func TestDispatcher_OnlyMentions(t *testing.T) {
	var pushCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pushCount++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Notify.DesktopWindows.Enabled = false
	cfg.Notify.Slack = config.SlackConfig{
		Enabled:    true,
		WebhookURL: srv.URL,
	}
	cfg.Notify.OnlyMentions = true
	cfg.Notify.MentionHandles = []string{"@alice"}

	d := NewDispatcher(cfg)
	ctx := context.Background()

	// 1. Non-mention item with Priority 4 -> Should be skipped
	nonMention := store.StoredItem{
		ID:         1,
		Title:      "General commit on repo",
		Body:       "Routine change",
		Priority:   4,
		SourceType: "github",
		Reason:     "subscribed",
	}
	if err := d.PushItem(ctx, nonMention); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pushCount != 0 {
		t.Errorf("expected 0 pushes for non-mention, got %d", pushCount)
	}

	// 2. Mention item with Priority 4 -> Should be pushed
	mention := store.StoredItem{
		ID:         2,
		Title:      "Review request",
		Body:       "Please check @alice",
		Priority:   4,
		SourceType: "github",
		Reason:     "mention",
	}
	if err := d.PushItem(ctx, mention); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pushCount != 1 {
		t.Errorf("expected 1 push for mention, got %d", pushCount)
	}

	// 3. Critical P5 outage item -> Should be pushed even if no mention
	p5Item := store.StoredItem{
		ID:         3,
		Title:      "CRITICAL: Production Outage",
		Body:       "All API endpoints down",
		Priority:   5,
		SourceType: "webhook",
	}
	if err := d.PushItem(ctx, p5Item); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pushCount != 2 {
		t.Errorf("expected 2 pushes after P5 item, got %d", pushCount)
	}

	// 4. Priority-4 GitHub team_mention with no personal handle in text -> Should be pushed
	teamMentionItem := store.StoredItem{
		ID:         4,
		Title:      "Security patch ready for review",
		Body:       "Please review the PR",
		Priority:   4,
		SourceType: "github",
		Reason:     "team_mention",
	}
	if err := d.PushItem(ctx, teamMentionItem); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pushCount != 3 {
		t.Errorf("expected 3 pushes after team_mention item, got %d", pushCount)
	}

	// 5. SendDigest with only non-mention items -> Should return true, nil without publishing
	nonMentionBatch := []store.StoredItem{nonMention}
	delivered, err := d.SendDigest(ctx, nonMentionBatch)
	if err != nil {
		t.Fatalf("unexpected error on digest: %v", err)
	}
	if !delivered {
		t.Errorf("expected delivered=true for filtered batch")
	}
	if pushCount != 3 {
		t.Errorf("expected pushCount to remain 3, got %d", pushCount)
	}

	// 6. SendDigest with a mention item -> Should deliver
	mentionBatch := []store.StoredItem{nonMention, mention}
	delivered, err = d.SendDigest(ctx, mentionBatch)
	if err != nil {
		t.Fatalf("unexpected error on digest with mention: %v", err)
	}
	if !delivered {
		t.Errorf("expected delivered=true for batch with mention")
	}
	if pushCount != 4 {
		t.Errorf("expected pushCount to increase to 4, got %d", pushCount)
	}
}

func TestIsMention_WordBoundariesAndDelimiters(t *testing.T) {
	// Parse handles with whitespace, commas, semicolons
	parsed := ParseMentionHandles("@alice, bob;  charlie \t dave\neve")
	expected := []string{"@alice", "bob", "charlie", "dave", "eve"}
	if len(parsed) != len(expected) {
		t.Fatalf("expected %d parsed handles, got %d (%v)", len(expected), len(parsed), parsed)
	}
	for i, ex := range expected {
		if parsed[i] != ex {
			t.Errorf("parsed[%d] expected %q, got %q", i, ex, parsed[i])
		}
	}

	// Boundary tests: "dan" should NOT match "standard" or "danger"
	danHandles := []string{"dan"}
	falseItems := []store.StoredItem{
		{Title: "Fixing standard library bug", Body: "routine commit"},
		{Title: "Danger zone update", Body: "guidance notes"},
	}
	for _, item := range falseItems {
		if IsMention(item, danHandles) {
			t.Errorf("expected false for substring match in %q, got true", item.Title)
		}
	}

	// True items: word boundaries around "dan"
	trueItems := []store.StoredItem{
		{Title: "Hey dan, can you review?", Body: "PR #42"},
		{Title: "Assigned to @dan", Body: "critical issue"},
		{Title: "Update", Sender: "dan@example.com"},
	}
	for _, item := range trueItems {
		if !IsMention(item, danHandles) {
			t.Errorf("expected true for boundary match in title %q / sender %q, got false", item.Title, item.Sender)
		}
	}

	// Unicode boundary tests:
	// 1. "dan" should NOT match "@dané"
	if IsMention(store.StoredItem{Title: "Hey @dané, check this out"}, danHandles) {
		t.Errorf("expected false for 'dan' matching '@dané'")
	}
	// 2. "@dané" should match "@dané"
	if !IsMention(store.StoredItem{Title: "Hey @dané, check this out"}, []string{"@dané"}) {
		t.Errorf("expected true for '@dané' matching '@dané'")
	}
	// 3. Multi-byte CJK handle "李" should NOT match "@李明"
	if IsMention(store.StoredItem{Title: "cc @李明 for signoff"}, []string{"李"}) {
		t.Errorf("expected false for '李' matching '@李明'")
	}
	// 4. Full CJK handle "@李明" should match "@李明"
	if !IsMention(store.StoredItem{Title: "cc @李明 for signoff"}, []string{"@李明"}) {
		t.Errorf("expected true for '@李明' matching '@李明'")
	}
}

func TestDispatcher_SlackAndMatrixDigest(t *testing.T) {
	var mu sync.Mutex
	slackReceived := 0
	matrixReceived := 0

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		slackReceived++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer slackSrv.Close()

	matrixSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		matrixReceived++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer matrixSrv.Close()

	cfg := config.Default()
	cfg.Notify.Ntfy.Enabled = false
	cfg.Notify.DesktopWindows.Enabled = false
	cfg.Notify.Slack = config.SlackConfig{
		Enabled:    true,
		WebhookURL: slackSrv.URL,
	}
	cfg.Notify.Matrix = config.MatrixConfig{
		Enabled:    true,
		WebhookURL: matrixSrv.URL,
	}

	d := NewDispatcher(cfg)
	ctx := context.Background()

	items := []store.StoredItem{
		{ID: 1, Title: "PR review ready", SourceName: "GitHub", Priority: 2},
		{ID: 2, Title: "New build notification", SourceName: "CI", Priority: 1},
	}

	delivered, err := d.SendDigest(ctx, items)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !delivered {
		t.Errorf("expected delivered=true, got false")
	}

	mu.Lock()
	sCount := slackReceived
	mCount := matrixReceived
	mu.Unlock()

	if sCount != 1 {
		t.Errorf("expected 1 Slack digest delivery, got %d", sCount)
	}
	if mCount != 1 {
		t.Errorf("expected 1 Matrix digest delivery, got %d", mCount)
	}
}

func TestDispatcher_SendTestAlert_AllChannels(t *testing.T) {
	var mu sync.Mutex
	slackTests := 0
	matrixTests := 0

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		slackTests++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer slackSrv.Close()

	matrixSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		matrixTests++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer matrixSrv.Close()

	cfg := config.Default()
	cfg.Notify.Ntfy.Enabled = false
	cfg.Notify.DesktopWindows.Enabled = false
	cfg.Notify.Slack = config.SlackConfig{
		Enabled:    true,
		WebhookURL: slackSrv.URL,
	}
	cfg.Notify.Matrix = config.MatrixConfig{
		Enabled:    true,
		WebhookURL: matrixSrv.URL,
	}

	d := NewDispatcher(cfg)
	sent, err := d.SendTestAlert(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(sent) != 2 {
		t.Fatalf("expected 2 sent channels, got %d: %v", len(sent), sent)
	}

	mu.Lock()
	sCount := slackTests
	mCount := matrixTests
	mu.Unlock()

	if sCount != 1 {
		t.Errorf("expected 1 Slack test, got %d", sCount)
	}
	if mCount != 1 {
		t.Errorf("expected 1 Matrix test, got %d", mCount)
	}
}

type privateErrorTransport struct{}

func (privateErrorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: context.DeadlineExceeded}
}
func TestSlackErrorsDoNotExposeWebhookSecrets(t *testing.T) {
	const endpoint = "https://hooks.slack.com/services/VERY_SECRET_PATH"
	publisher := NewSlackPublisher(config.SlackConfig{Enabled: true, WebhookURL: endpoint})
	publisher.client = &http.Client{Transport: privateErrorTransport{}}
	err := publisher.SendTest(context.Background(), endpoint, "")
	if err == nil || strings.Contains(err.Error(), "VERY_SECRET_PATH") || strings.Contains(err.Error(), "https://") || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("unsanitized timeout diagnostic: %v", err)
	}
	err = publisher.SendTest(context.Background(), "https://VERY_SECRET_PATH/%zz", "")
	if err == nil || strings.Contains(err.Error(), "VERY_SECRET_PATH") {
		t.Fatalf("URL parse diagnostic exposed a secret: %v", err)
	}
}
