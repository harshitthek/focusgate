package ingest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"html"
	"strings"
	"time"
	"unicode/utf8"
)

// IncomingItem is the normalized representation shared across RSS, GitHub,
// IMAP email, and HTTP webhook sources.
type IncomingItem struct {
	UserID           int64             `json:"-"`
	UserFeedID       int64             `json:"-"`
	LegacyExternalID string            `json:"-"`
	LegacyIdentity   *LegacyIdentity   `json:"-"`
	ExternalID       string            `json:"external_id"`
	SourceType       string            `json:"source_type"` // rss | github | imap | webhook | slack
	SourceName       string            `json:"source_name"`
	Title            string            `json:"title"`
	Body             string            `json:"body"`
	Sender           string            `json:"sender"`
	URL              string            `json:"url"`
	Tag              string            `json:"tag"`
	Reason           string            `json:"reason"` // e.g., GitHub notification reason
	Metadata         map[string]string `json:"metadata,omitempty"`
	PublishedAt      time.Time         `json:"published_at"`
}

// LegacyIdentity retains original comparison fields until a legacy ID is reconciled.
type LegacyIdentity struct {
	SourceName string
	Title      string
	Sender     string
	URL        string
}

// Normalize trims fields, caps body length to keep RAM/disk bounded, and
// guarantees a deterministic ExternalID for deduplication.
func (item *IncomingItem) Normalize(maxBodyChars int) {
	maxBodyChars = min(max(0, maxBodyChars), 4096)
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
		fields := []string{item.SourceType, item.SourceName, item.URL, item.Title, item.Sender, hex.EncodeToString(bodyDigest[:8])}
		// Preserve historical IDs where separators are unambiguous. Namespace the
		// length-prefixed format so embedded separators cannot collide with either format.
		ambiguous := false
		for _, field := range fields {
			ambiguous = ambiguous || strings.Contains(field, "|")
		}
		if ambiguous {
			legacy := sha256.Sum256([]byte(strings.Join(fields, "|")))
			item.LegacyExternalID = hex.EncodeToString(legacy[:16])
			item.LegacyIdentity = &LegacyIdentity{SourceName: item.SourceName, Title: item.Title, Sender: item.Sender, URL: item.URL}
			hash := sha256.New()
			for _, field := range fields {
				var size [8]byte
				binary.BigEndian.PutUint64(size[:], uint64(len(field)))
				hash.Write(size[:])
				hash.Write([]byte(field))
			}
			item.ExternalID = "v2:" + hex.EncodeToString(hash.Sum(nil)[:16])
		} else {
			sum := sha256.Sum256([]byte(strings.Join(fields, "|")))
			item.ExternalID = hex.EncodeToString(sum[:16])
		}
	}
	// Bound metadata independently of the preview body, including provider payloads.
	if len(item.ExternalID) > 4096 {
		item.LegacyExternalID = item.ExternalID
		digest := sha256.Sum256([]byte(item.ExternalID))
		item.ExternalID = "sha256:" + hex.EncodeToString(digest[:])
	}
	item.SourceType = TruncateRunes(item.SourceType, 32)
	item.SourceName = TruncateRunes(item.SourceName, 128)
	item.Title = TruncateRunes(item.Title, 512)
	item.Sender = TruncateRunes(item.Sender, 320)
	if utf8.RuneCountInString(item.URL) > 2048 {
		item.URL = ""
	}
	item.Tag = TruncateRunes(item.Tag, 128)
	item.Reason = TruncateRunes(item.Reason, 256)
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
	count := 0
	last := 0
	for index := range s {
		if count == maxChars {
			if maxChars <= 1 {
				return s[:index]
			}
			return s[:last] + "…"
		}
		last = index
		count++
	}
	return s
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
