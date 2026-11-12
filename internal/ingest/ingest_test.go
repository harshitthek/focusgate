package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestParseFeedXML_RSSAndAtom(t *testing.T) {
	rssXML := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Test</title>
  <item>
    <guid>rss-1</guid>
    <title>Hello &amp; <b>World</b></title>
    <link>https://example.com/1</link>
    <description>&lt;p&gt;Body   text&lt;/p&gt;</description>
    <pubDate>Mon, 02 Jan 2006 15:04:05 +0000</pubDate>
  </item>
</channel></rss>`

	items, err := ParseFeedXML(strings.NewReader(rssXML), "TestRSS", "dev", 200)
	if err != nil {
		t.Fatalf("ParseFeedXML RSS error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 RSS item, got %d", len(items))
	}
	if items[0].Title != "Hello & World" {
		t.Fatalf("unexpected RSS title: %q", items[0].Title)
	}
	if items[0].Body != "Body text" {
		t.Fatalf("unexpected RSS body: %q", items[0].Body)
	}

	atomXML := `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>atom-1</id>
    <title>Go 1.27 Released</title>
    <link rel="alternate" href="https://go.dev/blog/go1.27" />
    <summary>Low memory GC improvements.</summary>
    <updated>2026-11-10T10:00:00Z</updated>
    <author><name>Go Team</name></author>
  </entry>
</feed>`

	atomItems, err := ParseFeedXML(strings.NewReader(atomXML), "Go Blog", "dev", 200)
	if err != nil {
		t.Fatalf("ParseFeedXML Atom error: %v", err)
	}
	if len(atomItems) != 1 || atomItems[0].URL != "https://go.dev/blog/go1.27" || atomItems[0].Sender != "Go Team" {
		t.Fatalf("unexpected Atom item: %+v", atomItems)
	}

	cdataXML := `<rss version="2.0"><channel><item>
		<guid>c1</guid>
		<title>Go &lt;3 Low RAM</title>
		<description><![CDATA[<p>Tiny &amp; fast</p>]]></description>
	</item></channel></rss>`
	cdItems, err := ParseFeedXML(strings.NewReader(cdataXML), "CDATA", "dev", 200)
	if err != nil || len(cdItems) != 1 {
		t.Fatalf("CDATA parse failed: %v %+v", err, cdItems)
	}
	if cdItems[0].Title != "Go <3 Low RAM" || cdItems[0].Body != "Tiny & fast" {
		t.Fatalf("unexpected CDATA item: %+v", cdItems[0])
	}
}

func TestWebhookVerifyAndParse(t *testing.T) {
	body := []byte(`{"title":"Deploy failed","body":"502 Bad Gateway on /api","sender":"uptime","url":"https://status.example.com"}`)
	if !VerifyWebhookAuth("s3cr3t", "Bearer s3cr3t", "", "", body) {
		t.Fatal("expected Bearer auth to pass")
	}
	if VerifyWebhookAuth("s3cr3t", "Bearer wrong", "", "", body) {
		t.Fatal("expected wrong Bearer auth to fail")
	}

	item, err := ParseWebhook("uptime", "", "", body, 100)
	if err != nil {
		t.Fatalf("ParseWebhook failed: %v", err)
	}
	if item.Title != "Deploy failed" || item.Sender != "uptime" || !strings.HasPrefix(item.ExternalID, "wh:") {
		t.Fatalf("unexpected parsed webhook: %+v", item)
	}

	ghBody := []byte(`{"action":"opened","sender":{"login":"bob"},"repository":{"full_name":"me/app","html_url":"https://github.com/me/app"},"pull_request":{"title":"Fix bug","body":"Details","html_url":"https://github.com/me/app/pull/1"}}`)
	ghItem, err := ParseWebhook("github", "pull_request", "deliv-123", ghBody, 100)
	if err != nil || ghItem.ExternalID != "gh-webhook:deliv-123" || ghItem.SourceType != "github" {
		t.Fatalf("unexpected GitHub webhook item: %+v err=%v", ghItem, err)
	}
}

func TestEmailSnippetAndGitHubURLMapping(t *testing.T) {
	rawMIME := "Content-Type: multipart/alternative; boundary=\"boundary123\"\n\n--boundary123\nContent-Type: text/html; charset=utf-8\n\n<p>Server <b>disk</b> at 95%</p>\n--boundary123--"
	clean := cleanEmailSnippet(rawMIME)
	if clean != "Server disk at 95%" {
		t.Fatalf("unexpected cleaned email snippet: %q", clean)
	}

	// Multipart message with both plain text and HTML parts: should prefer text/plain
	multipartMIME := "Content-Type: multipart/alternative; boundary=\"part-boundary\"\n\n" +
		"--part-boundary\n" +
		"Content-Type: text/plain; charset=utf-8\n" +
		"Content-Transfer-Encoding: quoted-printable\n\n" +
		"Database replica=20out of sync\n" +
		"--part-boundary\n" +
		"Content-Type: text/html; charset=utf-8\n\n" +
		"<p>Different HTML content</p>\n" +
		"--part-boundary--"
	cleanPlain := cleanEmailSnippet(multipartMIME)
	if cleanPlain != "Database replica out of sync" {
		t.Fatalf("expected plain text part to be selected and decoded, got %q", cleanPlain)
	}

	// Multipart message with image attachment only: should produce empty preview
	imageOnlyMIME := "Content-Type: multipart/mixed; boundary=\"img-boundary\"\n\n" +
		"--img-boundary\n" +
		"Content-Type: image/png\n" +
		"Content-Transfer-Encoding: base64\n\n" +
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==\n" +
		"--img-boundary--"
	cleanImage := cleanEmailSnippet(imageOnlyMIME)
	if cleanImage != "" {
		t.Fatalf("expected empty snippet for non-text image attachment, got %q", cleanImage)
	}

	// Single text/plain message containing dashes/horizontal lines must be preserved intact
	dashMIME := "Hello,\n--\nHere is a diff:\n--- a/file\n+++ b/file\nEverything intact."
	cleanDash := cleanEmailSnippet(dashMIME)
	if !strings.Contains(cleanDash, "Everything intact") || !strings.Contains(cleanDash, "--- a/file") {
		t.Fatalf("expected single-part body with dashes to be preserved, got %q", cleanDash)
	}

	web := apiURLToWebURL("https://api.github.com/repos/foo/bar/pulls/12", "https://github.com/foo/bar")
	if web != "https://github.com/foo/bar/pull/12" {
		t.Fatalf("unexpected mapped GitHub URL: %q", web)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer srv.Close()
	if _, err := ParseFeedXML(strings.NewReader("<rss version=\"2.0\"><channel></channel></rss>"), "Empty", "", 100); err != nil {
		t.Fatalf("empty RSS channel should not error: %v", err)
	}

	noGuidRSS := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>NoGUID</title>
  <item>
    <title>Item Without Link</title>
    <description>Unique content body</description>
  </item>
</channel></rss>`
	noGuidItems, err := ParseFeedXML(strings.NewReader(noGuidRSS), "NoGUID", "dev", 200)
	if err != nil {
		t.Fatalf("ParseFeedXML failed: %v", err)
	}
	if len(noGuidItems) != 1 {
		t.Fatalf("expected 1 item, got %d", len(noGuidItems))
	}
	if strings.HasPrefix(noGuidItems[0].ExternalID, "rss:NoGUID:") {
		t.Fatalf("expected sha256 digest fallback for missing GUID/link, got %q", noGuidItems[0].ExternalID)
	}

	batch, nextUID := selectIMAPBatchUIDs([]imap.UID{12, 5, 8, 2, 10}, 3, 2)
	if len(batch) != 2 || batch[0] != 5 || batch[1] != 8 || nextUID != 8 {
		t.Fatalf("expected oldest unseen batch [5, 8] with watermark 8, got %v nextUID=%d", batch, nextUID)
	}
	_ = context.Background()
}
