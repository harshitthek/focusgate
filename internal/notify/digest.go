package notify

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

// Dispatcher fans out immediate notifications and scheduled digests to ntfy, Windows toast, Slack, and Matrix.
type Dispatcher struct {
	mu     sync.RWMutex
	cfg    config.Config
	ntfy   *NtfyPublisher
	slack  *SlackPublisher
	matrix *MatrixPublisher
}

// NewDispatcher constructs a Dispatcher from runtime configuration.
func NewDispatcher(cfg config.Config) *Dispatcher {
	return &Dispatcher{
		cfg:    cfg,
		ntfy:   NewNtfyPublisher(cfg.Notify.Ntfy),
		slack:  NewSlackPublisher(cfg.Notify.Slack),
		matrix: NewMatrixPublisher(cfg.Notify.Matrix),
	}
}

// UpdateNotifyConfig hot-reloads notification channel settings and mention filters.
func (d *Dispatcher) UpdateNotifyConfig(nc config.NotifyConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg.Notify = nc
	d.ntfy = NewNtfyPublisher(nc.Ntfy)
	d.slack = NewSlackPublisher(nc.Slack)
	d.matrix = NewMatrixPublisher(nc.Matrix)
}

// NotifyConfig returns the active notification configuration.
func (d *Dispatcher) NotifyConfig() config.NotifyConfig {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg.Notify
}

// PushItem delivers an immediate notification for a high-priority item.
func (d *Dispatcher) PushItem(ctx context.Context, item store.StoredItem) error {
	d.mu.RLock()
	cfg := d.cfg
	ntfyPub := d.ntfy
	slackPub := d.slack
	matrixPub := d.matrix
	d.mu.RUnlock()

	// If OnlyMentions is enabled, only push when the item represents an explicit mention,
	// assignment, or review request (unless P5 critical outage).
	if cfg.Notify.OnlyMentions && item.Priority < 5 {
		if !IsMention(item, cfg.Notify.MentionHandles) {
			return nil
		}
	}

	tag := tagForItem(item)
	clickURL := item.URL
	if clickURL == "" {
		clickURL = cfg.Server.PublicURL
	}
	body := item.Body
	if body == "" {
		body = item.DecisionReason
	}
	if item.Sender != "" && !strings.Contains(body, item.Sender) {
		body = fmt.Sprintf("[%s] %s", item.Sender, body)
	}

	var firstErr error
	if cfg.Notify.Ntfy.Enabled {
		msg := Message{
			Title:    item.Title,
			Body:     body,
			Priority: item.Priority,
			Tags:     tag,
			ClickURL: clickURL,
		}
		if err := ntfyPub.Publish(ctx, msg); err != nil {
			firstErr = err
		}
	}

	if cfg.Notify.DesktopWindows.Enabled {
		if err := SendDesktopToast(ctx, item.Title, body); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if cfg.Notify.Slack.Enabled && slackPub != nil {
		if err := slackPub.Publish(ctx, item, cfg.Server.PublicURL); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if cfg.Notify.Matrix.Enabled && matrixPub != nil {
		if err := matrixPub.Publish(ctx, item, cfg.Server.PublicURL); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// SendDigest formats queued items into a concise summary and dispatches it once.
// It returns delivered=true if at least one enabled channel succeeded (or no
// delivery channels are enabled), along with the first channel error if any failed.
func (d *Dispatcher) SendDigest(ctx context.Context, items []store.StoredItem) (bool, error) {
	if len(items) == 0 {
		return false, nil
	}

	d.mu.RLock()
	cfg := d.cfg
	ntfyPub := d.ntfy
	slackPub := d.slack
	matrixPub := d.matrix
	d.mu.RUnlock()

	slackEnabled := cfg.Notify.Slack.Enabled && slackPub != nil
	matrixEnabled := cfg.Notify.Matrix.Enabled && matrixPub != nil
	hasAny := cfg.Notify.Ntfy.Enabled || cfg.Notify.DesktopWindows.Enabled || slackEnabled || matrixEnabled
	if !hasAny {
		return true, nil
	}

	if cfg.Notify.OnlyMentions {
		filtered := make([]store.StoredItem, 0, len(items))
		for _, it := range items {
			if it.Priority >= 5 || IsMention(it, cfg.Notify.MentionHandles) {
				filtered = append(filtered, it)
			}
		}
		items = filtered
		if len(items) == 0 {
			return true, nil
		}
	}

	var b strings.Builder
	maxListed := len(items)
	if maxListed > 10 {
		maxListed = 10
	}
	for i := 0; i < maxListed; i++ {
		it := items[i]
		prefix := "•"
		if it.Uncertain {
			prefix = "• [review]"
		}
		fmt.Fprintf(&b, "%s [%s] %s\n", prefix, it.SourceName, ingest.TruncateRunes(it.Title, 80))
	}
	if len(items) > maxListed {
		fmt.Fprintf(&b, "+ %d more items in FocusGate PWA", len(items)-maxListed)
	}

	title := fmt.Sprintf("FocusGate Digest (%d items)", len(items))
	body := strings.TrimSpace(b.String())

	var firstErr error
	deliveredAny := false
	if cfg.Notify.Ntfy.Enabled {
		if err := ntfyPub.Publish(ctx, Message{
			Title:    title,
			Body:     body,
			Priority: 2,
			Tags:     "newspaper",
			ClickURL: cfg.Server.PublicURL,
		}); err != nil {
			firstErr = err
		} else {
			deliveredAny = true
		}
	}
	if cfg.Notify.DesktopWindows.Enabled {
		if err := SendDesktopToast(ctx, title, fmt.Sprintf("%d queued updates ready to review", len(items))); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			deliveredAny = true
		}
	}
	if slackEnabled {
		if err := slackPub.PublishDigest(ctx, title, body, cfg.Server.PublicURL); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			deliveredAny = true
		}
	}
	if matrixEnabled {
		if err := matrixPub.PublishDigest(ctx, title, body, cfg.Server.PublicURL); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			deliveredAny = true
		}
	}
	return deliveredAny, firstErr
}

// SendTestAlert dispatches a diagnostic notification to all enabled delivery channels
// and returns the list of channel names that succeeded alongside the first error, if any.
func (d *Dispatcher) SendTestAlert(ctx context.Context) ([]string, error) {
	d.mu.RLock()
	cfg := d.cfg
	ntfyPub := d.ntfy
	slackPub := d.slack
	matrixPub := d.matrix
	d.mu.RUnlock()

	var sent []string
	var firstErr error

	title := "FocusGate Test Alert"
	body := "Notification channel is healthy and reachable."

	if cfg.Notify.Ntfy.Enabled && ntfyPub != nil {
		if err := ntfyPub.Publish(ctx, Message{
			Title:    title,
			Body:     body,
			Priority: 3,
			Tags:     "white_check_mark",
			ClickURL: cfg.Server.PublicURL,
		}); err != nil {
			firstErr = err
		} else {
			sent = append(sent, "ntfy")
		}
	}

	if cfg.Notify.DesktopWindows.Enabled {
		if err := SendDesktopToast(ctx, title, body); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			sent = append(sent, "desktop")
		}
	}

	if cfg.Notify.Slack.Enabled && slackPub != nil {
		if err := slackPub.SendTest(ctx, cfg.Notify.Slack.WebhookURL, cfg.Notify.Slack.MentionHandle); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			sent = append(sent, "slack")
		}
	}

	if cfg.Notify.Matrix.Enabled && matrixPub != nil {
		if err := matrixPub.SendTest(ctx, cfg.Notify.Matrix.WebhookURL, cfg.Notify.Matrix.MentionHandle); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			sent = append(sent, "matrix")
		}
	}

	return sent, firstErr
}

func tagForItem(item store.StoredItem) string {
	if item.Priority >= 5 {
		return "rotating_light"
	}
	switch item.SourceType {
	case "github":
		return "octocat"
	case "imap":
		return "email"
	case "rss":
		return "newspaper"
	default:
		return "bell"
	}
}
