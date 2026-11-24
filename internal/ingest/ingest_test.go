package ingest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"focusgate/internal/config"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestLegacyTextEncodings(t *testing.T) {
	header := "Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable"
	if got := decodeEmailSnippet(header, "caf=E9"); got != "café" {
		t.Fatalf("lost declared MIME charset: %q", got)
	}
	multipart := "Content-Type: multipart/mixed; boundary=part"
	if got := decodeEmailSnippet(multipart, "--part\r\n\r\nplain child\r\n--part--"); got != "plain child" {
		t.Fatalf("lost headerless MIME child: %q", got)
	}
	reader, err := charsetReader("windows-1252", strings.NewReader("\x80"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded strings.Builder
	if _, err := io.Copy(&decoded, reader); err != nil || decoded.String() != "€" {
		t.Fatalf("incorrect Windows-1252 decoding: %q (%v)", decoded.String(), err)
	}
}

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

func TestFeedDocumentLimits(t *testing.T) {
	for _, tc := range []struct {
		name, open, entry, close string
	}{
		{"rss", "<rss><channel>", "<item/>", "</channel></rss>"},
		{"atom", "<feed>", "<entry/>", "</feed>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.open + strings.Repeat(tc.entry, maxFeedItems) + tc.close
			items, err := ParseFeedXML(strings.NewReader(body), "test", "", 64)
			if err != nil || len(items) != maxFeedItems {
				t.Fatalf("at entry limit: got %d items, %v", len(items), err)
			}

			body = tc.open + strings.Repeat(tc.entry, maxFeedItems+1) + tc.close
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("ETag", "new")
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			before := FeedCache{ETag: "previous"}
			items, after, changed, err := FetchFeed(context.Background(), srv.Client(), "test", srv.URL, "", before, 64)
			if err == nil || changed || after != before || len(items) != 0 {
				t.Fatalf("over limit: got %d items, cache %+v, changed %t, err %v", len(items), after, changed, err)
			}
		})
	}

	for name, body := range map[string]string{
		"nested links": "<feed><entry>" + strings.Repeat("<link/>", maxFeedElements) + "</entry></feed>",
		"deep XML":     "<rss>" + strings.Repeat("<a>", maxFeedDepth) + strings.Repeat("</a>", maxFeedDepth) + "</rss>",
		"bytes":        "<rss/>" + strings.Repeat(" ", maxFeedBytes),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseFeedXML(strings.NewReader(body), "test", "", 64); err == nil {
				t.Fatal("accepted feed over limit")
			}
		})
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer srv.Close()
	if _, err := ParseFeedXML(strings.NewReader("<rss version=\"2.0\"><channel></channel></rss>"), "Empty", "", 100); err != nil {
		t.Fatalf("empty RSS channel should not error: %v", err)
	}

	noGUIDRSS := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>NoGUID</title>
  <item>
    <title>Item Without Link</title>
    <description>Unique content body</description>
  </item>
  <item>
    <title>Second Item Without Link</title>
    <description>Different content body</description>
  </item>
</channel></rss>`
	noGUIDItems, err := ParseFeedXML(strings.NewReader(noGUIDRSS), "NoGUID", "dev", 200)
	if err != nil {
		t.Fatalf("ParseFeedXML failed: %v", err)
	}
	if len(noGUIDItems) != 2 {
		t.Fatalf("expected 2 items, got %d", len(noGUIDItems))
	}
	for i, it := range noGUIDItems {
		if len(it.ExternalID) != 32 {
			t.Fatalf("expected 32-char hex digest fallback for item %d, got %q", i, it.ExternalID)
		}
		if _, err := hex.DecodeString(it.ExternalID); err != nil {
			t.Fatalf("expected valid hex string for item %d ExternalID, got %q (err: %v)", i, it.ExternalID, err)
		}
	}
	if noGUIDItems[0].ExternalID == noGUIDItems[1].ExternalID {
		t.Fatalf("expected distinct items to receive unique fallback digests, got %q", noGUIDItems[0].ExternalID)
	}

	batch, err := selectIMAPBatchUIDs(imap.UIDSetNum(12, 5, 8, 2, 10), 3, 12, 2)
	if err != nil || len(batch) != 2 || batch[0] != 5 || batch[1] != 8 {
		t.Fatalf("expected oldest unseen batch [5, 8], got %v, %v", batch, err)
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

func TestIMAPBatchRangesAndSessionBudget(t *testing.T) {
	if end := imapWindowEnd(42, 0, 1000); end != 0 {
		t.Fatalf("missing UIDNEXT would advance the empty-search cursor to %d", end)
	}
	if end := imapWindowEnd(math.MaxUint32-3, math.MaxUint32, 1000); end != math.MaxUint32 {
		t.Fatalf("final UID window wrapped to %d", end)
	}
	var set imap.UIDSet
	set.AddRange(1, math.MaxUint32)
	batch, err := selectIMAPBatchUIDs(set, 0, 0, maxIMAPMessagesPerPoll)
	if err != nil || len(batch) != maxIMAPMessagesPerPoll || batch[0] != 1 || batch[len(batch)-1] != maxIMAPMessagesPerPoll {
		t.Fatalf("full UID range: %v, %v", batch, err)
	}
	batch, err = selectIMAPBatchUIDs(set, math.MaxUint32-1, 0, maxIMAPMessagesPerPoll)
	if err != nil || len(batch) != 1 || batch[0] != math.MaxUint32 {
		t.Fatalf("final UID: %v, %v", batch, err)
	}
	batch, err = selectIMAPBatchUIDs(set, 10, 11, maxIMAPMessagesPerPoll)
	if err != nil || len(batch) != 1 || batch[0] != 11 {
		t.Fatalf("search window: %v, %v", batch, err)
	}
	if _, err := selectIMAPBatchUIDs(imap.UIDSetNum(0), 0, 0, 20); err == nil {
		t.Fatal("unexpectedly accepted dynamic server result")
	}

	reader, writer := net.Pipe()
	defer reader.Close()
	defer writer.Close()
	go func() { defer writer.Close(); _, _ = io.WriteString(writer, "123456789") }()
	conn := &imapBudgetConn{Conn: reader, remaining: 8}
	received, err := io.ReadAll(conn)
	if err == nil || string(received) != "12345678" {
		t.Fatalf("connection read budget: %q, %v", received, err)
	}
}

func TestIMAPLiteralBoundsAndPortIdentity(t *testing.T) {
	small := strings.NewReader("preview")
	if got, err := readIMAPLiteral(small); err != nil || string(got) != "preview" {
		t.Fatalf("small literal: %q %v", got, err)
	}
	large := strings.NewReader(strings.Repeat("x", 2<<20))
	if _, err := readIMAPLiteral(large); err == nil {
		t.Fatal("oversized literal accepted")
	}
	if consumed := (2 << 20) - large.Len(); consumed != (1<<20)+1 {
		t.Fatalf("read past cap: %d", consumed)
	}
	medium := strings.NewReader(strings.Repeat("x", 8000))
	if got, err := readIMAPLiteral(medium); err != nil || len(got) != 2048 {
		t.Fatalf("preview not bounded: %d %v", len(got), err)
	}
	cfg := config.IMAPConfig{Host: "imap.example", Port: 993, Username: "alice", Mailbox: "INBOX"}
	legacy := "imap.example|alice|INBOX"
	if got := imapMailboxIdentity(cfg); got != legacy {
		t.Fatalf("default-port IDs lost compatibility: %q", got)
	}
	cfg.Port = 1993
	if got := imapMailboxIdentity(cfg); got == legacy {
		t.Fatal("different IMAP ports share IDs")
	}
}

type githubTransport func(*http.Request) (*http.Response, error)

func (f githubTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubPaginationDoesNotAdvanceWatermarkAfterPageShift(t *testing.T) {
	const old = "Wed, 18 Nov 2026 10:00:00 GMT"
	const newer = "Thu, 19 Nov 2026 10:00:00 GMT"
	client := &http.Client{Transport: githubTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("page") != "4" || r.Header.Get("If-Modified-Since") != "" {
			t.Errorf("did not resume pagination: %s, headers=%v", r.URL, r.Header)
		}
		header := make(http.Header)
		header.Set("Last-Modified", newer)
		// A removed notification can make an offset resume return an empty final page.
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader("[]"))}, nil
	})}
	_, watermark, cursor, err := FetchGitHubNotifications(context.Background(), client, "dummy-token", false, old, "https://api.github.com/notifications?page=4||"+newer, 1200)
	if err != nil || watermark != old || cursor != "" {
		t.Fatalf("resumed empty page advanced watermark: %q %q %v", watermark, cursor, err)
	}
}

func TestFallbackIdentitySeparatesEmbeddedDelimiters(t *testing.T) {
	first := IncomingItem{SourceType: "rss", SourceName: "a|b", URL: "c", Title: "title", Body: "body"}
	second := IncomingItem{SourceType: "rss", SourceName: "a", URL: "b|c", Title: "title", Body: "body"}
	replay := first
	first.Normalize(100)
	second.Normalize(100)
	replay.Normalize(100)
	if first.ExternalID == second.ExternalID {
		t.Fatal("distinct fields collided through a delimiter")
	}
	if first.ExternalID != replay.ExternalID {
		t.Fatal("identity is not deterministic")
	}
}

func TestIMAPFallbackContextHasIndependentDeadline(t *testing.T) {
	ctx, cancel := withIMAPDeadline(context.Background(), time.Second)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("fallback had no context timer")
	}
	original, stop := context.WithDeadline(context.Background(), time.Now().Add(2*time.Second))
	defer stop()
	preserved, done := withIMAPDeadline(original, time.Second)
	defer done()
	want, _ := original.Deadline()
	got, _ := preserved.Deadline()
	if !got.Equal(want) {
		t.Fatal("provided caller deadline was replaced")
	}
}

func TestFeedRejectsOutsideRootTextAndPreservesWebhooks(t *testing.T) {
	for _, text := range []string{"<rss><channel/></rss>truncated", "prefix<rss><channel/></rss>"} {
		if _, err := ParseFeedXML(strings.NewReader(text), "test", "", 1200); err == nil {
			t.Fatalf("accepted malformed feed: %s", text)
		}
	}
	for _, text := range []string{" \n<rss><channel/></rss>\n ", "\ufeff<rss><channel/></rss>"} {
		if _, err := ParseFeedXML(strings.NewReader(text), "test", "", 1200); err != nil {
			t.Fatalf("valid surrounding whitespace rejected: %v", err)
		}
	}
	plain, err := ParseWebhook("plain", "", "", []byte("Expected literal <b> here"), 1200)
	if err != nil || plain.Body != "Expected literal <b> here" {
		t.Fatalf("plain text changed: %q %v", plain.Body, err)
	}
	github, err := ParseWebhook("github", "issue_comment", "delivery", []byte("{\"repository\":{\"full_name\":\"owner/repo\"},\"comment\":{\"body\":\"Literal `<b>`\"},\"issue\":{\"title\":\"test\"}}"), 1200)
	if err != nil || github.Body != "Literal `<b>`" {
		t.Fatalf("Markdown changed: %q %v", github.Body, err)
	}
	item := IncomingItem{SourceType: "rss", URL: "https://example.com/" + strings.Repeat("x", 2048)}
	item.Normalize(1200)
	if item.URL != "" {
		t.Fatalf("oversized URL remained clickable: %q", item.URL)
	}
	item = IncomingItem{SourceType: "rss", URL: "https://example.com/article"}
	item.Normalize(1200)
	if item.URL != "https://example.com/article" {
		t.Fatal("ordinary URL changed")
	}
}

func TestOversizedIMAPLiteralReleasesDecoderOnAbort(t *testing.T) {
	conn, server := net.Pipe()
	defer server.Close()
	go func() {
		defer server.Close()
		_, _ = io.WriteString(server, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		if _, err := bufio.NewReader(server).ReadString('\n'); err != nil {
			return
		}
		_, _ = io.WriteString(server, "* 1 FETCH (UID 1 BODY[TEXT] {2097152}\r\n")
		_, _ = io.WriteString(server, strings.Repeat("x", 2<<20))
	}()
	client := imapclient.New(conn, nil)
	defer client.Close()
	if err := client.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		fetch := client.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{{Specifier: imap.PartSpecifierText, Peek: true}}})
		message := fetch.Next()
		if message == nil {
			done <- fmt.Errorf("missing fetch message")
			return
		}
		for part := message.Next(); part != nil; part = message.Next() {
			if part, ok := part.(imapclient.FetchItemDataBodySection); ok {
				_, err := readIMAPLiteral(part.Literal)
				if err == nil {
					done <- fmt.Errorf("oversized body accepted")
					return
				}
				abortIMAPFetch(conn, fetch)
				done <- nil
				return
			}
		}
		done <- fmt.Errorf("missing body literal")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("IMAP decoder retained pending literal after abort")
	}
}
