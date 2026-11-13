package ingest

import (
	"bytes"
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

	// Nested multipart: multipart/mixed containing multipart/alternative
	nestedMIME := "Content-Type: multipart/mixed; boundary=\"outer-bound\"\n\n" +
		"--outer-bound\n" +
		"Content-Type: multipart/alternative; boundary=\"inner-bound\"\n\n" +
		"--inner-bound\n" +
		"Content-Type: text/plain; charset=utf-8\n\n" +
		"Nested plain text body\n" +
		"--inner-bound\n" +
		"Content-Type: text/html; charset=utf-8\n\n" +
		"<p>Nested HTML body</p>\n" +
		"--inner-bound--\n" +
		"--outer-bound\n" +
		"Content-Type: application/pdf; name=\"report.pdf\"\n" +
		"Content-Disposition: attachment; filename=\"report.pdf\"\n\n" +
		"RAWPDFDATA\n" +
		"--outer-bound--"
	cleanNested := cleanEmailSnippet(nestedMIME)
	if cleanNested != "Nested plain text body" {
		t.Fatalf("expected nested plain text to be selected, got %q", cleanNested)
	}

	// Message with text/plain attachment and inline text/html: inline HTML must be preferred over attachment
	attachMIME := "Content-Type: multipart/mixed; boundary=\"mix-bound\"\n\n" +
		"--mix-bound\n" +
		"Content-Type: text/html; charset=utf-8\n" +
		"Content-Disposition: inline\n\n" +
		"<p>Main body announcement</p>\n" +
		"--mix-bound\n" +
		"Content-Type: text/plain; charset=utf-8\n" +
		"Content-Disposition: attachment; filename=\"log.txt\"\n\n" +
		"Log file attachment contents\n" +
		"--mix-bound--"
	cleanAttach := cleanEmailSnippet(attachMIME)
	if cleanAttach != "Main body announcement" {
		t.Fatalf("expected inline HTML to be preferred over text attachment, got %q", cleanAttach)
	}

	// Folded MIME headers across multiple lines
	foldedMIME := "Content-Type: multipart/alternative;\r\n boundary=\"folded-boundary\"\r\n\r\n" +
		"--folded-boundary\r\n" +
		"Content-Type: text/plain;\r\n charset=\"utf-8\"\r\n\r\n" +
		"Folded header preview body\r\n" +
		"--folded-boundary--"
	cleanFolded := cleanEmailSnippet(foldedMIME)
	if cleanFolded != "Folded header preview body" {
		t.Fatalf("expected folded header to be parsed and decoded, got %q", cleanFolded)
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

	// Atom feed with plain-text type preserving tags vs HTML type stripping tags
	atomXML := `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>tag:1</id>
    <title type="text">Generic &lt;T&gt; in Go</title>
    <summary type="html">&lt;p&gt;Paragraph &lt;b&gt;bold&lt;/b&gt;&lt;/p&gt;</summary>
  </entry>
</feed>`
	atomItems, err := ParseFeedXML(strings.NewReader(atomXML), "AtomTest", "tag", 200)
	if err != nil || len(atomItems) != 1 {
		t.Fatalf("failed to parse Atom feed: %v", err)
	}
	if atomItems[0].Title != "Generic <T> in Go" {
		t.Fatalf("expected plain text title with unescaped tags preserved, got %q", atomItems[0].Title)
	}
	if atomItems[0].Body != "Paragraph bold" {
		t.Fatalf("expected html summary with stripped tags, got %q", atomItems[0].Body)
	}

	// Non-UTF-8 XML encoding: ISO-8859-1
	isoXML := []byte("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n<rss version=\"2.0\"><channel><item><title>Caf\xe9</title><description>Boutique</description></item></channel></rss>")
	isoItems, err := ParseFeedXML(bytes.NewReader(isoXML), "ISOTest", "tag", 200)
	if err != nil || len(isoItems) != 1 {
		t.Fatalf("failed to parse ISO-8859-1 feed: %v", err)
	}
	if isoItems[0].Title != "Café" {
		t.Fatalf("expected Café, got %q", isoItems[0].Title)
	}

	// Case-sensitive nested boundary
	caseSensMIME := "Content-Type: multipart/mixed; boundary=\"Outer_Bound\"\r\n\r\n" +
		"--Outer_Bound\r\n" +
		"Content-Type: multipart/alternative; boundary=\"Inner_CaseSens_123\"\r\n\r\n" +
		"--Inner_CaseSens_123\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Case-sensitive boundary text\r\n" +
		"--Inner_CaseSens_123--\r\n" +
		"--Outer_Bound--"
	cleanCase := cleanEmailSnippet(caseSensMIME)
	if cleanCase != "Case-sensitive boundary text" {
		t.Fatalf("expected case-sensitive nested boundary to match, got %q", cleanCase)
	}

	// Base64 with truncated multibyte UTF-8 sequence
	// "Hello 世界" in UTF-8 is: "Hello " (6 bytes) + "\xe4\xb8\x96" (3 bytes) + "\xe7\x95\x8c" (3 bytes)
	// Base64 of "Hello \xe4\xb8\x96\xe7" (ends with partial rune \xe7) is "SGVsbG8g5LiW5w=="
	b64MIME := "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nSGVsbG8g5LiW5w=="
	cleanB64 := cleanEmailSnippet(b64MIME)
	if cleanB64 != "Hello 世" {
		t.Fatalf("expected base64 to keep valid prefix and drop incomplete trailing rune, got %q", cleanB64)
	}

	_ = context.Background()
}
