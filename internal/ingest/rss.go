package ingest

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

type atomDocument struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID        string     `xml:"id"`
	Title     xmlInner   `xml:"title"`
	Links     []atomLink `xml:"link"`
	Summary   xmlInner   `xml:"summary"`
	Content   xmlInner   `xml:"content"`
	Updated   string     `xml:"updated"`
	Published string     `xml:"published"`
	Author    struct {
		Name string `xml:"name"`
	} `xml:"author"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

// ParseFeedXML decodes either an RSS 2.0 or Atom XML stream into IncomingItems.
func ParseFeedXML(r io.Reader, sourceName, tag string, maxSnippet int) ([]IncomingItem, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxFeedBytes))
	if err != nil {
		return nil, err
	}

	var rss rssDocument
	if err := xml.Unmarshal(data, &rss); err == nil && len(rss.Channel.Items) > 0 {
		return convertRSSItems(rss.Channel.Items, sourceName, tag, maxSnippet), nil
	}

	var atom atomDocument
	if err := xml.Unmarshal(data, &atom); err == nil && (atom.XMLName.Local == "feed" || len(atom.Entries) > 0) {
		return convertAtomEntries(atom.Entries, sourceName, tag, maxSnippet), nil
	}

	// Empty valid RSS channel with 0 items should succeed rather than error.
	if rss.XMLName.Local == "rss" {
		return nil, nil
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
		extID := strings.TrimSpace(entry.GUID)
		if extID == "" {
			extID = strings.TrimSpace(entry.Link)
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
		body := entry.Summary.Raw
		if strings.TrimSpace(body) == "" {
			body = entry.Content.Raw
		}
		link := pickAtomLink(entry.Links)
		extID := strings.TrimSpace(entry.ID)
		if extID == "" {
			extID = link
		}
		ts := entry.Published
		if ts == "" {
			ts = entry.Updated
		}
		item := IncomingItem{
			ExternalID:  extID,
			SourceType:  "rss",
			SourceName:  sourceName,
			Title:       CleanFeedXMLText(entry.Title.Raw),
			Body:        CleanFeedXMLText(body),
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
