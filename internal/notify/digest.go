package notify

import (
	"context"
	"fmt"
	"strings"

	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

// Dispatcher fans out immediate notifications and scheduled digests to ntfy and Windows toast.
type Dispatcher struct {
	cfg  config.Config
	ntfy *NtfyPublisher
}

// NewDispatcher constructs a Dispatcher from runtime configuration.
func NewDispatcher(cfg config.Config) *Dispatcher {
	return &Dispatcher{
		cfg:  cfg,
		ntfy: NewNtfyPublisher(cfg.Notify.Ntfy),
	}
}

// PushItem delivers an immediate notification for a high-priority item.
func (d *Dispatcher) PushItem(ctx context.Context, item store.StoredItem) error {
	tag := tagForItem(item)
	clickURL := item.URL
	if clickURL == "" {
		clickURL = d.cfg.Server.PublicURL
	}
	body := item.Body
	if body == "" {
		body = item.DecisionReason
	}
	if item.Sender != "" && !strings.Contains(body, item.Sender) {
		body = fmt.Sprintf("[%s] %s", item.Sender, body)
	}

	var firstErr error
	if d.cfg.Notify.Ntfy.Enabled {
		msg := Message{
			Title:    item.Title,
			Body:     body,
			Priority: item.Priority,
			Tags:     tag,
			ClickURL: clickURL,
		}
		if err := d.ntfy.Publish(ctx, msg); err != nil {
			firstErr = err
		}
	}

	if d.cfg.Notify.DesktopWindows.Enabled {
		if err := SendDesktopToast(ctx, item.Title, body); err != nil && firstErr == nil {
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
	if !d.cfg.Notify.Ntfy.Enabled && !d.cfg.Notify.DesktopWindows.Enabled {
		return true, nil
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
	if d.cfg.Notify.Ntfy.Enabled {
		if err := d.ntfy.Publish(ctx, Message{
			Title:    title,
			Body:     body,
			Priority: 2,
			Tags:     "newspaper",
			ClickURL: d.cfg.Server.PublicURL,
		}); err != nil {
			firstErr = err
		} else {
			deliveredAny = true
		}
	}
	if d.cfg.Notify.DesktopWindows.Enabled {
		if err := SendDesktopToast(ctx, title, fmt.Sprintf("%d queued updates ready to review", len(items))); err != nil {
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
	var sent []string
	var firstErr error

	title := "FocusGate Test Alert"
	body := "Notification channel is healthy and reachable."

	if d.cfg.Notify.Ntfy.Enabled {
		if err := d.ntfy.Publish(ctx, Message{
			Title:    title,
			Body:     body,
			Priority: 3,
			Tags:     "white_check_mark",
			ClickURL: d.cfg.Server.PublicURL,
		}); err != nil {
			firstErr = err
		} else {
			sent = append(sent, "ntfy")
		}
	}

	if d.cfg.Notify.DesktopWindows.Enabled {
		if err := SendDesktopToast(ctx, title, body); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			sent = append(sent, "desktop")
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
