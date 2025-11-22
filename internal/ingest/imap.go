package ingest

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"focusgate/internal/config"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const maxIMAPMessagesPerPoll = 20

// FetchIMAPUnread connects over implicit TLS with strict context/deadline bounds,
// examines the mailbox read-only, and fetches envelopes plus a 2 KB BODY.PEEK text
// preview for unseen messages newer than lastUID.
func FetchIMAPUnread(ctx context.Context, cfg config.IMAPConfig, lastUID uint32, maxSnippet int) (items []IncomingItem, nextUID uint32, err error) {
	if !cfg.Enabled {
		return nil, lastUID, nil
	}
	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))

	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(30 * time.Second)
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second}
	rawConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, lastUID, fmt.Errorf("imap dial %s: %w", addr, err)
	}
	_ = rawConn.SetDeadline(deadline)

	tlsConn := tls.Client(rawConn, &tls.Config{
		ServerName: cfg.Host,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = tlsConn.Close()
		return nil, lastUID, fmt.Errorf("imap tls handshake %s: %w", addr, err)
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
		return nil, lastUID, fmt.Errorf("imap login %s: %w", cfg.Name, err)
	}
	defer func() { _ = client.Logout().Wait() }()

	if _, err := client.Select(cfg.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return nil, lastUID, fmt.Errorf("imap select %s: %w", cfg.Mailbox, err)
	}

	criteria := &imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
	}
	if lastUID == 0 {
		// On initial sync, inspect only recent unread messages (last 48h) so a cluttered inbox never floods.
		criteria.Since = time.Now().Add(-48 * time.Hour)
	}

	searchData, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, lastUID, fmt.Errorf("imap search %s: %w", cfg.Name, err)
	}

	allUIDs := searchData.AllUIDs()
	targetUIDs, highest := selectIMAPBatchUIDs(allUIDs, lastUID, maxIMAPMessagesPerPoll)
	if len(targetUIDs) == 0 {
		return nil, highest, nil
	}

	var uidSet imap.UIDSet
	for _, uid := range targetUIDs {
		uidSet.AddNum(uid)
	}

	bodySection := &imap.FetchItemBodySection{
		Specifier: imap.PartSpecifierText,
		Peek:      true,
		Partial:   &imap.SectionPartial{Offset: 0, Size: 2048},
	}
	fetchOpts := &imap.FetchOptions{
		Envelope:    true,
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{bodySection},
	}
	msgs, err := client.Fetch(uidSet, fetchOpts).Collect()
	if err != nil {
		return nil, lastUID, fmt.Errorf("imap fetch %s: %w", cfg.Name, err)
	}

	items = make([]IncomingItem, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Envelope == nil {
			continue
		}
		sender := formatIMAPSender(msg.Envelope.From)
		rawSnippet := string(msg.FindBodySection(bodySection))
		preview := cleanEmailSnippet(rawSnippet)
		extID := fmt.Sprintf("imap:%s:%d", cfg.Name, msg.UID)
		if msg.Envelope.MessageID != "" {
			extID = "imap:" + strings.TrimSpace(msg.Envelope.MessageID)
		}
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
			PublishedAt: published.UTC(),
		}
		item.Normalize(maxSnippet)
		items = append(items, item)
	}
	return items, highest, nil
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
// BODY.PEEK[TEXT] responses without pulling a heavy MIME parser into RAM.
func cleanEmailSnippet(raw string) string {
	if raw == "" {
		return ""
	}
	lines := strings.Split(raw, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(trimmed, "--") && len(trimmed) > 8 && !strings.Contains(trimmed, " ") {
			continue
		}
		if strings.HasPrefix(lower, "content-type:") ||
			strings.HasPrefix(lower, "content-transfer-encoding:") ||
			strings.HasPrefix(lower, "content-disposition:") {
			continue
		}
		filtered = append(filtered, trimmed)
	}
	return StripHTML(strings.Join(filtered, " "))
}
