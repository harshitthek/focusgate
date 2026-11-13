package ingest

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const maxFeedBytes = 1 << 20 // 1 MiB cap protects RAM on oversized feeds

// FeedCache stores HTTP conditional GET headers between poll cycles.
type FeedCache struct {
	ETag         string
	LastModified string
}

// FetchFeed downloads and parses an RSS 2.0 or Atom feed using conditional GET.
// When the feed returns 304 Not Modified, changed is false and items is nil.
func FetchFeed(ctx context.Context, client *http.Client, sourceName, feedURL, tag string, cache FeedCache, maxSnippet int) (items []IncomingItem, nextCache FeedCache, changed bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, cache, false, fmt.Errorf("build feed request: %w", err)
	}
	req.Header.Set("User-Agent", "FocusGate/1.0 (+low-footprint)")
	if cache.ETag != "" {
		req.Header.Set("If-None-Match", cache.ETag)
	}
	if cache.LastModified != "" {
		req.Header.Set("If-Modified-Since", cache.LastModified)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, cache, false, fmt.Errorf("fetch feed %q: %w", sourceName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, cache, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, cache, false, fmt.Errorf("feed %q returned HTTP %d", sourceName, resp.StatusCode)
	}

	nextCache = FeedCache{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}
	limited := io.LimitReader(resp.Body, maxFeedBytes)
	items, err = ParseFeedXML(limited, sourceName, tag, maxSnippet)
	if err != nil {
		return nil, cache, false, fmt.Errorf("parse feed %q: %w", sourceName, err)
	}
	return items, nextCache, true, nil
}

type rssDocument struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type xmlInner struct {
	Raw string `xml:",innerxml"`
}

type rssItem struct {
	GUID        string   `xml:"guid"`
	Title       xmlInner `xml:"title"`
	Link        string   `xml:"link"`
	Description xmlInner `xml:"description"`
	Author      string   `xml:"author"`
	Creator     string   `xml:"creator"`
	PubDate     string   `xml:"pubDate"`
}

type atomTextConstruct struct {
	Type string `xml:"type,attr"`
	Raw  string `xml:",innerxml"`
}

type atomDocument struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID        string            `xml:"id"`
	Title     atomTextConstruct `xml:"title"`
	Links     []atomLink        `xml:"link"`
	Summary   atomTextConstruct `xml:"summary"`
	Content   atomTextConstruct `xml:"content"`
	Updated   string            `xml:"updated"`
	Published string            `xml:"published"`
	Author    struct {
		Name string `xml:"name"`
	} `xml:"author"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

func cleanAtomTextConstruct(tc atomTextConstruct) string {
	t := strings.ToLower(strings.TrimSpace(tc.Type))
	if t == "html" || t == "xhtml" {
		return CleanFeedXMLText(tc.Raw)
	}
	return CollapseWhitespace(html.UnescapeString(tc.Raw))
}

func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	cs := strings.ToLower(strings.TrimSpace(charset))
	switch cs {
	case "utf-8", "utf8", "":
		return input, nil
	case "us-ascii", "ascii":
		return input, nil
	case "iso-8859-1", "latin1", "windows-1252":
		return &iso88591ToUTF8Reader{r: input}, nil
	default:
		return nil, fmt.Errorf("unsupported xml charset: %s", charset)
	}
}

type iso88591ToUTF8Reader struct {
	r   io.Reader
	buf []byte
}

func (ir *iso88591ToUTF8Reader) Read(p []byte) (int, error) {
	if len(ir.buf) > 0 {
		n := copy(p, ir.buf)
		ir.buf = ir.buf[n:]
		return n, nil
	}
	src := make([]byte, len(p))
	n, err := ir.r.Read(src)
	if n == 0 {
		return 0, err
	}
	var out []byte
	for i := 0; i < n; i++ {
		b := src[i]
		if b < 0x80 {
			out = append(out, b)
		} else {
			var rBuf [4]byte
			rn := utf8.EncodeRune(rBuf[:], rune(b))
			out = append(out, rBuf[:rn]...)
		}
	}
	written := copy(p, out)
	if written < len(out) {
		ir.buf = out[written:]
	}
	return written, err
}

func unmarshalXML(data []byte, v any) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.CharsetReader = charsetReader
	return dec.Decode(v)
}

// ParseFeedXML decodes either an RSS 2.0 or Atom XML stream into IncomingItems.
func ParseFeedXML(r io.Reader, sourceName, tag string, maxSnippet int) ([]IncomingItem, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxFeedBytes))
	if err != nil {
		return nil, err
	}

	var rss rssDocument
	rssErr := unmarshalXML(data, &rss)
	if rssErr == nil && len(rss.Channel.Items) > 0 {
		return convertRSSItems(rss.Channel.Items, sourceName, tag, maxSnippet), nil
	}

	var atom atomDocument
	if err := unmarshalXML(data, &atom); err == nil && (atom.XMLName.Local == "feed" || len(atom.Entries) > 0) {
		return convertAtomEntries(atom.Entries, sourceName, tag, maxSnippet), nil
	}

	// Empty valid RSS channel with 0 items should succeed only when XML unmarshalling succeeded.
	if rssErr == nil && rss.XMLName.Local == "rss" {
		return nil, nil
	}
	if rssErr != nil && rss.XMLName.Local == "rss" {
		return nil, rssErr
	}
	return nil, fmt.Errorf("unrecognized XML feed format")
}

func convertRSSItems(raw []rssItem, sourceName, tag string, maxSnippet int) []IncomingItem {
	items := make([]IncomingItem, 0, len(raw))
	for _, entry := range raw {
		sender := entry.Creator
		if sender == "" {
			sender = entry.Author
		}
		rawID := strings.TrimSpace(entry.GUID)
		if rawID == "" {
			rawID = strings.TrimSpace(entry.Link)
		}
		var extID string
		if rawID != "" {
			extID = "rss:" + sourceName + ":" + rawID
		}
		item := IncomingItem{
			ExternalID:  extID,
			SourceType:  "rss",
			SourceName:  sourceName,
			Title:       CleanFeedXMLText(entry.Title.Raw),
			Body:        CleanFeedXMLText(entry.Description.Raw),
			Sender:      sender,
			URL:         strings.TrimSpace(entry.Link),
			Tag:         tag,
			PublishedAt: parseAnyTime(entry.PubDate),
		}
		item.Normalize(maxSnippet)
		items = append(items, item)
	}
	return items
}

func convertAtomEntries(raw []atomEntry, sourceName, tag string, maxSnippet int) []IncomingItem {
	items := make([]IncomingItem, 0, len(raw))
	for _, entry := range raw {
		rawBody := entry.Summary
		if strings.TrimSpace(rawBody.Raw) == "" {
			rawBody = entry.Content
		}
		link := pickAtomLink(entry.Links)
		rawID := strings.TrimSpace(entry.ID)
		if rawID == "" {
			rawID = strings.TrimSpace(link)
		}
		var extID string
		if rawID != "" {
			extID = "atom:" + sourceName + ":" + rawID
		}
		ts := entry.Published
		if ts == "" {
			ts = entry.Updated
		}
		item := IncomingItem{
			ExternalID:  extID,
			SourceType:  "rss",
			SourceName:  sourceName,
			Title:       cleanAtomTextConstruct(entry.Title),
			Body:        cleanAtomTextConstruct(rawBody),
			Sender:      entry.Author.Name,
			URL:         link,
			Tag:         tag,
			PublishedAt: parseAnyTime(ts),
		}
		item.Normalize(maxSnippet)
		items = append(items, item)
	}
	return items
}

func pickAtomLink(links []atomLink) string {
	for _, l := range links {
		if l.Rel == "" || l.Rel == "alternate" {
			return strings.TrimSpace(l.Href)
		}
	}
	if len(links) > 0 {
		return strings.TrimSpace(links[0].Href)
	}
	return ""
}

func parseAnyTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Now().UTC()
	}
	layouts := []string{
		time.RFC3339,
		time.RFC3339Nano,
		time.RFC1123Z,
		time.RFC1123,
		time.RFC822Z,
		time.RFC822,
		"2006-01-02 15:04:05 -0700",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}
