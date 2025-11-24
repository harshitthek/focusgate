package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	if !p.cfg.Enabled || strings.TrimSpace(p.cfg.Topic) == "" {
		return nil
	}
	endpoint := strings.TrimRight(p.cfg.Server, "/") + "/" + strings.TrimLeft(p.cfg.Topic, "/")
	bodyText := ingest.TruncateRunes(strings.TrimSpace(msg.Body), 1200)
	if bodyText == "" {
		bodyText = msg.Title
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
		return r
	}, s)
}
