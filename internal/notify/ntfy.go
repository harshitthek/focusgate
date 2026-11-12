package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"focusgate/internal/config"
	"focusgate/internal/ingest"
)

// NtfyPublisher sends push notifications to ntfy (Android Redmi Note 11 5G & desktop).
type NtfyPublisher struct {
	cfg    config.NtfyConfig
	client *http.Client
}

// NewNtfyPublisher returns a publisher backed by a single pooled HTTP client.
func NewNtfyPublisher(cfg config.NtfyConfig) *NtfyPublisher {
	return &NtfyPublisher{
		cfg: cfg,
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				// 301, 302, and 303 switch POST to GET, dropping the notification body.
				if req.Response != nil && (req.Response.StatusCode == http.StatusMovedPermanently ||
					req.Response.StatusCode == http.StatusFound ||
					req.Response.StatusCode == http.StatusSeeOther) {
					return fmt.Errorf("ntfy redirect %d rejected to prevent method change from POST to GET", req.Response.StatusCode)
				}
				return nil
			},
		},
	}
}

// Message describes a notification payload sent to ntfy.
type Message struct {
	Title    string
	Body     string
	Priority int // 1 (min) .. 5 (max)
	Tags     string
	ClickURL string
	Actions  string
}

// Publish delivers a notification to the configured ntfy topic.
func (p *NtfyPublisher) Publish(ctx context.Context, msg Message) error {
	if !p.cfg.Enabled {
		return nil
	}
	if strings.TrimSpace(p.cfg.Topic) == "" {
		return errors.New("ntfy topic is required when ntfy is enabled")
	}
	endpoint := strings.TrimRight(p.cfg.Server, "/") + "/" + strings.TrimLeft(p.cfg.Topic, "/")
	bodyText := truncateBytesUTF8(ingest.TruncateRunes(strings.TrimSpace(msg.Body), 1200), 4000)
	if bodyText == "" {
		bodyText = truncateBytesUTF8(msg.Title, 4000)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(bodyText))
	if err != nil {
		return err
	}
	if p.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	}
	if msg.Title != "" {
		req.Header.Set("X-Title", sanitizeHeaderValue(ingest.TruncateRunes(msg.Title, 140)))
	}
	prio := msg.Priority
	if prio < 1 || prio > 5 {
		prio = 3
	}
	req.Header.Set("X-Priority", strconv.Itoa(prio))
	if msg.Tags != "" {
		req.Header.Set("X-Tags", sanitizeHeaderValue(msg.Tags))
	}
	if msg.ClickURL != "" {
		req.Header.Set("X-Click", sanitizeHeaderValue(msg.ClickURL))
	}
	if msg.Actions != "" {
		req.Header.Set("X-Actions", sanitizeHeaderValue(msg.Actions))
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy publish: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy HTTP %d", resp.StatusCode)
	}
	return nil
}

func sanitizeHeaderValue(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		if r == '\t' {
			return r
		}
		// Strip NUL, DEL, and ASCII control characters rejected by Go's HTTP transport.
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func truncateBytesUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	var total int
	for i, r := range s {
		rlen := utf8.RuneLen(r)
		if total+rlen > maxBytes {
			return s[:i]
		}
		total += rlen
	}
	return s
}
