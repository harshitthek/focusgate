package ingest

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"golang.org/x/text/encoding/ianaindex"
	"io"
	"math"
	"mime"
	"mime/quotedprintable"
	"net"
	"slices"
	"strings"
	"time"

	"focusgate/internal/config"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	maxIMAPMessagesPerPoll = 20
	maxIMAPResponseBytes   = 2 << 20
)

// The IMAP decoder has a single reader. Bound metadata as well as message
// literals, including SEARCH results and recipient lists inside ENVELOPE.
type imapBudgetConn struct {
	net.Conn
	remaining int
}

func (c *imapBudgetConn) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, fmt.Errorf("IMAP responses exceeded %d bytes per connection", maxIMAPResponseBytes)
	}
	n, err := c.Conn.Read(p[:min(len(p), c.remaining)])
	c.remaining -= n
	return n, err
}

// TestIMAPConnection connects over TLS, performs an authentication handshake, and verifies the credentials.
func TestIMAPConnection(ctx context.Context, host string, port int, username, password string) error {
	ctx, cancel := withIMAPDeadline(ctx, 15*time.Second)
	defer cancel()
	if port <= 0 {
		port = 993
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(15 * time.Second)
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	rawConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("could not connect to %s: %w", addr, err)
	}
	defer rawConn.Close()
	_ = rawConn.SetDeadline(deadline)

	tlsConn := tls.Client(rawConn, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("TLS handshake failed: %w", err)
	}

	doneWatch := make(chan struct{})
	defer close(doneWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = tlsConn.SetDeadline(time.Now())
			_ = tlsConn.Close()
		case <-doneWatch:
		}
	}()

	client := imapclient.New(&imapBudgetConn{Conn: tlsConn, remaining: maxIMAPResponseBytes}, nil)
	defer client.Close()

	if err := client.Login(username, password).Wait(); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}
	_ = client.Logout().Wait()
	return nil
}

// FetchIMAPUnread connects over implicit TLS with strict context/deadline bounds,
// examines the mailbox read-only, tracks UIDVALIDITY, and fetches bounded decoded
// previews for unseen messages newer than lastUID.
func FetchIMAPUnread(ctx context.Context, cfg config.IMAPConfig, lastUID uint32, prevUIDValidity uint32, maxSnippet int) (items []IncomingItem, nextUID uint32, uidValidity uint32, err error) {
	ctx, cancel := withIMAPDeadline(ctx, 30*time.Second)
	defer cancel()
	if !cfg.Enabled {
		return nil, lastUID, prevUIDValidity, nil
	}
	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))

	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(30 * time.Second)
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second}
	rawConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, lastUID, prevUIDValidity, fmt.Errorf("imap dial %s: %w", addr, err)
	}
	_ = rawConn.SetDeadline(deadline)

	tlsConn := tls.Client(rawConn, &tls.Config{
		ServerName: cfg.Host,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = tlsConn.Close()
		return nil, lastUID, prevUIDValidity, fmt.Errorf("imap tls handshake %s: %w", addr, err)
	}

	// Force any pending IMAP I/O to abort immediately if ctx is canceled.
	doneWatch := make(chan struct{})
	defer close(doneWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = tlsConn.SetDeadline(time.Now())
			_ = tlsConn.Close()
		case <-doneWatch:
		}
	}()

	client := imapclient.New(&imapBudgetConn{Conn: tlsConn, remaining: maxIMAPResponseBytes}, nil)
	defer client.Close()

	if err := client.Login(cfg.Username, cfg.Password).Wait(); err != nil {
		return nil, lastUID, prevUIDValidity, fmt.Errorf("imap login %s: %w", cfg.Name, err)
	}
	defer func() { _ = client.Logout().Wait() }()

	selData, err := client.Select(cfg.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return nil, lastUID, prevUIDValidity, fmt.Errorf("imap select %s: %w", cfg.Mailbox, err)
	}
	uidValidity = selData.UIDValidity
	effectiveLastUID := lastUID
	if prevUIDValidity != 0 && uidValidity != 0 && uidValidity != prevUIDValidity {
		effectiveLastUID = 0
	}

	criteria := &imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
	}
	var maxPossibleUID uint32
	if selData.UIDNext > 1 {
		maxPossibleUID = uint32(selData.UIDNext - 1)
	}

	var windowEnd uint32
	searchAfter := effectiveLastUID
	const windowSize = maxIMAPMessagesPerPoll * 50
	if effectiveLastUID == 0 {
		// On initial sync or UIDVALIDITY rollover, inspect unread messages within the last 48h.
		// Bound server-side search to recent UIDs [windowStart, maxPossibleUID] when known.
		criteria.Since = time.Now().Add(-48 * time.Hour)
		if maxPossibleUID > 0 {
			var windowStart uint32 = 1
			if maxPossibleUID > windowSize {
				windowStart = maxPossibleUID - windowSize + 1
			}
			searchAfter = windowStart - 1
			windowEnd = maxPossibleUID
			var uidSet imap.UIDSet
			uidSet.AddRange(imap.UID(windowStart), imap.UID(windowEnd))
			criteria.UID = []imap.UIDSet{uidSet}
		}
	} else if effectiveLastUID < math.MaxUint32 {
		if maxPossibleUID > 0 && effectiveLastUID >= maxPossibleUID {
			// Watermark is already at or past the highest existing UID.
			return nil, effectiveLastUID, uidValidity, nil
		}
		windowEnd = imapWindowEnd(effectiveLastUID, maxPossibleUID, windowSize)
		var uidSet imap.UIDSet
		uidSet.AddRange(imap.UID(effectiveLastUID+1), imap.UID(windowEnd))
		criteria.UID = []imap.UIDSet{uidSet}
	} else {
		return nil, effectiveLastUID, uidValidity, nil
	}

	searchData, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, effectiveLastUID, uidValidity, fmt.Errorf("imap search %s: %w", cfg.Name, err)
	}

	targetUIDs, err := selectIMAPBatchUIDs(searchData.All, searchAfter, windowEnd, maxIMAPMessagesPerPoll)
	if err != nil {
		return nil, effectiveLastUID, uidValidity, err
	}
	if len(targetUIDs) == 0 {
		return nil, max(effectiveLastUID, windowEnd), uidValidity, nil
	}
	highest := uint32(targetUIDs[len(targetUIDs)-1])

	var uidSet imap.UIDSet
	for _, uid := range targetUIDs {
		uidSet.AddNum(uid)
	}

	headerSection := &imap.FetchItemBodySection{
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: []string{"Content-Type", "Content-Transfer-Encoding", "Content-Disposition"},
		Peek:         true,
		Partial:      &imap.SectionPartial{Offset: 0, Size: 2048},
	}
	bodySection := &imap.FetchItemBodySection{
		Specifier: imap.PartSpecifierText,
		Peek:      true,
		Partial:   &imap.SectionPartial{Offset: 0, Size: 2048},
	}
	fetchOpts := &imap.FetchOptions{
		Envelope:    true,
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{headerSection, bodySection},
	}
	fetch := client.Fetch(uidSet, fetchOpts)
	var msgs []*imapclient.FetchMessageBuffer
	for message := fetch.Next(); message != nil; message = fetch.Next() {
		if len(msgs) >= maxIMAPMessagesPerPoll {
			abortIMAPFetch(tlsConn, fetch)
			return nil, effectiveLastUID, uidValidity, fmt.Errorf("imap server exceeded message batch limit")
		}
		msg := &imapclient.FetchMessageBuffer{SeqNum: message.SeqNum}
		for part := message.Next(); part != nil; part = message.Next() {
			switch part := part.(type) {
			case imapclient.FetchItemDataUID:
				msg.UID = part.UID
			case imapclient.FetchItemDataEnvelope:
				msg.Envelope = part.Envelope
			case imapclient.FetchItemDataBodySection:
				if part.Literal == nil {
					continue
				}
				body, err := readIMAPLiteral(part.Literal)
				if err != nil {
					abortIMAPFetch(tlsConn, fetch)
					return nil, effectiveLastUID, uidValidity, fmt.Errorf("imap body literal: %w", err)
				}
				if len(msg.BodySection) >= 2 {
					abortIMAPFetch(tlsConn, fetch)
					return nil, effectiveLastUID, uidValidity, fmt.Errorf("imap server exceeded section limit")
				}
				msg.BodySection = append(msg.BodySection, imapclient.FetchBodySectionBuffer{Section: part.Section, Bytes: body})
			}
		}
		msgs = append(msgs, msg)
	}
	if err := fetch.Close(); err != nil {
		return nil, effectiveLastUID, uidValidity, fmt.Errorf("imap fetch %s: %w", cfg.Name, err)
	}

	slices.SortFunc(msgs, func(a, b *imapclient.FetchMessageBuffer) int {
		if a.UID < b.UID {
			return -1
		}
		if a.UID > b.UID {
			return 1
		}
		return 0
	})

	mailboxIdentity := imapMailboxIdentity(cfg)
	items = make([]IncomingItem, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Envelope == nil {
			continue
		}
		sender := formatIMAPSender(msg.Envelope.From)
		hdrBytes := msg.FindBodySection(headerSection)
		if len(hdrBytes) > 2048 {
			hdrBytes = hdrBytes[:2048]
		}
		rawHeaders := string(hdrBytes)
		rawSnippet := string(msg.FindBodySection(bodySection))
		preview := decodeEmailSnippet(rawHeaders, rawSnippet)
		extID := fmt.Sprintf("imap:%s:%d:%d", mailboxIdentity, uidValidity, uint32(msg.UID))
		published := msg.Envelope.Date
		if published.IsZero() {
			published = time.Now().UTC()
		}

		item := IncomingItem{
			ExternalID:  extID,
			SourceType:  "imap",
			SourceName:  cfg.Name,
			Title:       msg.Envelope.Subject,
			Body:        preview,
			Sender:      sender,
			Tag:         cfg.Mailbox,
			Metadata:    map[string]string{"uid": fmt.Sprintf("%d", uint32(msg.UID))},
			PublishedAt: published.UTC(),
		}
		if cfg.Port != 0 && cfg.Port != 993 && prevUIDValidity != 0 && prevUIDValidity == uidValidity {
			legacy := cfg.Host + "|" + cfg.Username + "|" + cfg.Mailbox
			item.LegacyExternalID = fmt.Sprintf("imap:%s:%d:%d", legacy, uidValidity, uint32(msg.UID))
		}
		item.Normalize(maxSnippet)
		items = append(items, item)
	}
	return items, highest, uidValidity, nil
}

// Without UIDNEXT there is no proven upper bound. Zero means "*" in the
// search request, and an empty search must leave the saved cursor alone.
func imapWindowEnd(after, highestKnown, size uint32) uint32 {
	if highestKnown == 0 {
		return 0
	}
	return uint32(min(uint64(after)+uint64(size), uint64(highestKnown)))
}

// UIDSet ranges are ordered by the decoder. Walk only enough of each range for
// this poll, even if the server reports an entire mailbox in one range.
func selectIMAPBatchUIDs(all imap.NumSet, after, through uint32, limit int) ([]imap.UID, error) {
	if all == nil || limit <= 0 {
		return nil, nil
	}
	set, ok := all.(imap.UIDSet)
	if !ok || set.Dynamic() {
		return nil, fmt.Errorf("IMAP search returned an invalid UID set")
	}
	if through == 0 {
		through = math.MaxUint32
	}
	var selected []imap.UID
	for _, span := range set {
		first := max(uint64(span.Start), uint64(after)+1)
		last := min(uint64(span.Stop), uint64(through))
		for uid := first; uid <= last; uid++ {
			selected = append(selected, imap.UID(uid))
			if len(selected) == limit {
				return selected, nil
			}
		}
	}
	return selected, nil
}

func formatIMAPSender(addrs []imap.Address) string {
	if len(addrs) == 0 {
		return "unknown"
	}
	a := addrs[0]
	email := a.Addr()
	if a.Name != "" && email != "" {
		return fmt.Sprintf("%s <%s>", a.Name, email)
	}
	if email != "" {
		return email
	}
	return a.Name
}

// cleanEmailSnippet strips common MIME boundary headers and HTML from partial
// BODY.PEEK[TEXT] responses after decoding supported transfer encodings.
func cleanEmailSnippet(raw string) string {
	if idx := strings.Index(raw, "\r\n\r\n"); idx != -1 {
		return decodeEmailSnippet(raw[:idx], raw[idx+4:])
	}
	if idx := strings.Index(raw, "\n\n"); idx != -1 {
		return decodeEmailSnippet(raw[:idx], raw[idx+2:])
	}
	return decodeEmailSnippet("", raw)
}

type rawMIMEPart struct {
	mediaType   string
	contentType string
	encoding    string
	disposition string
	bodyLines   []string
}

func parseMediaType(val string) (mediaType string, params string) {
	val = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(val, "content-type:"), "Content-Type:"))
	parts := strings.SplitN(val, ";", 2)
	mediaType = strings.ToLower(strings.TrimSpace(parts[0]))
	if len(parts) > 1 {
		params = parts[1]
	}
	return mediaType, params
}

func parseDispositionToken(val string) string {
	val = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(val, "content-disposition:"), "Content-Disposition:"))
	parts := strings.SplitN(val, ";", 2)
	return strings.ToLower(strings.TrimSpace(parts[0]))
}

func parseMIMEParts(headers, body string) []rawMIMEPart {
	boundary := extractMIMEBoundary(headers)
	if boundary == "" {
		ct := extractContentType(headers)
		mt, _ := parseMediaType(ct)
		disp := parseDispositionToken(extractDisposition(headers))
		return []rawMIMEPart{{
			mediaType:   mt,
			contentType: ct,
			encoding:    extractTransferEncoding(headers),
			disposition: disp,
			bodyLines:   strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n"),
		}}
	}

	defaultMediaType := "text/plain"
	parentType, _ := parseMediaType(extractContentType(headers))
	if parentType == "multipart/digest" {
		defaultMediaType = "message/rfc822"
	}
	openMarker := "--" + boundary
	closeMarker := "--" + boundary + "--"
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")

	var rawParts []rawMIMEPart
	var currentPart *rawMIMEPart
	inPartHeaders := false
	var lastHeader string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)

		if trimmed == openMarker || trimmed == closeMarker {
			if currentPart != nil {
				rawParts = append(rawParts, *currentPart)
				currentPart = nil
			}
			lastHeader = ""
			if trimmed == openMarker {
				currentPart = &rawMIMEPart{mediaType: defaultMediaType}
				inPartHeaders = true
			} else {
				inPartHeaders = false
			}
			continue
		}

		if inPartHeaders {
			if trimmed == "" {
				inPartHeaders = false
				lastHeader = ""
				continue
			}
			if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				if currentPart != nil {
					switch lastHeader {
					case "content-type":
						currentPart.contentType += " " + strings.TrimSpace(trimmed)
						currentPart.mediaType, _ = parseMediaType(currentPart.contentType)
					case "content-transfer-encoding":
						currentPart.encoding += " " + lower
					case "content-disposition":
						// Parameters might be folded; primary token already stored
					}
				}
				continue
			}
			if strings.HasPrefix(lower, "content-type:") {
				lastHeader = "content-type"
				if currentPart != nil {
					val := strings.TrimSpace(trimmed[len("content-type:"):])
					currentPart.contentType = val
					currentPart.mediaType, _ = parseMediaType(val)
				}
				continue
			}
			if strings.HasPrefix(lower, "content-transfer-encoding:") {
				lastHeader = "content-transfer-encoding"
				if currentPart != nil {
					currentPart.encoding = strings.TrimSpace(strings.TrimPrefix(lower, "content-transfer-encoding:"))
				}
				continue
			}
			if strings.HasPrefix(lower, "content-disposition:") {
				lastHeader = "content-disposition"
				if currentPart != nil {
					val := strings.TrimSpace(trimmed[len("content-disposition:"):])
					currentPart.disposition = parseDispositionToken(val)
				}
				continue
			}
			lastHeader = "other"
			continue
		}

		if currentPart != nil {
			currentPart.bodyLines = append(currentPart.bodyLines, line)
		}
	}
	if currentPart != nil {
		rawParts = append(rawParts, *currentPart)
	}

	var flattened []rawMIMEPart
	for _, p := range rawParts {
		if p.disposition == "attachment" {
			continue
		}
		if strings.HasPrefix(p.mediaType, "multipart/") {
			subBody := strings.Join(p.bodyLines, "\n")
			subParts := parseMIMEParts(p.contentType, subBody)
			flattened = append(flattened, subParts...)
		} else {
			flattened = append(flattened, p)
		}
	}
	return flattened
}

func decodeEmailSnippet(rawHeaders, rawBody string) string {
	if strings.TrimSpace(rawBody) == "" {
		return ""
	}
	topEncoding := extractTransferEncoding(rawHeaders)
	topContentType := extractContentType(rawHeaders)
	topDisposition := extractDisposition(rawHeaders)
	topMediaType, _ := parseMediaType(topContentType)
	topDispToken := parseDispositionToken(topDisposition)

	boundary := extractMIMEBoundary(rawHeaders)
	isMultipart := boundary != ""

	if !isMultipart {
		if topDispToken == "attachment" {
			return ""
		}
		if topMediaType != "" && !strings.HasPrefix(topMediaType, "text/") {
			return ""
		}
		decoded := decodeMIMEText(rawBody, topEncoding, topContentType)
		return StripHTML(CollapseWhitespace(strings.ReplaceAll(decoded, "\n", " ")))
	}

	parts := parseMIMEParts(rawHeaders, rawBody)
	var chosen *rawMIMEPart

	// 1. Prefer inline text/plain (exclude attachments)
	for i := range parts {
		if parts[i].mediaType == "text/plain" &&
			parts[i].disposition != "attachment" &&
			len(parts[i].bodyLines) > 0 {
			chosen = &parts[i]
			break
		}
	}
	// 2. Fall back to inline text/html (exclude attachments)
	if chosen == nil {
		for i := range parts {
			if parts[i].mediaType == "text/html" &&
				parts[i].disposition != "attachment" &&
				len(parts[i].bodyLines) > 0 {
				chosen = &parts[i]
				break
			}
		}
	}
	// 3. Fall back to any non-attachment text/* part
	if chosen == nil {
		for i := range parts {
			if strings.HasPrefix(parts[i].mediaType, "text/") &&
				parts[i].disposition != "attachment" &&
				len(parts[i].bodyLines) > 0 {
				chosen = &parts[i]
				break
			}
		}
	}
	if chosen == nil {
		return ""
	}

	selectedText := strings.Join(chosen.bodyLines, "\n")
	selectedEncoding := chosen.encoding
	if selectedEncoding == "" {
		selectedEncoding = topEncoding
	}

	decoded := decodeMIMEText(selectedText, selectedEncoding, chosen.contentType)
	return StripHTML(CollapseWhitespace(strings.ReplaceAll(decoded, "\n", " ")))
}

func unfoldHeaders(headers string) []string {
	var unfolded []string
	for _, rawLine := range strings.Split(strings.ReplaceAll(headers, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimRight(rawLine, "\r\n")
		if (strings.HasPrefix(rawLine, " ") || strings.HasPrefix(rawLine, "\t")) && len(unfolded) > 0 {
			unfolded[len(unfolded)-1] += " " + strings.TrimSpace(trimmed)
		} else {
			unfolded = append(unfolded, trimmed)
		}
	}
	return unfolded
}

func extractTransferEncoding(headers string) string {
	for _, line := range unfoldHeaders(headers) {
		lower := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(lower, "content-transfer-encoding:") {
			return strings.TrimSpace(strings.TrimPrefix(lower, "content-transfer-encoding:"))
		}
	}
	return ""
}

func extractContentType(headers string) string {
	for _, line := range unfoldHeaders(headers) {
		lower := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(lower, "content-type:") {
			return strings.TrimSpace(strings.TrimPrefix(lower, "content-type:"))
		}
	}
	return ""
}

func extractDisposition(headers string) string {
	for _, line := range unfoldHeaders(headers) {
		lower := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(lower, "content-disposition:") {
			return strings.TrimSpace(strings.TrimPrefix(lower, "content-disposition:"))
		}
	}
	return ""
}

func extractMIMEBoundary(headers string) string {
	for _, line := range unfoldHeaders(headers) {
		lower := strings.ToLower(line)
		if idx := strings.Index(lower, "boundary="); idx != -1 {
			val := strings.TrimSpace(line[idx+9:])
			if strings.HasPrefix(val, "\"") {
				val = strings.TrimPrefix(val, "\"")
				if endIdx := strings.Index(val, "\""); endIdx != -1 {
					return val[:endIdx]
				}
				return val
			}
			if endIdx := strings.IndexAny(val, "; \t\r\n"); endIdx != -1 {
				return val[:endIdx]
			}
			return strings.TrimSpace(val)
		}
	}
	return ""
}

func applyTransferEncoding(text, encoding string) string {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		raw, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(text)))
		if err == nil && len(raw) > 0 {
			return string(raw)
		}
		if len(raw) > 0 {
			return string(raw)
		}
	case "base64":
		cleaned := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, text)
		// Trim trailing incomplete base64 quartet caused by bounded 2KB fetch.
		if rem := len(cleaned) % 4; rem != 0 {
			cleaned = cleaned[:len(cleaned)-rem]
		}
		if cleaned != "" {
			if raw, err := base64.StdEncoding.DecodeString(cleaned); err == nil {
				return string(raw)
			}
		}
	}
	return text
}

// decodeMIMEText converts a declared MIME charset after transfer decoding.
func decodeMIMEText(body, transfer, contentType string) string {
	text := applyTransferEncoding(body, transfer)
	_, params, err := mime.ParseMediaType(contentType)
	if err == nil {
		if charset := strings.TrimSpace(params["charset"]); charset != "" {
			if decoder, err := ianaindex.MIME.Encoding(charset); err == nil && decoder != nil {
				if raw, err := decoder.NewDecoder().Bytes([]byte(text)); err == nil {
					text = string(raw)
				}
			}
		}
	}
	return strings.ToValidUTF8(text, "")
}

// The implicit-TLS default keeps historical IDs. Other ports get distinct namespaces.
func imapMailboxIdentity(cfg config.IMAPConfig) string {
	host := cfg.Host
	if cfg.Port != 0 && cfg.Port != 993 {
		host = net.JoinHostPort(host, fmt.Sprint(cfg.Port))
	}
	return host + "|" + cfg.Username + "|" + cfg.Mailbox
}

// Buffer only the requested preview, and drain at most 1 MiB from a noncompliant server.
func readIMAPLiteral(r io.Reader) ([]byte, error) {
	const limit = 1 << 20
	preview, err := io.ReadAll(io.LimitReader(r, 2048))
	if err != nil {
		return nil, err
	}
	discarded, err := io.Copy(io.Discard, io.LimitReader(r, limit-int64(len(preview))+1))
	if err != nil {
		return nil, err
	}
	if int64(len(preview))+discarded > limit {
		return nil, fmt.Errorf("body section exceeds 1 MiB")
	}
	return preview, nil
}

func withIMAPDeadline(ctx context.Context, fallback time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, fallback)
}

// Closing the transport precedes draining the pending literal, which releases the decoder.
func abortIMAPFetch(conn io.Closer, fetch *imapclient.FetchCommand) {
	_ = conn.Close()
	_ = fetch.Close()
}
