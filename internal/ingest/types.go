package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"html"
	"strings"
	"time"
)

// IncomingItem is the normalized representation shared across RSS, GitHub,
// IMAP email, and HTTP webhook sources.
type IncomingItem struct {
	UserID      int64             `json:"-"`
	ExternalID  string            `json:"external_id"`
	SourceType  string            `json:"source_type"` // rss | github | imap | webhook
	SourceName  string            `json:"source_name"`
	Title       string            `json:"title"`
	Body        string            `json:"body"`
	Sender      string            `json:"sender"`
	URL         string            `json:"url"`
	Tag         string            `json:"tag"`
	Reason      string            `json:"reason"` // e.g., GitHub notification reason
	Metadata    map[string]string `json:"metadata,omitempty"`
	PublishedAt time.Time         `json:"published_at"`
}

// Normalize trims fields, caps body length to keep RAM/disk bounded, and
// guarantees a deterministic ExternalID for deduplication.
func (item *IncomingItem) Normalize(maxBodyChars int) {
	item.SourceType = strings.TrimSpace(strings.ToLower(item.SourceType))
	item.SourceName = strings.TrimSpace(item.SourceName)
	item.Title = CollapseWhitespace(item.Title)
	item.Sender = CollapseWhitespace(item.Sender)
	item.URL = strings.TrimSpace(item.URL)
	item.Tag = strings.TrimSpace(item.Tag)
	item.Reason = strings.TrimSpace(strings.ToLower(item.Reason))
	normalizedBody := CollapseWhitespace(item.Body)
	var bodyDigest [32]byte
	if strings.TrimSpace(item.ExternalID) == "" {
		bodyDigest = sha256.Sum256([]byte(normalizedBody))
	}
	item.Body = TruncateRunes(normalizedBody, maxBodyChars)

	if item.PublishedAt.IsZero() {
		item.PublishedAt = time.Now().UTC()
	}
	if strings.TrimSpace(item.ExternalID) == "" {
		sum := sha256.Sum256([]byte(item.SourceType + "|" + item.SourceName + "|" + item.URL + "|" + item.Title + "|" + item.Sender + "|" + hex.EncodeToString(bodyDigest[:8])))
		item.ExternalID = hex.EncodeToString(sum[:16])
	}
}

// CollapseWhitespace collapses consecutive whitespace runs into single spaces.
func CollapseWhitespace(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

// TruncateRunes safely truncates UTF-8 text on rune boundaries.
func TruncateRunes(s string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	if maxChars <= 1 {
		return string(runes[:maxChars])
	}
	return string(runes[:maxChars-1]) + "…"
}

// StripHTML removes HTML/XML tags and decodes HTML entities in a single fast
// pass without allocating a DOM tree. Stray '<' without a valid tag is preserved.
func StripHTML(input string) string {
	if !strings.ContainsAny(input, "<&") {
		return CollapseWhitespace(input)
	}
	var b strings.Builder
	b.Grow(len(input))
	for i := 0; i < len(input); i++ {
		if input[i] == '<' && i+1 < len(input) && isTagStart(input[i+1]) {
			maxScan := i + 1 + 512
			if maxScan > len(input) {
				maxScan = len(input)
			}
			if end := strings.IndexByte(input[i+1:maxScan], '>'); end >= 0 {
				b.WriteByte(' ')
				i += end + 1
				continue
			}
		}
		b.WriteByte(input[i])
	}
	return CollapseWhitespace(html.UnescapeString(b.String()))
}

// CleanFeedXMLText unwraps CDATA, strips XML/XHTML tags, unescapes RSS-escaped
// HTML tags, and returns clean readable plain text.
func CleanFeedXMLText(rawInnerXML string) string {
	if rawInnerXML == "" {
		return ""
	}
	s := strings.ReplaceAll(rawInnerXML, "<![CDATA[", "")
	s = strings.ReplaceAll(s, "]]>", "")
	// First pass strips direct XHTML tags and decodes RSS-escaped entities (&lt;p&gt;).
	pass1 := StripHTML(s)
	// Second pass strips HTML tags that were entity-escaped in the XML feed.
	if strings.ContainsRune(pass1, '<') {
		return StripHTML(pass1)
	}
	return pass1
}

func isTagStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '/' || c == '!' || c == '?'
}
