package ingest

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

const (
	maxFeedBytes    = 1 << 20
	maxFeedItems    = 512
	maxFeedElements = 16 << 10
	maxFeedDepth    = 64
)

var carrierGradeNAT = net.IPNet{
	IP:   net.ParseIP("100.64.0.0"),
	Mask: net.CIDRMask(10, 32),
}

// ValidateFeedURL checks that a URL uses http/https and does not target
// protected internal or loopback destinations (preventing SSRF).
func ValidateFeedURL(rawURL string) error {
	return ValidateFeedURLContext(context.Background(), rawURL)
}

// ValidateFeedURLContext performs context-aware DNS and structural destination verification.
func ValidateFeedURLContext(ctx context.Context, rawURL string) error {
	u, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return fmt.Errorf("invalid feed url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("feed url must use http or https")
	}
	if u.User != nil {
		return errors.New("feed url cannot contain credentials")
	}
	host := u.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") {
		return errors.New("feed url host cannot be localhost or empty")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("feed url points to protected destination: %s", ip.String())
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve feed host: %w", err)
	}
	if len(ips) == 0 {
		return errors.New("feed host has no addresses")
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("feed url points to protected destination: %s", ip.String())
		}
	}
	return nil
}

func isBlockedIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return true
	}
	if carrierGradeNAT.Contains(ip) {
		return true
	}
	return false
}

// FeedCache stores HTTP conditional GET headers between poll cycles.
type FeedCache struct {
	ETag         string
	LastModified string
}

// FetchFeed downloads and parses an RSS 2.0 or Atom feed using conditional GET.
// Trusted operator feeds use the supplied HTTP client directly without user-feed SSRF restrictions.
func FetchFeed(ctx context.Context, client *http.Client, sourceName, feedURL, tag string, cache FeedCache, maxSnippet int) (items []IncomingItem, nextCache FeedCache, changed bool, err error) {
	return fetchFeedInternal(ctx, client, sourceName, feedURL, tag, cache, maxSnippet, false)
}

// FetchFeedRestricted downloads and parses an untrusted user-supplied RSS/Atom feed,
// strictly enforcing SSRF destination validation and restricted direct transport.
func FetchFeedRestricted(ctx context.Context, client *http.Client, sourceName, feedURL, tag string, cache FeedCache, maxSnippet int) (items []IncomingItem, nextCache FeedCache, changed bool, err error) {
	return fetchFeedInternal(ctx, client, sourceName, feedURL, tag, cache, maxSnippet, true)
}

func fetchFeedInternal(ctx context.Context, client *http.Client, sourceName, feedURL, tag string, cache FeedCache, maxSnippet int, restricted bool) (items []IncomingItem, nextCache FeedCache, changed bool, err error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	if restricted {
		if err := ValidateFeedURLContext(fetchCtx, feedURL); err != nil {
			return nil, cache, false, fmt.Errorf("unsafe feed url %q: %w", sourceName, err)
		}
	}

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, feedURL, nil)
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

	httpClient := client
	var transportToClose *http.Transport
	if restricted || httpClient == nil {
		dialer := &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}

		transport := &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, fmt.Errorf("split host port: %w", err)
				}
				if strings.EqualFold(host, "localhost") {
					return nil, errors.New("connection to localhost blocked")
				}
				var ips []net.IP
				if ip := net.ParseIP(host); ip != nil {
					ips = []net.IP{ip}
				} else {
					resolved, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
					if err != nil || len(resolved) == 0 {
						return nil, fmt.Errorf("resolve host %q: %w", host, err)
					}
					ips = resolved
				}
				var safeIPs []net.IP
				for _, ip := range ips {
					if !isBlockedIP(ip) {
						safeIPs = append(safeIPs, ip)
					}
				}
				if len(safeIPs) == 0 {
					return nil, fmt.Errorf("all resolved IPs for %q are protected/blocked", host)
				}
				var lastErr error
				for _, safeIP := range safeIPs {
					targetAddr := net.JoinHostPort(safeIP.String(), port)
					conn, dialErr := dialer.DialContext(ctx, network, targetAddr)
					if dialErr == nil {
						return conn, nil
					}
					lastErr = dialErr
				}
				return nil, fmt.Errorf("dial all safe IPs for %q failed: %w", host, lastErr)
			},
			ResponseHeaderTimeout:  15 * time.Second,
			MaxResponseHeaderBytes: 16 << 10,
		}
		transportToClose = transport

		httpClient = &http.Client{
			Transport: transport,
			Timeout:   20 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return errors.New("redirect blocked: must be http or https")
				}
				if err := ValidateFeedURLContext(req.Context(), req.URL.String()); err != nil {
					return fmt.Errorf("redirect blocked to unsafe destination: %w", err)
				}
				return nil
			},
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		if transportToClose != nil {
			transportToClose.CloseIdleConnections()
		}
		return nil, cache, false, fmt.Errorf("fetch feed %q: %w", sourceName, err)
	}
	if transportToClose != nil {
		defer transportToClose.CloseIdleConnections()
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
	items, err = ParseFeedXML(resp.Body, sourceName, tag, maxSnippet)
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
	case "windows-1252":
		return charmap.Windows1252.NewDecoder().Reader(input), nil
	case "iso-8859-1", "latin1":
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

func feedDecoder(data []byte) *xml.Decoder {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.CharsetReader = charsetReader
	return dec
}

// Check the document before encoding/xml allocates its item and link slices.
// A rejected feed must leave its conditional GET validators untouched.
func checkFeedShape(data []byte) (string, error) {
	dec := feedDecoder(bytes.TrimPrefix(data, []byte("\ufeff")))
	var root, parent string
	var depth, elements, items int
	for {
		token, err := dec.Token()
		if err == io.EOF {
			return root, nil
		}
		if err != nil {
			return "", err
		}
		switch token := token.(type) {
		case xml.StartElement:
			depth++
			elements++
			if depth > maxFeedDepth || elements > maxFeedElements {
				return "", errors.New("feed XML exceeds structural limits")
			}
			name := token.Name.Local
			switch depth {
			case 1:
				if root != "" || (name != "rss" && name != "feed") {
					return "", errors.New("unrecognized XML feed format")
				}
				root = name
			case 2:
				parent = name
				if root == "feed" && name == "entry" {
					items++
				}
			case 3:
				if root == "rss" && parent == "channel" && name == "item" {
					items++
				}
			}
			if items > maxFeedItems {
				return "", fmt.Errorf("feed exceeds %d entries", maxFeedItems)
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && strings.Trim(string(token), " \t\r\n") != "" {
				return "", errors.New("text outside feed root")
			}
		}
	}
}

// ParseFeedXML decodes either an RSS 2.0 or Atom XML stream into IncomingItems.
func ParseFeedXML(r io.Reader, sourceName, tag string, maxSnippet int) ([]IncomingItem, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxFeedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFeedBytes {
		return nil, fmt.Errorf("feed exceeds %d bytes", maxFeedBytes)
	}
	root, err := checkFeedShape(data)
	if err != nil {
		return nil, err
	}
	dec := feedDecoder(data)
	switch root {
	case "rss":
		var rss rssDocument
		if err := dec.Decode(&rss); err != nil {
			return nil, err
		}
		return convertRSSItems(rss.Channel.Items, sourceName, tag, maxSnippet), nil
	case "feed":
		var atom atomDocument
		if err := dec.Decode(&atom); err != nil {
			return nil, err
		}
		return convertAtomEntries(atom.Entries, sourceName, tag, maxSnippet), nil
	}
	return nil, errors.New("unrecognized XML feed format")
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
