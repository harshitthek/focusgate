package ingest

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"mime/quotedprintable"
	"net"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"focusgate/internal/config"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const maxIMAPMessagesPerPoll = 20

// FetchIMAPUnread connects over implicit TLS with strict context/deadline bounds,
// examines the mailbox read-only, tracks UIDVALIDITY, and fetches bounded decoded
// previews for unseen messages newer than lastUID.
func FetchIMAPUnread(ctx context.Context, cfg config.IMAPConfig, lastUID uint32, prevUIDValidity uint32, maxSnippet int) (items []IncomingItem, nextUID uint32, uidValidity uint32, err error) {
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

	client := imapclient.New(tlsConn, nil)
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
	windowSize := uint32(maxIMAPMessagesPerPoll * 50)
	if windowSize == 0 {
		windowSize = 1000
	}
	if effectiveLastUID == 0 {
		// On initial sync or UIDVALIDITY rollover, inspect unread messages within the last 48h.
		// Bound server-side search to recent UIDs [windowStart, maxPossibleUID] when known.
		criteria.Since = time.Now().Add(-48 * time.Hour)
		if maxPossibleUID > 0 {
			var windowStart uint32 = 1
			if maxPossibleUID > windowSize {
				windowStart = maxPossibleUID - windowSize + 1
			}
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
		windowEnd = effectiveLastUID + windowSize
		if maxPossibleUID > 0 && windowEnd > maxPossibleUID {
			windowEnd = maxPossibleUID
		}
		if windowEnd >= effectiveLastUID+1 {
			var uidSet imap.UIDSet
			uidSet.AddRange(imap.UID(effectiveLastUID+1), imap.UID(windowEnd))
			criteria.UID = []imap.UIDSet{uidSet}
		} else {
			return nil, effectiveLastUID, uidValidity, nil
		}
	}

	searchData, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, effectiveLastUID, uidValidity, fmt.Errorf("imap search %s: %w", cfg.Name, err)
	}

	allUIDs := searchData.AllUIDs()
	targetUIDs, highest := selectIMAPBatchUIDs(allUIDs, effectiveLastUID, maxIMAPMessagesPerPoll)
	if len(targetUIDs) == 0 {
		if windowEnd > effectiveLastUID {
			advanceTo := windowEnd
			if maxPossibleUID > 0 && advanceTo > maxPossibleUID {
				advanceTo = maxPossibleUID
			}
			return nil, advanceTo, uidValidity, nil
		}
		return nil, highest, uidValidity, nil
	}

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
	msgs, err := client.Fetch(uidSet, fetchOpts).Collect()
	if err != nil {
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

	mailboxIdentity := cfg.Host + "|" + cfg.Username + "|" + cfg.Mailbox
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
		item.Normalize(maxSnippet)
		items = append(items, item)
	}
	return items, highest, uidValidity, nil
}

// selectIMAPBatchUIDs picks up to limit oldest unseen UIDs above lastUID in
// ascending order and advances the watermark only to the last selected UID.
func selectIMAPBatchUIDs(allUIDs []imap.UID, lastUID uint32, limit int) ([]imap.UID, uint32) {
	var pending []imap.UID
	for _, uid := range allUIDs {
		if uint32(uid) > lastUID {
			pending = append(pending, uid)
		}
	}
	if len(pending) == 0 {
		return nil, lastUID
	}
	slices.Sort(pending)
	if limit > 0 && len(pending) > limit {
		pending = pending[:limit]
	}
	return pending, uint32(pending[len(pending)-1])
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
				currentPart = &rawMIMEPart{}
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
		decoded := applyTransferEncoding(rawBody, topEncoding)
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

	decoded := applyTransferEncoding(selectedText, selectedEncoding)
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
		if err == nil && len(raw) > 0 && utf8.Valid(raw) {
			return string(raw)
		}
		if len(raw) > 0 {
			return strings.ToValidUTF8(string(raw), "")
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
				return strings.ToValidUTF8(string(raw), "")
			}
		}
	}
	return text
}
